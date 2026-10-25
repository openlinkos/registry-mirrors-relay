// Package breaker provides one circuit breaker per upstream: when an upstream is repeatedly
// unreachable, requests fail fast instead of piling up on timeouts.
//
// States: closed (requests pass normally) → consecutive failures reach the threshold → open (requests
// are refused during the cooldown) → after the cooldown, half-open (a single probe request is let
// through) → a successful probe returns to closed; a failed probe reopens the breaker and doubles the
// cooldown, up to the limit.
package breaker

import (
	"sync"
	"time"
)

type state int

const (
	closed state = iota
	open
	halfOpen
)

// Breaker is safe for concurrent use. nil means no circuit breaking; all methods can be called on nil.
type Breaker struct {
	mu        sync.Mutex
	threshold int
	base      time.Duration
	max       time.Duration
	now       func() time.Time

	st       state
	failures int
	cooldown time.Duration
	openedAt time.Time
	probing  bool
}

// New creates a breaker. When threshold is 0 it returns nil (no circuit breaking).
func New(threshold int, cooldown, maxCooldown time.Duration) *Breaker {
	if threshold <= 0 {
		return nil
	}
	if maxCooldown < cooldown {
		maxCooldown = cooldown
	}
	return &Breaker{threshold: threshold, base: cooldown, max: maxCooldown, cooldown: cooldown, now: time.Now}
}

// Allow reports whether a request may be sent; if not, it returns how much longer to wait. Done must
// be called when an allowed request finishes.
func (b *Breaker) Allow() (bool, time.Duration) {
	if b == nil {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.st {
	case open:
		if wait := b.openedAt.Add(b.cooldown).Sub(b.now()); wait > 0 {
			return false, wait
		}
		b.st, b.probing = halfOpen, true
		return true, 0
	case halfOpen:
		if b.probing {
			return false, time.Second
		}
		b.probing = true
		return true, 0
	}
	return true, 0
}

// Done records the result of a request. failed should cover only problems with the upstream itself
// (unreachable, timeout, 502/503/504), not the client disconnecting on its own.
func (b *Breaker) Done(failed bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !failed {
		b.st, b.failures, b.cooldown, b.probing = closed, 0, b.base, false
		return
	}
	switch b.st {
	case halfOpen:
		b.cooldown = min(b.cooldown*2, b.max)
		b.st, b.openedAt, b.probing = open, b.now(), false
	case closed:
		b.failures++
		if b.failures >= b.threshold {
			b.st, b.openedAt = open, b.now()
		}
	}
}

// Cancel is used when an allowed request has no result (for example the client disconnected on its
// own): it leaves the state unchanged and only gives up the probe slot.
func (b *Breaker) Cancel() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.st == halfOpen {
		b.probing = false
	}
}

// Open reports whether the breaker is currently open (for metrics and the console).
func (b *Breaker) Open() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.st != closed
}
