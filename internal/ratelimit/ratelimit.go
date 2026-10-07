// Package ratelimit provides an in-memory sliding-window limiter keyed by string
// (user id, client IP, username, or a single global key).
package ratelimit

import (
	"sync"
	"time"
)

// sweepEvery is how many Record calls happen between sweeps of idle keys, so a stream of
// distinct keys (e.g. random usernames) cannot grow the map without bound.
const sweepEvery = 1024

// Window allows at most Limit events per key within Window.
type Window struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
	calls  int
}

// New returns a limiter of limit events per window. limit must be >= 1.
func New(limit int, window time.Duration) *Window {
	if limit < 1 {
		panic("ratelimit: limit must be >= 1")
	}
	return &Window{limit: limit, window: window, hits: map[string][]time.Time{}}
}

func (w *Window) Limit() int            { return w.limit }
func (w *Window) Period() time.Duration { return w.window }

func (w *Window) pruneLocked(key string, now time.Time) []time.Time {
	cutoff := now.Add(-w.window)
	old := w.hits[key]
	kept := old[:0]
	for _, t := range old {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(w.hits, key)
		return nil
	}
	w.hits[key] = kept
	return kept
}

// Check reports whether one more event for key fits, without recording it. When it does
// not, the duration is how long until the oldest event leaves the window.
func (w *Window) Check(key string, now time.Time) (bool, time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	kept := w.pruneLocked(key, now)
	if len(kept) >= w.limit {
		return false, kept[0].Add(w.window).Sub(now)
	}
	return true, 0
}

// Record stores an event for key (whether or not it fits).
func (w *Window) Record(key string, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if w.calls%sweepEvery == 0 {
		for k := range w.hits {
			w.pruneLocked(k, now)
		}
	}
	w.hits[key] = append(w.pruneLocked(key, now), now)
}

// Allow is Check followed by Record when the event fits.
func (w *Window) Allow(key string, now time.Time) (bool, time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	kept := w.pruneLocked(key, now)
	if len(kept) >= w.limit {
		return false, kept[0].Add(w.window).Sub(now)
	}
	w.calls++
	if w.calls%sweepEvery == 0 {
		for k := range w.hits {
			if k != key {
				w.pruneLocked(k, now)
			}
		}
	}
	w.hits[key] = append(kept, now)
	return true, 0
}

// Keys is the number of keys currently tracked (for tests).
func (w *Window) Keys() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.hits)
}

// RetrySeconds rounds a retry duration up to whole seconds, at least 1.
func RetrySeconds(d time.Duration) int {
	s := int((d + time.Second - 1) / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}
