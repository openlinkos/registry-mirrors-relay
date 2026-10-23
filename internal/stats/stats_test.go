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
		t.Fatalf("每分钟的计数：%+v", series)
	}
	// 跨过整点：上一小时存进数据库
	now = now.Add(time.Hour)
	c.Log(entry(now, "198.51.100.3", "quay.io/x/y", 200, 5000))
	sum := c.Summarize(24*time.Hour, 10)
	if sum.Totals.Requests != 13 || sum.Totals.Bytes != 15000 {
		t.Fatalf("24 小时汇总：%+v", sum.Totals)
	}
	if len(sum.Images) != 2 || sum.Images[0].Key != "docker.io/library/nginx" || sum.Images[0].Bytes != 10000 {
		t.Fatalf("热门镜像：%+v", sum.Images)
	}
	if len(sum.Repeats) == 0 || sum.Repeats[0].Count != 10 || sum.Repeats[0].Key != "198.51.100.1 docker.io/library/nginx@sha256:aa" {
		t.Fatalf("反复完整下载：%+v", sum.Repeats)
	}
	if one := c.Summarize(time.Hour, 10); one.Totals.Requests != 1 {
		t.Fatalf("最近 1 小时只含当前小时：%+v", one.Totals)
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
		t.Fatalf("环形缓冲应只留 %d 条：%d", recentSize, len(all))
	}
	if got := c.Recent(Filter{ClientIP: "198.51.100.1"}, 10); len(got) != 10 || got[0].ClientIP != "198.51.100.1" {
		t.Fatalf("按 IP 过滤：%d", len(got))
	}
	for _, r := range c.Recent(Filter{Status: 4}, 50) {
		if r.Status/100 != 4 {
			t.Fatalf("按 4xx 过滤：%d", r.Status)
		}
	}
	// 最新的在前
	c.Log(&accesslog.Entry{Time: now, ClientIP: "203.0.113.9", Status: 200})
	if c.Recent(Filter{}, 1)[0].ClientIP != "203.0.113.9" {
		t.Fatal("最新的应在前")
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
		t.Fatalf("前若干名：%+v（共 %d 项）", top, len(k.items))
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
