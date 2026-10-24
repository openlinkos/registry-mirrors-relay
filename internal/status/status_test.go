package status

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestHistory(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC)
	h := NewHistory()
	h.Record("docker.io", true, 100*time.Millisecond, now)
	h.Record("docker.io", false, 0, now.Add(-time.Minute))
	h.Record("docker.io", true, 50*time.Millisecond, now.Add(-3*time.Hour))
	h.Record("docker.io", true, 50*time.Millisecond, now.Add(-25*time.Hour)) // 超出 24 小时，被同一格的新数据挤掉或不显示
	hs := h.Hours("docker.io", now)
	if len(hs) != Hours || hs[Hours-1].Start != now.Truncate(time.Hour).Unix() || hs[0].Start != now.Truncate(time.Hour).Add(-23*time.Hour).Unix() {
		t.Fatalf("应返回 24 个整点，最后一个是当前小时：%+v", hs[Hours-1])
	}
	if c := hs[Hours-1]; c.OK != 1 || c.Total != 2 {
		t.Fatalf("当前小时应是 1/2：%+v", c)
	}
	if c := hs[Hours-4]; c.OK != 1 || c.Total != 1 {
		t.Fatalf("三小时前应是 1/1：%+v", c)
	}
	var total int
	for _, x := range hs {
		total += x.Total
	}
	if total != 3 {
		t.Fatalf("24 小时以前的数据不应出现：共 %d 次", total)
	}
	if hs := h.Hours("ghcr.io", now); len(hs) != Hours || hs[0].Total != 0 {
		t.Fatal("没有记录的上游应返回 24 个空格")
	}
}

func TestBuild(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC)
	in := Input{
		Version: "0.1.3", Started: now.Add(-2 * time.Hour), Now: now, Interval: time.Minute,
		Names: []string{"quay.io", "docker.io", "ghcr.io", "gcr.io"},
		Probes: map[string]Probe{
			"docker.io": {OK: true, Status: 401, Latency: 123 * time.Millisecond, Checked: now},
			"ghcr.io":   {OK: false, Error: `Get "https://ghcr.io/v2/": dial tcp 140.82.112.33:443: i/o timeout`, Checked: now},
			"quay.io":   {OK: false, Status: 503, Error: "上游返回 503 Service Unavailable", Checked: now},
		},
		Open: map[string]bool{"quay.io": true},
		Requests: []RequestsHour{
			{Start: 1, Requests: 1000, Status5xx: 2},
			{Start: 2, Requests: 5, Status5xx: 5},
		},
		History: NewHistory(),
	}
	r := Build(in)
	if r.Status != "degraded" || r.Version != "0.1.3" || r.Interval != 60 {
		t.Fatalf("部分上游异常应是 degraded：%+v", r)
	}
	got := map[string]Upstream{}
	for _, u := range r.Upstreams {
		got[u.Name] = u
	}
	if r.Upstreams[0].Name != "docker.io" || len(r.Upstreams) != 4 {
		t.Fatal("上游按名字排序")
	}
	if u := got["docker.io"]; u.Status != "ok" || u.LatencyMS != 123 || u.Checked == nil {
		t.Fatalf("docker.io：%+v", u)
	}
	if u := got["ghcr.io"]; u.Status != "down" || u.Reason != "timeout" {
		t.Fatalf("ghcr.io 应是 down/timeout：%+v", u)
	}
	if u := got["quay.io"]; u.Status != "open" || u.Reason != "http_503" {
		t.Fatalf("熔断中的上游应是 open：%+v", u)
	}
	if u := got["gcr.io"]; u.Status != "unknown" || u.Checked != nil {
		t.Fatalf("还没探测过的上游应是 unknown：%+v", u)
	}
	if v := r.Service.Hours[0].Value; v == nil || *v != 0.998 {
		t.Fatalf("成功率应是 0.998：%v", v)
	}
	if r.Service.Hours[1].Value != nil {
		t.Fatal("请求太少的小时不算成功率")
	}
	b, _ := json.Marshal(r)
	for _, leak := range []string{"140.82", "dial tcp", "Service Unavailable", "requests", "1000"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("status.json 不应包含 %q：%s", leak, b)
		}
	}

	in.Probes = map[string]Probe{"docker.io": {OK: false, Error: "x"}}
	in.Open, in.Names = nil, []string{"docker.io"}
	if r := Build(in); r.Status != "down" {
		t.Fatalf("全部上游异常应是 down：%s", r.Status)
	}
	in.Probes = nil
	if r := Build(in); r.Status != "unknown" {
		t.Fatalf("没有探测结果应是 unknown：%s", r.Status)
	}
	in.Probes = map[string]Probe{"docker.io": {OK: true}}
	if r := Build(in); r.Status != "ok" {
		t.Fatalf("全部正常应是 ok：%s", r.Status)
	}
}

func TestReason(t *testing.T) {
	cases := map[string]string{
		`Get "https://x/v2/": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`: "timeout",
		`dial tcp: lookup gcr.io on 10.43.0.10:53: no such host`:                                          "dns",
		`tls: failed to verify certificate: x509: certificate signed by unknown authority`:                "tls",
		`dial tcp 1.2.3.4:443: connect: connection refused`:                                               "connect",
		`read tcp 10.0.0.1:1234->1.2.3.4:443: read: connection reset by peer`:                             "connect",
		`something else`: "error",
	}
	for msg, want := range cases {
		if got := Reason(0, msg); got != want {
			t.Errorf("%q：%s，应为 %s", msg, got, want)
		}
	}
	if got := Reason(502, "上游返回 502"); got != "http_502" {
		t.Errorf("有状态码时应是 http_<状态码>：%s", got)
	}
}
