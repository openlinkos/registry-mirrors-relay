// Package breaker 是每个上游一个的熔断器：上游连续连不上时快速失败，不让请求在超时上越堆越多。
//
// 状态：关闭（正常放行）→ 连续失败达到阈值 → 打开（冷却期内直接拒绝）→ 冷却期过后半开（只放一个请求试探）
// → 试探成功回到关闭；试探失败重新打开，冷却时间加倍，不超过上限。
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

// Breaker 并发安全。nil 表示不熔断，所有方法都可以在 nil 上调用。
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

// New 创建熔断器。threshold 为 0 时返回 nil（不熔断）。
func New(threshold int, cooldown, maxCooldown time.Duration) *Breaker {
	if threshold <= 0 {
		return nil
	}
	if maxCooldown < cooldown {
		maxCooldown = cooldown
	}
	return &Breaker{threshold: threshold, base: cooldown, max: maxCooldown, cooldown: cooldown, now: time.Now}
}

// Allow 判断能否发请求；不能时返回还要等多久。允许的请求结束后必须调用 Done。
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

// Done 记录一次请求的结果。failed 只应包含上游本身的问题（连不上、超时、502/503/504），
// 不包括客户端自己断开。
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

// Cancel 用在允许的请求没有结果时（例如客户端自己断开）：不改变状态，只让出试探名额。
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

// Open 表示当前是否处于打开状态（给指标和后台看）。
func (b *Breaker) Open() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.st != closed
}
