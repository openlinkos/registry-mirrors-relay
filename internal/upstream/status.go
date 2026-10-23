package upstream

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Quota 是上游在响应头里报告的拉取额度（Docker Hub 的 Ratelimit-Limit、Ratelimit-Remaining）。
// 只给管理后台看，不转给客户端。
type Quota struct {
	Limit     int
	Remaining int
	Window    time.Duration // 额度的统计周期，上游没给时为 0
	Observed  time.Time
}

// Quota 返回最近一次看到的额度，没看到过时为 nil。
func (u *Upstream) Quota() *Quota { return u.quota.Load() }

func (u *Upstream) observeQuota(h http.Header) {
	limit, _, ok1 := parseRatelimit(h.Get("Ratelimit-Limit"))
	remaining, window, ok2 := parseRatelimit(h.Get("Ratelimit-Remaining"))
	if !ok1 || !ok2 {
		return
	}
	u.quota.Store(&Quota{Limit: limit, Remaining: remaining, Window: window, Observed: time.Now()})
}

// parseRatelimit 解析 "100;w=21600" 这样的值。
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

// Health 是一次连通探测的结果。
type Health struct {
	OK      bool          `json:"ok"`
	Status  int           `json:"status,omitempty"`
	Latency time.Duration `json:"latency"`
	Error   string        `json:"error,omitempty"`
	Checked time.Time     `json:"checked"`
}

// Probe 请求一次上游的 /v2/（不带令牌，不算拉取次数）。返回 200 或 401 都说明 registry 在正常应答。
// 探测不经过熔断器，也不影响它。
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
		h.Error = "上游返回 " + resp.Status
	}
	return h
}
