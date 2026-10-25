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
		t.Fatalf("/ should return the usage page: %v %d", ok, rec.Code)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	script := between(rec.Body.Bytes(), "<script>", "</script>")
	sum := sha256.Sum256(script)
	if len(script) < 100 || !strings.Contains(csp, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Fatalf("CSP should allow the inline script by its hash: %s", csp)
	}
	if rec, _ := get(h, http.MethodHead, "/robots.txt"); rec.Body.Len() != 0 || rec.Header().Get("Content-Length") == "0" {
		t.Fatal("HEAD should return no body but a length")
	}
	if _, ok := get(h, http.MethodGet, "/etc/passwd"); ok {
		t.Fatal("other paths should not be handled")
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
		t.Fatalf("wrong site.json: %+v", s)
	}
	if s.Login || len(s.LoginKeys) != 1 || s.LoginKeys[0] != "docker" || !s.Status {
		t.Fatalf("wrong login and status flags in site.json: %+v", s)
	}
	if !read(true).Login {
		t.Fatal("site.json seen by an entrance that requires signing in should have login: true")
	}
}

func TestStatusJSON(t *testing.T) {
	h := handler(t, "")
	rec, ok := get(h, http.MethodGet, "/status.json")
	if !ok || rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("/status.json: %v %d %q", ok, rec.Code, rec.Body.String())
	}
	off := handler(t, "site: {show_status: false}")
	if _, ok := get(off, http.MethodGet, "/status.json"); ok {
		t.Fatal("status.json should not be served with show_status: false")
	}
	rec, _ = get(off, http.MethodGet, "/site.json")
	if !strings.Contains(rec.Body.String(), `"status": false`) {
		t.Fatalf("with show_status: false, site.json should have status: false: %s", rec.Body.String())
	}
}
