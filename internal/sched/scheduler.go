// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package sched

// Rules for Stage implementations (in addition to the Stage interface docs):
//
//   - The database has a single connection (SPEC §3.3 SetMaxOpenConns(1)).
//     Inside a Store.Tx callback a stage must not call the LLM client
//     (APIPool.Acquire reads the budget ledger) and must not wait for
//     anything that itself needs the database (another goroutine's query,
//     a progress flush, a channel fed by such code): it would deadlock.
//     Collect results in memory and open the transaction only to write them.
//   - The progress callback never blocks and never touches the database; it
//     may be called from any goroutine, as often as convenient.
//   - While a job runs the scheduler is the only writer of job.stats: the llm
//     client reports tokens with ReportUsage, stages change other fields with
//     UpdateStats (StatsSink, stage.go). Never call Store.SetJobStats for a
//     running job. Stats are persisted and added to the daily usage ledger
//     every flush interval and once more before the final status.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"lectureflow/internal/config"
	"lectureflow/internal/db"
	"lectureflow/internal/devices"
	"lectureflow/internal/model"
	"lectureflow/internal/settings"
	"lectureflow/internal/state"
)

// Options configures a Scheduler. Zero durations take the SPEC defaults.
type Options struct {
	Store   db.Store
	Config  *config.Config
	Stages  map[string]Stage // keyed by Stage.Name(); more can be added with Register
	Events  Events           // nil = NopEvents
	DataDir string

	// Settings is the source of the current settings (v1.1 A5). nil = settings.Open(Store, Config).
	Settings *settings.Service
	// Pool is the API pool shared with the llm client. nil = NewPool(Config, Store, {Now: Now}).
	Pool *Pool
	// GPUQuery reads GPU memory; nil = nvidia-smi (Config.NvidiaSMI).
	GPUQuery GPUQueryFunc
	// Now is the clock for retry backoff and the incremental-translate debounce (default time.Now).
	Now func() time.Time

	ScanInterval        time.Duration   // default 1s (SPEC §5)
	VRAMSampleInterval  time.Duration   // default 2s (SPEC §17.2)
	RetryBackoff        []time.Duration // default 10s, 60s (SPEC §4.2)
	CPUSlots            int             // default NumCPU/2, at least 1 (SPEC §5)
	CancelWait          time.Duration   // how long Cancel waits for a running job to stop (default 5s)
	ProgressMinInterval time.Duration   // progress/usage flush interval (default 500ms, SPEC §8.2)
	IncrementalDebounce time.Duration   // quiet time before an incremental translate starts (default 10s, NOTES.md)
	DeleteWait          time.Duration   // how long a forced delete waits for running jobs (default DefaultDeleteWait, A7)
	// FilenameRuleDebounce is the quiet time after the last RequestFilenameRule
	// of a course before its trigger is evaluated (v2 ruling 2; default 5s).
	FilenameRuleDebounce time.Duration

	// SPEC-v2 I8/I9 (phase 3b freeze, asr_devices.go). All optional: with
	// Devices nil the scheduler behaves as before 3b (one GPU pool, the
	// legacy ASR settings as the single cuda:0 device).
	Devices DeviceSource // the device registry (devices.Registry)
	// Power reads the power state on every scan; nil = always on AC power.
	Power func() devices.Power
	// MemAvailMB reads the available system memory (Intel devices'
	// admission); nil = devices.AvailableMemoryMB.
	MemAvailMB func() (int, error)
	// DeviceTest runs one device self-test (stages.Deps.DeviceTest, wired
	// by cmd); nil = StartDeviceTest refuses with ErrDeviceTestUnavailable.
	DeviceTest DeviceTestFunc

	// Power management (v2-step20, power.go). KeepAwake is called with true
	// when the first pipeline asr job or self-test starts running and with
	// false once the last one ended and the next scan started none (so a
	// run requeued by the power policy, or the next queued job, that starts
	// right away keeps it held; at the latest when the loop ends). cmd wires
	// devices.KeepAwake.Set; nil = none. Not called while nothing changes;
	// the calls are serialised.
	KeepAwake func(on bool)

	// SPEC-v3 V14 (remote.go): the Colab backend (nil = none; needs
	// Devices). OnRemote is called when colab:remote enters or leaves the
	// device table, or its label, model or precision change (cmd wires
	// devices.Registry.SetRemote); never with a lock held.
	Remote   RemoteBackend
	OnRemote func(ctx context.Context, present bool, row model.ASRDevice)
}

// GPUStatus is the GPU row of GET /api/system (SPEC §17.1).
type GPUStatus struct {
	OK             bool // false when nvidia-smi could not be read (or fake ASR mode)
	TotalMB        int
	FreeMB         int
	UsedMB         int
	Paused         bool
	SlotsUsed      int
	SlotsMax       int
	EstimatedJobMB int
	Error          string
}

// run is one running job.
type run struct {
	job        model.Job
	stage      Stage
	res        ResourceKind
	cancel     context.CancelFunc
	done       chan struct{}
	userCancel bool // guarded by Scheduler.mu

	// GPU accounting (SPEC §17.2), guarded by Scheduler.mu
	estimateMB  int
	baseUsedMB  int
	sample      bool
	peakMB      int
	computeType string
	asrModel    string // model of the job's asr snapshot: the vram_observed key
	// overlapped: another GPU run was running at some point during this
	// run, so its "peak increase over the pre-start baseline" includes the
	// other run's memory and must not calibrate vram_observed (§17.2).
	// With Options.Devices only runs on the same device count.
	overlapped bool

	// SPEC-v2 I8 (Options.Devices set; asr_devices.go), guarded by Scheduler.mu
	device         string          // the asr device the run uses ("" without Devices)
	devRow         model.ASRDevice // its row at admission
	bind           bool            // auto snapshot: state.BindASRDevice in the start transaction
	memEstimateMB  int             // Intel devices: the admission estimate
	memBaseAvailMB int             // available memory at admission
	memSample      bool
	memPeakMB      int       // largest drop of the available memory since admission
	startedAt      time.Time // set before the run is registered (read under Scheduler.mu)
	scopeID        string    // r.job.ScopeID, likewise

	// Power management (power.go), guarded by Scheduler.mu.
	fixed       bool   // its snapshot is fixed (devices.PolicyRun.Fixed)
	workerMemMB int    // the worker memory budget's estimate (admission)
	powerAction string // devices.PowerMigrate | PowerSuspend once the policy interrupted it
	powerTo     string // PowerMigrate: the device it continues on
	// powerOnBattery: the reading that interrupted it was on battery (a
	// suspension on AC — a CPU run on a machine with a battery — writes no
	// "已拔掉电源" event or notice; backend Gate round 1, fix 7).
	powerOnBattery bool
	// cancelCause cancels the run's context with a cause (ErrPowerMigrate /
	// ErrPowerSuspend); cancel is cancelCause(nil).
	cancelCause context.CancelCauseFunc
	awake       bool // counted in the keep-awake (pipeline asr)

	// progress: the callback only touches these (pmu is never held across I/O)
	pmu    sync.Mutex
	latest float64
	dirty  bool

	// wmu serialises this run's database writes outside Run: progress
	// flushes, usage accounting and the final transition.
	wmu       sync.Mutex
	live      bool // set once start() has the job running and accounted initialised
	finished  bool
	accounted model.JobStats // stats already added to the usage ledger
	lastFlush float64

	// in-memory job.stats (StatsSink); smu is never held across I/O
	smu        sync.Mutex
	stats      model.JobStats
	statsDirty bool
	cfgCost    func(model.JobStats) (float64, bool)
}

// Scheduler moves jobs pending → queued → running → done|failed|cancelled
// under the resource pools of SPEC §5. Every status write goes through
// state.TransitionJob / state.FailJob.
type Scheduler struct {
	st      db.Store
	cfg     *config.Config
	set     *settings.Service // current settings (v1.1 A5); never nil after New
	ev      Events
	pool    *Pool
	dataDir string
	gpuQ    GPUQueryFunc
	now     func() time.Time

	scanInterval   time.Duration
	sampleInterval time.Duration
	backoffs       []time.Duration
	cpuSlots       int
	cancelWait     time.Duration
	flushEvery     time.Duration
	debounce       time.Duration
	deleteWait     time.Duration
	fnDebounce     time.Duration

	// scanMu is held by every scan, by Rerun until the rerun's transaction
	// and file cleanup are complete, and by DeleteLecture until the rows and
	// directories are gone, so no job of that lecture can start in between.
	scanMu sync.Mutex

	mu       sync.Mutex
	stages   map[string]Stage
	running  map[string]*run
	backoff  map[string]time.Time  // queued job id → not before (automatic retry)
	reasons  map[string]string     // queued job id → waiting_for from the last scan
	deferred map[string]bool       // lecture ids whose incremental translate waits for export
	incrReq  map[string]time.Time  // lecture id → last incremental-translate request (debounce)
	fnReq    map[string]*fnRequest // course id → pending filename-rule request (v2, debounce)
	paused   bool
	// dispatchHolds counts the PauseDispatch callers not yet resumed
	// (dispatch_pause.go); > 0 = no scan runs, so nothing is dispatched.
	dispatchHolds int
	vramObs       map[string]int
	lastGPU       GPUStatus
	started       bool
	stopped       bool

	kick       chan struct{}
	baseCtx    context.Context
	baseCancel context.CancelFunc
	loopDone   chan struct{}
	wg         sync.WaitGroup

	// SPEC-v2 I8/I9 (phase 3b; asr_devices.go). Set by New from Options.
	devs     DeviceSource
	powerFn  func() devices.Power
	memAvail func() (int, error)
	devTest  DeviceTestFunc
	// memEstimate is devices.MemEstimateMB (tests replace it). Called by
	// the scan only, never inside a Store transaction.
	memEstimate func(ctx context.Context, st db.Store, dataDir string, d model.ASRDevice) int
	// guarded by mu
	devPaused map[string]bool        // device id → its own pause switch (kv asr_device_paused:{id})
	tests     map[string]*devTestRun // device id → its self-test since the program started
	wanted    map[string]bool        // devices a queued pipeline asr job could use (last scan)
	// wantedOnly: devices a queued pipeline asr job waits for and is its
	// only candidate (last scan): a self-test there is preempted.
	wantedOnly map[string]bool
	// memWait: a queued pipeline asr job waited for room in the worker
	// memory budget in the last scan, one that fits it by itself (no
	// self-test takes a slot meanwhile; backend Gate round 1, fix 2).
	memWait   bool
	power     devices.Power // the last scan's reading
	powerRead bool
	// onBattery mirrors power.OnBatteryPower() of the last reading (set by
	// readPower with power). deviceSlots reads it, also with mu held, so it
	// is atomic rather than guarded by mu (SPEC-v3 V5: mlx:gpu runs one job
	// at a time on battery).
	onBattery atomic.Bool

	// Power management (power.go). keepAwake is Options.KeepAwake. Guarded
	// by awakeMu (which also serialises the calls): awakeN counts the
	// running pipeline asr jobs and self-tests; awakeHeld is the request's
	// state; awakePending: the count returned to 0 and the release waits for
	// the end of the first scan numbered above awakeDropScan (awakeScans
	// numbers the scans). Guarded by mu: notices (the last PowerNoticeMax),
	// and the last scan's power_* waits.
	keepAwake     func(on bool)
	awakeMu       sync.Mutex
	awakeN        int
	awakeHeld     bool
	awakePending  bool
	awakeScans    uint64
	awakeDropScan uint64
	notices       []PowerNotice
	powerWaitAC   int
	powerNoDev    int
	powerHolds    []string

	// SPEC-v3 V14 (remote.go): Options.Remote / OnRemote; rem is guarded by mu.
	remote   RemoteBackend
	onRemote func(ctx context.Context, present bool, row model.ASRDevice)
	rem      remoteState
}

// New builds a scheduler and loads the persisted GPU pause flag (SPEC §17.2).
func New(o Options) (*Scheduler, error) {
	if o.Store == nil || o.Config == nil {
		return nil, errors.New("sched: Store and Config are required")
	}
	s := &Scheduler{
		st: o.Store, cfg: o.Config, ev: o.Events, pool: o.Pool, dataDir: o.DataDir, gpuQ: o.GPUQuery, now: o.Now,
		scanInterval: o.ScanInterval, sampleInterval: o.VRAMSampleInterval, backoffs: o.RetryBackoff,
		cpuSlots: o.CPUSlots, cancelWait: o.CancelWait, flushEvery: o.ProgressMinInterval, debounce: o.IncrementalDebounce,
		deleteWait: o.DeleteWait, fnDebounce: o.FilenameRuleDebounce, stages: map[string]Stage{}, fnReq: map[string]*fnRequest{},
		running: map[string]*run{}, backoff: map[string]time.Time{},
		reasons: map[string]string{}, deferred: map[string]bool{}, incrReq: map[string]time.Time{}, vramObs: map[string]int{},
		kick: make(chan struct{}, 1),
		devs: o.Devices, powerFn: o.Power, memAvail: o.MemAvailMB, devTest: o.DeviceTest, keepAwake: o.KeepAwake,
		remote: o.Remote, onRemote: o.OnRemote,
		memEstimate: memEstimateWith(o.Config), devPaused: map[string]bool{}, tests: map[string]*devTestRun{}, wanted: map[string]bool{},
	}
	if s.memAvail == nil {
		s.memAvail = devices.AvailableMemoryMB
	}
	if s.ev == nil {
		s.ev = NopEvents{}
	}
	if s.now == nil {
		s.now = time.Now
	}
	if o.Settings == nil {
		// Tests (and older callers) that only set Config: the legacy
		// config.toml fields are imported once (A5.1); provider ids are the
		// config keys, e.g. "deepseek".
		set, err := settings.Open(context.Background(), o.Store, o.Config)
		if err != nil {
			return nil, err
		}
		o.Settings = set
	}
	s.set = o.Settings
	if s.pool == nil {
		if o.Settings != nil {
			s.pool = NewPoolWithSettings(o.Store, o.Settings, PoolOptions{Now: s.now})
		} else {
			s.pool = NewPool(o.Config, o.Store, PoolOptions{Now: s.now})
		}
	}
	if s.gpuQ == nil {
		s.gpuQ = NvidiaSMIQuery(o.Config.NvidiaSMI)
	}
	if s.scanInterval <= 0 {
		s.scanInterval = time.Second
	}
	if s.sampleInterval <= 0 {
		s.sampleInterval = 2 * time.Second
	}
	if len(s.backoffs) == 0 {
		for _, sec := range model.RetryBackoffSeconds {
			s.backoffs = append(s.backoffs, time.Duration(sec)*time.Second)
		}
	}
	if s.cpuSlots <= 0 {
		s.cpuSlots = runtime.NumCPU() / 2
		if s.cpuSlots < 1 {
			s.cpuSlots = 1
		}
	}
	if s.cancelWait <= 0 {
		s.cancelWait = 5 * time.Second
	}
	if s.flushEvery <= 0 {
		s.flushEvery = 500 * time.Millisecond
	}
	if s.debounce <= 0 {
		s.debounce = 10 * time.Second
	}
	if s.deleteWait <= 0 {
		s.deleteWait = DefaultDeleteWait
	}
	if s.fnDebounce <= 0 {
		s.fnDebounce = DefaultFilenameRuleDebounce
	}
	for name, stg := range o.Stages {
		s.stages[name] = stg
	}
	v, ok, err := o.Store.GetKV(context.Background(), model.KVGPUPaused)
	if err != nil {
		return nil, err
	}
	s.paused = ok && (v == "1" || v == "true")
	if err := s.loadDevicePauses(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

// Register adds a stage (keyed by its Name). Call before Start.
func (s *Scheduler) Register(stg Stage) {
	s.mu.Lock()
	s.stages[stg.Name()] = stg
	s.mu.Unlock()
}

// Pool returns the API pool (to hand to the llm client).
func (s *Scheduler) Pool() *Pool { return s.pool }

// Start launches the scan loop and the progress/usage flusher. Run
// state.Recover before Start (SPEC §4.7). Start first resumes incremental
// translations that a restart may have lost (lectures whose translate is
// done but still have stale or untranslated sentences), and the glossary
// review flags whose review ended unseen (v2, see reviewEnded).
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("sched: already started")
	}
	s.started = true
	s.baseCtx, s.baseCancel = context.WithCancel(ctx)
	s.loopDone = make(chan struct{})
	s.mu.Unlock()
	if err := s.resumeIncremental(s.baseCtx); err != nil {
		slog.Error("resume incremental translate", "err", err)
	}
	if err := s.resumeReviewFlags(s.baseCtx); err != nil {
		slog.Error("resume glossary review flags", "err", err)
	}
	s.wg.Add(1)
	go s.flusher()
	go s.loop()
	return nil
}

// Stop ends the loop and cancels running jobs as a shutdown: their status
// stays running and startup recovery (§4.7) handles them on the next start.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.mu.Unlock()
	s.baseCancel()
	<-s.loopDone
	s.wg.Wait()
}

// Kick triggers a scan now (call after API operations).
func (s *Scheduler) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *Scheduler) loop() {
	defer close(s.loopDone)
	defer s.settleAwake() // no scan decides a deferred release any more
	t := time.NewTicker(s.scanInterval)
	defer t.Stop()
	for {
		s.scan()
		select {
		case <-s.baseCtx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
	}
}

// ---------- scanning ----------

type scanState struct {
	gpuRead bool
	gpu     GPUMem
	gpuErr  error

	// SPEC-v2 I8 and power management (asr_devices.go), read once per scan
	asrRead bool
	policyBase
	// switchHold is the first power_switch_pending of the scan's FIFO
	// order (lead ruling 9.3, devices.SwitchHold).
	switchHold string
	memRead    bool
	memMB      int
	memErr     error
	memEst     map[string]int  // memEstimate per device|model|precision
	wanted     map[string]bool // devices a queued asr job that did not start could use
	wantedOnly map[string]bool // ... and is the only device that job can use
	// memWait: a queued asr job did not start for the worker memory
	// budget (and fits it by itself); memPreempt: the devices of the
	// self-tests whose memory alone kept a job from starting (backend Gate
	// round 1, fix 2).
	memWait    bool
	memPreempt map[string]bool

	// SPEC-v3 V14 (remote.go): the Colab state of the scan, the planned
	// queue (audio seconds of the lectures that wait and could go to
	// Colab), how many such lectures there are, whether a job was sent to
	// Colab in this scan, and a shutdown's hold.
	remote         remoteScan
	remoteQueue    []float64
	remoteEligible int
	remoteTaken    bool
	remoteHold     bool
}

func (s *Scheduler) scan() {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	// The keep-awake's deferred release is decided when the scan ends, on
	// every return path (power.go, awake).
	defer s.awakeScanEnd(s.awakeScanBegin())
	ctx := s.baseCtx
	if ctx.Err() != nil || s.dispatchHeld() {
		return
	}
	s.readPower()
	s.processIncremental(ctx)
	s.retryDeferred(ctx)
	s.processFilenameRules(ctx)
	jobs, err := s.st.ListCurrentJobsByStatus(ctx, model.StatusPending, model.StatusQueued)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("scheduler scan", "err", err)
		}
		return
	}
	reasons := map[string]string{}
	queued := map[string]bool{}
	var sc scanState
	sc.remote = s.readRemote(ctx) // SPEC-v3 V14
	s.mu.Lock()
	sc.remoteHold = s.rem.hold
	s.mu.Unlock()
	s.enforceTestPolicy() // after the power reading: on battery only the battery device is tested
	var waitAC, noDev int
	holds := []string{}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return
		}
		depsOK := false
		if j.Status == model.StatusPending {
			ok, _, err := s.deps(ctx, j)
			if err != nil {
				if ctx.Err() == nil {
					slog.Error("scheduler deps", "job", j.ID, "err", err)
				}
				continue
			}
			if !ok {
				continue
			}
			// "只转写" (transcribe_only.go): a deferred lecture gets its
			// LLM snapshots before its first LLM job leaves pending.
			if _, err := s.fillDeferred(ctx, j); err != nil {
				if ctx.Err() == nil {
					slog.Error("scheduler: record deferred snapshots", "job", j.ID, "err", err)
				}
				continue
			}
			if err := state.TransitionJob(ctx, s.st, j.ID, model.StatusPending, model.StatusQueued, db.JobFinish{}); err != nil {
				if !errors.Is(err, state.ErrConflict) && ctx.Err() == nil {
					slog.Error("scheduler pending→queued", "job", j.ID, "err", err)
				}
				continue
			}
			j.Status = model.StatusQueued
			// The listing may predate the launch's snapshots (a lecture
			// launched while this scan ran: its prep finished meanwhile).
			// The device policy and the bind need the snapshot as stored
			// (power management: a stale "{}" would place an auto job
			// without binding it, and the stage would use the launch's
			// device even on battery).
			if fresh, err := s.st.GetJob(ctx, j.ID); err == nil && fresh.Status == model.StatusQueued {
				j = fresh
			}
			depsOK = true
			s.emit(ctx, j.ID)
		}
		queued[j.ID] = true
		if r := s.tryStart(ctx, j, depsOK, &sc); r != "" {
			reasons[j.ID] = r
			switch r {
			case model.WaitPowerWaitAC:
				waitAC++
				if id := suspendedOn(j); id != "" && !slices.Contains(holds, id) {
					holds = append(holds, id)
				}
			case model.WaitPowerNoSaverDevice, model.WaitPowerNoDevice:
				noDev++
			}
		}
	}
	now := s.now()
	s.mu.Lock()
	s.reasons = reasons
	s.powerWaitAC, s.powerNoDev, s.powerHolds = waitAC, noDev, holds
	for id, nb := range s.backoff {
		// Only expired entries: a job requeued after this scan listed the
		// jobs must keep its fresh backoff.
		if !queued[id] && !now.Before(nb) {
			delete(s.backoff, id)
		}
	}
	if sc.wanted == nil {
		sc.wanted = map[string]bool{}
	}
	s.wanted, s.wantedOnly, s.memWait = sc.wanted, sc.wantedOnly, sc.memWait
	s.mu.Unlock()
	s.preemptDeviceTests(sc.memPreempt)
	s.startDeviceTests(ctx, &sc) // after the pipeline jobs had their turn
	s.endRemoteScan(&sc)
}

// tryStart starts a queued job if its dependencies still hold and its
// resources are available, and returns the waiting_for reason otherwise
// ("" when started, or when no stage implementation is registered).
func (s *Scheduler) tryStart(ctx context.Context, j model.Job, depsOK bool, sc *scanState) string {
	s.mu.Lock()
	stg := s.stages[j.Stage]
	nb, inBackoff := s.backoff[j.ID]
	s.mu.Unlock()
	if stg == nil {
		return "" // no implementation registered (yet): stays queued
	}
	if inBackoff {
		if left := nb.Sub(s.now()); left > 0 {
			if s.devs != nil && stg.Resource() == ResourceGPU {
				s.markWanted(sc, j) // it waits for its devices too (self-tests yield)
			}
			return backoffReason(left)
		}
	}
	// A queued job may have got there by retry/auto-retry/recovery after its
	// dependencies changed (e.g. a syllabus uploaded meanwhile): re-check.
	if !depsOK {
		ok, w, err := s.deps(ctx, j)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("scheduler deps", "job", j.ID, "err", err)
			}
			return ""
		}
		if !ok {
			return w
		}
		// A queued job of a deferred lecture (e.g. retried): its snapshots
		// are recorded before it starts (transcribe_only.go).
		filled, err := s.fillDeferred(ctx, j)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("scheduler: record deferred snapshots", "job", j.ID, "err", err)
			}
			return ""
		}
		if filled {
			if fresh, err := s.st.GetJob(ctx, j.ID); err == nil {
				j = fresh
			}
		}
	}
	r := &run{job: j, stage: stg, res: stg.Resource()}
	switch r.res {
	case ResourceCPU:
		if s.countRunning(ResourceCPU) >= s.cpuSlots {
			return model.WaitCPUSlot
		}
	case ResourceGPU:
		if s.devs != nil {
			if s.remote != nil && remotePinned(j) {
				// SPEC-v3 V14.4: "改用 Colab 处理" (or a fixed colab snapshot).
				if reason := s.remoteOnlyAdmit(ctx, sc, r); reason != "" {
					return reason
				}
			} else if reason := s.deviceAdmit(ctx, sc, r); reason != "" {
				// SPEC-v2 I8: per-device pools; SPEC-v3 V14: Colab takes a
				// lecture the local devices cannot take now.
				if reason = s.remoteOverflow(ctx, sc, r, reason); reason != "" {
					return reason
				}
			}
		} else if reason := s.gpuAdmit(ctx, sc, r); reason != "" {
			return reason
		}
	case ResourceAPI:
		p := s.providerOf(stg, j)
		// v2 P12.6: an offpeak_only provider starts no job in its peak period.
		if w, until := s.pool.OffpeakWait(p, s.now()); w {
			return OffpeakReason(until)
		}
		if s.pool.Full(p) {
			return model.WaitAPISlot
		}
		if s.pool.BudgetExceeded(ctx) {
			return model.WaitBudget
		}
	}
	s.start(ctx, r)
	return ""
}

func backoffReason(left time.Duration) string {
	return model.WaitRetryBackoffPrefix + strconv.Itoa(int(math.Ceil(left.Seconds())))
}

// providerOf is the LLM provider a job uses (v1.1 A4.1): a lecture job the
// provider of its own snapshot, a course/material job and an auxiliary task
// (v2 ruling 1) the provider of its role's current setting (those have no
// snapshot). The stage's own answer
// (ProviderStage) is the fallback, e.g. for a lecture job without a valid
// LLM snapshot, which the stage then fails with a readable error.
func (s *Scheduler) providerOf(stg Stage, j model.Job) string {
	if j.Scope == model.ScopeLecture && !model.IsAuxStage(j.Stage) {
		if snap, ok := model.ParseLLMSnapshot(j.ConfigSnapshot); ok {
			return snap.ProviderID
		}
	} else if role := model.RoleOfStage(j.Stage); role != "" {
		if t, err := s.set.RoleTarget(role); err == nil {
			return t.ProviderID
		}
	}
	if ps, ok := stg.(ProviderStage); ok {
		return ps.Provider()
	}
	return ""
}

func (s *Scheduler) countRunning(res ResourceKind) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.running {
		if r.res == res {
			n++
		}
	}
	return n
}

// scopeCreated returns the created_at of the job's lecture/material/course
// (the FIFO key of SPEC §5), falling back to the job's own created_at.
func (s *Scheduler) scopeCreated(ctx context.Context, j model.Job) string {
	switch j.Scope {
	case model.ScopeLecture:
		if l, err := s.st.GetLecture(ctx, j.ScopeID); err == nil {
			return l.CreatedAt
		}
	case model.ScopeMaterial:
		if m, err := s.st.GetMaterial(ctx, j.ScopeID); err == nil {
			return m.CreatedAt
		}
	case model.ScopeCourse:
		if c, err := s.st.GetCourse(ctx, j.ScopeID); err == nil {
			return c.CreatedAt
		}
	}
	return j.CreatedAt
}

// ---------- running ----------

func (s *Scheduler) start(ctx context.Context, r *run) {
	runCtx, cancelCause := context.WithCancelCause(s.baseCtx)
	cancel := func() { cancelCause(nil) }
	r.cancel, r.cancelCause, r.done = cancel, cancelCause, make(chan struct{})
	r.startedAt, r.scopeID = time.Now(), r.job.ScopeID
	s.mu.Lock()
	s.running[r.job.ID] = r
	s.mu.Unlock()
	zero := 0.0
	if err := s.st.Tx(ctx, func(tx db.Store) error {
		if err := state.TransitionJob(ctx, tx, r.job.ID, model.StatusQueued, model.StatusRunning, db.JobFinish{Progress: &zero}); err != nil {
			return err
		}
		if err := state.RecordPromptVersion(ctx, tx, r.job.ID); err != nil {
			return err
		}
		if r.bind {
			// SPEC-v2 I8, lead ruling 1: an auto-mode job never runs
			// without its device in the snapshot.
			return state.BindASRDevice(ctx, tx, r.job.ID, r.devRow)
		}
		return nil
	}); err != nil {
		s.mu.Lock()
		delete(s.running, r.job.ID)
		s.mu.Unlock()
		cancel()
		close(r.done)
		if !errors.Is(err, state.ErrConflict) && ctx.Err() == nil {
			slog.Error("scheduler queued→running", "job", r.job.ID, "err", err)
		}
		return
	}
	if r.res == ResourceGPU {
		r.awake = true
		s.awake(1) // released by finish
		s.mu.Lock()
		for id, x := range s.running {
			// With devices only a run on the same card shares its memory.
			if id != r.job.ID && x.res == ResourceGPU && (s.devs == nil || x.device == r.device) {
				x.overlapped, r.overlapped = true, true
			}
		}
		s.mu.Unlock()
	}
	// Power management: the reading this start is decided on (the scan's).
	onBattery := s.Power().OnBatteryPower()
	r.wmu.Lock()
	if j, err := s.st.GetJob(ctx, r.job.ID); err == nil {
		r.job = j
	}
	// Baseline: the stats in the database (empty for a new job; a retry keeps
	// the accumulated values of earlier attempts, already in the ledger).
	r.accounted = r.job.Stats
	r.smu.Lock()
	r.stats = r.job.Stats
	if r.stats.Warning != "" || r.stats.WarningInfo != nil {
		// A new attempt starts without the previous attempt's warning
		// (Gate B ruling 4) and its coded form; persisted on the next flush.
		r.stats.Warning, r.stats.WarningInfo = "", nil
		r.statsDirty = true
	}
	// Power management: a job the policy suspended (power_wait_ac) starts
	// again: the stage card and a notice say "已恢复" (power-brief §3.4) —
	// only on AC, the text says "已接通电源" (backend Gate round 1, fix 6).
	// Started on battery (it may run on the battery device after all, e.g.
	// once that is its own device): the suspended event is dropped, so the
	// running card does not say "等待电源适配器", and no notice is written.
	resumed := r.device != "" && r.stats.PowerEvent != nil && r.stats.PowerEvent.Code == CodePowerSuspended
	if resumed && onBattery {
		r.stats.PowerEvent, r.statsDirty, resumed = nil, true, false
	}
	if resumed {
		from := r.stats.PowerEvent.FromDevice
		if r.stats.ASRResume != nil {
			from = r.stats.ASRResume.DeviceID
		}
		r.stats.PowerEvent = &model.PowerEvent{Code: CodePowerResumed, At: s.now().UTC().Format(time.RFC3339), FromDevice: from, ToDevice: r.device}
		r.statsDirty = true
	}
	r.smu.Unlock()
	// estimated_cost with the prices of the provider the job runs with (its
	// snapshot's provider for lecture jobs, A5.1 prices live on the provider).
	stage, provider := r.job.Stage, s.providerOf(r.stage, r.job)
	r.cfgCost = func(d model.JobStats) (float64, bool) {
		if provider == "" {
			return EstimateCost(s.cfg, stage, d)
		}
		return s.pool.ProviderCost(provider, d.PromptTokens, d.CacheHitTokens, d.CompletionTokens)
	}
	r.live = true
	r.wmu.Unlock()
	// FIFO key for the API pool (any stage may call the LLM), looked up once
	// here so Acquire never queries the database for it.
	s.pool.setPriority(r.job, s.scopeCreated(ctx, r.job))
	s.mu.Lock()
	delete(s.backoff, r.job.ID)
	s.mu.Unlock()
	s.emitJob(ctx, r.job)
	if resumed {
		s.addPowerNotice(CodePowerResumed, r.job.ID, r.scopeID, r.device)
	}
	if r.device != "" {
		slog.Info("job started", "job", r.job.ID, "scope_id", r.job.ScopeID, "stage", r.job.Stage, "attempt", r.job.Attempt, "device", r.device)
	} else {
		slog.Info("job started", "job", r.job.ID, "scope_id", r.job.ScopeID, "stage", r.job.Stage, "attempt", r.job.Attempt)
	}
	s.wg.Add(1)
	go s.execute(runCtx, r)
}

func (s *Scheduler) execute(ctx context.Context, r *run) {
	defer s.wg.Done()
	defer close(r.done)
	started := s.now()
	sampCtx, stopSampling := context.WithCancel(ctx)
	var sampWG sync.WaitGroup
	if r.res == ResourceGPU && r.sample {
		sampWG.Add(1)
		go func() {
			defer sampWG.Done()
			s.sampleVRAM(sampCtx, r)
		}()
	}
	if r.res == ResourceGPU && r.memSample {
		sampWG.Add(1)
		go func() {
			defer sampWG.Done()
			s.sampleMem(sampCtx, r)
		}()
	}
	err := s.runStage(ctx, r)
	stopSampling()
	sampWG.Wait()
	var final string
	if r.job.Stage == model.StageGlossaryReview {
		final = s.finishReview(r, err, s.now().Sub(started)) // v2: with the fallback of a review that did not complete
	} else {
		final = s.finish(r, err, s.now().Sub(started))
	}
	if final == model.StatusDone {
		s.postRunStaleCheck(r)
		if r.job.Stage == model.StageSummary {
			s.queueTitle(r.job.ScopeID) // v2 P13.5: title and blurb from the new summary
		}
	}
	s.reviewEndedAfterRun(r, final) // v2: one more glossary review for terms that arrived meanwhile
	s.Kick()
}

func (s *Scheduler) runStage(ctx context.Context, r *run) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic in stage %s: %v", r.job.Stage, p)
			slog.Error("stage panic", "job", r.job.ID, "stage", r.job.Stage, "panic", p)
		}
	}()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	h, err := r.stage.InputHash(ctx, r.job.ScopeID)
	if err != nil {
		return fmt.Errorf("input hash: %w", err)
	}
	if h != "" {
		if err := s.st.SetJobInputHash(context.WithoutCancel(ctx), r.job.ID, h); err != nil {
			return fmt.Errorf("store input hash: %w", err)
		}
		r.wmu.Lock()
		r.job.InputHash = &h
		r.wmu.Unlock()
	}
	r.wmu.Lock()
	j := r.job
	r.wmu.Unlock()
	return r.stage.Run(WithRemote(WithStatsSink(WithJob(ctx, j), r), s.remote), j.ScopeID, r.progress)
}

// progress is the callback handed to Stage.Run: it only records the latest
// value in memory. It never blocks on I/O (the flusher writes it).
func (r *run) progress(p float64) {
	if math.IsNaN(p) {
		return
	}
	p = math.Max(0, math.Min(1, p))
	r.pmu.Lock()
	if p != r.latest {
		r.latest, r.dirty = p, true
	}
	r.pmu.Unlock()
}

// flusher writes the progress of running jobs (at most once per interval
// per job, SPEC §8.2) and adds their token usage to the daily ledger.
func (s *Scheduler) flusher() {
	defer s.wg.Done()
	t := time.NewTicker(s.flushEvery)
	defer t.Stop()
	for {
		select {
		case <-s.baseCtx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		runs := make([]*run, 0, len(s.running))
		for _, r := range s.running {
			runs = append(runs, r)
		}
		s.mu.Unlock()
		for _, r := range runs {
			s.flushRun(r)
		}
	}
}

func (s *Scheduler) flushRun(r *run) {
	r.wmu.Lock()
	defer r.wmu.Unlock()
	if !r.live || r.finished {
		return
	}
	r.pmu.Lock()
	p, dirty := r.latest, r.dirty
	r.dirty = false
	r.pmu.Unlock()
	ctx := context.Background()
	if dirty && p != r.lastFlush {
		if err := s.st.SetJobProgress(ctx, r.job.ID, p); err != nil {
			slog.Warn("progress write", "job", r.job.ID, "err", err)
		} else {
			r.lastFlush = p
			j := r.job
			j.Status, j.Progress = model.StatusRunning, p
			s.ev.Job(j)
		}
	}
	s.persistStatsLocked(ctx, r, nil)
}

func (s *Scheduler) sampleVRAM(ctx context.Context, r *run) {
	t := time.NewTicker(s.sampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		m, err := s.gpuQ(ctx)
		if err != nil {
			continue
		}
		s.rememberGPU(m, nil)
		if d := m.UsedMB - r.baseUsedMB; d > 0 {
			s.mu.Lock()
			if d > r.peakMB {
				r.peakMB = d
			}
			s.mu.Unlock()
		}
	}
}

// finish records the outcome of a run and returns the status it left the
// job in.
func (s *Scheduler) finish(r *run, runErr error, took time.Duration) string {
	ctx := context.Background()
	id := r.job.ID
	s.mu.Lock()
	userCancel := r.userCancel
	peak := r.peakMB
	powerAction, powerTo, powerOnBattery := r.powerAction, r.powerTo, r.powerOnBattery
	s.mu.Unlock()
	shutdown := s.baseCtx.Err() != nil
	// Power management (power-brief §3.4): the run was killed by the
	// power-aware policy. Not a failure: requeued without a retry being
	// used up (power_interrupts), the device it ran on kept for its
	// checkpoint (asr_resume), the event for the stage card (power_event).
	// A run that finished successfully although the action was pending is
	// done as usual (the kill came too late).
	power := runErr != nil && powerAction != "" && !userCancel && !shutdown
	// SPEC-v3 V14: Colab handed the lecture back (fallback, "立即关机"):
	// requeued for the local devices without using up a retry.
	remoteRun := isRemote(r.device)
	handBack := remoteRun && !power && !userCancel && !shutdown && remoteHandBack(runErr)
	powerCode := CodePowerSuspended
	if powerAction == devices.PowerMigrate {
		powerCode = CodePowerSwitched
	}
	// A suspension on AC (a CPU run on a machine with a battery, fix 7):
	// requeued and counted like any other, but without the "已拔掉电源"
	// event and notice; its waiting_for (power_no_device) or its next
	// device explains it.
	quiet := powerAction == devices.PowerSuspend && !powerOnBattery
	at := s.now().UTC().Format(time.RFC3339)
	// Under wmu: persist the final stats and account the last tokens before
	// any status change (also on failure, cancel and shutdown), and make sure
	// no flush lands after the final transition.
	r.wmu.Lock()
	final := s.persistStatsLocked(ctx, r, func(st *model.JobStats) {
		st.DurationMS = took.Milliseconds()
		st.Status = "" // transient worker status (e.g. downloading_model)
		if peak > 0 {
			st.VRAMPeakMB = int64(peak)
		}
		if remoteRun {
			st.Remote = nil // the hover card's sub-steps belong to the running remote run
		}
		switch {
		case runErr == nil:
			st.ASRResume = nil // done: no checkpoint to continue from (power_event stays)
		case handBack:
			st.RemoteRequeues++
			st.RemotePin = false
			st.RemoteFallback = remoteErrorInfo(runErr)
		case power:
			st.PowerInterrupts++
			st.ASRResume = &model.ASRResume{DeviceID: r.device, Backend: model.DeviceBackend(r.device), At: at}
			if !quiet {
				st.PowerEvent = &model.PowerEvent{Code: powerCode, At: at, FromDevice: r.device, ToDevice: powerTo}
			}
		}
	})
	r.finished = true
	r.wmu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, id)
		s.mu.Unlock()
		if r.awake {
			s.awake(-1)
		}
		if remoteRun {
			s.mu.Lock()
			s.rem.runEnded = true
			s.mu.Unlock()
		}
		s.pool.clearPriority(id)
		s.emit(ctx, id)
	}()

	if runErr != nil && !userCancel && shutdown {
		// Leave it running: §4.7 recovery requeues it on the next start.
		slog.Info("job interrupted by shutdown", "job", id, "stage", r.job.Stage)
		return model.StatusRunning
	}
	empty := ""
	switch {
	case runErr == nil:
		s.recordVRAM(r) // before done, so observers of "done" see the calibration
		one := 1.0
		// A stage that finished with a partial problem (stats.warning, e.g.
		// repair chunks without patches, SPEC §6.4) is done with job.error set.
		doneErr := final.Warning
		if err := state.TransitionJob(ctx, s.st, id, model.StatusRunning, model.StatusDone, db.JobFinish{Error: &doneErr, Progress: &one}); err != nil {
			slog.Error("job running→done", "job", id, "err", err)
			return ""
		}
		if doneErr != "" {
			slog.Warn("job done with warning", "job", id, "stage", r.job.Stage, "ms", took.Milliseconds(), "warning", doneErr)
		} else {
			slog.Info("job done", "job", id, "stage", r.job.Stage, "ms", took.Milliseconds())
		}
		return model.StatusDone
	case userCancel:
		if err := state.TransitionJob(ctx, s.st, id, model.StatusRunning, model.StatusCancelled, db.JobFinish{Error: &empty}); err != nil {
			slog.Error("job running→cancelled", "job", id, "err", err)
			return ""
		}
		slog.Info("job cancelled", "job", id, "stage", r.job.Stage)
		return model.StatusCancelled
	case handBack:
		s.remoteRequeue(ctx, r, runErr)
		return model.StatusQueued
	case power:
		// running → failed → queued in one transaction (§4.7 pattern; no
		// retry limit, job.error cleared, no failure notification).
		if err := state.PowerRequeue(ctx, s.st, id); err != nil {
			slog.Error("job power requeue", "job", id, "err", err)
			return ""
		}
		slog.Info("job interrupted by the power-aware policy", "job", id, "stage", r.job.Stage, "action", powerAction,
			"from", r.device, "to", powerTo, "err", runErr.Error())
		dev := r.device
		if powerAction == devices.PowerMigrate {
			dev = powerTo
		}
		if !quiet {
			s.addPowerNotice(powerCode, id, r.scopeID, dev)
		}
		return model.StatusQueued
	default:
		// v2 P14.3 (ruling 6): a coded error is stored as its ErrorInfo JSON.
		msg := state.EncodeJobError(runErr)
		// The attempts that count against the limit leave out the power
		// policy's requeues (model.CountedAttempts, power-brief §1 ruling 2).
		counted := r.job
		counted.Stats = final
		attempts := model.CountedAttempts(counted)
		// An auxiliary task is never retried automatically (v2 ruling 1).
		retry := !IsPermanent(runErr) && attempts < model.MaxAutoRetries && !model.IsAuxStage(r.job.Stage)
		if retry {
			wait := s.backoffs[len(s.backoffs)-1]
			if attempts < len(s.backoffs) {
				wait = s.backoffs[attempts]
			}
			// Register the backoff before the job becomes queued, so no scan
			// can start it early.
			s.mu.Lock()
			s.backoff[id] = s.now().Add(wait)
			s.mu.Unlock()
		}
		// running → failed (→ queued) in one transaction: no intermediate
		// "failed" is ever committed or emitted for a job that will retry.
		requeued, err := state.FailJob(ctx, s.st, id, msg, retry)
		if err != nil || !requeued {
			s.mu.Lock()
			delete(s.backoff, id)
			s.mu.Unlock()
		}
		if err != nil {
			slog.Error("job running→failed", "job", id, "err", err)
			return ""
		}
		slog.Warn("job failed", "job", id, "stage", r.job.Stage, "attempt", r.job.Attempt,
			"permanent", IsPermanent(runErr), "will_retry", requeued, "err", runErr.Error())
		if requeued {
			return model.StatusQueued
		}
		return model.StatusFailed
	}
}

// postRunStaleCheck catches input changes that happened while the stage was
// running (SPEC §4.4): the hash is recomputed right after done and the job
// becomes stale when it differs from the recorded input_hash.
func (s *Scheduler) postRunStaleCheck(r *run) {
	ctx := context.Background()
	j, err := s.st.GetJob(ctx, r.job.ID)
	if err != nil {
		return
	}
	stale, err := state.CheckStaleJob(ctx, s.st, r.stage, j)
	if err != nil {
		slog.Warn("post-run invalidation check", "job", j.ID, "stage", j.Stage, "err", err)
		return
	}
	if stale {
		slog.Info("inputs changed while the stage ran; marked stale", "job", j.ID, "stage", j.Stage)
		s.emit(ctx, j.ID)
	}
}

// ---------- events ----------

func (s *Scheduler) emit(ctx context.Context, jobID string) {
	j, err := s.st.GetJob(context.WithoutCancel(ctx), jobID)
	if err != nil {
		return
	}
	s.emitJob(ctx, j)
}

func (s *Scheduler) emitJob(ctx context.Context, j model.Job) {
	s.ev.Job(j)
	switch j.Scope {
	case model.ScopeLecture:
		s.ev.Changed("lecture", j.ScopeID)
	case model.ScopeCourse:
		s.ev.Changed("course", j.ScopeID)
	case model.ScopeMaterial:
		if m, err := s.st.GetMaterial(context.WithoutCancel(ctx), j.ScopeID); err == nil {
			s.ev.Changed("course", m.CourseID)
		}
	}
}

func (s *Scheduler) emitScope(ctx context.Context, scope, scopeID string) {
	jobs, err := s.st.ListCurrentJobs(ctx, scope, scopeID)
	if err != nil {
		return
	}
	for _, j := range jobs {
		s.emitJob(ctx, j)
	}
}
