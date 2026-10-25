package web

import (
	"encoding/json"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/ui"
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

func page(t *testing.T, h *Handler, login bool) string {
	t.Helper()
	rec, ok := getAs(h, http.MethodGet, "/", login)
	if !ok || rec.Code != 200 {
		t.Fatalf("/ should return the usage page: %v %d", ok, rec.Code)
	}
	return rec.Body.String()
}

// pageData is the data the page hands its script.
func pageData(t *testing.T, body string) map[string]any {
	t.Helper()
	m := regexp.MustCompile(`data-page="([^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("the page carries no data for its script")
	}
	raw := strings.NewReplacer("&#34;", `"`, "&quot;", `"`, "&amp;", "&", "&lt;", "<", "&gt;", ">", "&#39;", "'", "&#43;", "+").Replace(m[1])
	var d map[string]any
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("page data: %v\n%s", err, raw)
	}
	return d
}

// checkPage holds every rendering to the page's rules: no inline script or style, nothing the CSP would
// block, both languages, and every icon it refers to carried along.
func checkPage(t *testing.T, name, body string) {
	t.Helper()
	for _, bad := range []*regexp.Regexp{
		regexp.MustCompile(`\sstyle=`),
		regexp.MustCompile(`<script(?:\s[^>]*)?>[^<\s]`), // an inline script body
		regexp.MustCompile(`<script(?:(?:\s+type="module")?\s*)>`),
		regexp.MustCompile(`\son[a-z]+=`),
		regexp.MustCompile(`<style`),
		regexp.MustCompile(`https?://(?:fonts|cdn|unpkg)`),
	} {
		if bad.MatchString(body) {
			t.Errorf("%s: %s", name, bad.FindString(body))
		}
	}
	if !strings.Contains(body, `<span data-l="en">Get started</span><span data-l="zh" lang="zh-CN">`+messagesZH["nav.start"]+`</span>`) {
		t.Errorf("%s: the page should carry both languages", name)
	}
	if strings.Contains(body, ui.SpriteMark) {
		t.Errorf("%s: the icons were not put in", name)
	}
	for _, m := range regexp.MustCompile(`<use href="#(i-[a-z0-9-]+)"/>`).FindAllStringSubmatch(body, -1) {
		if !strings.Contains(body, `<symbol id="`+m[1]+`"`) {
			t.Errorf("%s: icon %s is missing", name, m[1])
		}
	}
	for _, m := range regexp.MustCompile(`(?:src|href)="(/assets/[^"]+)"`).FindAllStringSubmatch(body, -1) {
		if m[1] == "/assets/" {
			t.Errorf("%s: an asset with no file", name)
		}
	}
}

func TestPage(t *testing.T) {
	h := handler(t, "")
	rec, ok := get(h, http.MethodGet, "/")
	if !ok || rec.Code != 200 || rec.Header().Get("Content-Security-Policy") != CSP || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("/: %v %d %v", ok, rec.Code, rec.Header())
	}
	if strings.Contains(CSP, "unsafe-inline") || !strings.Contains(CSP, "trusted-types 'none'") {
		t.Fatalf("the CSP should forbid inline code: %s", CSP)
	}
	body := rec.Body.String()
	checkPage(t, "default", body)
	// The default entrance answers on any host: the script puts the page's own host in
	if !strings.Contains(body, `<span data-single>&lt;host&gt;</span>`) || strings.Contains(body, "data-brand=") {
		t.Errorf("default page: single host placeholder or brand wrong")
	}
	if rec, _ := get(h, http.MethodHead, "/robots.txt"); rec.Body.Len() != 0 || rec.Header().Get("Content-Length") == "0" {
		t.Fatal("HEAD should return no body but a length")
	}
	for _, p := range []string{"/etc/passwd", "/assets/", "/assets/ui.css", "/favicon.ico"} {
		if _, ok := get(h, http.MethodGet, p); ok {
			t.Fatalf("%s should not be handled", p)
		}
	}
}

func TestAssets(t *testing.T) {
	h := handler(t, "site: {brand: linkos-mirrors}")
	body := page(t, h, false)
	refs := regexp.MustCompile(`(?:src|href)="(/assets/([^"]+))"`).FindAllStringSubmatch(body, -1)
	want := map[string]string{".css": ui.CSS, ".js": ui.JS, ".svg": ui.SVG}
	seen := map[string]bool{}
	for _, m := range refs {
		rec, ok := get(h, http.MethodGet, m[1])
		ext := m[2][strings.LastIndex(m[2], "."):]
		if !ok || rec.Code != 200 || rec.Header().Get("Content-Type") != want[ext] || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Errorf("%s: %v %d %v", m[1], ok, rec.Code, rec.Header())
		}
		if !regexp.MustCompile(`\.[0-9a-f]{10}\.`).MatchString(m[2]) {
			t.Errorf("%s has no hash in its name", m[1])
		}
		if ext == ".svg" && rec.Header().Get("Content-Security-Policy") != svgCSP {
			t.Errorf("%s: SVG CSP %q", m[1], rec.Header().Get("Content-Security-Policy"))
		}
		seen[ext] = true
	}
	if len(seen) != 3 {
		t.Fatalf("the page should refer to its stylesheets, scripts and marks: %v", refs)
	}
	if !strings.Contains(body, "brands-linkos-mirrors-logo-dark.") || !strings.Contains(body, `data-brand="linkos-mirrors"`) {
		t.Error("the brand's marks and colours should be used")
	}
	// Fonts sit next to the stylesheet
	css := regexp.MustCompile(`href="(/assets/ui\.[0-9a-f]{10}\.css)"`).FindStringSubmatch(body)
	rec, _ := get(h, http.MethodGet, css[1])
	font := regexp.MustCompile(`url\((fonts/[^)]+\.woff2)\)`).FindStringSubmatch(rec.Body.String())
	if font == nil {
		t.Fatal("the stylesheet should refer to its fonts")
	}
	if rec, ok := get(h, http.MethodGet, "/assets/"+font[1]); !ok || rec.Header().Get("Content-Type") != "font/woff2" {
		t.Errorf("font %s is not served", font[1])
	}
	if rec, ok := get(h, http.MethodGet, "/favicon.svg"); !ok || !strings.Contains(rec.Body.String(), "<svg") {
		t.Error("/favicon.svg should be the brand's favicon")
	}
}

const fixedYAML = `
site:
  name: LinkOS Mirrors
  operator: LinkOS
  links: {home: "https://linkos.io", source: "https://github.com/openlinkos/registry-mirrors-relay", status: "javascript:alert(1)"}
entrances:
  - {hosts: [docker.example.com], upstream: docker.io, auth: basic}
  - {hosts: [ghcr.example.com], upstream: ghcr.io, auth: basic}
  - {hosts: [quay.example.com], upstream: quay.io}
access:
  users_file: /dev/null
`

func TestFixedSubdomains(t *testing.T) {
	h := handler(t, fixedYAML)
	open, login := page(t, h, false), page(t, h, true)
	checkPage(t, "fixed", open)
	checkPage(t, "fixed login", login)
	for _, want := range []string{
		"docker.example.com", "ghcr.example.com", "quay.example.com", // the mirrors
		`https://linkos.io`, // a configured link
		"docker login docker.example.com\ndocker login ghcr.example.com", // signing in to each that needs it
		"Sign-in needed",
		"Run by LinkOS.",
	} {
		if !strings.Contains(open, want) {
			t.Errorf("fixed page lacks %q", want)
		}
	}
	// Docker Hub needs a sign-in, which registry-mirrors does not send: pull through the address instead
	if strings.Contains(open, "registry-mirrors&#34;: [") || !strings.Contains(open, "docker pull docker.example.com/library/nginx:1.27") {
		t.Error("with a Docker Hub mirror that needs signing in, the page should not offer registry-mirrors")
	}
	if strings.Contains(open, "javascript:") {
		t.Error("only http(s) links are shown")
	}
	// gcr.io and registry.k8s.io have no entrance here, so they are left out
	d := pageData(t, open)
	ups, _ := d["upstreams"].([]any)
	if len(ups) != 3 || d["single"] != nil {
		t.Fatalf("upstreams: %v single: %v", ups, d["single"])
	}
	if strings.Contains(open, "Pulls need an account") || !strings.Contains(login, "Pulls need an account") {
		t.Error("the sign-in version says pulls need an account, the open one does not")
	}
	if d := pageData(t, login); d["login"] != true {
		t.Error("the sign-in version should say so to the script")
	}
	if !strings.Contains(open, "faq.5xx") && !strings.Contains(open, `href="#status"`) {
		t.Error("without a status link, the 5xx answer points at the status section")
	}
}

func TestSingleDomain(t *testing.T) {
	h := handler(t, `
entrances:
  - {name: mirror, hosts: [mirror.example.com, mirror2.example.com]}
`)
	body := page(t, h, false)
	checkPage(t, "single", body)
	for _, want := range []string{
		// registry-mirrors for Docker Hub at the root of the single host
		`&#34;registry-mirrors&#34;: [&#34;<span data-scheme>https</span>://<span data-single>mirror.example.com</span>&#34;]`,
		// Podman names the registry after the host as a prefix; Docker Hub needs none
		`location = &#34;<span data-single>mirror.example.com</span>/ghcr.io&#34;`,
		`location = &#34;<span data-single>mirror.example.com</span>&#34;`,
		// containerd picks the registry from its ns parameter, so the host alone is enough
		`[host.&#34;<span data-scheme>https</span>://<span data-single>mirror.example.com</span>&#34;]`,
		`sudo kubeadm init --image-repository <span data-single>mirror.example.com</span>/registry.k8s.io`,
		// plain HTTP needs insecure-registries; the script shows it on http pages
		`<span data-http hidden>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("single-domain page lacks %s", want)
		}
	}
	d := pageData(t, body)
	s, _ := d["single"].(map[string]any)
	if s == nil || s["default"] != "docker.io" || len(s["hosts"].([]any)) != 2 {
		t.Fatalf("single: %v", d["single"])
	}
	if ups, _ := d["upstreams"].([]any); len(ups) != 5 {
		t.Fatalf("every built-in upstream is reached through the single host: %v", ups)
	}
}

func TestStatusOff(t *testing.T) {
	on := page(t, handler(t, ""), false)
	off := page(t, handler(t, "site: {show_status: false}"), false)
	if !strings.Contains(on, `id="status"`) || strings.Contains(off, `id="status"`) || strings.Contains(off, "data-status-pill") {
		t.Error("the status section and pill follow site.show_status")
	}
	if pageData(t, off)["status"] != false {
		t.Error("the script should not fetch status.json when it is off")
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
	// A mixed setup: ghcr.io and Docker Hub on their own hosts, the rest under the single host
	body := page(t, h, false)
	checkPage(t, "mixed", body)
	if !strings.Contains(body, `<span data-single>mirror.example.com</span>/quay.io`) || !strings.Contains(body, "ghcr.example.com") {
		t.Error("mixed setups should use each upstream's own host, and the single host for the rest")
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

func TestMessages(t *testing.T) {
	if !slices.Equal(slices.Sorted(maps.Keys(messagesEN)), slices.Sorted(maps.Keys(messagesZH))) {
		for k := range messagesEN {
			if _, ok := messagesZH[k]; !ok {
				t.Errorf("Chinese is missing %s", k)
			}
		}
		for k := range messagesZH {
			if _, ok := messagesEN[k]; !ok {
				t.Errorf("English is missing %s", k)
			}
		}
	}
	// Placeholders match
	ph := regexp.MustCompile(`\{[a-z]+\}`)
	for k, en := range messagesEN {
		a, b := ph.FindAllString(en, -1), ph.FindAllString(messagesZH[k], -1)
		slices.Sort(a)
		slices.Sort(b)
		if !slices.Equal(a, b) {
			t.Errorf("%s: placeholders %v and %v differ", k, a, b)
		}
	}
	// Every message is used: by the template, the Go code or the script
	var sources strings.Builder
	_ = fs.WalkDir(files, ".", func(p string, d fs.DirEntry, _ error) error {
		if !d.IsDir() {
			b, _ := files.ReadFile(p)
			sources.Write(b)
		}
		return nil
	})
	for _, f := range []string{"web.go", "page.go"} {
		b, _ := os.ReadFile(f)
		sources.Write(b)
	}
	src := sources.String()
	for k := range messagesEN {
		used := strings.Contains(src, `"`+k+`"`)
		if name, ok := strings.CutPrefix(k, "js."); ok {
			used = used || strings.Contains(src, `'`+name+`'`) || strings.Contains(src, "`"+strings.SplitN(name, ".", 2)[0]+".")
		}
		if strings.HasPrefix(k, "footer.") {
			used = true // footer.<link> is composed in the template
		}
		if !used {
			t.Errorf("message %s is not used", k)
		}
	}
}
