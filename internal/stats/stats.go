// Package stats aggregates access statistics for the admin console and the API. Everything is kept in
// memory, with limits:
//   - per-minute counters for the last 24 hours;
//   - the current hour's top images, top clients, and "the same client fully downloading the same
//     layer again and again" (space-saving algorithm, only the top entries are kept);
//   - summaries of the last 5000 requests (ring buffer).
//
// At the end of each hour that hour's summary is written to the operational database, so the console
// can show a longer history. The per-minute counters and the accounts' last requests can be saved too
// (Snapshot) and restored after a restart (Restore); the current hour's summary saved on exit is picked
// up again when the service starts within the same hour.
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

// Minute holds the counters for one minute.
type Minute struct {
	Start     int64  `json:"start"` // unix seconds, on the minute
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

// Request is the summary of one request.
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

// Hour is the one-hour summary written to the operational database.
type Hour struct {
	Start   int64  `json:"start"`
	Totals  Minute `json:"totals"`
	Images  []Item `json:"images"`
	Clients []Item `json:"clients"`
	Repeats []Item `json:"repeats"`
}

// Collector implements relay's AccessLogger.
type Collector struct {
	mu      sync.Mutex
	ring    [minutes]Minute
	hour    int64 // current hour (unix seconds)
	hourTot Minute
	images  *TopK
	clients *TopK
	repeats *TopK
	recent  []Request
	pos     int
	unlist  map[string]int       // "upstream host" → number of redirects outside the allowlist
	seen    map[string]time.Time // client account → time of its last request (since start)
	hits    map[string]uint64    // image rule → number of refusals (since start)
	store   store.Store
	now     func() time.Time
}

// New creates a Collector. store may be nil (nothing is persisted).
func New(s store.Store) *Collector {
	c := &Collector{images: NewTopK(topSize), clients: NewTopK(topSize), repeats: NewTopK(topSize),
		recent: make([]Request, 0, recentSize), unlist: map[string]int{}, seen: map[string]time.Time{}, hits: map[string]uint64{},
		store: s, now: time.Now}
	c.hour = c.now().Truncate(time.Hour).Unix()
	c.resumeHour()
	return c
}

// resumeHour continues the current hour's summary saved before a restart within the same hour, so that
// saving the hour again does not replace what was counted before.
func (c *Collector) resumeHour() {
	if c.store == nil {
		return
	}
	for _, b := range c.store.HourStats(time.Unix(c.hour, 0)) {
		var h Hour
		if json.Unmarshal(b, &h) != nil || h.Start != c.hour {
			continue
		}
		c.hourTot.add(h.Totals)
		for _, it := range h.Images {
			c.images.Add(it.Key, it.Count, it.Bytes)
		}
		for _, it := range h.Clients {
			c.clients.Add(it.Key, it.Count, it.Bytes)
		}
		for _, it := range h.Repeats {
			c.repeats.Add(it.Key, it.Count, it.Bytes)
		}
	}
}

// savedState is what Snapshot saves: the per-minute counters as [start, requests, bytes, 4xx, 5xx,
// limited, cache hits] and the time of each account's last request.
type savedState struct {
	Minutes [][7]uint64          `json:"minutes"`
	Seen    map[string]time.Time `json:"seen,omitempty"`
}

// Snapshot returns the per-minute counters of the last 24 hours and each client account's last request,
// as JSON for Restore after a restart.
func (c *Collector) Snapshot() []byte {
	c.mu.Lock()
	now := c.now().Unix() / 60 * 60
	cut := now - (minutes-1)*60
	st := savedState{Seen: make(map[string]time.Time, len(c.seen))}
	for _, m := range c.ring {
		if m.Start >= cut && m.Start <= now && m.Requests > 0 {
			st.Minutes = append(st.Minutes, [7]uint64{uint64(m.Start), m.Requests, m.Bytes, m.Status4xx, m.Status5xx, m.Limited, m.CacheHits})
		}
	}
	for k, t := range c.seen {
		st.Seen[k] = t
	}
	c.mu.Unlock()
	sort.Slice(st.Minutes, func(i, j int) bool { return st.Minutes[i][0] < st.Minutes[j][0] })
	raw, _ := json.Marshal(st)
	return raw
}

// Restore adds the counters saved by Snapshot that still fall within the last 24 hours (a minute also
// counted since start adds up) and keeps the later last request of each account. It returns the earliest
// minute restored (zero when none was).
func (c *Collector) Restore(raw []byte) (time.Time, error) {
	var st savedState
	if err := json.Unmarshal(raw, &st); err != nil {
		return time.Time{}, err
	}
	var first int64
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().Unix() / 60 * 60
	cut := now - (minutes-1)*60
	for _, v := range st.Minutes {
		start := int64(v[0])
		if start < cut || start > now || start%60 != 0 {
			continue
		}
		slot := &c.ring[(start/60)%minutes]
		if slot.Start != start {
			*slot = Minute{Start: start}
		}
		slot.add(Minute{Requests: v[1], Bytes: v[2], Status4xx: v[3], Status5xx: v[4], Limited: v[5], CacheHits: v[6]})
		if first == 0 || start < first {
			first = start
		}
	}
	for k, t := range st.Seen {
		if prev, ok := c.seen[k]; ok && !t.After(prev) {
			continue
		}
		if _, ok := c.seen[k]; ok || len(c.seen) < maxTracked {
			c.seen[k] = t
		}
	}
	if first == 0 {
		return time.Time{}, nil
	}
	return time.Unix(first, 0).UTC(), nil
}

// Log records one request.
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
	if e.UserID != "" {
		if _, ok := c.seen[e.UserID]; ok || len(c.seen) < maxTracked {
			c.seen[e.UserID] = t
		}
	}
	// A layer was fully downloaded (the actual byte count equals Content-Length)
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

// UnlistedRedirect records a redirect outside the allowlist that was handed to the client (the console
// uses this to suggest additions to the allowlist).
func (c *Collector) UnlistedRedirect(upstream, host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := upstream + " " + strings.ToLower(host)
	if _, ok := c.unlist[k]; ok || len(c.unlist) < 1000 {
		c.unlist[k]++
	}
}

// maxTracked is the maximum number of entries kept for account last-used times and for rule hit
// counts, each.
const maxTracked = 10000

// RuleHit records one refusal by an image rule.
func (c *Collector) RuleHit(rule string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.hits[rule]; ok || len(c.hits) < maxTracked {
		c.hits[rule]++
	}
}

// RuleHits returns the number of refusals by each image rule (since start).
func (c *Collector) RuleHits() map[string]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]uint64, len(c.hits))
	for k, v := range c.hits {
		out[k] = v
	}
	return out
}

// LastSeen returns the time of each client account's last request (since start).
func (c *Collector) LastSeen() map[string]time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]time.Time, len(c.seen))
	for k, v := range c.seen {
		out[k] = v
	}
	return out
}

// Unlisted returns the redirect hosts outside the allowlist: upstream → host → count.
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

// rotate, when the hour boundary is crossed, saves the previous hour's summary and starts a new hour.
// The caller holds the lock.
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

// Flush also saves the current (unfinished) hour; it is called on exit.
func (c *Collector) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saveHourLocked()
}

// Run checks once a minute whether the hour boundary has been crossed (so the hour is saved promptly
// even when there are no requests), until ctx is done.
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

// Series returns the per-minute counters from since onwards (minutes without requests are included,
// with zero counts).
func (c *Collector) Series(since time.Time) []Minute {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().Unix() / 60 * 60
	from := max(since.Unix()/60*60, now-(minutes-1)*60)
	if from > now {
		return nil // since is in the future (the clock went backwards)
	}
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

// Summary holds the totals and rankings for a period.
type Summary struct {
	Totals  Minute `json:"totals"`
	Images  []Item `json:"images"`
	Clients []Item `json:"clients"`
	Repeats []Item `json:"repeats"`
}

// Summarize aggregates the last d (in whole hours, including the current hour) and keeps the top n of
// each ranking.
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

// Filter holds the conditions for querying recent requests; empty fields do not filter.
type Filter struct {
	ClientIP, Upstream, Image string
	User                      string // the account, exactly
	Query                     string // found in the URI, User-Agent, client IP or image, or equal to the account name
	Status                    int    // 0 means any; 4 means 4xx, 5 means 5xx
}

func (f Filter) match(r *Request) bool {
	switch {
	case f.ClientIP != "" && r.ClientIP != f.ClientIP:
		return false
	case f.User != "" && r.User != f.User:
		return false
	case f.Upstream != "" && r.Upstream != f.Upstream:
		return false
	case f.Image != "" && !strings.Contains(r.Image, f.Image):
		return false
	case f.Query != "" && !strings.Contains(r.URI, f.Query) && !strings.Contains(r.Agent, f.Query) &&
		!strings.Contains(r.ClientIP, f.Query) && r.User != f.Query && !strings.Contains(r.Image, f.Query):
		return false
	case f.Status >= 100 && r.Status != f.Status:
		return false
	case f.Status > 0 && f.Status < 10 && r.Status/100 != f.Status:
		return false
	}
	return true
}

// Recent returns the recent requests, newest first.
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

// Item is one entry in a ranking. Count and Bytes are estimates: an entry that was pushed out and
// comes back in inherits the counts of the entry it replaces (the space-saving upper bound).
type Item struct {
	Key   string `json:"key"`
	Count uint64 `json:"count"`
	Bytes int64  `json:"bytes"`
}

// TopK keeps only the top k entries (by bytes, then by count on ties). Once it is full, a new entry
// replaces the smallest one.
type TopK struct {
	k     int
	items map[string]*Item
}

// NewTopK creates a ranking.
func NewTopK(k int) *TopK { return &TopK{k: k, items: make(map[string]*Item, k)} }

func less(a, b *Item) bool {
	if a.Bytes != b.Bytes {
		return a.Bytes < b.Bytes
	}
	return a.Count < b.Count
}

// Add counts one occurrence.
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

// Top returns the top n entries.
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
