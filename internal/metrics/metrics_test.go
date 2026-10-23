package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/accesslog"
)

func TestExposition(t *testing.T) {
	m := New("test")
	m.Log(&accesslog.Entry{Upstream: "docker.io", Kind: "blob", Status: 200, Size: 1000, Duration: 2 * time.Second})
	m.Log(&accesslog.Entry{Upstream: "docker.io", Kind: "blob", Status: 206, Size: 10, Duration: 10 * time.Millisecond})
	m.Log(&accesslog.Entry{Upstream: "ghcr.io", Kind: "manifest", Status: 404, Cache: "negative", Duration: time.Millisecond})
	m.Register("relay_manifest_cache_bytes", "缓存占用", "gauge", func() []Sample { return []Sample{{Value: 4096}} })
	m.Register("relay_upstream_breaker_open", "熔断", "gauge", func() []Sample {
		return []Sample{{Labels: Labels("upstream", `we"ird`), Value: 1}}
	})
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	out := rec.Body.String()
	for _, want := range []string{
		`relay_requests_total{upstream="docker.io",kind="blob",code="2xx",cache=""} 2`,
		`relay_requests_total{upstream="ghcr.io",kind="manifest",code="4xx",cache="negative"} 1`,
		`relay_response_bytes_total{upstream="docker.io",kind="blob"} 1010`,
		`relay_request_duration_seconds_bucket{kind="blob",le="0.025"} 1`,
		`relay_request_duration_seconds_bucket{kind="blob",le="5"} 2`,
		`relay_request_duration_seconds_bucket{kind="blob",le="+Inf"} 2`,
		`relay_request_duration_seconds_count{kind="blob"} 2`,
		`relay_manifest_cache_bytes 4096`,
		`relay_upstream_breaker_open{upstream="we\"ird"} 1`,
		`relay_build_info{version="test",go="`,
		"# TYPE relay_requests_total counter",
		"go_goroutines ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少：%s", want)
		}
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatal("Content-Type 不对")
	}
}
