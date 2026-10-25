// Package metrics exposes metrics in the Prometheus text format. There are few metrics, so writing
// them by hand is simpler than pulling in client_golang (a dozen or so dependencies).
//
// Request counters come from each access log record (Metrics implements relay's AccessLogger); state
// such as the cache, rate limits and circuit breakers is read by callbacks at scrape time.
package metrics

import (
	"bufio"
	"fmt"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/accesslog"
)

// Sample is a labelled value. Labels is produced with Labels().
type Sample struct {
	Labels string
	Value  float64
}

// Labels produces {k="v",...} in the given order, with the values escaped.
func Labels(kv ...string) string {
	if len(kv) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(kv[i])
		b.WriteString(`="`)
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(kv[i+1])
		b.WriteString(v)
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

type gauge struct {
	name, help, typ string
	fn              func() []Sample
}

// Request duration buckets (seconds). Layer downloads can take a long time, so the upper bound is
// extended to 10 minutes.
var buckets = []float64{0.005, 0.025, 0.1, 0.5, 1, 5, 30, 120, 600}

type hist struct {
	counts [10]atomic.Uint64 // len(buckets)+1, the last slot is +Inf
	sum    atomic.Uint64     // microseconds
}

// Metrics is safe for concurrent use.
type Metrics struct {
	requests sync.Map // labels → *atomic.Uint64
	bytes    sync.Map
	durs     sync.Map // kind → *hist
	mu       sync.Mutex
	gauges   []gauge
	start    time.Time
	version  string
}

// New creates a Metrics.
func New(version string) *Metrics { return &Metrics{start: time.Now(), version: version} }

func counter(m *sync.Map, key string) *atomic.Uint64 {
	if v, ok := m.Load(key); ok {
		return v.(*atomic.Uint64)
	}
	v, _ := m.LoadOrStore(key, new(atomic.Uint64))
	return v.(*atomic.Uint64)
}

func statusClass(s int) string {
	if s < 100 || s > 599 {
		return "other"
	}
	return strconv.Itoa(s/100) + "xx"
}

// Log records one request.
func (m *Metrics) Log(e *accesslog.Entry) {
	kind := e.Kind
	if kind == "" {
		kind = "other"
	}
	up := e.Upstream
	if up == "" {
		up = "none"
	}
	counter(&m.requests, Labels("upstream", up, "kind", kind, "code", statusClass(e.Status), "cache", e.Cache)).Add(1)
	if e.Size > 0 {
		counter(&m.bytes, Labels("upstream", up, "kind", kind)).Add(uint64(e.Size))
	}
	v, ok := m.durs.Load(kind)
	if !ok {
		v, _ = m.durs.LoadOrStore(kind, &hist{})
	}
	h := v.(*hist)
	sec := e.Duration.Seconds()
	i := sort.SearchFloat64s(buckets, sec)
	h.counts[i].Add(1)
	h.sum.Add(uint64(e.Duration.Microseconds()))
}

// Register adds a metric computed at scrape time. typ is gauge or counter.
func (m *Metrics) Register(name, help, typ string, fn func() []Sample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gauges = append(m.gauges, gauge{name: name, help: help, typ: typ, fn: fn})
}

func writeMap(w *bufio.Writer, name, help, typ string, mp *sync.Map) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	var keys []string
	vals := map[string]uint64{}
	mp.Range(func(k, v any) bool {
		keys = append(keys, k.(string))
		vals[k.(string)] = v.(*atomic.Uint64).Load()
		return true
	})
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s%s %d\n", name, k, vals[k])
	}
}

func fmtFloat(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// ServeHTTP writes out all metrics.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	bw := bufio.NewWriter(w)
	defer func() { _ = bw.Flush() }()

	writeMap(bw, "relay_requests_total", "Requests handled", "counter", &m.requests)
	writeMap(bw, "relay_response_bytes_total", "Bytes written to clients", "counter", &m.bytes)

	fmt.Fprintf(bw, "# HELP relay_request_duration_seconds Request duration (layer downloads include transfer time)\n# TYPE relay_request_duration_seconds histogram\n")
	var kinds []string
	m.durs.Range(func(k, _ any) bool { kinds = append(kinds, k.(string)); return true })
	sort.Strings(kinds)
	for _, k := range kinds {
		v, _ := m.durs.Load(k)
		h := v.(*hist)
		var cum uint64
		for i, le := range append(append([]float64{}, buckets...), math.Inf(1)) {
			cum += h.counts[i].Load()
			fmt.Fprintf(bw, "relay_request_duration_seconds_bucket%s %d\n", Labels("kind", k, "le", fmtFloat(le)), cum)
		}
		fmt.Fprintf(bw, "relay_request_duration_seconds_sum%s %s\n", Labels("kind", k), fmtFloat(float64(h.sum.Load())/1e6))
		fmt.Fprintf(bw, "relay_request_duration_seconds_count%s %d\n", Labels("kind", k), cum)
	}

	m.mu.Lock()
	gs := append([]gauge(nil), m.gauges...)
	m.mu.Unlock()
	for _, g := range gs {
		fmt.Fprintf(bw, "# HELP %s %s\n# TYPE %s %s\n", g.name, g.help, g.name, g.typ)
		for _, s := range g.fn() {
			fmt.Fprintf(bw, "%s%s %s\n", g.name, s.Labels, fmtFloat(s.Value))
		}
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Fprintf(bw, "# HELP relay_build_info Version\n# TYPE relay_build_info gauge\nrelay_build_info%s 1\n", Labels("version", m.version, "go", runtime.Version()))
	fmt.Fprintf(bw, "# TYPE relay_start_time_seconds gauge\nrelay_start_time_seconds %d\n", m.start.Unix())
	fmt.Fprintf(bw, "# TYPE go_goroutines gauge\ngo_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(bw, "# TYPE go_memstats_heap_inuse_bytes gauge\ngo_memstats_heap_inuse_bytes %d\n", ms.HeapInuse)
	fmt.Fprintf(bw, "# TYPE go_memstats_sys_bytes gauge\ngo_memstats_sys_bytes %d\n", ms.Sys)
	fmt.Fprintf(bw, "# TYPE go_gc_cycles_total counter\ngo_gc_cycles_total %d\n", ms.NumGC)
	if rss := residentBytes(); rss > 0 {
		fmt.Fprintf(bw, "# TYPE process_resident_memory_bytes gauge\nprocess_resident_memory_bytes %d\n", rss)
	}
}

// residentBytes reads /proc/self/statm (Linux only).
func residentBytes() int64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	pages, _ := strconv.ParseInt(f[1], 10, 64)
	return pages * int64(os.Getpagesize())
}
