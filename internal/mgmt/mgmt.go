// Package mgmt is the management entrance: the web console (/admin/), the management API
// (/admin/api/v1/), Prometheus metrics (/metrics) and pprof (/debug/pprof/). By default it shares the
// port with the pull entrance; when listen.admin is set it is served only on that port.
//
// The console and the management API are open to all requests by default (admin.public) and are
// protected by login. Metrics and pprof, which have no login, and the console and API when
// admin.public: false, answer only two kinds of request; everything else is treated as not found (404):
//   - Direct connection: the request carries no proxy headers (see clientip.Proxied) and the TCP peer is
//     in admin.allow (by default the local machine and private networks). kubectl port-forward, SSH
//     tunnels and Prometheus inside the cluster are like this; requests through a CDN or reverse proxy
//     all carry proxy headers and cannot get in.
//   - Deliberately proxied: admin.proxied_allow and admin.public_url are set, Host is the public_url
//     domain, the TCP peer is a trusted proxy, and the client address the proxy passes on is in
//     proxied_allow.
//
// A direct connection is judged only by the TCP peer, not by X-Forwarded-For: anyone who bypasses the CDN
// and connects to the origin directly can forge that header. Layer 4 load balancers, NodePort SNAT and
// Docker's docker-proxy (IPv6 clients) can make external clients look like private addresses; in such
// environments use a separate listen.admin.
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

// Handler serves every path of the management entrance.
type Handler struct {
	app *app.App
	ops *ops.Ops
	mux *http.ServeMux
}

// New creates the Handler: web console, management API, metrics and pprof. Metrics and pprof check the
// current configuration on every request, so turning them on or off takes effect on hot reload.
func New(a *app.App, log *slog.Logger) (*Handler, error) {
	o := ops.New(a)
	web, err := admin.New(a, o, admin.Options{Log: log})
	if err != nil {
		return nil, err
	}
	h := &Handler{app: a, ops: o, mux: http.NewServeMux()}
	h.mux.Handle(api.Prefix+"/", api.New(a, o, api.Options{Session: web.Session, Lang: admin.Lang}))
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

// enabled reports whether the path is currently switched on: the console and API always are, metrics
// need metrics.enabled, and pprof also needs metrics.pprof. Paths outside the management entrance
// return false.
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

// console reports whether the path belongs to the console or the management API (the parts protected
// by login).
func console(path string) bool { return path == "/admin" || strings.HasPrefix(path, "/admin/") }

// Allowed reports whether the request may enter the management entrance: with admin.public the console
// and API are let through, everything else follows Gated. If not, reason explains why (it goes into the
// access log).
func Allowed(rt *app.Runtime, r *http.Request) (ok bool, reason string) {
	if rt.Mgmt.Public && console(r.URL.Path) {
		return true, ""
	}
	return Gated(rt, r)
}

// Gated is the address-based rule: direct connections are checked against admin.allow, proxied ones
// against admin.proxied_allow.
func Gated(rt *app.Runtime, r *http.Request) (ok bool, reason string) {
	p := rt.Mgmt
	if !clientip.Proxied(r) {
		if contains(p.Allow, clientip.Remote(r)) {
			return true, ""
		}
		return false, "management entrance: peer is not in admin.allow"
	}
	if len(p.ProxiedAllow) == 0 {
		return false, "management entrance: request came through a proxy and admin.proxied_allow does not allow it"
	}
	if p.PublicHost == "" || !strings.EqualFold(r.Host, p.PublicHost) {
		return false, "management entrance: proxied request is not for the admin.public_url domain"
	}
	c, ok := rt.IPs.Forwarded(r)
	if !ok {
		return false, "management entrance: proxied request does not come from a trusted proxy"
	}
	if !contains(p.ProxiedAllow, c) {
		return false, "management entrance: client is not in admin.proxied_allow"
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

// Shared mounts the management entrance on the pull entrance: allowed management requests go to the
// console, API and metrics; all other requests (including management requests that were not allowed,
// and metrics that are switched off) go to next unchanged, whose 404 for unknown paths is the same as for
// any other path, so nothing reveals that a console is here.
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

// Standalone is the separate management port: only the management entrance's paths, with the root path
// redirecting to the console; requests that are not allowed get 404.
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
			log.Debug("management entrance rejected request", "path", r.URL.Path, "remote", r.RemoteAddr, "reason", reason)
			http.NotFound(w, r)
			return
		}
		h.mux.ServeHTTP(w, r)
	})
}

// Ops returns the operations the console and the API share. The local admin socket uses the same
// instance, so that a password set from the command line signs out that administrator's console sessions.
func (h *Handler) Ops() *ops.Ops { return h.ops }
