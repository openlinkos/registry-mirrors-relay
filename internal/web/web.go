// Package web 提供浏览器打开镜像域名时看到的用法页。页面最早来自 registry-mirrors-caddy 的 web/，三个仓库共用一份。
// 页面纯静态、不加载外部资源；site.json 由配置生成，告诉页面各上游的加速域名、哪些要登录和页脚链接；
// status.json 是服务状态（见 status 包），没有这个文件时页面不显示状态，所以页面在 Caddy 版里也能用。
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

// 页面里各上游的键
var pageKeys = map[string]string{
	"docker.io": "docker", "gcr.io": "gcr", "ghcr.io": "ghcr", "registry.k8s.io": "k8s", "quay.io": "quay",
}

// Handler 提供用法页的几个文件。
type Handler struct {
	files     map[string]file
	siteLogin file       // 需要登录的入口看到的 site.json
	report    func() any // status.json 的内容，nil 表示不提供
	csp       string
}

type file struct {
	body  []byte
	ctype string
}

// New 由配置生成 site.json，并计算页面内联脚本的哈希用于 CSP。report 生成 status.json，
// 为 nil 或者 site.show_status 关掉时不提供。
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

	// 内联脚本按哈希放行；页面里有 style 属性，样式只能允许内联
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

// siteJSON：domains 取每个固定上游入口的第一个域名；没有这种入口时为空，页面会按当前地址推导。
// login_keys 是其中要登录才能拉取的；login 表示当前这个入口要登录，status 表示有 status.json。
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

// Serve 处理用法页的路径，处理了返回 true。login 表示这个入口拉取要登录。只接受 GET 和 HEAD，调用方已检查过方法。
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
