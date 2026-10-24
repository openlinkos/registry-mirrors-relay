package web

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
)

func handler(t *testing.T, y string) *Handler {
	t.Helper()
	res, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(res.Config, func() any { return map[string]string{"status": "ok"} })
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(h *Handler, method, path string) (*httptest.ResponseRecorder, bool) {
	return getAs(h, method, path, false)
}

func getAs(h *Handler, method, path string, login bool) (*httptest.ResponseRecorder, bool) {
	rec := httptest.NewRecorder()
	ok := h.Serve(rec, httptest.NewRequest(method, path, nil), login)
	return rec, ok
}

func TestPage(t *testing.T) {
	h := handler(t, "")
	rec, ok := get(h, http.MethodGet, "/")
	if !ok || rec.Code != 200 || !strings.Contains(rec.Body.String(), "data-variant=\"direct\"") {
		t.Fatalf("/ 应返回用法页：%v %d", ok, rec.Code)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	script := between(rec.Body.Bytes(), "<script>", "</script>")
	sum := sha256.Sum256(script)
	if len(script) < 100 || !strings.Contains(csp, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Fatalf("CSP 应按哈希放行内联脚本：%s", csp)
	}
	if rec, _ := get(h, http.MethodHead, "/robots.txt"); rec.Body.Len() != 0 || rec.Header().Get("Content-Length") == "0" {
		t.Fatal("HEAD 不返回正文，但要有长度")
	}
	if _, ok := get(h, http.MethodGet, "/etc/passwd"); ok {
		t.Fatal("其他路径不应处理")
	}
}

func TestSiteJSON(t *testing.T) {
	h := handler(t, `
site:
  operator: OpenLinkOS
  links: {home: "https://linkos.org"}
entrances:
  - hosts: [mirror.example.com]
  - hosts: [ghcr.example.com, ghcr2.example.com]
    upstream: ghcr.io
  - hosts: [docker.example.com]
    upstream: docker.io
    auth: basic
access:
  users_file: /dev/null
`)
	type site struct {
		Operator  string
		Domains   map[string]string
		Links     map[string]string
		Login     bool
		LoginKeys []string `json:"login_keys"`
		Status    bool
	}
	read := func(login bool) site {
		rec, _ := getAs(h, http.MethodGet, "/site.json", login)
		var s site
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := read(false)
	if s.Operator != "OpenLinkOS" || s.Domains["ghcr"] != "ghcr.example.com" || s.Domains["docker"] != "docker.example.com" || len(s.Domains) != 2 || s.Links["home"] != "https://linkos.org" {
		t.Fatalf("site.json 不对：%+v", s)
	}
	if s.Login || len(s.LoginKeys) != 1 || s.LoginKeys[0] != "docker" || !s.Status {
		t.Fatalf("site.json 的登录和状态标记不对：%+v", s)
	}
	if !read(true).Login {
		t.Fatal("要登录的入口看到的 site.json 应带 login: true")
	}
}

func TestStatusJSON(t *testing.T) {
	h := handler(t, "")
	rec, ok := get(h, http.MethodGet, "/status.json")
	if !ok || rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("/status.json：%v %d %q", ok, rec.Code, rec.Body.String())
	}
	off := handler(t, "site: {show_status: false}")
	if _, ok := get(off, http.MethodGet, "/status.json"); ok {
		t.Fatal("show_status: false 时不应提供 status.json")
	}
	rec, _ = get(off, http.MethodGet, "/site.json")
	if !strings.Contains(rec.Body.String(), `"status": false`) {
		t.Fatalf("show_status: false 时 site.json 应带 status: false：%s", rec.Body.String())
	}
}
