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
	if code != 0 || !strings.Contains(out, "没有问题") {
		t.Fatalf("示例配置应通过：%d %s %s", code, out, errs)
	}
	code, out, _ = runCmd(t, "", "check", "-config", filepath.Join("..", "..", "config.example.yaml"), "-print")
	if code != 0 || !strings.Contains(out, "upstreams:") || !strings.Contains(out, "mcr.microsoft.com") {
		t.Fatalf("-print 应输出合并后的上游：%d\n%s", code, out)
	}
}

// TestCheckDeployConfigs 检查仓库里所有部署用的配置文件（Docker、Compose、kustomize、测试）都能通过 check，
// 防止改了配置的写法却漏改某个部署文件。
func TestCheckDeployConfigs(t *testing.T) {
	var files []string
	err := filepath.WalkDir(filepath.Join("..", "..", "deploy"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "config.yaml" {
			files = append(files, p)
		}
		return err
	})
	if err != nil || len(files) < 6 {
		t.Fatalf("找到的配置文件太少：%v %v", files, err)
	}
	for _, f := range files {
		if code, out, errs := runCmd(t, "", "check", "-config", f); code != 0 {
			t.Errorf("%s 没有通过 check：%s %s", f, out, errs)
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
	// -wait：前两次 503，第三次 200
	var n atomic.Int32
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer flaky.Close()
	if code, _, errs := runCmd(t, "", "probe", "-url", flaky.URL, "-wait", "10s"); code != 0 || n.Load() != 3 {
		t.Fatalf("-wait 应重试到成功：%d，请求了 %d 次，%s", code, n.Load(), errs)
	}
	start := time.Now()
	if code, _, _ := runCmd(t, "", "probe", "-url", bad.URL, "-wait", "1500ms"); code != 1 || time.Since(start) < 1500*time.Millisecond {
		t.Fatalf("一直失败时应等满 -wait 再返回 1：%d，%v", code, time.Since(start))
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

func TestAdminCommand(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("data_dir: "+filepath.ToSlash(dir)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := runCmd(t, "short\n", "admin", "create", "-config", cfg, "-name", "ops"); code != 1 || !strings.Contains(errs, "12") {
		t.Fatalf("口令太短：%d %s", code, errs)
	}
	if code, _, _ := runCmd(t, "long-enough-password\n", "admin", "create", "-config", cfg, "-name", "bad name"); code != 2 {
		t.Fatalf("名字不合法：%d", code)
	}
	if code, out, errs := runCmd(t, "long-enough-password\n", "admin", "create", "-config", cfg, "-name", "ops", "-role", "viewer"); code != 0 || !strings.Contains(out, "ops") {
		t.Fatalf("创建：%d %s %s", code, out, errs)
	}
	if code, _, errs := runCmd(t, "long-enough-password\n", "admin", "create", "-config", cfg, "-name", "ops"); code != 1 || !strings.Contains(errs, "已存在") {
		t.Fatalf("重复创建：%d %s", code, errs)
	}
	if code, _, errs := runCmd(t, "another-password-1\n", "admin", "password", "-config", cfg, "-name", "nobody"); code != 1 || !strings.Contains(errs, "没有管理员") {
		t.Fatalf("重设不存在的管理员：%d %s", code, errs)
	}
	if code, _, _ := runCmd(t, "another-password-1\n", "admin", "password", "-config", cfg, "-name", "ops"); code != 0 {
		t.Fatalf("重设口令：%d", code)
	}
	code, out, _ := runCmd(t, "", "admin", "list", "-config", cfg)
	if code != 0 || !strings.Contains(out, "ops  viewer  正常") {
		t.Fatalf("列表：%d %q", code, out)
	}
	if code, _, _ := runCmd(t, "", "admin", "frobnicate", "-config", cfg); code != 2 {
		t.Fatalf("不认识的子命令：%d", code)
	}
	if code, _, errs := runCmd(t, "", "admin", "list", "-config", cfg); code != 0 {
		t.Fatalf("%s", errs)
	}
	ro := filepath.Join(dir, "ro.yaml")
	_ = os.WriteFile(ro, []byte("admin: {store: {driver: none}}\n"), 0o600)
	if code, _, errs := runCmd(t, "", "admin", "list", "-config", ro); code != 1 || !strings.Contains(errs, "只读") {
		t.Fatalf("只读存储：%d %s", code, errs)
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
			t.Errorf("%+v：%s %v %v", c.l, got, insecure, err)
		}
	}
	if _, _, err := probeURL(config.Listen{}); err == nil {
		t.Error("都没开时应报错")
	}
	// 按配置文件找端口
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	cfg := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(cfg, []byte("listen: {http: \""+strings.TrimPrefix(srv.URL, "http://")+"\"}\n"), 0o600)
	if code, _, errs := runCmd(t, "", "probe", "-config", cfg); code != 0 {
		t.Fatalf("按配置探测：%d %s", code, errs)
	}
}
