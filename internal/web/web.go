// Package web serves the usage page that a browser sees when it opens a mirror host. The page is rendered
// from the configuration whenever it changes, in two versions (for entrances open to everyone and for those
// that require signing in to pull), and does not vary otherwise, so a CDN can cache it. It is built from the
// design system (internal/ui), loads nothing from elsewhere, and carries both English and Chinese; a small
// script picks the language, fills in the live status and converts image addresses.
//
// site.json (the mirror hosts, the footer links and whether status.json is served) and status.json (the
// service status, see the status package) are served as before.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/i18n"
	"github.com/openlinkos/registry-mirrors-relay/internal/ui"
)

//go:embed templates static
var files embed.FS

// CSP is the page's content security policy: its own files only, no inline script or style, and Trusted
// Types with no policy, so the script can only write text.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; " +
	"require-trusted-types-for 'script'; trusted-types 'none'"

// svgCSP: the marks carry their colours in an embedded <style>.
const svgCSP = "default-src 'none'; style-src 'unsafe-inline'; sandbox"

// Keys of the upstreams in site.json, as the pages of the other editions expect them.
var pageKeys = map[string]string{
	"docker.io": "docker", "gcr.io": "gcr", "ghcr.io": "ghcr", "registry.k8s.io": "k8s", "quay.io": "quay",
}

// Handler serves the usage page and its files.
type Handler struct {
	page    [2][]byte // [open, sign-in]
	site    [2][]byte // site.json, the same way
	assets  *ui.Set
	favicon *ui.Asset
	robots  []byte
	report  func() any // content of status.json, nil means it is not served
}

// New renders the page from the configuration. report produces status.json, which is not served when
// report is nil or site.show_status is turned off.
func New(cfg *config.Config, report func() any) (*Handler, error) {
	h := &Handler{}
	if cfg.Site.ShowStatus {
		h.report = report
	}
	var err error
	if h.assets, err = ui.NewSet(); err != nil {
		return nil, err
	}
	for name, ctype := range map[string]string{"site.css": ui.CSS, "head.js": ui.JS, "site.js": ui.JS} {
		body, err := files.ReadFile("static/" + name)
		if err != nil {
			return nil, err
		}
		h.assets.Add(name, ctype, body)
	}
	h.favicon, _ = h.assets.Lookup(h.assets.File(ui.Mark(cfg.Site.Brand, "favicon.svg")))
	if h.robots, err = files.ReadFile("static/robots.txt"); err != nil {
		return nil, err
	}
	tmpl, err := pageTemplate()
	if err != nil {
		return nil, err
	}
	m := buildModel(cfg)
	for i, login := range []bool{false, true} {
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, h.view(cfg, m, login)); err != nil {
			return nil, err
		}
		h.page[i] = ui.WithSprite(buf.Bytes())
		if h.site[i], err = siteJSON(cfg, login, h.report != nil); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// ---- Rendering

// L is a value in each language.
type L struct{ EN, ZH string }

// footerLink is one of the site's links.
type footerLink struct{ Key, URL string }

// card is one upstream in the Mirrors section.
type card struct {
	U       upstream
	ID      string
	Mirror  template.HTML
	Example template.HTML
}

// view is the data the page template gets.
type view struct {
	h                    *Handler
	Name, Operator       string
	Brand                string
	Login, ShowStatus    bool
	Fixed                bool
	StatusLink, Privacy  string
	Links                []footerLink
	Cards                []card
	S                    snippets
	Registries           L
	SingleExample        template.HTML
	ENName, ZHName, Data string
}

func (h *Handler) view(cfg *config.Config, m model, login bool) *view {
	v := &view{h: h, Name: cfg.Site.Name, Operator: cfg.Site.Operator, Brand: cfg.Site.Brand, Login: login,
		ShowStatus: cfg.Site.ShowStatus, Fixed: m.anyFixed(), S: buildSnippets(m),
		ENName: i18n.EN.Name(), ZHName: i18n.ZH.Name()}
	for _, k := range []string{"status", "terms", "privacy", "abuse", "source", "home"} {
		u := cfg.Site.Links[k]
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			continue
		}
		v.Links = append(v.Links, footerLink{Key: k, URL: u})
		switch k {
		case "status":
			v.StatusLink = u
		case "privacy":
			v.Privacy = u
		}
	}
	var labels []string
	for i := range m.Upstreams {
		u := &m.Upstreams[i]
		labels = append(labels, u.Label)
		v.Cards = append(v.Cards, card{U: *u, ID: "mirror-" + strconv.Itoa(i), Mirror: m.mirrorHTML(u), Example: m.exampleHTML(u)})
		if v.SingleExample == "" && u.Domain == "" && !u.Root {
			v.SingleExample = (&code{}).mirror(m, u).text("/" + u.Example).html()
		}
	}
	v.Registries = L{EN: join(labels, i18n.EN), ZH: join(labels, i18n.ZH)}

	data := struct {
		Status    bool                         `json:"status"`
		Login     bool                         `json:"login"`
		Single    *single                      `json:"single,omitempty"`
		Upstreams []upstream                   `json:"upstreams"`
		Messages  map[string]map[string]string `json:"messages"`
	}{Status: cfg.Site.ShowStatus, Login: login, Single: m.Single, Upstreams: m.Upstreams,
		Messages: map[string]map[string]string{"en": jsMessages(messagesEN), "zh": jsMessages(messagesZH)}}
	if data.Upstreams == nil {
		data.Upstreams = []upstream{}
	}
	b, _ := json.Marshal(data)
	v.Data = string(b)
	return v
}

// Asset is the address of one of the page's files.
func (v *view) Asset(name string) string { return "/assets/" + v.h.assets.File(name) }

// Mark is the address of one file of the site's mark.
func (v *view) Mark(file string) string { return v.Asset(ui.Mark(v.Brand, file)) }

// join lists names the way a sentence does in each language: "a, b and c".
func join(names []string, l i18n.Lang) string {
	msgs := messagesEN
	if l == i18n.ZH {
		msgs = messagesZH
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], msgs["comma"]) + msgs["and"] + names[len(names)-1]
}

func jsMessages(all map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range all {
		if name, ok := strings.CutPrefix(k, "js."); ok {
			out[name] = v
		}
	}
	return out
}

// fill puts arguments (pairs of name and value) into a message. In markup, string values are escaped;
// template.HTML values are markup made by the page; L values differ by language.
func fill(text string, lang i18n.Lang, markup bool, args []any) string {
	for i := 0; i+1 < len(args); i += 2 {
		var val string
		switch a := args[i+1].(type) {
		case template.HTML:
			val = string(a)
		case L:
			val = a.EN
			if lang == i18n.ZH {
				val = a.ZH
			}
			if markup {
				val = template.HTMLEscapeString(val)
			}
		default:
			val = fmt.Sprint(a)
			if markup {
				val = template.HTMLEscapeString(val)
			}
		}
		text = strings.ReplaceAll(text, "{"+fmt.Sprint(args[i])+"}", val)
	}
	return text
}

// message is a message's text in a language; a missing key is a development error the tests catch.
func message(key string, lang i18n.Lang) string {
	msgs := messagesEN
	if lang == i18n.ZH {
		msgs = messagesZH
	}
	text, ok := msgs[key]
	if !ok {
		panic("usage page: no message " + key)
	}
	return text
}

var funcs = template.FuncMap{
	// t writes a message in both languages; the page's stylesheet shows the one in use.
	"t": func(key string, args ...any) template.HTML {
		return template.HTML(`<span data-l="en">` + fill(message(key, i18n.EN), i18n.EN, true, args) + //nolint:gosec // trusted messages, escaped arguments
			`</span><span data-l="zh" lang="zh-CN">` + fill(message(key, i18n.ZH), i18n.ZH, true, args) + `</span>`)
	},
	// te and tz are a message in English and in Chinese, for attributes (the script swaps them).
	"te":   func(key string, args ...any) string { return fill(message(key, i18n.EN), i18n.EN, false, args) },
	"tz":   func(key string, args ...any) string { return fill(message(key, i18n.ZH), i18n.ZH, false, args) },
	"icon": ui.Icon,
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[fmt.Sprint(kv[i])] = kv[i+1]
		}
		return m
	},
}

var pageTemplate = sync.OnceValues(func() (*template.Template, error) {
	return template.New("page.html").Funcs(funcs).ParseFS(files, "templates/page.html")
})

// siteJSON: domains takes the first host of each entrance with a fixed upstream; when there is no such
// entrance it is empty and the page derives the domains from the current address. login_keys are those
// among them that require signing in to pull; login means the current entrance requires signing in, and
// status means status.json is available.
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

// ---- Serving

// Serve handles the usage page's paths and returns true if it handled the request. login means pulls
// through this entrance require signing in. Only GET and HEAD are accepted; the caller has already
// checked the method.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, login bool) bool {
	variant := 0
	if login {
		variant = 1
	}
	var body []byte
	ctype, cache := "", "no-cache"
	csp := ""
	switch p := r.URL.Path; {
	case p == "/" || p == "/index.html":
		body, ctype, csp = h.page[variant], "text/html; charset=utf-8", CSP
	case p == "/site.json":
		body, ctype = h.site[variant], "application/json"
	case p == "/status.json" && h.report != nil:
		b, err := json.Marshal(h.report())
		if err != nil {
			return false
		}
		body, ctype, cache = b, "application/json", "no-store"
	case p == "/robots.txt":
		body, ctype = h.robots, "text/plain; charset=utf-8"
	case p == "/favicon.svg" && h.favicon != nil:
		body, ctype, csp = h.favicon.Body, h.favicon.Type, svgCSP
	case strings.HasPrefix(p, "/assets/"):
		a, ok := h.assets.Lookup(strings.TrimPrefix(p, "/assets/"))
		if !ok {
			return false
		}
		body, ctype, cache = a.Body, a.Type, "public, max-age=31536000, immutable"
		if a.Type == ui.SVG {
			csp = svgCSP
		}
	default:
		return false
	}
	hd := w.Header()
	hd.Set("Content-Type", ctype)
	hd.Set("Content-Length", strconv.Itoa(len(body)))
	hd.Set("Cache-Control", cache)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "no-referrer")
	if csp != "" {
		hd.Set("Content-Security-Policy", csp)
	}
	if ctype == "text/html; charset=utf-8" {
		hd.Set("X-Frame-Options", "DENY")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body) //nolint:gosec // rendered from the configuration or JSON, served with its own type, nosniff and CSP
	}
	return true
}
