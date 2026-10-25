// Package api is the admin console's JSON API. Every path is under /admin/api/v1/, and it is shared by
// automation scripts and the console's web pages. Validation and writes live in internal/ops; this
// package only authenticates, parses requests and writes JSON.
//
// Security:
//   - authentication with Authorization: Bearer <token> (tokens come from admin.api_tokens in the
//     configuration or are created in the console; only their SHA-256 is stored); web console sessions
//     work too, and writes must carry X-CSRF-Token (see Options.Session);
//   - viewer can only read, writes require admin; every write is recorded in the audit log;
//   - with admin.public: false, Host must be the admin.public_url host, localhost or an IP address
//     (otherwise 421), against DNS rebinding;
//   - failed authentication is limited per IP; request bodies are at most 64 KiB and only JSON is accepted.
package api

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/clientip"
	"github.com/openlinkos/registry-mirrors-relay/internal/ops"
	"github.com/openlinkos/registry-mirrors-relay/internal/stats"
	"github.com/openlinkos/registry-mirrors-relay/internal/version"
)

// Prefix is the API's path prefix.
const Prefix = "/admin/api/v1"

// Session authenticates a web console session; it is called when the request has no Authorization
// header. ok is false when there is no session; err is returned when there is a session but a write fails
// the CSRF or origin check (the API answers 403).
type Session func(r *http.Request) (who ops.Actor, role string, ok bool, err error)

// Options are optional settings.
type Options struct {
	Session Session
}

// API handles requests under /admin/api/v1/.
type API struct {
	app *app.App
	ops *ops.Ops
	opt Options
	mux *http.ServeMux
}

// New creates the API.
func New(a *app.App, o *ops.Ops, opt Options) *API {
	x := &API{app: a, ops: o, opt: opt, mux: http.NewServeMux()}
	r := func(pattern string, role string, h func(w http.ResponseWriter, r *http.Request, who ops.Actor)) {
		method, path, _ := strings.Cut(pattern, " ")
		x.mux.HandleFunc(method+" "+Prefix+path, func(w http.ResponseWriter, r *http.Request) {
			who, whoRole, ok := x.authenticate(w, r)
			if !ok {
				return
			}
			if role == "admin" && whoRole != "admin" {
				writeErr(w, http.StatusForbidden, "只读身份不能修改")
				return
			}
			h(w, r, who)
		})
	}
	r("GET /status", "viewer", x.status)
	r("GET /status/report", "viewer", x.statusReport)
	r("GET /stats/series", "viewer", x.series)
	r("GET /stats/summary", "viewer", x.summary)
	r("GET /requests", "viewer", x.requests)
	r("GET /upstreams", "viewer", x.upstreams)
	r("POST /upstreams/{name}/check", "admin", x.checkUpstream)
	r("POST /upstreams/{name}/redirect-hosts", "admin", x.addRedirectHost)
	r("DELETE /upstreams/{name}/redirect-hosts/{host}", "admin", x.delRedirectHost)
	r("GET /users", "viewer", x.listUsers)
	r("POST /users", "admin", x.createUser)
	r("PATCH /users/{name}", "admin", x.patchUser)
	r("DELETE /users/{name}", "admin", x.deleteUser)
	r("GET /rules", "viewer", x.listRules)
	r("POST /rules/ip", "admin", x.addIPRule)
	r("DELETE /rules/ip", "admin", x.delIPRule)
	r("POST /bans", "admin", x.addBan)
	r("DELETE /bans", "admin", x.delBan)
	r("POST /rules/image", "admin", x.addImageRule)
	r("DELETE /rules/image", "admin", x.delImageRule)
	r("POST /rules/image/test", "viewer", x.testImage)
	r("GET /tokens", "viewer", x.listTokens)
	r("POST /tokens", "admin", x.createToken)
	r("DELETE /tokens/{name}", "admin", x.deleteToken)
	r("GET /admins", "viewer", x.listAdmins)
	r("POST /admins", "admin", x.createAdmin)
	r("PATCH /admins/{name}", "admin", x.patchAdmin)
	r("DELETE /admins/{name}", "admin", x.deleteAdmin)
	r("GET /site", "viewer", x.site)
	r("PUT /site", "admin", x.setSite)
	r("DELETE /site", "admin", x.resetSite)
	r("GET /audit", "viewer", x.audit)
	r("GET /config", "viewer", x.config)
	r("GET /config/overrides", "viewer", x.listOverrides)
	r("PUT /config/overrides/{key...}", "admin", x.setOverride)
	r("DELETE /config/overrides/{key...}", "admin", x.resetOverride)
	r("GET /config/export", "viewer", x.exportConfig)
	r("POST /reload", "admin", x.reload)
	return x
}

func (x *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	if !ops.HostAllowed(x.app.Runtime().Config.Admin, r) {
		writeErr(w, http.StatusMisdirectedRequest, "请用 admin.public_url、localhost 或 IP 访问管理后台")
		return
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	}
	x.mux.ServeHTTP(w, r)
}

// clientAddr is the requester's address: when the request comes through a reverse proxy listed in
// client_ip.trusted_proxies, the address the proxy forwards is used.
func (x *API) clientAddr(r *http.Request) netip.Addr {
	if c, _ := x.app.Runtime().IPs.ClientIP(r); c.IsValid() {
		return c
	}
	return clientip.Remote(r)
}

func (x *API) authenticate(w http.ResponseWriter, r *http.Request) (ops.Actor, string, bool) {
	ip := x.clientAddr(r)
	if ok, wait := x.app.Limiter.AuthAllowed(ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+0.999)))
		writeErr(w, http.StatusTooManyRequests, "认证失败次数太多，稍后再试")
		return ops.Actor{}, "", false
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		if tok, ok := strings.CutPrefix(auth, "Bearer "); ok && tok != "" {
			if t, found := x.app.Runtime().Tokens[sha256.Sum256([]byte(tok))]; found {
				return ops.Actor{Kind: "token", Name: t.Name, IP: ip}, t.Role, true
			}
		}
		x.app.Limiter.AuthFailed(ip)
	} else if x.opt.Session != nil {
		who, role, ok, err := x.opt.Session(r)
		if err != nil {
			writeErr(w, http.StatusForbidden, err.Error())
			return ops.Actor{}, "", false
		}
		if ok {
			return who, role, true
		}
	}
	w.Header().Set("Www-Authenticate", `Bearer realm="registry-mirrors-relay admin"`)
	writeErr(w, http.StatusUnauthorized, "需要有效的 API 令牌")
	return ops.Actor{}, "", false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeOpsErr writes an error returned by ops: an *ops.Error carries its own status code, other errors
// are 500.
func writeOpsErr(w http.ResponseWriter, err error) {
	writeErr(w, ops.Status(err), err.Error())
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "请求体要用 application/json")
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不对："+err.Error())
		return false
	}
	return true
}

// done writes the successful result when err is nil, and the error otherwise.
func done(w http.ResponseWriter, err error, status int, v any) {
	switch {
	case err != nil:
		writeOpsErr(w, err)
	case status == http.StatusNoContent:
		w.WriteHeader(status)
	default:
		writeJSON(w, status, v)
	}
}

// ---- Status and statistics

// statusReport is the same service status as the usage page's /status.json: the overall status, each
// upstream's availability over the last 24 hours, and this site's request success rate. It is available
// here even when the usage page has the service status turned off (site.show_status: false).
func (x *API) statusReport(w http.ResponseWriter, _ *http.Request, _ ops.Actor) {
	writeJSON(w, http.StatusOK, x.app.StatusReport())
}

func (x *API) status(w http.ResponseWriter, _ *http.Request, _ ops.Actor) {
	rt := x.app.Runtime()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	ups := map[string]bool{}
	for _, n := range rt.Pool.Names() {
		ups[n] = rt.Pool.Get(n).BreakerOpen()
	}
	var memLimit any
	if l := debug.SetMemoryLimit(-1); l < 1<<62 {
		memLimit = l
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":            version.Version,
		"go":                 runtime.Version(),
		"started":            x.app.Started,
		"runtime_built":      rt.Built,
		"last_reload":        x.app.LastReload(),
		"store_writable":     x.app.Store.Writable(),
		"store_driver":       rt.Config.Admin.Store.Driver,
		"manifest_cache":     rt.Manifests.Stats(),
		"limiter":            x.app.Limiter.Stats(),
		"breaker_open":       ups,
		"upstream_health":    x.app.Health(),
		"accesslog_dropped":  x.app.AccessLogDropped(),
		"goroutines":         runtime.NumGoroutine(),
		"heap_inuse_bytes":   ms.HeapInuse,
		"memory_limit_bytes": memLimit,
	})
}

func intParam(r *http.Request, name string, def, lo, hi int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	return min(max(v, lo), hi)
}

func (x *API) series(w http.ResponseWriter, r *http.Request, _ ops.Actor) {
	m := intParam(r, "minutes", 60, 1, 1440)
	writeJSON(w, http.StatusOK, x.app.Stats.Series(time.Now().Add(-time.Duration(m-1)*time.Minute)))
}

func (x *API) summary(w http.ResponseWriter, r *http.Request, _ ops.Actor) {
	h := intParam(r, "hours", 24, 1, 90*24)
	n := intParam(r, "n", 20, 1, 200)
	writeJSON(w, http.StatusOK, x.app.Stats.Summarize(time.Duration(h)*time.Hour, n))
}

func (x *API) requests(w http.ResponseWriter, r *http.Request, _ ops.Actor) {
	q := r.URL.Query()
	f := stats.Filter{ClientIP: q.Get("client_ip"), Upstream: q.Get("upstream"), Image: q.Get("image"), Query: q.Get("q")}
	f.Status, _ = strconv.Atoi(q.Get("status"))
	writeJSON(w, http.StatusOK, x.app.Stats.Recent(f, intParam(r, "limit", 100, 1, 5000)))
}

// ---- Upstreams

func (x *API) upstreams(w http.ResponseWriter, _ *http.Request, _ ops.Actor) {
	writeJSON(w, http.StatusOK, x.ops.Upstreams())
}

func (x *API) checkUpstream(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	h, err := x.ops.CheckUpstream(r.Context(), who, r.PathValue("name"))
	done(w, err, http.StatusOK, h)
}

func (x *API) addRedirectHost(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	var body struct {
		Host string `json:"host"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	name := r.PathValue("name")
	host, err := x.ops.AddRedirectHost(who, name, body.Host)
	done(w, err, http.StatusCreated, map[string]string{"upstream": name, "host": host})
}

func (x *API) delRedirectHost(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	done(w, x.ops.DeleteRedirectHost(who, r.PathValue("name"), r.PathValue("host")), http.StatusNoContent, nil)
}

// ---- Client accounts

func (x *API) listUsers(w http.ResponseWriter, _ *http.Request, _ ops.Actor) {
	writeJSON(w, http.StatusOK, x.ops.Users())
}

func (x *API) createUser(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	var in ops.UserInput
	if !readJSON(w, r, &in) {
		return
	}
	done(w, x.ops.CreateUser(who, in), http.StatusCreated, map[string]string{"name": in.Name})
}

func (x *API) patchUser(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	var in ops.UserInput
	if !readJSON(w, r, &in) {
		return
	}
	name := r.PathValue("name")
	done(w, x.ops.UpdateUser(who, name, in), http.StatusOK, map[string]string{"name": name})
}

func (x *API) deleteUser(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	done(w, x.ops.DeleteUser(who, r.PathValue("name")), http.StatusNoContent, nil)
}

// ---- Rules and bans

func (x *API) listRules(w http.ResponseWriter, _ *http.Request, _ ops.Actor) {
	writeJSON(w, http.StatusOK, x.ops.Rules())
}

func (x *API) addIPRule(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	var b struct {
		CIDR string `json:"cidr"`
		Kind string `json:"kind"`
		Note string `json:"note"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	cidr, err := x.ops.AddIPRule(who, b.CIDR, b.Kind, b.Note)
	done(w, err, http.StatusCreated, map[string]string{"cidr": cidr, "kind": b.Kind})
}

func (x *API) delIPRule(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	q := r.URL.Query()
	done(w, x.ops.DeleteIPRule(who, q.Get("cidr"), q.Get("kind")), http.StatusNoContent, nil)
}

func (x *API) addBan(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	var b struct {
		CIDR     string `json:"cidr"`
		Duration string `json:"duration"`
		Reason   string `json:"reason"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	cidr, until, err := x.ops.AddBan(who, b.CIDR, b.Duration, b.Reason)
	done(w, err, http.StatusCreated, map[string]any{"cidr": cidr, "until": until})
}

func (x *API) delBan(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	done(w, x.ops.DeleteBan(who, r.URL.Query().Get("cidr")), http.StatusNoContent, nil)
}

func (x *API) addImageRule(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	var in ops.ImageRuleInput
	if !readJSON(w, r, &in) {
		return
	}
	p, err := x.ops.AddImageRule(who, in)
	done(w, err, http.StatusCreated, map[string]string{"pattern": p, "kind": in.Kind})
}

func (x *API) delImageRule(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	q := r.URL.Query()
	done(w, x.ops.DeleteImageRule(who, q.Get("pattern"), q.Get("kind")), http.StatusNoContent, nil)
}

func (x *API) testImage(w http.ResponseWriter, r *http.Request, _ ops.Actor) {
	var b struct {
		Image string `json:"image"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	writeJSON(w, http.StatusOK, x.ops.TestImage(b.Image))
}

// ---- Tokens and administrators

func (x *API) listTokens(w http.ResponseWriter, _ *http.Request, _ ops.Actor) {
	writeJSON(w, http.StatusOK, x.ops.Tokens())
}

func (x *API) createToken(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	var b struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	tok, err := x.ops.CreateToken(who, b.Name, b.Role)
	done(w, err, http.StatusCreated, map[string]string{"name": b.Name, "role": b.Role, "token": tok,
		"note": "令牌只显示这一次，请妥善保存"})
}

func (x *API) deleteToken(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	done(w, x.ops.DeleteToken(who, r.PathValue("name")), http.StatusNoContent, nil)
}

func (x *API) listAdmins(w http.ResponseWriter, _ *http.Request, _ ops.Actor) {
	writeJSON(w, http.StatusOK, x.ops.Admins())
}

func (x *API) createAdmin(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	var b struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	done(w, x.ops.CreateAdmin(who, b.Name, b.Password, b.Role), http.StatusCreated, map[string]string{"name": b.Name, "role": b.Role})
}

func (x *API) patchAdmin(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	var in ops.AdminInput
	if !readJSON(w, r, &in) {
		return
	}
	name := r.PathValue("name")
	done(w, x.ops.UpdateAdmin(who, name, in), http.StatusOK, map[string]string{"name": name})
}

func (x *API) deleteAdmin(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	done(w, x.ops.DeleteAdmin(who, r.PathValue("name")), http.StatusNoContent, nil)
}

// ---- Site, audit log, configuration, reload

func (x *API) site(w http.ResponseWriter, _ *http.Request, _ ops.Actor) {
	writeJSON(w, http.StatusOK, x.ops.Site())
}

func (x *API) setSite(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	var in ops.SiteInput
	if !readJSON(w, r, &in) {
		return
	}
	if err := x.ops.SetSite(who, in); err != nil {
		writeOpsErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, x.ops.Site())
}

func (x *API) resetSite(w http.ResponseWriter, _ *http.Request, who ops.Actor) {
	if err := x.ops.ResetSite(who); err != nil {
		writeOpsErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, x.ops.Site())
}

func (x *API) audit(w http.ResponseWriter, r *http.Request, _ ops.Actor) {
	writeJSON(w, http.StatusOK, x.app.Store.AuditLog(intParam(r, "limit", 100, 1, 1000)))
}

// config is the effective configuration (with the console overrides applied); ?view=file is the
// configuration file alone.
func (x *API) config(w http.ResponseWriter, r *http.Request, _ ops.Actor) {
	b, err := x.ops.ConfigText(r.URL.Query().Get("view"))
	done(w, err, http.StatusOK, map[string]string{"yaml": string(b)})
}

func (x *API) listOverrides(w http.ResponseWriter, _ *http.Request, _ ops.Actor) {
	writeJSON(w, http.StatusOK, map[string]any{"editable": x.ops.ConfigEditable(), "overrides": x.ops.Overrides()})
}

// setOverride takes the section's JSON as the request body: limits, cache, proxy, site and
// upstreams/<name> are objects, entrances is an array.
func (x *API) setOverride(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "请求体要用 application/json")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || !json.Valid(body) {
		writeErr(w, http.StatusBadRequest, "请求体不是合法的 JSON")
		return
	}
	key := r.PathValue("key")
	warnings, err := x.ops.SetConfig(who, key, string(body))
	if err != nil {
		writeOpsErr(w, err)
		return
	}
	// When everything matches the configuration file the override is removed and override is null
	var cur *ops.OverrideView
	for _, o := range x.ops.Overrides() {
		if o.Key == key {
			cur = &o
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"override": cur, "warnings": append([]string{}, warnings...)})
}

func (x *API) resetOverride(w http.ResponseWriter, r *http.Request, who ops.Actor) {
	done(w, x.ops.ResetConfig(who, r.PathValue("key")), http.StatusNoContent, nil)
}

// exportConfig exports the console overrides in effect: ?format=overrides (the default, only the changed
// parts) or merged (merged into the configuration file).
func (x *API) exportConfig(w http.ResponseWriter, r *http.Request, _ ops.Actor) {
	b, err := x.ops.ExportConfig(r.URL.Query().Get("format"))
	done(w, err, http.StatusOK, map[string]string{"yaml": string(b)})
}

func (x *API) reload(w http.ResponseWriter, _ *http.Request, who ops.Actor) {
	st, err := x.ops.Reload(who)
	done(w, err, http.StatusOK, st)
}
