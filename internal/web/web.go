// Package web serves the usage page that a browser sees when it opens a mirror domain. The page
// originally came from web/ in registry-mirrors-caddy, and the three repositories share one copy.
// The page is purely static and loads no external resources; site.json is generated from the
// configuration and tells the page each upstream's mirror domain, which ones require signing in, and
// the footer links; status.json is the service status (see the status package), and without this file
// the page shows no status, so the page also works in the Caddy edition.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
)

//go:embed static
var static embed.FS

// Keys of the upstreams in the page
var pageKeys = map[string]string{
	"docker.io": "docker", "gcr.io": "gcr", "ghcr.io": "ghcr", "registry.k8s.io": "k8s", "quay.io": "quay",
}

// Handler serves the files of the usage page.
type Handler struct {
	files     map[string]file
	siteLogin file       // site.json as seen by entrances that require signing in
	report    func() any // content of status.json, nil means it is not served
	csp       string
}

type file struct {
	body  []byte
	ctype string
}

// New generates site.json from the configuration and computes the hash of the page's inline script for
// the CSP. report produces status.json, which is not served when report is nil or site.show_status is
// turned off.
func New(cfg *config.Config, report func() any) (*Handler, error) {
	h := &Handler{files: map[string]file{}}
	if cfg.Site.ShowStatus {
		h.report = report
	}
	for name, ctype := range map[string]string{
		"index.html": "text/html; charset=utf-8", "favicon.svg": "image/svg+xml",
		"favicon.ico": "image/x-icon", "robots.txt": "text/plain; charset=utf-8",
	} {
		b, err := static.ReadFile("static/" + name)
		if err != nil {
			return nil, err
		}
		h.files["/"+name] = file{body: b, ctype: ctype}
	}
	h.files["/"] = h.files["/index.html"]
	for _, login := range []bool{false, true} {
		site, err := siteJSON(cfg, login, h.report != nil)
		if err != nil {
			return nil, err
		}
		f := file{body: site, ctype: "application/json"}
		if login {
			h.siteLogin = f
		} else {
			h.files["/site.json"] = f
		}
	}

	// The inline script is allowed by its hash; the page has style attributes, so inline styles must
	// be allowed
	script := between(h.files["/index.html"].body, "<script>", "</script>")
	sum := sha256.Sum256(script)
	h.csp = "default-src 'none'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; " +
		"style-src 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; " +
		"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	return h, nil
}

func between(b []byte, start, end string) []byte {
	i := bytes.Index(b, []byte(start))
	if i < 0 {
		return nil
	}
	b = b[i+len(start):]
	if j := bytes.Index(b, []byte(end)); j >= 0 {
		return b[:j]
	}
	return nil
}

// siteJSON: domains takes the first domain of each entrance with a fixed upstream; when there is no
// such entrance it is empty and the page derives the domains from the current address. login_keys are
// those among them that require signing in to pull; login means the current entrance requires signing
// in, and status means status.json is available.
func siteJSON(cfg *config.Config, login, status bool) ([]byte, error) {
	domains := map[string]string{}
	loginKeys := []string{}
	for _, e := range cfg.Entrances {
		key, ok := pageKeys[e.Upstream]
		if !ok || len(e.Hosts) == 0 || e.Hosts[0] == "*" {
			continue
		}
		if _, seen := domains[key]; !seen {
			domains[key] = e.Hosts[0]
			if e.Auth == "basic" {
				loginKeys = append(loginKeys, key)
			}
		}
	}
	sort.Strings(loginKeys)
	links := map[string]string{}
	keys := make([]string, 0, len(cfg.Site.Links))
	for k := range cfg.Site.Links {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		links[k] = cfg.Site.Links[k]
	}
	return json.MarshalIndent(map[string]any{
		"operator":   cfg.Site.Operator,
		"domains":    domains,
		"links":      links,
		"login":      login,
		"login_keys": loginKeys,
		"status":     status,
	}, "", "  ")
}

// Serve handles the usage page paths and returns true if it handled the request. login means pulls
// through this entrance require signing in. Only GET and HEAD are accepted; the caller has already
// checked the method.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, login bool) bool {
	f, ok := h.files[r.URL.Path]
	cache := "no-cache"
	switch {
	case r.URL.Path == "/site.json" && login:
		f = h.siteLogin
	case r.URL.Path == "/status.json" && h.report != nil:
		b, err := json.Marshal(h.report())
		if err != nil {
			return false
		}
		f, ok, cache = file{body: b, ctype: "application/json"}, true, "no-store"
	}
	if !ok {
		return false
	}
	hd := w.Header()
	hd.Set("Content-Type", f.ctype)
	hd.Set("Content-Length", strconv.Itoa(len(f.body)))
	hd.Set("Cache-Control", cache)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "no-referrer")
	if f.ctype == "text/html; charset=utf-8" {
		hd.Set("Content-Security-Policy", h.csp)
		hd.Set("X-Frame-Options", "DENY")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(f.body)
	}
	return true
}
