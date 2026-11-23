// Package ratelimit is a small attempt counter keyed by string. A key's
// window restarts with each counted attempt, so it resets only after a quiet
// period of one window.
package ratelimit

import (
	"sync"
	"time"
)

type entry struct {
	count int
	start time.Time // time of the last counted attempt
}

// Limiter allows at most limit counted attempts per key; the key's window
// restarts with each counted attempt (see Reserve).
type Limiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu        sync.Mutex
	entries   map[string]*entry
	lastSweep time.Time
}

// New returns a Limiter. If limit <= 0, Reserve always returns true.
func New(limit int, window time.Duration, now func() time.Time) *Limiter {
	return &Limiter{limit: limit, window: window, now: now, entries: map[string]*entry{}, lastSweep: now()}
}

// Reserve counts one attempt for key. It returns false, without counting,
// if key already has limit attempts in its current window. Each counted
// attempt restarts the key's window, so a key resets only once window has
// passed since its last counted attempt (mirroring AD's reset after a quiet
// period). A refused call does not count, so it does not restart the window.
// Expired keys are dropped when at least window has passed since the last
// sweep.
func (l *Limiter) Reserve(key string) bool {
	if l.limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	if t.Sub(l.lastSweep) >= l.window {
		for k, e := range l.entries {
			if t.Sub(e.start) > l.window {
				delete(l.entries, k)
			}
		}
		l.lastSweep = t
	}
	e, ok := l.entries[key]
	if !ok || t.Sub(e.start) > l.window {
		l.entries[key] = &entry{count: 1, start: t}
		return true
	}
	if e.count >= l.limit {
		return false
	}
	e.count++
	e.start = t
	return true
}

// Release gives one attempt back, never going below 0.
func (l *Limiter) Release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[key]; ok && e.count > 0 {
		e.count--
	}
}

// Reset forgets key.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}
