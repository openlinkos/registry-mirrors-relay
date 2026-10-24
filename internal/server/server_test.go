package server

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/testutil"
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

// startTLS 用给定的配置启动 Server，返回 HTTPS 入口的地址和停止函数。
func startTLS(t *testing.T, yaml string) (*Server, func()) {
	t.Helper()
	res, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	s := New(res.Config, slog.New(slog.DiscardHandler), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "proto="+r.Proto)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()
	if !s.waitReady(5 * time.Second) {
		t.Fatal("没有开始监听")
	}
	return s, func() { cancel(); <-done }
}

func TestTLSFilesAndReload(t *testing.T) {
	ca := testutil.NewCA(t)
	dir := t.TempDir()
	certFile, keyFile := ca.LeafFiles(t, dir, "127.0.0.1", "mirror.test")
	old := reloadEvery
	reloadEvery = 0
	t.Cleanup(func() { reloadEvery = old })
	s, stop := startTLS(t, "listen: {http: \"\", https: \"127.0.0.1:0\"}\ntls: {mode: files, cert_file: \""+filepath.ToSlash(certFile)+"\", key_file: \""+filepath.ToSlash(keyFile)+"\"}\n")
	defer stop()
	addr := s.Bound()[0]
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool}, ForceAttemptHTTP2: true}}
	get := func() (*http.Response, string) {
		t.Helper()
		resp, err := client.Get("https://" + addr + "/v2/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, body := get()
	if body != "proto=HTTP/2.0" {
		t.Fatalf("HTTPS 入口应支持 HTTP/2：%s", body)
	}
	first := resp.TLS.PeerCertificates[0].SerialNumber
	// 证书续期：换掉文件，新的连接拿到新证书
	time.Sleep(10 * time.Millisecond)
	ca.LeafFiles(t, dir, "127.0.0.1", "mirror.test")
	client.CloseIdleConnections()
	resp, _ = get()
	if resp.TLS.PeerCertificates[0].SerialNumber.Cmp(first) == 0 {
		t.Fatal("证书文件变了应重新加载")
	}
	// 坏文件：继续用原来的证书
	if err := os.WriteFile(certFile, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	client.CloseIdleConnections()
	if resp, _ := get(); resp.StatusCode != 200 {
		t.Fatal("证书文件坏了应继续用旧证书")
	}
}

func TestTLSFilesMissing(t *testing.T) {
	res, err := config.Parse([]byte("listen: {https: \"127.0.0.1:0\"}\ntls: {mode: files, cert_file: /nonexistent/c.pem, key_file: /nonexistent/k.pem}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := New(res.Config, slog.New(slog.DiscardHandler), http.NotFoundHandler()).Run(context.Background()); err == nil || !strings.Contains(err.Error(), "证书") {
		t.Fatalf("证书文件不存在时应启动失败：%v", err)
	}
}

func TestACMEWiring(t *testing.T) {
	dir := t.TempDir()
	var hosts atomic.Pointer[[]string]
	hosts.Store(&[]string{"mirror.example.com"})
	conf, wrap, err := tlsSetup(mustConfig(t, "data_dir: \""+filepath.ToSlash(dir)+"\"\nlisten: {http: \"127.0.0.1:0\", https: \"127.0.0.1:0\"}\ntls: {mode: acme, acme: {hosts: [mirror.example.com], directory_url: \"https://acme.invalid/directory\"}}\n"),
		slog.New(slog.DiscardHandler), func() []string { return *hosts.Load() })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(conf.NextProtos, "acme-tls/1") || conf.MinVersion != tls.VersionTLS12 {
		t.Fatalf("要支持 TLS-ALPN-01：%v", conf.NextProtos)
	}
	if fi, err := os.Stat(filepath.Join(dir, "acme")); err != nil || !fi.IsDir() {
		t.Fatal("要建好证书缓存目录")
	}
	// 明文入口：HTTP-01 的验证路径由 autocert 处理，其他请求照常交给 handler
	h := wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "relay") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://mirror.example.com/v2/", nil))
	if rec.Body.String() != "relay" {
		t.Fatalf("普通请求照常处理：%d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://mirror.example.com/.well-known/acme-challenge/unknown-token", nil))
	if rec.Body.String() == "relay" {
		t.Fatal("验证路径应由 autocert 处理")
	}
	// 不在名单里的域名不申请证书
	if _, err := conf.GetCertificate(&tls.ClientHelloInfo{ServerName: "new.example.com"}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("不在 tls.acme.hosts 里的域名不能申请证书：%v", err)
	}
	// 生效的配置里加了域名（例如在后台加了入口），不用重启就放行
	hosts.Store(&[]string{"mirror.example.com", "new.example.com"})
	if _, err := conf.GetCertificate(&tls.ClientHelloInfo{ServerName: "new.example.com"}); err == nil || strings.Contains(err.Error(), "not configured") {
		t.Fatalf("新加的域名应放行（之后连不上 ACME 服务而失败）：%v", err)
	}
}

func mustConfig(t *testing.T, y string) *config.Config {
	t.Helper()
	res, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return res.Config
}
