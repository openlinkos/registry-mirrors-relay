// Package stats 汇总访问统计，给管理后台和 API 用。全部在内存里、有上限：
//   - 最近 24 小时每分钟一格的计数；
//   - 当前小时的热门镜像、热门客户端，以及"同一客户端反复完整下载同一层"（space-saving 算法，只记前若干名）；
//   - 最近 5000 条请求的摘要（环形缓冲）。
//
// 每过一个小时，把这一小时的汇总写进运营数据库，后台可以看更长时间的历史。
package stats

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/accesslog"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
)

const (
	minutes    = 24 * 60
	recentSize = 5000
	topSize    = 256
)

// Minute 是一分钟的计数。
type Minute struct {
	Start     int64  `json:"start"` // unix 秒，整分钟
	Requests  uint64 `json:"requests"`
	Bytes     uint64 `json:"bytes"`
	Status4xx uint64 `json:"status_4xx"`
	Status5xx uint64 `json:"status_5xx"`
	Limited   uint64 `json:"limited"`
	CacheHits uint64 `json:"cache_hits"`
}

func (m *Minute) add(o Minute) {
	m.Requests += o.Requests
	m.Bytes += o.Bytes
	m.Status4xx += o.Status4xx
	m.Status5xx += o.Status5xx
	m.Limited += o.Limited
	m.CacheHits += o.CacheHits
}

// Request 是一条请求的摘要。
type Request struct {
	Time     time.Time `json:"time"`
	ClientIP string    `json:"client_ip"`
	User     string    `json:"user,omitempty"`
	Method   string    `json:"method"`
	Host     string    `json:"host"`
	URI      string    `json:"uri"`
	Status   int       `json:"status"`
	Size     int64     `json:"size"`
	Duration float64   `json:"duration"`
	Upstream string    `json:"upstream,omitempty"`
	Image    string    `json:"image,omitempty"`
	Cache    string    `json:"cache,omitempty"`
	Agent    string    `json:"user_agent,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// Hour 是写进运营数据库的一小时汇总。
type Hour struct {
	Start   int64  `json:"start"`
	Totals  Minute `json:"totals"`
	Images  []Item `json:"images"`
	Clients []Item `json:"clients"`
	Repeats []Item `json:"repeats"`
}

// Collector 实现了 relay 的 AccessLogger。
type Collector struct {
	mu      sync.Mutex
	ring    [minutes]Minute
	hour    int64 // 当前小时（unix 秒）
	hourTot Minute
	images  *TopK
	clients *TopK
	repeats *TopK
	recent  []Request
	pos     int
	unlist  map[string]int // "上游 主机" → 白名单外的重定向次数
	store   store.Store
	now     func() time.Time
}

// New 创建 Collector。store 可以为 nil（不落盘）。
func New(s store.Store) *Collector {
	c := &Collector{images: NewTopK(topSize), clients: NewTopK(topSize), repeats: NewTopK(topSize),
		recent: make([]Request, 0, recentSize), unlist: map[string]int{}, store: s, now: time.Now}
	c.hour = c.now().Truncate(time.Hour).Unix()
	return c
}

// Log 记一次请求。
func (c *Collector) Log(e *accesslog.Entry) {
	t := e.Time
	if t.IsZero() {
		t = c.now()
	}
	m := Minute{Requests: 1, Bytes: uint64(max(e.Size, 0))}
	switch {
	case e.Status >= 500:
		m.Status5xx = 1
	case e.Status >= 400:
		m.Status4xx = 1
	}
	if e.Limited {
		m.Limited = 1
	}
	if e.Cache == "hit" {
		m.CacheHits = 1
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rotate(t)
	start := t.Unix() / 60 * 60
	slot := &c.ring[(start/60)%minutes]
	if slot.Start != start {
		*slot = Minute{Start: start}
	}
	slot.add(m)
	c.hourTot.add(m)
	if e.Image != "" && e.Size > 0 {
		c.images.Add(e.Image, 1, e.Size)
	}
	if e.ClientIP != "" {
		c.clients.Add(e.ClientIP, 1, max(e.Size, 0))
	}
	// 完整下载了一个层（实际字节数等于 Content-Length）
	if e.Kind == "blob" && e.Status == 200 && e.Size > 0 && e.RespContentLength == strconv.FormatInt(e.Size, 10) {
		c.repeats.Add(e.ClientIP+" "+e.Image+"@"+e.Ref, 1, e.Size)
	}
	r := Request{
		Time: t, ClientIP: e.ClientIP, User: e.UserID, Method: e.Method, Host: e.Host, URI: e.URI,
		Status: e.Status, Size: e.Size, Duration: e.Duration.Seconds(), Upstream: e.Upstream,
		Image: e.Image, Cache: e.Cache, Agent: truncate(e.UserAgent, 160), Error: truncate(e.Error, 200),
	}
	if len(c.recent) < recentSize {
		c.recent = append(c.recent, r)
	} else {
		c.recent[c.pos] = r
	}
	c.pos = (c.pos + 1) % recentSize
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// UnlistedRedirect 记一次白名单外、交给了客户端的重定向（后台据此建议补充白名单）。
func (c *Collector) UnlistedRedirect(upstream, host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := upstream + " " + strings.ToLower(host)
	if _, ok := c.unlist[k]; ok || len(c.unlist) < 1000 {
		c.unlist[k]++
	}
}

// Unlisted 返回白名单外的重定向主机：上游 → 主机 → 次数。
func (c *Collector) Unlisted() map[string]map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]map[string]int{}
	for k, n := range c.unlist {
		up, host, _ := strings.Cut(k, " ")
		if out[up] == nil {
			out[up] = map[string]int{}
		}
		out[up][host] = n
	}
	return out
}

// rotate 跨过整点时，把上一小时的汇总存起来，开始新的一小时。调用方持锁。
func (c *Collector) rotate(t time.Time) {
	h := t.Truncate(time.Hour).Unix()
	if h <= c.hour {
		return
	}
	c.saveHourLocked()
	c.hour = h
	c.hourTot = Minute{}
	c.images, c.clients, c.repeats = NewTopK(topSize), NewTopK(topSize), NewTopK(topSize)
}

func (c *Collector) snapshotLocked() Hour {
	tot := c.hourTot
	tot.Start = c.hour
	return Hour{Start: c.hour, Totals: tot, Images: c.images.Top(50), Clients: c.clients.Top(50), Repeats: c.repeats.Top(50)}
}

func (c *Collector) saveHourLocked() {
	if c.store == nil || c.hourTot.Requests == 0 {
		return
	}
	b, _ := json.Marshal(c.snapshotLocked())
	_ = c.store.SaveHour(time.Unix(c.hour, 0), b)
}

// Flush 把当前小时（未结束的）也存一次，退出时调用。
func (c *Collector) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saveHourLocked()
}

// Run 每分钟检查一次是否跨过整点（没有请求时也能及时存档），直到 ctx 结束。
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.Flush()
			return
		case <-t.C:
			c.mu.Lock()
			c.rotate(c.now())
			c.mu.Unlock()
		}
	}
}

// Series 返回从 since 起每分钟的计数（没有请求的分钟也给出，计数为 0）。
func (c *Collector) Series(since time.Time) []Minute {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().Unix() / 60 * 60
	from := max(since.Unix()/60*60, now-(minutes-1)*60)
	out := make([]Minute, 0, (now-from)/60+1)
	for s := from; s <= now; s += 60 {
		m := c.ring[(s/60)%minutes]
		if m.Start != s {
			m = Minute{Start: s}
		}
		out = append(out, m)
	}
	return out
}

// Summary 是一段时间的汇总和排行。
type Summary struct {
	Totals  Minute `json:"totals"`
	Images  []Item `json:"images"`
	Clients []Item `json:"clients"`
	Repeats []Item `json:"repeats"`
}

// Summarize 汇总最近 d（按整小时，当前小时算进去），排行取前 n。
func (c *Collector) Summarize(d time.Duration, n int) Summary {
	c.mu.Lock()
	cur := c.snapshotLocked()
	cur.Images, cur.Clients, cur.Repeats = c.images.Top(topSize), c.clients.Top(topSize), c.repeats.Top(topSize)
	start := c.hour
	c.mu.Unlock()
	hours := []Hour{cur}
	if c.store != nil && d > time.Hour {
		for _, b := range c.store.HourStats(time.Unix(start, 0).Add(-d + time.Hour)) {
			var h Hour
			if json.Unmarshal(b, &h) == nil && h.Start < start {
				hours = append(hours, h)
			}
		}
	}
	var s Summary
	img, cli, rep := NewTopK(topSize*4), NewTopK(topSize*4), NewTopK(topSize*4)
	for _, h := range hours {
		s.Totals.add(h.Totals)
		for _, it := range h.Images {
			img.Add(it.Key, it.Count, it.Bytes)
		}
		for _, it := range h.Clients {
			cli.Add(it.Key, it.Count, it.Bytes)
		}
		for _, it := range h.Repeats {
			rep.Add(it.Key, it.Count, it.Bytes)
		}
	}
	s.Images, s.Clients, s.Repeats = img.Top(n), cli.Top(n), rep.Top(n)
	return s
}

// Filter 是查询最近请求的条件，空字段不过滤。
type Filter struct {
	ClientIP, Upstream, Image, Query string
	Status                           int // 0 表示不限；4 表示 4xx，5 表示 5xx
}

func (f Filter) match(r *Request) bool {
	switch {
	case f.ClientIP != "" && r.ClientIP != f.ClientIP:
		return false
	case f.Upstream != "" && r.Upstream != f.Upstream:
		return false
	case f.Image != "" && !strings.Contains(r.Image, f.Image):
		return false
	case f.Query != "" && !strings.Contains(r.URI, f.Query) && !strings.Contains(r.Agent, f.Query):
		return false
	case f.Status >= 100 && r.Status != f.Status:
		return false
	case f.Status > 0 && f.Status < 10 && r.Status/100 != f.Status:
		return false
	}
	return true
}

// Recent 返回最近的请求，最新的在前。
func (c *Collector) Recent(f Filter, limit int) []Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Request
	n := len(c.recent)
	for i := 0; i < n && len(out) < limit; i++ {
		r := &c.recent[(c.pos-1-i+n)%n]
		if f.match(r) {
			out = append(out, *r)
		}
	}
	return out
}

// Item 是排行里的一项。Count 和 Bytes 是估计值：被挤出又重新进入时会带上被替换项的计数（space-saving 的上界）。
type Item struct {
	Key   string `json:"key"`
	Count uint64 `json:"count"`
	Bytes int64  `json:"bytes"`
}

// TopK 只记前 k 名（按字节数，相同时按次数）。满了以后新出现的项替换最小的一项。
type TopK struct {
	k     int
	items map[string]*Item
}

// NewTopK 创建排行。
func NewTopK(k int) *TopK { return &TopK{k: k, items: make(map[string]*Item, k)} }

func less(a, b *Item) bool {
	if a.Bytes != b.Bytes {
		return a.Bytes < b.Bytes
	}
	return a.Count < b.Count
}

// Add 计入一次。
func (t *TopK) Add(key string, count uint64, bytes int64) {
	if it, ok := t.items[key]; ok {
		it.Count += count
		it.Bytes += bytes
		return
	}
	if len(t.items) < t.k {
		t.items[key] = &Item{Key: key, Count: count, Bytes: bytes}
		return
	}
	var minIt *Item
	for _, it := range t.items {
		if minIt == nil || less(it, minIt) {
			minIt = it
		}
	}
	delete(t.items, minIt.Key)
	t.items[key] = &Item{Key: key, Count: minIt.Count + count, Bytes: minIt.Bytes + bytes}
}

// Top 返回前 n 名。
func (t *TopK) Top(n int) []Item {
	out := make([]Item, 0, len(t.items))
	for _, it := range t.items {
		out = append(out, *it)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}
