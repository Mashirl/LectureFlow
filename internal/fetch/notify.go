// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package fetch

import (
	"context"
	"sync"
	"time"

	"lectureflow/internal/bridge"
)

// keyedThrottle calls fn(id) at most once per interval for each id; a call
// that comes too soon is deferred to the end of the interval, so the last
// change of an id is never lost (SSE changed {kind: "downloads", id}).
type keyedThrottle struct {
	interval time.Duration
	fn       func(id int64)

	mu      sync.Mutex
	last    map[int64]time.Time
	timers  map[int64]*time.Timer
	stopped bool
}

func newKeyedThrottle(interval time.Duration, fn func(int64)) *keyedThrottle {
	return &keyedThrottle{interval: interval, fn: fn, last: map[int64]time.Time{}, timers: map[int64]*time.Timer{}}
}

func (t *keyedThrottle) kick(id int64) {
	if t == nil || t.fn == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.timers[id] != nil {
		return
	}
	now := time.Now()
	wait := t.interval - now.Sub(t.last[id])
	if wait <= 0 {
		t.last[id] = now
		if len(t.last) > 1024 {
			for k, v := range t.last {
				if now.Sub(v) > time.Minute {
					delete(t.last, k)
				}
			}
		}
		go t.fn(id)
		return
	}
	t.timers[id] = time.AfterFunc(wait, func() {
		t.mu.Lock()
		delete(t.timers, id)
		if t.stopped {
			t.mu.Unlock()
			return
		}
		t.last[id] = time.Now()
		t.mu.Unlock()
		t.fn(id)
	})
}

func (t *keyedThrottle) stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.stopped = true
	for id, tm := range t.timers {
		tm.Stop()
		delete(t.timers, id)
	}
	t.mu.Unlock()
}

// subscriber delivers status pushes to one bridge connection (ext-brief
// §3): the latest status per task, progress-only changes of a task at most
// once per interval, a state change at once; a slow reader loses
// intermediate pushes, never the last one.
type subscriber struct {
	out      chan bridge.Status
	wake     chan struct{}
	interval time.Duration

	mu       sync.Mutex
	pending  map[int64]bridge.Status
	order    []int64
	lastSent map[int64]sentStatus
}

type sentStatus struct {
	at    time.Time
	state string
}

func newSubscriber(interval time.Duration) *subscriber {
	return &subscriber{
		out: make(chan bridge.Status), wake: make(chan struct{}, 1), interval: interval,
		pending: map[int64]bridge.Status{}, lastSent: map[int64]sentStatus{},
	}
}

func (s *subscriber) push(st bridge.Status) {
	s.mu.Lock()
	if _, ok := s.pending[st.TaskID]; !ok {
		s.order = append(s.order, st.TaskID)
	}
	s.pending[st.TaskID] = st
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// next returns the first status that may go out now, or when the next
// deferred one becomes due (zero: nothing pending).
func (s *subscriber) next(now time.Time) (bridge.Status, bool, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due time.Time
	for i, id := range s.order {
		st := s.pending[id]
		last, seen := s.lastSent[id]
		if seen && last.state == st.State && now.Sub(last.at) < s.interval {
			if d := last.at.Add(s.interval); due.IsZero() || d.Before(due) {
				due = d
			}
			continue
		}
		s.order = append(s.order[:i:i], s.order[i+1:]...)
		delete(s.pending, id)
		s.lastSent[id] = sentStatus{at: now, state: st.State}
		if len(s.lastSent) > 1024 {
			for k, v := range s.lastSent {
				if now.Sub(v.at) > time.Minute {
					delete(s.lastSent, k)
				}
			}
		}
		return st, true, time.Time{}
	}
	return bridge.Status{}, false, due
}

// run sends until ctx ends, then closes out.
func (s *subscriber) run(ctx context.Context, done func()) {
	defer close(s.out)
	defer done()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		st, ok, due := s.next(time.Now())
		if ok {
			select {
			case s.out <- st:
				continue
			case <-ctx.Done():
				return
			}
		}
		if !due.IsZero() {
			timer.Reset(time.Until(due))
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-timer.C:
		}
	}
}
