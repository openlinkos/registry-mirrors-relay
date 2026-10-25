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
		t.Fatalf("/readyz should return 503 before listening starts, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("/healthz should return 200 and not be cached: %d %v", rec.Code, rec.Header())
	}
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("other paths go to the handler: %d", rec.Code)
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
		t.Fatal("Run did not become ready")
	}
	base := "http://" + s.Bound()[0]
	resp, err := http.Get(base + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz: %v %v", resp, err)
	}
	_ = resp.Body.Close()

	// An in-flight request: shutdown has to wait for it to finish
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
		t.Fatalf("/readyz should return 503 once shutdown starts, got %d", rec.Code)
	}
	close(release)
	if body := <-got; body != "done" {
		t.Fatalf("the in-flight request should finish: %q", body)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run should return normally: %v", err)
	}
}

// startTLS starts a Server with the given configuration and returns the HTTPS entrance's address and a stop
// function.
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
		t.Fatal("did not start listening")
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
		t.Fatalf("the HTTPS entrance should support HTTP/2: %s", body)
	}
	first := resp.TLS.PeerCertificates[0].SerialNumber
	// Certificate renewal: replace the files, new connections get the new certificate
	time.Sleep(10 * time.Millisecond)
	ca.LeafFiles(t, dir, "127.0.0.1", "mirror.test")
	client.CloseIdleConnections()
	resp, _ = get()
	if resp.TLS.PeerCertificates[0].SerialNumber.Cmp(first) == 0 {
		t.Fatal("changed certificate files should be reloaded")
	}
	// Broken file: keep using the previous certificate
	if err := os.WriteFile(certFile, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	client.CloseIdleConnections()
	if resp, _ := get(); resp.StatusCode != 200 {
		t.Fatal("a broken certificate file should keep the old certificate in use")
	}
}

func TestTLSFilesMissing(t *testing.T) {
	res, err := config.Parse([]byte("listen: {https: \"127.0.0.1:0\"}\ntls: {mode: files, cert_file: /nonexistent/c.pem, key_file: /nonexistent/k.pem}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := New(res.Config, slog.New(slog.DiscardHandler), http.NotFoundHandler()).Run(context.Background()); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("startup should fail when the certificate files do not exist: %v", err)
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
		t.Fatalf("TLS-ALPN-01 must be supported: %v", conf.NextProtos)
	}
	if fi, err := os.Stat(filepath.Join(dir, "acme")); err != nil || !fi.IsDir() {
		t.Fatal("the certificate cache directory must be created")
	}
	// Plain-text entrance: the HTTP-01 validation path is handled by autocert, other requests go to the
	// handler as usual
	h := wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "relay") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://mirror.example.com/v2/", nil))
	if rec.Body.String() != "relay" {
		t.Fatalf("ordinary requests should be handled as usual: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://mirror.example.com/.well-known/acme-challenge/unknown-token", nil))
	if rec.Body.String() == "relay" {
		t.Fatal("the validation path should be handled by autocert")
	}
	// Domains not on the list get no certificate
	if _, err := conf.GetCertificate(&tls.ClientHelloInfo{ServerName: "new.example.com"}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("domains not in tls.acme.hosts must not get a certificate: %v", err)
	}
	// A domain added to the effective configuration (for example an entrance added in the console) is allowed
	// without a restart
	hosts.Store(&[]string{"mirror.example.com", "new.example.com"})
	if _, err := conf.GetCertificate(&tls.ClientHelloInfo{ServerName: "new.example.com"}); err == nil || strings.Contains(err.Error(), "not configured") {
		t.Fatalf("the new domain should be allowed (and then fail because the ACME service is unreachable): %v", err)
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
