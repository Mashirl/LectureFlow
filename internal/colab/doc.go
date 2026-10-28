// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

// Package colab is the Google Colab remote transcription backend (SPEC-v3
// V14): a Go reimplementation of the parts of the official google-colab-cli
// (Apache-2.0, Python, Linux/macOS only) that LectureFlow needs, usable on
// every platform without the Python CLI. UPSTREAM.md lists which upstream
// file and version every part follows.
//
// # What one borrowing does (V14.3)
//
//  1. Credential (credential.go): gcloud's application default
//     credentials are imported once (ImportADC) with the user's lifecycle
//     ("7d" or N days, "session" = this run only, "manual"), stored in kv
//     colab.credential, checked strictly before every use; expired =
//     deleted, never renewed past the limit; Disconnect revokes at Google
//     and deletes the local copy.
//  2. Accelerator (session.go, api.go): /tun/m/assign with exponential
//     back-off while Colab refuses for capacity (waiting_for remote_queue)
//     up to Settings.MaxQueueWaitMin, then boot until the runtime's Jupyter
//     server answers. The session record (kv colab.session, no tokens)
//     lets Startup release a session a crash left behind.
//  3. Lecture (job.go, runtime.go, ws.go, events.go): once per runtime the
//     first kernel, the memory probe, the worker scripts; per lecture a cell
//     that attaches to the runtime's resident worker
//     (worker/colab_remote.py --attach; the worker installs with uv,
//     downloads and loads the model while the FLAC uploads through the
//     Contents API, and keeps the model for the next lectures), job.json
//     last, its stdout parsed strictly as JSON events (progress, stage,
//     done, error; anything else is CodeContractViolation), result.json
//     downloaded as soon as the done event arrives, checked against its
//     sha256, written atomically; the job's directory is then removed in the
//     background by one cell in its kernel (removal.go; a release waits for
//     it).
//     Batches (batch.go, colab-batch): the scheduler's plan (SetPlan)
//     lets the next lectures upload ahead of their turn while one
//     transcribes (Held tells which); Slots lectures at once (two on the
//     larger GPUs, one process and one model); "极速模式" (Settings.FastMode)
//     cuts each lecture into parts at silences instead. A lecture that
//     leaves the plan or is cancelled has its remote files deleted at once
//     while the runtime stays for the others.
//  4. Estimate (estimate.go, pure): 4-hour fair-use window minus what the
//     session occupied (queue and boot included), seconds per audio minute
//     measured per accelerator/model/precision (kv colab.rates), balance
//     and credential checks.
//  5. Release: queue drained, idle, window used up, failure, cancel,
//     "处理完当前这节后关机", "立即关机" (the lecture goes back to the queue).
//  6. Account and readings (account.go, samples.go; colab-obs-core): the
//     level follows ccu-info (paid with a paid balance, else free), the
//     accelerators beyond T4 unlock from eligibleGpus with a paid
//     balance, a pick the account does not allow borrows a T4 meanwhile
//     (Status.EffectiveAccelerator, LockedReason); the attaching process
//     sends the runtime's machine and samples (events.go), the runtime's
//     /api/colab/resources fills in while a front end watches.
//  7. Memory mode (memory.go; SPEC-ext "Colab 内存运行", kv
//     colab.memory_mode, on by default, the settings page's switch writes
//     "off"): before
//     the uploads a probe decides whether model x 2 + Python environment +
//     recording x 2 + 3 GB + 2 GB fit the runtime's free memory with no swap
//     enabled; if so, scripts, recording, result, the pip --target packages
//     and the Hugging Face cache live on a RAM disk (/dev/shm/lectureflow or
//     a tmpfs at /mnt/lectureflow), else the lecture runs on the disk as
//     usual and the log says why. It keeps data off the runtime's
//     persistent disk; the machine is still Google's.
//
// # API used by the scheduler, the asr stage and the HTTP layer (wired in cmd/lectureflow/colab.go, internal/sched/remote.go, internal/stages/asr_colab.go, internal/api/handlers_colab.go)
//
//	b, _ := colab.New(colab.Options{KV: store, WorkerDir: workerDir, FFmpeg: cfg.FFmpeg, Logger: slog.Default()})
//	b.Startup(ctx)          // once at start: lifecycle cleanup, leftover session release
//	go b.Run(ctx)           // Tick every 30 s: idle / window / credential-ending releases
//	defer b.Close(ctx)      // app exit: release; delete a "session" credential
//
//	ok, waitingFor := b.CanDispatch(ctx, lectureSec) // before sending a lecture to device model.DeviceColab
//	path, err := b.Transcribe(ctx, colab.Job{ID: jobID, AudioPath: wav, DurationSec: sec,
//		Language: lang, InitialPrompt: prompt, OutDir: lectureDir}, onEvent)
//	// path: lectureDir/result.json, the local faster-whisper worker's structure
//	//       (prompt_terms_used, sentences with words, timing): parse it like asrResult.
//	// err:  colab.IsFallback(err)  → hand the lecture to the local devices
//	//       colab.IsRequeue(err)   → "立即关机": back to the queue
//	//       colab.IsBusy(err)      → the runtime is being released or started: back to the queue, Colab again later
//	//       colab.WaitingFor(err)  → remote_auth / remote_window / remote_balance / remote_queue
//	//       errors.Is(err, context.Canceled) → the user cancelled (runtime released)
//	//       otherwise a local problem (missing audio): an ordinary job error
//	//       Every error is a *model.CodedError (code + params, i18n errors.colab.*).
//
//	b.SetQueue(audioSecs)           // planned lectures: "本次还能再处理约 N 节", plan cost
//	b.SetPlan(entries); b.Held(); b.Slots() // colab-batch: uploads ahead, lectures at once
//	b.QueueDrained(ctx)             // nothing more coming: release now / after the lecture
//	b.ShutdownAfterCurrent(ctx)     // 处理完当前这节后关机
//	b.ShutdownNow(ctx)              // 立即关机 (running lecture → IsRequeue)
//	b.Status(ctx)                   // resource bar / 工作台 line (no network): account, machine, usage
//	b.TouchAccount(ctx); b.RefreshAccountSoon(ctx); b.RefreshAccount(ctx) // the account (account.go)
//	b.SetViewers(res.Clients)       // the resources poll only while a front end watches
//	b.DetectADC(); b.ImportADC(ctx, lifecycle); b.Credential(ctx); b.Disconnect(ctx)
//	l := b.Login(opts); l.Start(lifecycle); l.Status(); l.Cancel(wait) // in-app gcloud sign-in (login.go)
//	b.Settings(); b.UserSettings(); b.SetSettings(ctx, s) // in effect / the user's pick
//
// Event (onEvent) carries the hover card's step (wait_accelerator →
// upload_audio → remote_transcribe → download_result → verify), the remote
// stage, the step's and the lecture's progress, an ETA, the session state
// and, while waiting for an accelerator, WaitingFor = model.WaitRemoteQueue
// (and "" once assigned).
//
// The backend never sends audio-derived text anywhere but result.json, and
// no exported value, error or log line carries a credential or token
// (secretSet scrubs errors as a last line of defence; tests check logs,
// errors, kv and Status).
package colab
