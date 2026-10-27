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
	// Over 24 hours old: pushed out by newer data in the same slot, or not shown
	h.Record("docker.io", true, 50*time.Millisecond, now.Add(-25*time.Hour))
	hs := h.Hours("docker.io", now)
	if len(hs) != Hours || hs[Hours-1].Start != now.Truncate(time.Hour).Unix() || hs[0].Start != now.Truncate(time.Hour).Add(-23*time.Hour).Unix() {
		t.Fatalf("want 24 whole hours, the last being the current hour: %+v", hs[Hours-1])
	}
	if c := hs[Hours-1]; c.OK != 1 || c.Total != 2 {
		t.Fatalf("current hour should be 1/2: %+v", c)
	}
	if c := hs[Hours-4]; c.OK != 1 || c.Total != 1 {
		t.Fatalf("three hours ago should be 1/1: %+v", c)
	}
	var total int
	for _, x := range hs {
		total += x.Total
	}
	if total != 3 {
		t.Fatalf("data older than 24 hours should not appear: %d probes in total", total)
	}
	if hs := h.Hours("ghcr.io", now); len(hs) != Hours || hs[0].Total != 0 {
		t.Fatal("an upstream with no records should return 24 empty slots")
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
			"quay.io":   {OK: false, Status: 503, Error: "upstream returned 503 Service Unavailable", Checked: now},
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
		t.Fatalf("some upstreams failing should be degraded: %+v", r)
	}
	got := map[string]Upstream{}
	for _, u := range r.Upstreams {
		got[u.Name] = u
	}
	if r.Upstreams[0].Name != "docker.io" || len(r.Upstreams) != 4 {
		t.Fatal("upstreams should be sorted by name")
	}
	if u := got["docker.io"]; u.Status != "ok" || u.LatencyMS != 123 || u.Checked == nil {
		t.Fatalf("docker.io: %+v", u)
	}
	if u := got["ghcr.io"]; u.Status != "down" || u.Reason != "timeout" {
		t.Fatalf("ghcr.io should be down/timeout: %+v", u)
	}
	if u := got["quay.io"]; u.Status != "open" || u.Reason != "http_503" {
		t.Fatalf("an upstream with an open circuit breaker should be open: %+v", u)
	}
	if u := got["gcr.io"]; u.Status != "unknown" || u.Checked != nil {
		t.Fatalf("an upstream not yet probed should be unknown: %+v", u)
	}
	if v := r.Service.Hours[0].Value; v == nil || *v != 0.998 {
		t.Fatalf("success rate should be 0.998: %v", v)
	}
	if r.Service.Hours[1].Value != nil {
		t.Fatal("hours with too few requests should have no success rate")
	}
	b, _ := json.Marshal(r)
	for _, leak := range []string{"140.82", "dial tcp", "Service Unavailable", "requests", "1000"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("status.json should not contain %q: %s", leak, b)
		}
	}

	in.Probes = map[string]Probe{"docker.io": {OK: false, Error: "x"}}
	in.Open, in.Names = nil, []string{"docker.io"}
	if r := Build(in); r.Status != "down" {
		t.Fatalf("all upstreams failing should be down: %s", r.Status)
	}
	in.Probes = nil
	if r := Build(in); r.Status != "unknown" {
		t.Fatalf("no probe results should be unknown: %s", r.Status)
	}
	in.Probes = map[string]Probe{"docker.io": {OK: true}}
	if r := Build(in); r.Status != "ok" {
		t.Fatalf("all healthy should be ok: %s", r.Status)
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
			t.Errorf("%q: got %s, want %s", msg, got, want)
		}
	}
	if got := Reason(502, "upstream returned 502"); got != "http_502" {
		t.Errorf("with a status code the reason should be http_<status code>: %s", got)
	}
}

func TestHistorySnapshotRestore(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC)
	h := NewHistory()
	h.Record("docker.io", true, 100*time.Millisecond, now)
	h.Record("docker.io", false, 0, now.Add(-2*time.Hour))
	h.Record("quay.io", true, 30*time.Millisecond, now.Add(-23*time.Hour))
	raw := h.Snapshot(now)

	// An hour later, after a restart that already probed once in the current hour
	later := now.Add(time.Hour)
	r := NewHistory()
	r.Record("docker.io", true, 80*time.Millisecond, later)
	if err := r.Restore(raw, later); err != nil {
		t.Fatal(err)
	}
	hs := r.Hours("docker.io", later)
	if c := hs[Hours-2]; c.OK != 1 || c.Total != 1 {
		t.Fatalf("the hour before the restart should be restored: %+v", c)
	}
	if c := hs[Hours-4]; c.OK != 0 || c.Total != 1 {
		t.Fatalf("a failed probe should be restored as failed: %+v", c)
	}
	if c := hs[Hours-1]; c.OK != 1 || c.Total != 1 {
		t.Fatalf("the probe since the restart should stay: %+v", c)
	}
	// quay.io's probe was 23 hours before the snapshot, so 24 hours before later: outside the window
	for _, x := range r.Hours("quay.io", later) {
		if x.Total != 0 {
			t.Fatalf("data older than 24 hours should not be restored: %+v", x)
		}
	}
	// Restoring the same hour after a restart within it adds up
	again := NewHistory()
	again.Record("docker.io", true, 0, now)
	if err := again.Restore(raw, now); err != nil {
		t.Fatal(err)
	}
	if c := again.Hours("docker.io", now)[Hours-1]; c.OK != 2 || c.Total != 2 {
		t.Fatalf("a restart within the hour should add up: %+v", c)
	}
	if err := NewHistory().Restore([]byte("{"), now); err == nil {
		t.Fatal("broken JSON should be an error")
	}
	if err := NewHistory().Restore([]byte(`{"x":[{"start":1,"ok":5,"total":1}]}`), now); err != nil {
		t.Fatal(err)
	}
}

func TestBuildSince(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 30, 0, 0, time.UTC)
	started := now.Add(-10 * time.Minute)
	h := NewHistory()
	in := Input{Started: started, Now: now, Names: []string{"docker.io"}, History: h}
	if r := Build(in); !r.Since.Equal(started) {
		t.Fatalf("without data, since should be the start: %v", r.Since)
	}
	h.Record("docker.io", true, 0, now.Add(-5*time.Hour))
	if r := Build(in); !r.Since.Equal(now.Truncate(time.Hour).Add(-5 * time.Hour)) {
		t.Fatalf("since should be the earliest hour with probes: %v", r.Since)
	}
	in.Requests = []RequestsHour{{Start: now.Truncate(time.Hour).Add(-8 * time.Hour).Unix(), Requests: 3}}
	if r := Build(in); !r.Since.Equal(now.Truncate(time.Hour).Add(-8 * time.Hour)) {
		t.Fatalf("since should count the hours with requests: %v", r.Since)
	}
}
