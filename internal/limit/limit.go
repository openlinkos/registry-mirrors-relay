// Package limit 是限流和并发上限。
//
// 客户端按登录的用户名计，没登录时按 IP 计（IPv6 按 /64，一台机器通常有整个 /64）。
// manifest 请求用令牌桶（HEAD 按一个小权重算）；镜像层按每个客户端的并发流数限制；另有全局的并发上限。
// 记住的客户端数有上限（LRU），超过时淘汰最久没来的，不会整表清空。
package limit

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/lru"
)

// Key 是客户端的计数键。
func Key(user string, ip netip.Addr) string {
	if user != "" {
		return "user:" + user
	}
	ip = ip.Unmap()
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return "ip:" + p.String()
	}
	return "ip:" + ip.String()
}

// Config 是限流参数，热重载时整体替换。
type Config struct {
	ManifestRate         float64 // 每秒，0 表示不限
	ManifestBurst        int
	HeadWeight           float64
	BlobStreamsPerClient int // 0 表示不限
	MaxInflight          int // 0 表示不限
	MaxBlobStreams       int // 0 表示不限
	TrackedClients       int
	AuthFailuresPerMin   int // 每个 IP 每分钟允许的登录失败次数，0 表示不限
	Exempt               []netip.Prefix
}

type bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// Limiter 并发安全。计数状态在参数替换时保留。
type Limiter struct {
	cfg     atomic.Pointer[Config]
	buckets *lru.Cache[string, *bucket]
	authBkt *lru.Cache[string, *bucket]
	now     func() time.Time

	mu      sync.Mutex
	streams map[string]int // 只记有流在进行的客户端，流结束就删掉，所以大小受并发数限制

	inflight    atomic.Int64
	blobStreams atomic.Int64
	limited     atomic.Uint64
}

// New 创建 Limiter。
func New(c Config) *Limiter {
	l := &Limiter{
		buckets: lru.New[string, *bucket](max(c.TrackedClients, 1), 0),
		authBkt: lru.New[string, *bucket](max(c.TrackedClients, 1), 0),
		now:     time.Now, streams: map[string]int{},
	}
	l.cfg.Store(&c)
	return l
}

// SetConfig 替换参数（热重载）。已有的计数保留；TrackedClients 的变化在重启后生效。
func (l *Limiter) SetConfig(c Config) { l.cfg.Store(&c) }

func (l *Limiter) exempt(ip netip.Addr) bool {
	for _, p := range l.cfg.Load().Exempt {
		if p.Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}

// Override 是某个账号单独的限流（0 表示用默认值）。
type Override struct {
	ManifestRate float64
	BlobStreams  int
}

// AllowManifest 对一次 manifest 请求扣令牌。不允许时返回建议的重试等待时间。
func (l *Limiter) AllowManifest(key string, ip netip.Addr, head bool, ov Override) (bool, time.Duration) {
	c := l.cfg.Load()
	rate, burst := c.ManifestRate, float64(c.ManifestBurst)
	if ov.ManifestRate > 0 {
		rate = ov.ManifestRate
		burst = max(burst, rate*60) // 单独放宽的账号，突发至少能用一分钟的量
	}
	if rate <= 0 || l.exempt(ip) {
		return true, 0
	}
	cost := 1.0
	if head {
		cost = c.HeadWeight
	}
	now := l.now()
	b, ok := l.buckets.Get(key)
	if !ok {
		b = &bucket{tokens: burst, last: now}
		l.buckets.Add(key, b, 0)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens >= cost {
		b.tokens -= cost
		return true, 0
	}
	l.limited.Add(1)
	wait := time.Duration((cost - b.tokens) / rate * float64(time.Second))
	return false, max(wait, time.Second)
}

// authBucket 返回这个 IP 的登录失败令牌桶（补满后可以失败 AuthFailuresPerMin 次，每分钟补回同样的数）。
func (l *Limiter) authBucket(ip netip.Addr) (*bucket, float64, float64) {
	c := l.cfg.Load()
	burst := float64(c.AuthFailuresPerMin)
	k := Key("", ip)
	b, ok := l.authBkt.Get(k)
	if !ok {
		b = &bucket{tokens: burst, last: l.now()}
		l.authBkt.Add(k, b, 0)
	}
	return b, burst, burst / 60
}

// AuthAllowed 判断这个 IP 现在能不能再尝试登录（只看，不扣）。
func (l *Limiter) AuthAllowed(ip netip.Addr) (bool, time.Duration) {
	if l.cfg.Load().AuthFailuresPerMin <= 0 {
		return true, 0
	}
	b, burst, rate := l.authBucket(ip)
	b.mu.Lock()
	defer b.mu.Unlock()
	now := l.now()
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens >= 1 {
		return true, 0
	}
	l.limited.Add(1)
	return false, max(time.Duration((1-b.tokens)/rate*float64(time.Second)), time.Second)
}

// AuthFailed 记一次登录失败。
func (l *Limiter) AuthFailed(ip netip.Addr) {
	if l.cfg.Load().AuthFailuresPerMin <= 0 {
		return
	}
	b, _, _ := l.authBucket(ip)
	b.mu.Lock()
	b.tokens = max(0, b.tokens-1)
	b.mu.Unlock()
}

// AcquireBlob 占一个镜像层流的名额（每个客户端的和全局的）。ok 为 false 时 reason 是 client 或 global。
// 成功时必须调用 release。
func (l *Limiter) AcquireBlob(key string, ip netip.Addr, ov Override) (release func(), ok bool, reason string) {
	c := l.cfg.Load()
	if c.MaxBlobStreams > 0 && l.blobStreams.Add(1) > int64(c.MaxBlobStreams) {
		l.blobStreams.Add(-1)
		l.limited.Add(1)
		return nil, false, "global"
	} else if c.MaxBlobStreams <= 0 {
		l.blobStreams.Add(1)
	}
	per := c.BlobStreamsPerClient
	if ov.BlobStreams > 0 {
		per = ov.BlobStreams
	}
	if per > 0 && !l.exempt(ip) {
		l.mu.Lock()
		if l.streams[key] >= per {
			l.mu.Unlock()
			l.blobStreams.Add(-1)
			l.limited.Add(1)
			return nil, false, "client"
		}
		l.streams[key]++
		l.mu.Unlock()
		var once sync.Once
		return func() {
			once.Do(func() {
				l.mu.Lock()
				if l.streams[key]--; l.streams[key] <= 0 {
					delete(l.streams, key)
				}
				l.mu.Unlock()
				l.blobStreams.Add(-1)
			})
		}, true, ""
	}
	var once sync.Once
	return func() { once.Do(func() { l.blobStreams.Add(-1) }) }, true, ""
}

// AcquireInflight 占一个全局的请求名额。成功时必须调用 release。
func (l *Limiter) AcquireInflight() (release func(), ok bool) {
	c := l.cfg.Load()
	n := l.inflight.Add(1)
	if c.MaxInflight > 0 && n > int64(c.MaxInflight) {
		l.inflight.Add(-1)
		l.limited.Add(1)
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(func() { l.inflight.Add(-1) }) }, true
}

// Stats 是当前的计数。
type Stats struct {
	Inflight, BlobStreams int64
	Limited               uint64
	TrackedClients        int
}

// Stats 返回计数。
func (l *Limiter) Stats() Stats {
	return Stats{Inflight: l.inflight.Load(), BlobStreams: l.blobStreams.Load(), Limited: l.limited.Load(), TrackedClients: l.buckets.Len()}
}
