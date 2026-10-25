package upstream

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Quota is the pull quota the upstream reports in its response headers (Docker Hub's Ratelimit-Limit and
// Ratelimit-Remaining). It is shown only in the admin console, never passed to clients.
type Quota struct {
	Limit     int
	Remaining int
	Window    time.Duration // quota window, 0 when the upstream does not give one
	Observed  time.Time
}

// Quota returns the most recently seen quota, or nil if none has been seen.
func (u *Upstream) Quota() *Quota { return u.quota.Load() }

func (u *Upstream) observeQuota(h http.Header) {
	limit, _, ok1 := parseRatelimit(h.Get("Ratelimit-Limit"))
	remaining, window, ok2 := parseRatelimit(h.Get("Ratelimit-Remaining"))
	if !ok1 || !ok2 {
		return
	}
	u.quota.Store(&Quota{Limit: limit, Remaining: remaining, Window: window, Observed: time.Now()})
}

// parseRatelimit parses values such as "100;w=21600".
func parseRatelimit(v string) (n int, window time.Duration, ok bool) {
	if v == "" {
		return 0, 0, false
	}
	first, rest, _ := strings.Cut(v, ";")
	n, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil || n < 0 {
		return 0, 0, false
	}
	for _, p := range strings.Split(rest, ";") {
		if s, found := strings.CutPrefix(strings.TrimSpace(p), "w="); found {
			if sec, err := strconv.Atoi(s); err == nil && sec > 0 {
				window = time.Duration(sec) * time.Second
			}
		}
	}
	return n, window, true
}

// Health is the result of a connectivity probe.
type Health struct {
	OK      bool          `json:"ok"`
	Status  int           `json:"status,omitempty"`
	Latency time.Duration `json:"latency"`
	Error   string        `json:"error,omitempty"`
	Checked time.Time     `json:"checked"`
}

// Probe requests the upstream's /v2/ once (without a token, not counted as a pull). Either 200 or 401 means
// the registry is answering normally. Probes bypass the circuit breaker and do not affect it.
func (u *Upstream) Probe(ctx context.Context) Health {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	h := Health{Checked: start}
	req, err := u.newRequest(ctx, http.MethodGet, &url.URL{Scheme: "https", Host: u.Registry, Path: "/v2/"}, nil)
	if err != nil {
		h.Error = err.Error()
		return h
	}
	resp, err := u.client.http.Do(req)
	h.Latency = time.Since(start)
	if err != nil {
		h.Error = err.Error()
		return h
	}
	discard(resp)
	h.Status = resp.StatusCode
	h.OK = resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized
	if !h.OK {
		h.Error = "upstream returned " + resp.Status
	}
	return h
}
