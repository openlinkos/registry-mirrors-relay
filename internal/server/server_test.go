package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	res, err := config.Parse([]byte("listen:\n  http: \"127.0.0.1:0\"\ntimeouts:\n  shutdown_grace: 2s\n"))
	if err != nil {
		t.Fatal(err)
	}
	return res.Config
}

func TestProbes(t *testing.T) {
	s := New(testConfig(t), slog.New(slog.DiscardHandler), http.NotFoundHandler())
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("还没开始监听时 /readyz 应返回 503，实际 %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("/healthz 应返回 200 且不缓存：%d %v", rec.Code, rec.Header())
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("其他路径交给 handler：%d", rec.Code)
	}
}

func TestRunAndGracefulShutdown(t *testing.T) {
	release := make(chan struct{})
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = io.WriteString(w, "done")
	})
	s := New(testConfig(t), slog.New(slog.DiscardHandler), slow)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	if !s.waitReady(2 * time.Second) {
		t.Fatal("Run 没有就绪")
	}
	base := "http://" + s.Bound()[0]
	resp, err := http.Get(base + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz：%v %v", resp, err)
	}
	_ = resp.Body.Close()

	// 一个进行中的请求：退出时要等它完成
	got := make(chan string, 1)
	go func() {
		resp, err := http.Get(base + "/v2/slow")
		if err != nil {
			got <- "err: " + err.Error()
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		got <- string(b)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("开始退出后 /readyz 应返回 503，实际 %d", rec.Code)
	}
	close(release)
	if body := <-got; body != "done" {
		t.Fatalf("进行中的请求应完成：%q", body)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run 应正常返回：%v", err)
	}
}
