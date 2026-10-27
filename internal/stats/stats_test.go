package stats

import (
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/accesslog"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
)

func entry(t time.Time, ip, image string, status int, size int64) *accesslog.Entry {
	return &accesslog.Entry{Time: t, ClientIP: ip, Image: image, Status: status, Size: size, Kind: "blob", Ref: "sha256:aa",
		RespContentLength: strconv.FormatInt(size, 10), URI: "/v2/" + image + "/blobs/sha256:aa", Upstream: "docker.io"}
}

func TestSeriesAndSummary(t *testing.T) {
	s, err := store.OpenBolt(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := New(s)
	now := time.Date(2026, 10, 23, 10, 0, 30, 0, time.UTC)
	c.now = func() time.Time { return now }
	c.hour = now.Truncate(time.Hour).Unix()

	for i := 0; i < 10; i++ {
		c.Log(entry(now, "198.51.100.1", "docker.io/library/nginx", 200, 1000))
	}
	c.Log(entry(now, "198.51.100.2", "ghcr.io/a/b", 503, 0))
	now = now.Add(2 * time.Minute)
	c.Log(entry(now, "198.51.100.2", "ghcr.io/a/b", 404, 0))

	series := c.Series(now.Add(-2 * time.Minute))
	if len(series) != 3 || series[0].Requests != 11 || series[0].Status5xx != 1 || series[1].Requests != 0 || series[2].Status4xx != 1 {
		t.Fatalf("per-minute counters: %+v", series)
	}
	// crossing the hour boundary: the previous hour is saved to the database
	now = now.Add(time.Hour)
	c.Log(entry(now, "198.51.100.3", "quay.io/x/y", 200, 5000))
	sum := c.Summarize(24*time.Hour, 10)
	if sum.Totals.Requests != 13 || sum.Totals.Bytes != 15000 {
		t.Fatalf("24-hour totals: %+v", sum.Totals)
	}
	if len(sum.Images) != 2 || sum.Images[0].Key != "docker.io/library/nginx" || sum.Images[0].Bytes != 10000 {
		t.Fatalf("top images: %+v", sum.Images)
	}
	if len(sum.Repeats) == 0 || sum.Repeats[0].Count != 10 || sum.Repeats[0].Key != "198.51.100.1 docker.io/library/nginx@sha256:aa" {
		t.Fatalf("repeated full downloads: %+v", sum.Repeats)
	}
	if one := c.Summarize(time.Hour, 10); one.Totals.Requests != 1 {
		t.Fatalf("the last hour should include only the current hour: %+v", one.Totals)
	}
}

func TestRecentFilter(t *testing.T) {
	c := New(nil)
	now := time.Now()
	for i := 0; i < recentSize+100; i++ {
		c.Log(entry(now, fmt.Sprintf("198.51.100.%d", i%3), "docker.io/library/x", 200+i%2*204, 1))
	}
	all := c.Recent(Filter{}, recentSize*2)
	if len(all) != recentSize {
		t.Fatalf("ring buffer should keep only %d entries: %d", recentSize, len(all))
	}
	if got := c.Recent(Filter{ClientIP: "198.51.100.1"}, 10); len(got) != 10 || got[0].ClientIP != "198.51.100.1" {
		t.Fatalf("filter by IP: %d", len(got))
	}
	for _, r := range c.Recent(Filter{Status: 4}, 50) {
		if r.Status/100 != 4 {
			t.Fatalf("filter by 4xx: %d", r.Status)
		}
	}
	// newest first
	c.Log(&accesslog.Entry{Time: now, ClientIP: "203.0.113.9", Status: 200})
	if c.Recent(Filter{}, 1)[0].ClientIP != "203.0.113.9" {
		t.Fatal("the newest entry should come first")
	}
}

func TestTopKBounded(t *testing.T) {
	k := NewTopK(10)
	for i := 0; i < 1000; i++ {
		k.Add(fmt.Sprint(i), 1, int64(i%7))
	}
	k.Add("big", 1, 1_000_000)
	top := k.Top(3)
	if len(k.items) != 10 || top[0].Key != "big" {
		t.Fatalf("top entries: %+v (%d items in total)", top, len(k.items))
	}
}

func TestUnlisted(t *testing.T) {
	c := New(nil)
	c.UnlistedRedirect("quay.io", "CDN99.quay.io")
	c.UnlistedRedirect("quay.io", "cdn99.quay.io")
	if c.Unlisted()["quay.io"]["cdn99.quay.io"] != 2 {
		t.Fatalf("%v", c.Unlisted())
	}
}

func TestSeriesSinceInFuture(t *testing.T) {
	c := New(nil)
	if got := c.Series(time.Now().Add(time.Hour)); len(got) != 0 {
		t.Fatalf("since in the future should return nothing: %d", len(got))
	}
}

func TestSnapshotRestore(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 30, 0, time.UTC)
	c := New(nil)
	c.now = func() time.Time { return now }
	c.hour = now.Truncate(time.Hour).Unix()
	c.Log(entry(now.Add(-25*time.Hour), "198.51.100.1", "docker.io/library/old", 200, 1)) // outside the ring
	c.Log(entry(now.Add(-3*time.Hour), "198.51.100.1", "docker.io/library/nginx", 200, 1000))
	c.Log(entry(now, "198.51.100.2", "ghcr.io/a/b", 503, 0))
	e := entry(now, "198.51.100.3", "quay.io/x/y", 404, 0)
	e.UserID = "ci"
	c.Log(e)
	raw := c.Snapshot()

	// After a restart ten minutes later that already served a request in the current minute
	later := now.Add(10 * time.Minute)
	r := New(nil)
	r.now = func() time.Time { return later }
	r.hour = later.Truncate(time.Hour).Unix()
	r.Log(entry(later, "198.51.100.4", "docker.io/library/nginx", 200, 10))
	first, err := r.Restore(raw)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(-3 * time.Hour).Truncate(time.Minute); !first.Equal(want) {
		t.Fatalf("Restore should report the earliest minute restored: %v, want %v", first, want)
	}
	series := r.Series(later.Add(-24 * time.Hour))
	var req, b5, bytes uint64
	for _, m := range series {
		req += m.Requests
		b5 += m.Status5xx
		bytes += m.Bytes
	}
	if req != 4 || b5 != 1 || bytes != 1010 {
		t.Fatalf("restored counters: %d requests, %d 5xx, %d bytes", req, b5, bytes)
	}
	if !r.LastSeen()["ci"].Equal(now) {
		t.Fatalf("last use of an account should be restored: %v", r.LastSeen())
	}
	if _, err := r.Restore([]byte("{")); err == nil {
		t.Fatal("broken JSON should be an error")
	}
}

func TestResumeHour(t *testing.T) {
	s, err := store.OpenBolt(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hour := time.Now().Truncate(time.Hour)
	c := New(s)
	for i := 0; i < 3; i++ {
		c.Log(entry(time.Now(), "198.51.100.1", "docker.io/library/nginx", 200, 100))
	}
	c.Flush() // as on exit
	r := New(s)
	r.Log(entry(time.Now(), "198.51.100.2", "ghcr.io/a/b", 200, 50))
	if !time.Now().Truncate(time.Hour).Equal(hour) {
		t.Skip("the hour changed during the test")
	}
	sum := r.Summarize(time.Hour, 10)
	if sum.Totals.Requests != 4 || len(sum.Images) != 2 || sum.Images[0].Bytes != 300 {
		t.Fatalf("the hour saved before a restart should carry on: %+v", sum)
	}
	r.Flush()
	r2 := New(s)
	if got := r2.Summarize(time.Hour, 10).Totals.Requests; got != 4 {
		t.Fatalf("saving again should keep what was counted before the restart: %d", got)
	}
}
