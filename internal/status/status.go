// Package status assembles the service status shown publicly on the usage page (/status.json): each
// upstream's latest probe, circuit breaker and hourly availability over the last 24 hours, and this
// service's hourly request success rate over the last 24 hours.
//
// Anyone can see this, so it holds only what is harmless to publish: failure reasons are given only as
// categories (timeout, DNS, connection, TLS, HTTP status code), never the raw error text (which may
// contain addresses); no upstream quota, request volume or client information. The probe history is
// kept in memory; with a writable operational database the app saves it (Snapshot) and restores it
// after a restart (Restore).
package status

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Hours is the number of hours shown.
const Hours = 24

// History records each upstream's probe results per hour.
type History struct {
	mu   sync.Mutex
	runs map[string]*[Hours]bucket
}

type bucket struct {
	start   int64 // unix seconds of the start of the hour
	ok      int
	total   int
	latency time.Duration // sum of the durations of successful probes
}

// NewHistory creates a History.
func NewHistory() *History { return &History{runs: map[string]*[Hours]bucket{}} }

// Record records one probe.
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

// savedHour is one hour of an upstream's probes as saved by Snapshot.
type savedHour struct {
	Start   int64         `json:"start"`
	OK      int           `json:"ok"`
	Total   int           `json:"total"`
	Latency time.Duration `json:"latency"`
}

// Snapshot returns the probe results of the last 24 hours as JSON, for Restore after a restart.
func (h *History) Snapshot(now time.Time) []byte {
	cut := now.Truncate(time.Hour).Unix() - (Hours-1)*3600
	out := map[string][]savedHour{}
	h.mu.Lock()
	for name, r := range h.runs {
		for _, b := range r {
			if b.start >= cut && b.total > 0 {
				out[name] = append(out[name], savedHour{Start: b.start, OK: b.ok, Total: b.total, Latency: b.latency})
			}
		}
	}
	h.mu.Unlock()
	for _, list := range out {
		sort.Slice(list, func(i, j int) bool { return list[i].Start < list[j].Start })
	}
	raw, _ := json.Marshal(out)
	return raw
}

// Restore adds the probe results saved by Snapshot that still fall within the last 24 hours. An hour
// that was also recorded since start (a restart within the hour) adds up. It returns the start of the
// earliest hour restored (zero when none was).
func (h *History) Restore(raw []byte, now time.Time) (time.Time, error) {
	var in map[string][]savedHour
	if err := json.Unmarshal(raw, &in); err != nil {
		return time.Time{}, err
	}
	var first int64
	cur := now.Truncate(time.Hour).Unix()
	cut := cur - (Hours-1)*3600
	h.mu.Lock()
	defer h.mu.Unlock()
	for name, list := range in {
		for _, s := range list {
			if s.Start < cut || s.Start > cur || s.Start%3600 != 0 || s.Total <= 0 || s.OK < 0 || s.OK > s.Total {
				continue
			}
			r := h.runs[name]
			if r == nil {
				r = new([Hours]bucket)
				h.runs[name] = r
			}
			b := &r[(s.Start/3600)%Hours]
			if b.start != s.Start {
				*b = bucket{start: s.Start}
			}
			b.ok += s.OK
			b.total += s.Total
			b.latency += max(s.Latency, 0)
			if first == 0 || s.Start < first {
				first = s.Start
			}
		}
	}
	if first == 0 {
		return time.Time{}, nil
	}
	return time.Unix(first, 0).UTC(), nil
}

// Hour holds the statistics for one hour. Total 0 means there is no data for that hour (the service
// had not started yet, or there were no probes).
type Hour struct {
	Start int64 `json:"start"` // unix seconds of the start of the hour
	OK    int   `json:"ok"`
	Total int   `json:"total"`
}

// Hours returns the statistics for name over the last 24 hours, oldest first; the last item is the
// current hour.
func (h *History) Hours(name string, now time.Time) []Hour {
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

// Probe is the result of an upstream's latest probe, converted by the caller from upstream.Health.
type Probe struct {
	OK      bool
	Status  int // HTTP status code returned by the upstream, 0 when there was no response
	Latency time.Duration
	Error   string // raw error text, used only for classification
	Checked time.Time
}

// Input holds the data needed to build the report.
type Input struct {
	Version  string
	Started  time.Time
	Since    time.Time // the earliest data restored after a restart; zero when nothing was restored
	Now      time.Time
	Interval time.Duration    // probe interval, 0 means no probing
	Names    []string         // upstreams, sorted by name
	Probes   map[string]Probe // latest probe; upstreams not yet probed are absent
	Open     map[string]bool  // upstreams whose circuit breaker is open
	Requests []RequestsHour   // this service's hourly request and 5xx counts for 24 hours, oldest first
	History  *History         // may be nil
}

// RequestsHour holds this service's request count for one hour and how many of them were 5xx.
type RequestsHour struct {
	Start     int64
	Requests  uint64
	Status5xx uint64
}

// Report is the content of /status.json.
type Report struct {
	Status    string     `json:"status"` // ok | degraded | down | unknown
	Version   string     `json:"version"`
	Started   time.Time  `json:"started"`
	Since     time.Time  `json:"since"` // where the figures begin: the earliest data restored after a restart, or started
	Generated time.Time  `json:"generated"`
	Interval  int        `json:"probe_interval"` // seconds, 0 means upstreams are not probed
	Service   Service    `json:"service"`
	Upstreams []Upstream `json:"upstreams"`
}

// Service holds this service's hourly request success rate (share of non-5xx) over the last 24 hours.
type Service struct {
	Hours []Ratio `json:"hours"`
}

// Ratio is the rate for one hour. Value is nil when there are too few requests, so that one or two
// failures do not make things look bad.
type Ratio struct {
	Start int64    `json:"start"`
	Value *float64 `json:"value"`
}

// MinRequests is the minimum number of requests in an hour; below it no success rate is computed.
const MinRequests = 20

// Upstream is the status of one upstream.
type Upstream struct {
	Name      string     `json:"name"`
	Status    string     `json:"status"`           // ok | down | open (circuit breaker open) | unknown
	Reason    string     `json:"reason,omitempty"` // timeout | dns | connect | tls | http_<status code> | error
	LatencyMS int64      `json:"latency_ms,omitempty"`
	Checked   *time.Time `json:"checked,omitempty"`
	Hours     []Hour     `json:"hours"`
}

// Build builds the report.
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
			u.Hours = in.History.Hours(name, in.Now)
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
	r.Since = r.Started
	if !in.Since.IsZero() && in.Since.Before(in.Started) {
		r.Since = in.Since.UTC().Truncate(time.Second)
	}
	return r
}

// Level puts availability into three bands, matching the usage page: ok (≥99%), mid (≥90%), bad.
func Level(v float64) string {
	switch {
	case v >= 0.99:
		return "ok"
	case v >= 0.9:
		return "mid"
	}
	return "bad"
}

// Reason sorts probe failures into a few categories. The raw error text may contain addresses and is
// not published.
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
