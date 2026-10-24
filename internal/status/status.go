// Package status 汇总用法页上公开展示的服务状态（/status.json）：各上游最近一次探测、熔断，近 24 小时每小时的
// 可用率，以及本服务近 24 小时每小时的请求成功率。
//
// 这些内容谁都能看到，所以只放公开也无妨的东西：失败原因只给分类（超时、DNS、连接、TLS、HTTP 状态码），
// 不给错误原文（可能带地址）；不给上游额度、请求量和客户端信息。数据只在内存里，重启后从头统计。
package status

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Hours 是展示的小时数。
const Hours = 24

// History 记下每个上游每小时的探测结果。
type History struct {
	mu   sync.Mutex
	runs map[string]*[Hours]bucket
}

type bucket struct {
	start   int64 // 整点的 unix 秒
	ok      int
	total   int
	latency time.Duration // 成功探测的耗时之和
}

// NewHistory 创建 History。
func NewHistory() *History { return &History{runs: map[string]*[Hours]bucket{}} }

// Record 记一次探测。
func (h *History) Record(name string, ok bool, latency time.Duration, at time.Time) {
	start := at.Truncate(time.Hour).Unix()
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.runs[name]
	if r == nil {
		r = new([Hours]bucket)
		h.runs[name] = r
	}
	b := &r[(start/3600)%Hours]
	if b.start != start {
		*b = bucket{start: start}
	}
	b.total++
	if ok {
		b.ok++
		b.latency += latency
	}
}

// Hour 是一小时的统计。Total 为 0 表示这一小时没有数据（还没启动，或者没有探测）。
type Hour struct {
	Start int64 `json:"start"` // 整点的 unix 秒
	OK    int   `json:"ok"`
	Total int   `json:"total"`
}

// hours 返回 name 近 24 小时的统计，旧的在前，最后一项是当前小时。
func (h *History) hours(name string, now time.Time) []Hour {
	cur := now.Truncate(time.Hour).Unix()
	out := make([]Hour, Hours)
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.runs[name]
	for i := range out {
		s := cur - int64(Hours-1-i)*3600
		out[i] = Hour{Start: s}
		if r == nil {
			continue
		}
		if b := r[(s/3600)%Hours]; b.start == s {
			out[i].OK, out[i].Total = b.ok, b.total
		}
	}
	return out
}

// Probe 是一个上游最近一次探测的结果，由调用方从 upstream.Health 转过来。
type Probe struct {
	OK      bool
	Status  int // 上游返回的 HTTP 状态码，没有响应时为 0
	Latency time.Duration
	Error   string // 错误原文，只用来分类
	Checked time.Time
}

// Input 是生成报告需要的数据。
type Input struct {
	Version  string
	Started  time.Time
	Now      time.Time
	Interval time.Duration    // 探测间隔，0 表示不探测
	Names    []string         // 上游，按名字排序
	Probes   map[string]Probe // 最近一次探测，还没探测过的不在里面
	Open     map[string]bool  // 熔断中的上游
	Requests []RequestsHour   // 本服务近 24 小时每小时的请求数和 5xx 数，旧的在前
	History  *History         // 可以为 nil
}

// RequestsHour 是本服务一小时的请求数和其中的 5xx 数。
type RequestsHour struct {
	Start     int64
	Requests  uint64
	Status5xx uint64
}

// Report 是 /status.json 的内容。
type Report struct {
	Status    string     `json:"status"` // ok | degraded | down | unknown
	Version   string     `json:"version"`
	Started   time.Time  `json:"started"`
	Generated time.Time  `json:"generated"`
	Interval  int        `json:"probe_interval"` // 秒，0 表示不探测上游
	Service   Service    `json:"service"`
	Upstreams []Upstream `json:"upstreams"`
}

// Service 是本服务近 24 小时每小时的请求成功率（非 5xx 的比例）。
type Service struct {
	Hours []Ratio `json:"hours"`
}

// Ratio 是一小时的比例。请求太少时 Value 为 nil，免得一两个失败就显得很糟。
type Ratio struct {
	Start int64    `json:"start"`
	Value *float64 `json:"value"`
}

// MinRequests 是一小时里至少要有的请求数，少于它时不算成功率。
const MinRequests = 20

// Upstream 是一个上游的状态。
type Upstream struct {
	Name      string     `json:"name"`
	Status    string     `json:"status"`           // ok | down | open（熔断中）| unknown
	Reason    string     `json:"reason,omitempty"` // timeout | dns | connect | tls | http_<状态码> | error
	LatencyMS int64      `json:"latency_ms,omitempty"`
	Checked   *time.Time `json:"checked,omitempty"`
	Hours     []Hour     `json:"hours"`
}

// Build 生成报告。
func Build(in Input) Report {
	r := Report{
		Version: in.Version, Started: in.Started.UTC().Truncate(time.Second), Generated: in.Now.UTC().Truncate(time.Second),
		Interval: int(in.Interval / time.Second),
	}
	names := append([]string(nil), in.Names...)
	sort.Strings(names)
	ok, bad := 0, 0
	for _, name := range names {
		u := Upstream{Name: name, Status: "unknown", Hours: make([]Hour, 0)}
		if in.History != nil {
			u.Hours = in.History.hours(name, in.Now)
		}
		if p, seen := in.Probes[name]; seen {
			checked := p.Checked.UTC().Truncate(time.Second)
			u.Checked = &checked
			if p.OK {
				u.Status, u.LatencyMS = "ok", p.Latency.Milliseconds()
			} else {
				u.Status, u.Reason = "down", Reason(p.Status, p.Error)
			}
		}
		if in.Open[name] {
			u.Status = "open"
		}
		switch u.Status {
		case "ok":
			ok++
		case "down", "open":
			bad++
		}
		r.Upstreams = append(r.Upstreams, u)
	}
	switch {
	case ok == 0 && bad == 0:
		r.Status = "unknown"
	case bad == 0:
		r.Status = "ok"
	case ok == 0:
		r.Status = "down"
	default:
		r.Status = "degraded"
	}
	r.Service.Hours = make([]Ratio, 0, len(in.Requests))
	for _, h := range in.Requests {
		x := Ratio{Start: h.Start}
		if h.Requests >= MinRequests {
			v := 1 - float64(h.Status5xx)/float64(h.Requests)
			v = float64(int(v*10000)) / 10000
			x.Value = &v
		}
		r.Service.Hours = append(r.Service.Hours, x)
	}
	return r
}

// Reason 把探测失败归成几类。错误原文可能带地址，不公开。
func Reason(status int, msg string) string {
	if status != 0 {
		return "http_" + strconv.Itoa(status)
	}
	s := strings.ToLower(msg)
	switch {
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline exceeded"):
		return "timeout"
	case strings.Contains(s, "no such host"):
		return "dns"
	case strings.Contains(s, "tls"), strings.Contains(s, "x509"), strings.Contains(s, "certificate"):
		return "tls"
	case strings.Contains(s, "connect"), strings.Contains(s, "refused"), strings.Contains(s, "reset"), strings.Contains(s, "unreachable"):
		return "connect"
	}
	return "error"
}
