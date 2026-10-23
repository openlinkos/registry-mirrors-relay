// Package api 是管理后台的 JSON API，路径都在 /admin/api/v1/ 下，后台网页和自动化脚本共用。
//
// 安全：
//   - 用 Authorization: Bearer <令牌> 认证（令牌来自配置的 admin.api_tokens 或在后台创建，只存 SHA-256）；
//   - viewer 只能读，写操作要 admin；所有写操作记审计；
//   - 没有配置 admin.public_url 时只接受用回环地址访问（Host 不对返回 421），防 DNS 重绑定；
//   - 认证失败按 IP 限次数；请求体最多 64 KiB，只接受 JSON。
package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/openlinkos/registry-mirrors-relay/internal/access"
	"github.com/openlinkos/registry-mirrors-relay/internal/app"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/stats"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
	"github.com/openlinkos/registry-mirrors-relay/internal/version"
)

// Prefix 是 API 的路径前缀。
const Prefix = "/admin/api/v1"

// API 处理 /admin/api/v1/ 下的请求。
type API struct {
	app *app.App
	mux *http.ServeMux
}

// New 创建 API。
func New(a *app.App) *API {
	x := &API{app: a, mux: http.NewServeMux()}
	r := func(pattern string, role string, h func(w http.ResponseWriter, r *http.Request, who identity)) {
		method, path, _ := strings.Cut(pattern, " ")
		x.mux.HandleFunc(method+" "+Prefix+path, func(w http.ResponseWriter, r *http.Request) {
			who, ok := x.authenticate(w, r)
			if !ok {
				return
			}
			if role == "admin" && who.Role != "admin" {
				writeErr(w, http.StatusForbidden, "只读令牌不能修改")
				return
			}
			h(w, r, who)
		})
	}
	r("GET /status", "viewer", x.status)
	r("GET /stats/series", "viewer", x.series)
	r("GET /stats/summary", "viewer", x.summary)
	r("GET /requests", "viewer", x.requests)
	r("GET /upstreams", "viewer", x.upstreams)
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
	r("GET /audit", "viewer", x.audit)
	r("GET /config", "viewer", x.config)
	r("POST /reload", "admin", x.reload)
	return x
}

type identity struct {
	Name, Role string
	IP         netip.Addr
}

func (who identity) actor() string { return "token:" + who.Name }

func (x *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	if !x.hostAllowed(r) {
		writeErr(w, http.StatusMisdirectedRequest, "请用 admin.public_url 或回环地址访问管理后台")
		return
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	}
	x.mux.ServeHTTP(w, r)
}

// hostAllowed：配置了 public_url 时 Host 必须和它一致，否则只接受回环地址。
func (x *API) hostAllowed(r *http.Request) bool {
	host := r.Host
	if pu := x.app.Runtime().Config.Admin.PublicURL; pu != "" {
		u, err := url.Parse(pu)
		return err == nil && strings.EqualFold(u.Host, host)
	}
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.IsLoopback()
}

func clientAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, _ := netip.ParseAddr(host)
	return a.Unmap()
}

func (x *API) authenticate(w http.ResponseWriter, r *http.Request) (identity, bool) {
	ip := clientAddr(r)
	if ok, wait := x.app.Limiter.AuthAllowed(ip); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds()+0.999)))
		writeErr(w, http.StatusTooManyRequests, "认证失败次数太多，稍后再试")
		return identity{}, false
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if ok && tok != "" {
		if t, found := x.app.Runtime().Tokens[sha256.Sum256([]byte(tok))]; found {
			return identity{Name: t.Name, Role: t.Role, IP: ip}, true
		}
		x.app.Limiter.AuthFailed(ip)
	}
	w.Header().Set("Www-Authenticate", `Bearer realm="registry-mirrors-relay admin"`)
	writeErr(w, http.StatusUnauthorized, "需要有效的 API 令牌")
	return identity{}, false
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

// change 修改运营数据、记审计、重新生成 Runtime。
func (x *API) change(w http.ResponseWriter, who identity, action, target, detail string, fn func(*store.Data) error) bool {
	err := x.app.Store.Update(fn)
	var ue *userError
	switch {
	case errors.Is(err, store.ErrReadOnly):
		writeErr(w, http.StatusConflict, err.Error())
		return false
	case errors.As(err, &ue):
		writeErr(w, ue.status, ue.msg)
		return false
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
		return false
	}
	_ = x.app.Store.Audit(store.AuditEntry{Actor: who.actor(), IP: who.IP.String(), Action: action, Target: target, Detail: detail})
	if err := x.app.Rebuild(); err != nil {
		writeErr(w, http.StatusInternalServerError, "已保存，但应用时出错："+err.Error())
		return false
	}
	return true
}

type userError struct {
	status int
	msg    string
}

func (e *userError) Error() string { return e.msg }

func bad(msg string) error      { return &userError{http.StatusBadRequest, msg} }
func notFound(msg string) error { return &userError{http.StatusNotFound, msg} }
func conflict(msg string) error { return &userError{http.StatusConflict, msg} }

// ---- 状态与统计

func (x *API) status(w http.ResponseWriter, _ *http.Request, _ identity) {
	rt := x.app.Runtime()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	ups := map[string]bool{}
	for _, n := range rt.Pool.Names() {
		ups[n] = rt.Pool.Get(n).BreakerOpen()
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
		"accesslog_dropped":  x.app.AccessLogDropped(),
		"goroutines":         runtime.NumGoroutine(),
		"heap_inuse_bytes":   ms.HeapInuse,
		"memory_limit_bytes": nil,
	})
}

func intParam(r *http.Request, name string, def, lo, hi int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	return min(max(v, lo), hi)
}

func (x *API) series(w http.ResponseWriter, r *http.Request, _ identity) {
	m := intParam(r, "minutes", 60, 1, 1440)
	writeJSON(w, http.StatusOK, x.app.Stats.Series(time.Now().Add(-time.Duration(m-1)*time.Minute)))
}

func (x *API) summary(w http.ResponseWriter, r *http.Request, _ identity) {
	h := intParam(r, "hours", 24, 1, 90*24)
	n := intParam(r, "n", 20, 1, 200)
	writeJSON(w, http.StatusOK, x.app.Stats.Summarize(time.Duration(h)*time.Hour, n))
}

func (x *API) requests(w http.ResponseWriter, r *http.Request, _ identity) {
	q := r.URL.Query()
	f := stats.Filter{ClientIP: q.Get("client_ip"), Upstream: q.Get("upstream"), Image: q.Get("image"), Query: q.Get("q")}
	f.Status, _ = strconv.Atoi(q.Get("status"))
	writeJSON(w, http.StatusOK, x.app.Stats.Recent(f, intParam(r, "limit", 100, 1, 5000)))
}

// ---- 上游

func (x *API) upstreams(w http.ResponseWriter, _ *http.Request, _ identity) {
	rt := x.app.Runtime()
	unlisted := x.app.Stats.Unlisted()
	type up struct {
		Name             string         `json:"name"`
		Registry         string         `json:"registry"`
		BreakerOpen      bool           `json:"breaker_open"`
		RedirectHosts    []string       `json:"redirect_hosts"`
		AddedHosts       []string       `json:"added_redirect_hosts"`
		UnlistedHosts    map[string]int `json:"unlisted_redirect_hosts"`
		CredentialsInUse bool           `json:"credentials"`
	}
	var out []up
	for _, n := range rt.Pool.Names() {
		c := rt.Config.Upstreams[n]
		hosts := append(append([]string{}, c.RedirectHosts...), c.ExtraRedirectHosts...)
		for _, h := range rt.Data.ExtraRedirectHosts[n] {
			hosts = append(hosts, regexp.QuoteMeta(h)) // 生效的完整白名单，后台加的主机按字面匹配
		}
		out = append(out, up{
			Name: n, Registry: c.Registry, BreakerOpen: rt.Pool.Get(n).BreakerOpen(),
			RedirectHosts: hosts,
			AddedHosts:    rt.Data.ExtraRedirectHosts[n], UnlistedHosts: unlisted[n], CredentialsInUse: c.Credentials != nil,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

var hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

func (x *API) addRedirectHost(w http.ResponseWriter, r *http.Request, who identity) {
	name := r.PathValue("name")
	var body struct {
		Host string `json:"host"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(body.Host)), ".")
	if _, ok := x.app.Runtime().Config.Upstreams[name]; !ok {
		writeErr(w, http.StatusNotFound, "没有这个上游")
		return
	}
	if !hostnameRe.MatchString(host) {
		writeErr(w, http.StatusBadRequest, "要写一个完整的主机名，例如 cdn99.quay.io（不支持通配，通配请改配置文件）")
		return
	}
	if x.change(w, who, "upstream.redirect_host.add", name, host, func(d *store.Data) error {
		if d.ExtraRedirectHosts == nil {
			d.ExtraRedirectHosts = map[string][]string{}
		}
		for _, h := range d.ExtraRedirectHosts[name] {
			if h == host {
				return conflict("已经在白名单里")
			}
		}
		d.ExtraRedirectHosts[name] = append(d.ExtraRedirectHosts[name], host)
		return nil
	}) {
		writeJSON(w, http.StatusCreated, map[string]string{"upstream": name, "host": host})
	}
}

func (x *API) delRedirectHost(w http.ResponseWriter, r *http.Request, who identity) {
	name, host := r.PathValue("name"), strings.ToLower(r.PathValue("host"))
	if x.change(w, who, "upstream.redirect_host.delete", name, host, func(d *store.Data) error {
		hs := d.ExtraRedirectHosts[name]
		for i, h := range hs {
			if h == host {
				d.ExtraRedirectHosts[name] = append(hs[:i], hs[i+1:]...)
				return nil
			}
		}
		return notFound("白名单里没有这个主机（配置文件里的要改配置文件）")
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---- 客户端账号

type userView struct {
	Name         string    `json:"name"`
	Source       string    `json:"source"`
	Disabled     bool      `json:"disabled"`
	ManifestRate float64   `json:"manifest_rate,omitempty"`
	BlobStreams  int       `json:"blob_streams,omitempty"`
	Note         string    `json:"note,omitempty"`
	Created      time.Time `json:"created,omitzero"`
}

func (x *API) listUsers(w http.ResponseWriter, _ *http.Request, _ identity) {
	rt := x.app.Runtime()
	notes := map[string]store.ClientUser{}
	for _, u := range rt.Data.Users {
		notes[u.Name] = u
	}
	var out []userView
	for _, u := range rt.Users {
		v := userView{Name: u.Name, Source: u.Source, Disabled: u.Disabled, ManifestRate: u.ManifestRPS, BlobStreams: u.BlobStreams}
		if su, ok := notes[u.Name]; ok && u.Source == "store" {
			v.Note, v.Created = su.Note, su.Created
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

var userNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type userBody struct {
	Name         string   `json:"name"`
	Password     *string  `json:"password,omitempty"`
	Hash         *string  `json:"hash,omitempty"`
	Disabled     *bool    `json:"disabled,omitempty"`
	ManifestRate *float64 `json:"manifest_rate,omitempty"`
	BlobStreams  *int     `json:"blob_streams,omitempty"`
	Note         *string  `json:"note,omitempty"`
}

// hashFrom 从口令或现成的 bcrypt 哈希得到要存的哈希。
func hashFrom(b userBody) (string, error) {
	switch {
	case b.Password != nil && b.Hash != nil:
		return "", bad("password 和 hash 只能给一个")
	case b.Password != nil:
		if n := len(*b.Password); n < 12 || n > 72 {
			return "", bad("口令要 12 到 72 个字节")
		}
		h, err := bcrypt.GenerateFromPassword([]byte(*b.Password), 12)
		return string(h), err
	case b.Hash != nil:
		if !access.ValidHash(*b.Hash) {
			return "", bad("hash 只接受 bcrypt")
		}
		return *b.Hash, nil
	}
	return "", nil
}

func applyUser(u *store.ClientUser, b userBody) error {
	if b.Disabled != nil {
		u.Disabled = *b.Disabled
	}
	if b.ManifestRate != nil {
		if *b.ManifestRate < 0 || *b.ManifestRate > 1000 {
			return bad("manifest_rate 应在 0 到 1000 之间")
		}
		u.ManifestRate = *b.ManifestRate
	}
	if b.BlobStreams != nil {
		if *b.BlobStreams < 0 || *b.BlobStreams > 10000 {
			return bad("blob_streams 应在 0 到 10000 之间")
		}
		u.BlobStreams = *b.BlobStreams
	}
	if b.Note != nil {
		if len(*b.Note) > 500 {
			return bad("note 太长")
		}
		u.Note = *b.Note
	}
	return nil
}

func (x *API) createUser(w http.ResponseWriter, r *http.Request, who identity) {
	var b userBody
	if !readJSON(w, r, &b) {
		return
	}
	if !userNameRe.MatchString(b.Name) {
		writeErr(w, http.StatusBadRequest, "用户名只能用字母、数字和 . _ -，最长 64 个字符")
		return
	}
	if u, ok := x.app.Runtime().Users[b.Name]; ok && u.Source == "config" {
		writeErr(w, http.StatusConflict, "配置文件的 users_file 里已有这个用户")
		return
	}
	h, err := hashFrom(b)
	if err == nil && h == "" {
		err = bad("要给 password 或 hash")
	}
	if err != nil {
		var ue *userError
		if errors.As(err, &ue) {
			writeErr(w, ue.status, ue.msg)
		} else {
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if x.change(w, who, "user.create", b.Name, "", func(d *store.Data) error {
		for _, u := range d.Users {
			if u.Name == b.Name {
				return conflict("用户已存在")
			}
		}
		u := store.ClientUser{Name: b.Name, Hash: h, Created: time.Now().UTC(), CreatedBy: who.actor()}
		if err := applyUser(&u, b); err != nil {
			return err
		}
		d.Users = append(d.Users, u)
		return nil
	}) {
		writeJSON(w, http.StatusCreated, map[string]string{"name": b.Name})
	}
}

func (x *API) patchUser(w http.ResponseWriter, r *http.Request, who identity) {
	name := r.PathValue("name")
	var b userBody
	if !readJSON(w, r, &b) {
		return
	}
	h, err := hashFrom(b)
	if err != nil {
		var ue *userError
		if errors.As(err, &ue) {
			writeErr(w, ue.status, ue.msg)
		} else {
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	detail := []string{}
	if h != "" {
		detail = append(detail, "password")
	}
	if b.Disabled != nil {
		detail = append(detail, "disabled="+strconv.FormatBool(*b.Disabled))
	}
	if x.change(w, who, "user.update", name, strings.Join(detail, ","), func(d *store.Data) error {
		for i := range d.Users {
			if d.Users[i].Name == name {
				if h != "" {
					d.Users[i].Hash = h
				}
				return applyUser(&d.Users[i], b)
			}
		}
		return notFound("没有这个用户（users_file 里的用户要改文件）")
	}) {
		writeJSON(w, http.StatusOK, map[string]string{"name": name})
	}
}

func (x *API) deleteUser(w http.ResponseWriter, r *http.Request, who identity) {
	name := r.PathValue("name")
	if x.change(w, who, "user.delete", name, "", func(d *store.Data) error {
		for i, u := range d.Users {
			if u.Name == name {
				d.Users = append(d.Users[:i], d.Users[i+1:]...)
				return nil
			}
		}
		return notFound("没有这个用户（users_file 里的用户要改文件）")
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---- 规则与封禁

func (x *API) listRules(w http.ResponseWriter, _ *http.Request, _ identity) {
	rt := x.app.Runtime()
	type ipRule struct {
		CIDR   string `json:"cidr"`
		Kind   string `json:"kind"`
		Source string `json:"source"`
		Note   string `json:"note,omitempty"`
	}
	type imgRule struct {
		Pattern string `json:"pattern"`
		Kind    string `json:"kind"`
		Status  int    `json:"status,omitempty"`
		Message string `json:"message,omitempty"`
		Source  string `json:"source"`
		Note    string `json:"note,omitempty"`
	}
	ips := []ipRule{}
	for _, c := range rt.Config.Access.IPAllow {
		ips = append(ips, ipRule{CIDR: c, Kind: "allow", Source: "config"})
	}
	for _, c := range rt.Config.Access.IPDeny {
		ips = append(ips, ipRule{CIDR: c, Kind: "deny", Source: "config"})
	}
	for _, r := range rt.Data.IPRules {
		ips = append(ips, ipRule{CIDR: r.CIDR, Kind: r.Kind, Source: "store", Note: r.Note})
	}
	imgs := []imgRule{}
	for _, p := range rt.Config.Access.Images.Allow {
		imgs = append(imgs, imgRule{Pattern: p, Kind: "allow", Source: "config"})
	}
	for _, d := range rt.Config.Access.Images.Deny {
		imgs = append(imgs, imgRule{Pattern: d.Pattern, Kind: "deny", Status: d.Status, Message: d.Message, Source: "config"})
	}
	for _, r := range rt.Data.ImageRules {
		imgs = append(imgs, imgRule{Pattern: r.Pattern, Kind: r.Kind, Status: r.Status, Message: r.Message, Source: "store", Note: r.Note})
	}
	bans := []store.Ban{}
	now := time.Now()
	for _, b := range rt.Data.Bans {
		if now.Before(b.Until) {
			bans = append(bans, b)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ip": ips, "bans": bans, "images": imgs})
}

func normCIDR(s string) (string, error) {
	ps, err := config.ParsePrefixes([]string{s})
	if err != nil || len(ps) != 1 {
		return "", bad("cidr 应写成 IP 或 CIDR，例如 198.51.100.7、198.51.100.0/24")
	}
	if ps[0].Bits() == 0 {
		return "", bad("不能整段放开或封掉所有地址")
	}
	return ps[0].String(), nil
}

func writeUserErr(w http.ResponseWriter, err error) {
	var ue *userError
	if errors.As(err, &ue) {
		writeErr(w, ue.status, ue.msg)
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}

func (x *API) addIPRule(w http.ResponseWriter, r *http.Request, who identity) {
	var b struct {
		CIDR string `json:"cidr"`
		Kind string `json:"kind"`
		Note string `json:"note"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	cidr, err := normCIDR(b.CIDR)
	if err == nil && b.Kind != "allow" && b.Kind != "deny" {
		err = bad("kind 只能是 allow 或 deny")
	}
	if err != nil {
		writeUserErr(w, err)
		return
	}
	if x.change(w, who, "rule.ip.add", cidr, b.Kind, func(d *store.Data) error {
		for _, ir := range d.IPRules {
			if ir.CIDR == cidr && ir.Kind == b.Kind {
				return conflict("规则已存在")
			}
		}
		d.IPRules = append(d.IPRules, store.IPRule{CIDR: cidr, Kind: b.Kind, Note: b.Note, Created: time.Now().UTC(), CreatedBy: who.actor()})
		return nil
	}) {
		writeJSON(w, http.StatusCreated, map[string]string{"cidr": cidr, "kind": b.Kind})
	}
}

func (x *API) delIPRule(w http.ResponseWriter, r *http.Request, who identity) {
	cidr, err := normCIDR(r.URL.Query().Get("cidr"))
	kind := r.URL.Query().Get("kind")
	if err != nil {
		writeUserErr(w, err)
		return
	}
	if x.change(w, who, "rule.ip.delete", cidr, kind, func(d *store.Data) error {
		for i, ir := range d.IPRules {
			if ir.CIDR == cidr && ir.Kind == kind {
				d.IPRules = append(d.IPRules[:i], d.IPRules[i+1:]...)
				return nil
			}
		}
		return notFound("没有这条规则（配置文件里的要改配置文件）")
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (x *API) addBan(w http.ResponseWriter, r *http.Request, who identity) {
	var b struct {
		CIDR     string `json:"cidr"`
		Duration string `json:"duration"`
		Reason   string `json:"reason"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	cidr, err := normCIDR(b.CIDR)
	if err != nil {
		writeUserErr(w, err)
		return
	}
	d, err := time.ParseDuration(b.Duration)
	if strings.HasSuffix(b.Duration, "d") {
		n, e := strconv.Atoi(strings.TrimSuffix(b.Duration, "d"))
		d, err = time.Duration(n)*24*time.Hour, e
	}
	if err != nil || d <= 0 || d > 365*24*time.Hour {
		writeErr(w, http.StatusBadRequest, "duration 应写成 30m、2h、7d 这样，最长 365 天")
		return
	}
	until := time.Now().Add(d).UTC()
	if x.change(w, who, "ban.add", cidr, b.Reason, func(data *store.Data) error {
		kept := data.Bans[:0]
		for _, bn := range data.Bans {
			if bn.CIDR != cidr && time.Now().Before(bn.Until) { // 同时清掉过期的
				kept = append(kept, bn)
			}
		}
		data.Bans = append(kept, store.Ban{CIDR: cidr, Until: until, Reason: b.Reason, Created: time.Now().UTC(), CreatedBy: who.actor()})
		return nil
	}) {
		writeJSON(w, http.StatusCreated, map[string]any{"cidr": cidr, "until": until})
	}
}

func (x *API) delBan(w http.ResponseWriter, r *http.Request, who identity) {
	cidr, err := normCIDR(r.URL.Query().Get("cidr"))
	if err != nil {
		writeUserErr(w, err)
		return
	}
	if x.change(w, who, "ban.delete", cidr, "", func(d *store.Data) error {
		for i, bn := range d.Bans {
			if bn.CIDR == cidr {
				d.Bans = append(d.Bans[:i], d.Bans[i+1:]...)
				return nil
			}
		}
		return notFound("没有这个封禁")
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (x *API) addImageRule(w http.ResponseWriter, r *http.Request, who identity) {
	var b struct {
		Pattern string `json:"pattern"`
		Kind    string `json:"kind"`
		Status  int    `json:"status"`
		Message string `json:"message"`
		Note    string `json:"note"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	b.Pattern = strings.TrimSpace(b.Pattern)
	if _, err := access.CompileImagePattern(b.Pattern); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if reg, _, _ := strings.Cut(b.Pattern, "/"); reg != "*" {
		if _, ok := x.app.Runtime().Config.Upstreams[strings.ToLower(reg)]; !ok {
			writeErr(w, http.StatusBadRequest, "第一段要写已配置的上游（或 *）")
			return
		}
	}
	switch b.Kind {
	case "allow":
		b.Status, b.Message = 0, ""
	case "deny":
		if b.Status == 0 {
			b.Status = 403
		}
		if b.Status != 403 && b.Status != 451 {
			writeErr(w, http.StatusBadRequest, "status 只能是 403 或 451")
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "kind 只能是 allow 或 deny")
		return
	}
	if x.change(w, who, "rule.image.add", b.Pattern, b.Kind, func(d *store.Data) error {
		for _, ir := range d.ImageRules {
			if ir.Pattern == b.Pattern && ir.Kind == b.Kind {
				return conflict("规则已存在")
			}
		}
		d.ImageRules = append(d.ImageRules, store.ImageRule{Pattern: b.Pattern, Kind: b.Kind, Status: b.Status, Message: b.Message,
			Note: b.Note, Created: time.Now().UTC(), CreatedBy: who.actor()})
		return nil
	}) {
		writeJSON(w, http.StatusCreated, map[string]string{"pattern": b.Pattern, "kind": b.Kind})
	}
}

func (x *API) delImageRule(w http.ResponseWriter, r *http.Request, who identity) {
	pattern, kind := r.URL.Query().Get("pattern"), r.URL.Query().Get("kind")
	if x.change(w, who, "rule.image.delete", pattern, kind, func(d *store.Data) error {
		for i, ir := range d.ImageRules {
			if ir.Pattern == pattern && ir.Kind == kind {
				d.ImageRules = append(d.ImageRules[:i], d.ImageRules[i+1:]...)
				return nil
			}
		}
		return notFound("没有这条规则（配置文件里的要改配置文件）")
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (x *API) testImage(w http.ResponseWriter, r *http.Request, _ identity) {
	var b struct {
		Image string `json:"image"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	name := strings.ToLower(strings.TrimSpace(b.Image))
	if i := strings.LastIndexAny(name, ":@"); i > strings.LastIndex(name, "/") {
		name = name[:i] // 去掉 tag 或 digest
	}
	rt := x.app.Runtime()
	first, rest, ok := strings.Cut(name, "/")
	up, known := rt.Router.Upstream(first)
	if !ok || !strings.ContainsAny(first, ".:") || !known {
		// 没写上游时按 Docker Hub 处理，和 docker pull 一样
		up, rest = config.DefaultUpstream, name
	}
	if u := rt.Config.Upstreams[up]; u != nil && u.LibraryPrefix && !strings.Contains(rest, "/") {
		rest = "library/" + rest
	}
	full := up + "/" + rest
	allowed, status, msg, rule := rt.Images.Decide(full)
	writeJSON(w, http.StatusOK, map[string]any{"image": full, "allowed": allowed, "status": status, "message": msg, "rule": rule})
}

// ---- 令牌

func (x *API) listTokens(w http.ResponseWriter, _ *http.Request, _ identity) {
	type tok struct {
		Name    string    `json:"name"`
		Role    string    `json:"role"`
		Source  string    `json:"source"`
		Created time.Time `json:"created,omitzero"`
	}
	rt := x.app.Runtime()
	out := []tok{}
	for _, t := range rt.Config.Admin.APITokens {
		out = append(out, tok{Name: t.Name, Role: t.Role, Source: "config"})
	}
	for _, t := range rt.Data.APITokens {
		out = append(out, tok{Name: t.Name, Role: t.Role, Source: "store", Created: t.Created})
	}
	writeJSON(w, http.StatusOK, out)
}

func (x *API) createToken(w http.ResponseWriter, r *http.Request, who identity) {
	var b struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if !readJSON(w, r, &b) {
		return
	}
	if !userNameRe.MatchString(b.Name) || (b.Role != "admin" && b.Role != "viewer") {
		writeErr(w, http.StatusBadRequest, "name 只能用字母、数字和 . _ -；role 只能是 admin 或 viewer")
		return
	}
	for _, t := range x.app.Runtime().Config.Admin.APITokens {
		if t.Name == b.Name {
			writeErr(w, http.StatusConflict, "配置文件里已有同名令牌")
			return
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	token := "rmr_" + hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	if x.change(w, who, "token.create", b.Name, b.Role, func(d *store.Data) error {
		for _, t := range d.APITokens {
			if t.Name == b.Name {
				return conflict("令牌已存在")
			}
		}
		d.APITokens = append(d.APITokens, store.APIToken{Name: b.Name, Hash: hex.EncodeToString(sum[:]), Role: b.Role,
			Created: time.Now().UTC(), CreatedBy: who.actor()})
		return nil
	}) {
		writeJSON(w, http.StatusCreated, map[string]string{"name": b.Name, "role": b.Role, "token": token,
			"note": "令牌只显示这一次，请妥善保存"})
	}
}

func (x *API) deleteToken(w http.ResponseWriter, r *http.Request, who identity) {
	name := r.PathValue("name")
	if x.change(w, who, "token.delete", name, "", func(d *store.Data) error {
		for i, t := range d.APITokens {
			if t.Name == name {
				d.APITokens = append(d.APITokens[:i], d.APITokens[i+1:]...)
				return nil
			}
		}
		return notFound("没有这个令牌（配置文件里的要改配置文件）")
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---- 审计、配置、重载

func (x *API) audit(w http.ResponseWriter, r *http.Request, _ identity) {
	writeJSON(w, http.StatusOK, x.app.Store.AuditLog(intParam(r, "limit", 100, 1, 1000)))
}

func (x *API) config(w http.ResponseWriter, _ *http.Request, _ identity) {
	b, err := x.app.Runtime().Config.Print()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"yaml": string(b)})
}

func (x *API) reload(w http.ResponseWriter, _ *http.Request, who identity) {
	err := x.app.ReloadConfig()
	detail := "ok"
	if err != nil {
		detail = err.Error()
	}
	_ = x.app.Store.Audit(store.AuditEntry{Actor: who.actor(), IP: who.IP.String(), Action: "config.reload", Detail: truncate(detail, 300)})
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, x.app.LastReload())
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
