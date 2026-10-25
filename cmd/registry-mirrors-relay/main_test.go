package main

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
)

func runCmd(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestCheckExampleConfig(t *testing.T) {
	t.Setenv("RELAY_ADMIN_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("RELAY_ADMIN_PASSWORD", "example-admin-password")
	code, out, errs := runCmd(t, "", "check", "-config", filepath.Join("..", "..", "config.example.yaml"))
	if code != 0 || !strings.Contains(out, "configuration is valid") {
		t.Fatalf("the example configuration should pass: %d %s %s", code, out, errs)
	}
	code, out, _ = runCmd(t, "", "check", "-config", filepath.Join("..", "..", "config.example.yaml"), "-print")
	if code != 0 || !strings.Contains(out, "upstreams:") || !strings.Contains(out, "mcr.microsoft.com") {
		t.Fatalf("-print should print the merged upstreams: %d\n%s", code, out)
	}
}

// TestCheckDeployConfigs checks that every deployment configuration file in the repository (Docker,
// Compose, kustomize, tests) passes check, so that a change to the configuration format cannot miss one
// of the deployment files.
func TestCheckDeployConfigs(t *testing.T) {
	var files []string
	err := filepath.WalkDir(filepath.Join("..", "..", "deploy"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "config.yaml" {
			files = append(files, p)
		}
		return err
	})
	if err != nil || len(files) < 6 {
		t.Fatalf("too few configuration files found: %v %v", files, err)
	}
	for _, f := range files {
		if code, out, errs := runCmd(t, "", "check", "-config", f); code != 0 {
			t.Errorf("%s did not pass check: %s %s", f, out, errs)
		}
	}
}

func TestCheckBadConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(p, []byte("entrances:\n  - hosts: [a.example.com]\n    upstream: nope.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs := runCmd(t, "", "check", "-config", p)
	if code != 1 || !strings.Contains(errs, "nope.example.com") {
		t.Fatalf("a bad configuration should return 1 and point out the problem: %d %s", code, errs)
	}
	code, _, _ = runCmd(t, "", "check", "-config", filepath.Join(t.TempDir(), "missing.yaml"))
	if code != 1 {
		t.Fatal("an explicitly given file that does not exist should be an error")
	}
}

func TestHashPassword(t *testing.T) {
	code, out, errs := runCmd(t, "correct horse battery\n", "hash-password", "-user", "alice", "-cost", "10")
	if code != 0 {
		t.Fatalf("hash-password: %d %s", code, errs)
	}
	user, hash, ok := strings.Cut(strings.TrimSpace(out), ":")
	if !ok || user != "alice" || bcrypt.CompareHashAndPassword([]byte(hash), []byte("correct horse battery")) != nil {
		t.Fatalf("output should be alice:<bcrypt>: %q", out)
	}
	if code, _, _ := runCmd(t, "short\n", "hash-password"); code != 1 {
		t.Fatal("a password that is too short should be rejected")
	}
	if code, _, _ := runCmd(t, "correct horse battery\n", "hash-password", "-user", "a:b"); code != 2 {
		t.Fatal("a username with a colon should be rejected")
	}
}

func TestProbe(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	if code, _, _ := runCmd(t, "", "probe", "-url", ok.URL); code != 0 {
		t.Fatal("probe should return 0 on 200")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer bad.Close()
	if code, _, _ := runCmd(t, "", "probe", "-url", bad.URL); code != 1 {
		t.Fatal("probe should return 1 on a status other than 200")
	}
	// -wait: 503 twice, then 200
	var n atomic.Int32
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer flaky.Close()
	if code, _, errs := runCmd(t, "", "probe", "-url", flaky.URL, "-wait", "10s"); code != 0 || n.Load() != 3 {
		t.Fatalf("-wait should retry until it succeeds: %d, %d requests, %s", code, n.Load(), errs)
	}
	start := time.Now()
	if code, _, _ := runCmd(t, "", "probe", "-url", bad.URL, "-wait", "1500ms"); code != 1 || time.Since(start) < 1500*time.Millisecond {
		t.Fatalf("on constant failure it should wait the full -wait, then return 1: %d, %v", code, time.Since(start))
	}
}

func TestVersionAndUnknown(t *testing.T) {
	if code, out, _ := runCmd(t, "", "version"); code != 0 || !strings.HasPrefix(out, "registry-mirrors-relay ") {
		t.Fatalf("version: %d %q", code, out)
	}
	if code, _, _ := runCmd(t, "", "frobnicate"); code != 2 {
		t.Fatal("an unknown subcommand should return 2")
	}
}

func TestAdminCommand(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("data_dir: "+filepath.ToSlash(dir)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runCmd(t, "short\n", "admin", "create", "-config", cfg, "-name", "ops"); code != 1 || !strings.Contains(errs, "12") {
		t.Fatalf("password too short: %d %s", code, errs)
	}
	if code, _, _ := runCmd(t, "long-enough-password\n", "admin", "create", "-config", cfg, "-name", "bad name"); code != 2 {
		t.Fatalf("invalid name: %d", code)
	}
	if code, out, errs := runCmd(t, "long-enough-password\n", "admin", "create", "-config", cfg, "-name", "ops", "-role", "viewer"); code != 0 || !strings.Contains(out, "ops") {
		t.Fatalf("create: %d %s %s", code, out, errs)
	}
	if code, _, errs := runCmd(t, "long-enough-password\n", "admin", "create", "-config", cfg, "-name", "ops"); code != 1 || !strings.Contains(errs, "already exists") {
		t.Fatalf("duplicate create: %d %s", code, errs)
	}
	if code, _, errs := runCmd(t, "another-password-1\n", "admin", "password", "-config", cfg, "-name", "nobody"); code != 1 || !strings.Contains(errs, "no administrator") {
		t.Fatalf("resetting a nonexistent administrator: %d %s", code, errs)
	}
	if code, _, _ := runCmd(t, "another-password-1\n", "admin", "password", "-config", cfg, "-name", "ops"); code != 0 {
		t.Fatalf("password reset: %d", code)
	}
	code, out, _ := runCmd(t, "", "admin", "list", "-config", cfg)
	if code != 0 || !strings.Contains(out, "ops  viewer  active") {
		t.Fatalf("list: %d %q", code, out)
	}
	if code, _, _ := runCmd(t, "", "admin", "frobnicate", "-config", cfg); code != 2 {
		t.Fatalf("unknown subcommand: %d", code)
	}
	if code, _, errs := runCmd(t, "", "admin", "list", "-config", cfg); code != 0 {
		t.Fatalf("%s", errs)
	}
	ro := filepath.Join(dir, "ro.yaml")
	_ = os.WriteFile(ro, []byte("admin: {store: {driver: none}}\n"), 0o600)
	if code, _, errs := runCmd(t, "", "admin", "list", "-config", ro); code != 1 || !strings.Contains(errs, "read-only") {
		t.Fatalf("read-only store: %d %s", code, errs)
	}
}

func TestProbeURL(t *testing.T) {
	for _, c := range []struct {
		l        config.Listen
		want     string
		insecure bool
	}{
		{config.Listen{HTTP: ":8080"}, "http://127.0.0.1:8080/healthz", false},
		{config.Listen{HTTP: "0.0.0.0:80", HTTPS: ":443"}, "http://127.0.0.1:80/healthz", false},
		{config.Listen{HTTP: "[::]:8080"}, "http://127.0.0.1:8080/healthz", false},
		{config.Listen{HTTP: "10.0.0.5:8080"}, "http://10.0.0.5:8080/healthz", false},
		{config.Listen{HTTPS: ":443"}, "https://127.0.0.1:443/healthz", true},
	} {
		got, insecure, err := probeURL(c.l)
		if err != nil || got != c.want || insecure != c.insecure {
			t.Errorf("%+v: %s %v %v", c.l, got, insecure, err)
		}
	}
	if _, _, err := probeURL(config.Listen{}); err == nil {
		t.Error("should fail when neither is enabled")
	}
	// Find the port from the configuration file
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	cfg := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(cfg, []byte("listen: {http: \""+strings.TrimPrefix(srv.URL, "http://")+"\"}\n"), 0o600)
	if code, _, errs := runCmd(t, "", "probe", "-config", cfg); code != 0 {
		t.Fatalf("probe from the configuration: %d %s", code, errs)
	}
}
