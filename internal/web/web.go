// Package web 提供浏览器打开镜像域名时看到的用法页。页面来自 registry-mirrors-caddy 的 web/，三个仓库共用一份。
// 页面纯静态、不加载外部资源；site.json 由配置生成，告诉页面各上游的加速域名和页脚链接。
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
	files map[string]file
	csp   string
}

type file struct {
	body  []byte
	ctype string
}

// New 由配置生成 site.json，并计算页面内联脚本的哈希用于 CSP。
func New(cfg *config.Config) (*Handler, error) {
	h := &Handler{files: map[string]file{}}
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
	site, err := siteJSON(cfg)
	if err != nil {
		return nil, err
	}
	h.files["/site.json"] = file{body: site, ctype: "application/json"}

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
func siteJSON(cfg *config.Config) ([]byte, error) {
	domains := map[string]string{}
	for _, e := range cfg.Entrances {
		key, ok := pageKeys[e.Upstream]
		if !ok || len(e.Hosts) == 0 || e.Hosts[0] == "*" {
			continue
		}
		if _, seen := domains[key]; !seen {
			domains[key] = e.Hosts[0]
		}
	}
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
		"operator": cfg.Site.Operator,
		"domains":  domains,
		"links":    links,
	}, "", "  ")
}

// Serve 处理用法页的路径，处理了返回 true。只接受 GET 和 HEAD，调用方已检查过方法。
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request) bool {
	f, ok := h.files[r.URL.Path]
	if !ok {
		return false
	}
	hd := w.Header()
	hd.Set("Content-Type", f.ctype)
	hd.Set("Content-Length", strconv.Itoa(len(f.body)))
	hd.Set("Cache-Control", "no-cache")
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
