// Package mgmt 是管理入口：网页后台（/admin/）、管理 API（/admin/api/v1/）、Prometheus 指标（/metrics）
// 和 pprof（/debug/pprof/）。默认和拉取入口共用端口；写了 listen.admin 时只在那个端口提供。
//
// 后台和管理 API 默认（admin.public）对所有请求开放，靠登录保护。指标和 pprof 没有登录，
// 以及 admin.public: false 时的后台和 API，只回应两种请求，其余一律当作不存在（404）：
//   - 直连：请求里没有任何代理头（见 clientip.Proxied），并且 TCP 对端在 admin.allow 里（默认本机和内网）。
//     kubectl port-forward、SSH 隧道、集群里的 Prometheus 都是这种；经 CDN、反向代理来的请求都带代理头，进不来。
//   - 有意经代理：写了 admin.proxied_allow 和 admin.public_url，Host 是 public_url 的域名，TCP 对端是受信任的代理，
//     并且代理传来的客户端地址在 proxied_allow 里。
//
// 直连只看 TCP 对端，不看 X-Forwarded-For：绕过 CDN 直连源站的人可以伪造它。四层负载均衡、NodePort 的 SNAT、
// Docker 的 docker-proxy（IPv6 客户端）会让外部客户端看起来像内网地址，这类环境请用单独的 listen.admin。
package mgmt

import (
	"log/slog"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"strings"

	"github.com/openlinkos/registry-mirrors-relay/internal/accesslog"
	"github.com/openlinkos/registry-mirrors-relay/internal/admin"
	"github.com/openlinkos/registry-mirrors-relay/internal/api"
	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/clientip"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
)

// Handler 处理管理入口的全部路径。
type Handler struct {
	app *app.App
	mux *http.ServeMux
}

// New 创建 Handler：网页后台、管理 API、指标和 pprof。指标和 pprof 每次请求都看当时的配置，开关能热重载。
func New(a *app.App, log *slog.Logger) (*Handler, error) {
	o := ops.New(a)
	web, err := admin.New(a, o, admin.Options{Log: log})
	if err != nil {
		return nil, err
	}
	h := &Handler{app: a, mux: http.NewServeMux()}
	h.mux.Handle(api.Prefix+"/", api.New(a, o, api.Options{Session: web.Session}))
	h.mux.Handle("/admin/", web)
	h.mux.Handle("/admin", web)
	h.mux.Handle("/metrics", a.Metrics)
	h.mux.HandleFunc("/debug/pprof/", pprof.Index)
	h.mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	h.mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	h.mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	h.mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return h, nil
}

// enabled 判断这个路径现在是不是开着：后台和 API 一直开，指标要 metrics.enabled，pprof 还要 metrics.pprof。
// 不是管理入口的路径返回 false。
func (h *Handler) enabled(path string) bool {
	m := h.app.Runtime().Config.Metrics
	switch {
	case console(path):
		return true
	case path == "/metrics":
		return m.Enabled
	case path == "/debug/pprof" || strings.HasPrefix(path, "/debug/pprof/"):
		return m.Enabled && m.Pprof
	}
	return false
}

// console 判断路径是不是后台或管理 API（有登录保护的部分）。
func console(path string) bool { return path == "/admin" || strings.HasPrefix(path, "/admin/") }

// Allowed 判断请求能不能进管理入口：admin.public 时后台和 API 都放行，其余按 Gated。不能时 reason 说明原因（写进访问日志）。
func Allowed(rt *app.Runtime, r *http.Request) (ok bool, reason string) {
	if rt.Mgmt.Public && console(r.URL.Path) {
		return true, ""
	}
	return Gated(rt, r)
}

// Gated 是按地址放行的规则：直连看 admin.allow，经代理看 admin.proxied_allow。
func Gated(rt *app.Runtime, r *http.Request) (ok bool, reason string) {
	p := rt.Mgmt
	if !clientip.Proxied(r) {
		if contains(p.Allow, clientip.Remote(r)) {
			return true, ""
		}
		return false, "管理入口：对端不在 admin.allow 里"
	}
	if len(p.ProxiedAllow) == 0 {
		return false, "管理入口：请求经过了代理，admin.proxied_allow 没有放行"
	}
	if p.PublicHost == "" || !strings.EqualFold(r.Host, p.PublicHost) {
		return false, "管理入口：经代理的请求不是访问 admin.public_url 的域名"
	}
	c, ok := rt.IPs.Forwarded(r)
	if !ok {
		return false, "管理入口：经代理的请求不是来自受信任的代理"
	}
	if !contains(p.ProxiedAllow, c) {
		return false, "管理入口：客户端不在 admin.proxied_allow 里"
	}
	return true, ""
}

func contains(ps []netip.Prefix, a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	a = a.Unmap()
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Shared 把管理入口挂在拉取入口上：放行的管理请求交给后台、API 和指标，其余请求（包括没被放行的管理请求、
// 关着的指标）原样交给 next，它对未知路径的 404 和任何别的路径一样，看不出这里有后台。
func (h *Handler) Shared(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.enabled(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if ok, reason := Allowed(h.app.Runtime(), r); !ok {
			next.ServeHTTP(w, r.WithContext(accesslog.WithNote(r.Context(), reason)))
			return
		}
		h.mux.ServeHTTP(w, r)
	})
}

// Standalone 是单独的管理端口：只有管理入口的路径，根路径跳到后台；没被放行的请求返回 404。
func (h *Handler) Standalone(log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			if ok, _ := Gated(h.app.Runtime(), r); ok || h.app.Runtime().Mgmt.Public {
				http.Redirect(w, r, "/admin/", http.StatusSeeOther)
				return
			}
		}
		if !h.enabled(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		if ok, reason := Allowed(h.app.Runtime(), r); !ok {
			log.Debug("管理入口拒绝了请求", "path", r.URL.Path, "remote", r.RemoteAddr, "reason", reason)
			http.NotFound(w, r)
			return
		}
		h.mux.ServeHTTP(w, r)
	})
}
