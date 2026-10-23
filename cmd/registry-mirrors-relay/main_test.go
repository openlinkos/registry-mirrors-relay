package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func runCmd(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestCheckExampleConfig(t *testing.T) {
	t.Setenv("RELAY_ADMIN_TOKEN", "0123456789abcdef0123456789abcdef")
	code, out, errs := runCmd(t, "", "check", "-config", filepath.Join("..", "..", "config.example.yaml"))
	if code != 0 || !strings.Contains(out, "没有问题") {
		t.Fatalf("示例配置应通过：%d %s %s", code, out, errs)
	}
	code, out, _ = runCmd(t, "", "check", "-config", filepath.Join("..", "..", "config.example.yaml"), "-print")
	if code != 0 || !strings.Contains(out, "upstreams:") || !strings.Contains(out, "mcr.microsoft.com") {
		t.Fatalf("-print 应输出合并后的上游：%d\n%s", code, out)
	}
}

func TestCheckBadConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(p, []byte("entrances:\n  - hosts: [a.example.com]\n    upstream: nope.example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errs := runCmd(t, "", "check", "-config", p)
	if code != 1 || !strings.Contains(errs, "nope.example.com") {
		t.Fatalf("错误配置应返回 1 并指出问题：%d %s", code, errs)
	}
	code, _, _ = runCmd(t, "", "check", "-config", filepath.Join(t.TempDir(), "missing.yaml"))
	if code != 1 {
		t.Fatal("明确指定的文件不存在时应报错")
	}
}

func TestHashPassword(t *testing.T) {
	code, out, errs := runCmd(t, "correct horse battery\n", "hash-password", "-user", "alice", "-cost", "10")
	if code != 0 {
		t.Fatalf("hash-password：%d %s", code, errs)
	}
	user, hash, ok := strings.Cut(strings.TrimSpace(out), ":")
	if !ok || user != "alice" || bcrypt.CompareHashAndPassword([]byte(hash), []byte("correct horse battery")) != nil {
		t.Fatalf("输出应是 alice:<bcrypt>：%q", out)
	}
	if code, _, _ := runCmd(t, "short\n", "hash-password"); code != 1 {
		t.Fatal("太短的口令应拒绝")
	}
	if code, _, _ := runCmd(t, "correct horse battery\n", "hash-password", "-user", "a:b"); code != 2 {
		t.Fatal("用户名含冒号应拒绝")
	}
}

func TestProbe(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	if code, _, _ := runCmd(t, "", "probe", "-url", ok.URL); code != 0 {
		t.Fatal("200 时 probe 应返回 0")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer bad.Close()
	if code, _, _ := runCmd(t, "", "probe", "-url", bad.URL); code != 1 {
		t.Fatal("非 200 时 probe 应返回 1")
	}
}

func TestVersionAndUnknown(t *testing.T) {
	if code, out, _ := runCmd(t, "", "version"); code != 0 || !strings.HasPrefix(out, "registry-mirrors-relay ") {
		t.Fatalf("version：%d %q", code, out)
	}
	if code, _, _ := runCmd(t, "", "frobnicate"); code != 2 {
		t.Fatal("不认识的子命令应返回 2")
	}
}
