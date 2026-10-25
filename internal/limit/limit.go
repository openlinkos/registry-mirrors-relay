// Package limit provides rate limits and concurrency caps.
//
// Clients are counted by their signed-in user name, or by IP when not signed in (IPv6 by /64, since a
// machine usually has a whole /64). Manifest requests use a token bucket (HEAD counts with a small
// weight); layers are limited by the number of concurrent streams per client; there are also global
// concurrency caps. The number of remembered clients is bounded (LRU): beyond the limit the client
// seen least recently is evicted, rather than clearing the whole table.
package limit

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/lru"
)

// Key is the counting key for a client.
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

// Config holds the rate limit parameters and is replaced as a whole on hot reload.
type Config struct {
	ManifestRate         float64 // per second, 0 means unlimited
	ManifestBurst        int
	HeadWeight           float64
	BlobStreamsPerClient int // 0 means unlimited
	MaxInflight          int // 0 means unlimited
	MaxBlobStreams       int // 0 means unlimited
	TrackedClients       int
	AuthFailuresPerMin   int // sign-in failures allowed per IP per minute, 0 means unlimited
	Exempt               []netip.Prefix
}

type bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// Limiter is safe for concurrent use. Counter state is kept when the parameters are replaced.
type Limiter struct {
	cfg     atomic.Pointer[Config]
	buckets *lru.Cache[string, *bucket]
	authBkt *lru.Cache[string, *bucket]
	now     func() time.Time

	mu sync.Mutex
	// Only clients with streams in progress are recorded and they are removed when their streams
	// end, so the size is bounded by concurrency
	streams map[string]int

	inflight    atomic.Int64
	blobStreams atomic.Int64
	limited     atomic.Uint64
}

// New creates a Limiter.
func New(c Config) *Limiter {
	l := &Limiter{
		buckets: lru.New[string, *bucket](max(c.TrackedClients, 1), 0),
		authBkt: lru.New[string, *bucket](max(c.TrackedClients, 1), 0),
		now:     time.Now, streams: map[string]int{},
	}
	l.cfg.Store(&c)
	return l
}

// SetConfig replaces the parameters (hot reload). Existing counters are kept; a change to
// TrackedClients takes effect after a restart.
func (l *Limiter) SetConfig(c Config) { l.cfg.Store(&c) }

func (l *Limiter) exempt(ip netip.Addr) bool {
	for _, p := range l.cfg.Load().Exempt {
		if p.Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}

// Override is a per-account rate limit (0 means use the default).
type Override struct {
	ManifestRate float64
	BlobStreams  int
}

// AllowManifest takes tokens for one manifest request. When the request is not allowed it returns the
// suggested wait before retrying.
func (l *Limiter) AllowManifest(key string, ip netip.Addr, head bool, ov Override) (bool, time.Duration) {
	c := l.cfg.Load()
	rate, burst := c.ManifestRate, float64(c.ManifestBurst)
	if ov.ManifestRate > 0 {
		rate = ov.ManifestRate
		burst = max(burst, rate*60) // an account with a raised limit gets a burst of at least one minute's worth
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

// authBucket returns the sign-in failure token bucket for this IP (when full it allows
// AuthFailuresPerMin failures, and the same number is refilled every minute).
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

// AuthAllowed reports whether this IP may attempt to sign in again now (it only checks and does not
// take a token).
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

// AuthFailed records one sign-in failure.
func (l *Limiter) AuthFailed(ip netip.Addr) {
	if l.cfg.Load().AuthFailuresPerMin <= 0 {
		return
	}
	b, _, _ := l.authBucket(ip)
	b.mu.Lock()
	b.tokens = max(0, b.tokens-1)
	b.mu.Unlock()
}

// AcquireBlob takes a layer stream slot (both per client and global). When ok is false, reason is
// client or global. On success, release must be called.
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

// AcquireInflight takes a global request slot. On success, release must be called.
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

// Stats holds the current counters.
type Stats struct {
	Inflight, BlobStreams int64
	Limited               uint64
	TrackedClients        int
}

// Stats returns the counters.
func (l *Limiter) Stats() Stats {
	return Stats{Inflight: l.inflight.Load(), BlobStreams: l.blobStreams.Load(), Limited: l.limited.Load(), TrackedClients: l.buckets.Len()}
}
