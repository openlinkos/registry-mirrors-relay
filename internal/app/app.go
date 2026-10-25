// Package app assembles the configuration and the operational data into one immutable Runtime and
// replaces it as a whole when the configuration file or the operational data changes (hot reload).
// Console overrides are stored in the operational database and layered on top of the configuration
// file; see overrides.go.
//
// Replacing keeps as much state as possible: when the upstream and proxy settings are unchanged the old
// connection pool is reused (and with it the token cache), and when the cache settings are unchanged the
// manifest cache is reused; rate limit counters, the password check cache and the statistics are always
// kept. Changes such as the listen addresses, TLS or how the operational data is stored take effect only
// after a restart, which is noted in the log.
package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/openlinkos/registry-mirrors-relay/internal/access"
	"github.com/openlinkos/registry-mirrors-relay/internal/accesslog"
	"github.com/openlinkos/registry-mirrors-relay/internal/clientip"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/i18n"
	"github.com/openlinkos/registry-mirrors-relay/internal/limit"
	"github.com/openlinkos/registry-mirrors-relay/internal/manifest"
	"github.com/openlinkos/registry-mirrors-relay/internal/metrics"
	"github.com/openlinkos/registry-mirrors-relay/internal/route"
	"github.com/openlinkos/registry-mirrors-relay/internal/stats"
	"github.com/openlinkos/registry-mirrors-relay/internal/status"
	"github.com/openlinkos/registry-mirrors-relay/internal/store"
	"github.com/openlinkos/registry-mirrors-relay/internal/upstream"
	"github.com/openlinkos/registry-mirrors-relay/internal/version"
	"github.com/openlinkos/registry-mirrors-relay/internal/web"
)

// Token is the identity of a management API token.
type Token struct {
	Name   string
	Role   string // admin | viewer
	Source string // config | store
}

// Runtime is all the settings in effect at one moment. It is immutable: a request takes it once at the
// start and uses it throughout.
type Runtime struct {
	Config    *config.Config
	Router    *route.Router
	IPs       *clientip.Resolver
	Web       *web.Handler
	Users     map[string]*access.User
	IPRules   *access.IPRules
	Images    *access.ImageRules
	Pool      *upstream.Pool
	Manifests *manifest.Cache
	Tokens    map[[32]byte]Token // SHA-256(token) → identity
	Data      store.Data         // a snapshot of the operational data (for the API listings)
	Site      config.Site        // the effective site information: the console's if set, else the configuration file's
	Mgmt      MgmtPolicy         // who may reach the management entrance (console, API, metrics)
	Built     time.Time

	// FileConfig is the configuration parsed from the configuration file alone (without console
	// overrides); Config is the effective configuration with the console overrides layered on top
	FileConfig *config.Config
	Overrides  []OverrideState // the console overrides and their states

	poolKey, cacheKey string
}

// MgmtPolicy sets who may reach the management entrance; the mgmt package uses it.
type MgmtPolicy struct {
	Public       bool           // the console and API are open to every address (admin.public)
	Allow        []netip.Prefix // peers allowed on a direct connection (admin.allow)
	ProxiedAllow []netip.Prefix // clients allowed when proxied (admin.proxied_allow)
	PublicHost   string         // the Host of admin.public_url, the only one accepted when proxied
}

// Options is mainly for tests.
type Options struct {
	ConfigPath string // empty means defaults only, with no file to watch
	Upstream   upstream.Options
	Log        *slog.Logger
	Store      store.Store // opened as configured when nil
	// Hook is called as each access log entry is written (tests use it to check the entries)
	Hook func(*accesslog.Entry)
	// GenerateAdmin creates an administrator with a generated password, logged once, when the writable
	// operational database has none and the configuration names no initial_admin (serve sets it).
	GenerateAdmin bool
}

// App holds the state that survives reloads.
type App struct {
	log       *slog.Logger
	opt       Options
	Store     store.Store
	Stats     *stats.Collector
	Metrics   *metrics.Metrics
	Limiter   *limit.Limiter
	Auth      *access.Authenticator
	accessLog *accesslog.Logger
	Started   time.Time

	rt      atomic.Pointer[Runtime]
	mu      sync.Mutex // serialises reloads and console overrides; also guards the three fields below
	cfgHash [32]byte
	fileRaw []byte // the content of the configuration file, which console overrides are layered on
	// warnings are the warnings for the effective configuration; saving an override in the console
	// reports only the new ones
	warnings []i18n.Message
	reload   atomic.Pointer[ReloadStatus]

	healthMu sync.Mutex
	health   map[string]upstream.Health
	probes   *status.History // hourly probe results, for the service status on the usage page
}

// ReloadStatus is the result of the last reload.
type ReloadStatus struct {
	Time    time.Time
	Err     string // English; Cause is the error itself, for showing it in the reader's language
	Cause   error
	Restart []string // changes that take effect only after a restart
}

// New assembles the App. res is the validated parse result of the configuration file raw; the console
// overrides from the operational database are layered on here.
func New(res *config.Result, raw []byte, opt Options) (*App, error) {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	cfg := res.Config
	a := &App{log: opt.Log, opt: opt, Started: time.Now(), cfgHash: sha256.Sum256(raw), probes: status.NewHistory(),
		fileRaw: raw}
	a.Store = opt.Store
	if a.Store == nil {
		switch cfg.Admin.Store.Driver {
		case "bolt":
			s, err := store.OpenBolt(cfg.Admin.Store.Path)
			if err != nil {
				return nil, err
			}
			a.Store = s
		default:
			a.Store = store.NewReadOnly()
		}
	}
	if err := a.bootstrapAdmin(cfg); err != nil {
		return nil, err
	}
	al, err := accesslog.New(cfg.Log.Access)
	if err != nil {
		return nil, fmt.Errorf("access log: %w", err)
	}
	a.accessLog = al
	a.Stats = stats.New(a.Store)
	a.Metrics = metrics.New(version.Version)
	a.Limiter = limit.New(limitConfig(cfg))
	a.Auth = access.NewAuthenticator()
	rt, eff, err := a.assemble(raw, res, nil)
	if err != nil {
		_ = a.Store.Close()
		return nil, err
	}
	for _, w := range eff.Warnings {
		if !hasMessage(res.Warnings, w) {
			a.log.Warn(w.String())
		}
	}
	a.noteOverrides(nil, rt.Overrides)
	a.commit(rt, nil)
	a.warnings = eff.Warnings
	a.registerMetrics()
	a.registerOverrideMetrics()
	return a, nil
}

// Runtime returns the settings currently in effect.
func (a *App) Runtime() *Runtime { return a.rt.Load() }

// ConfigPath is the path of the configuration file; it is empty when only defaults are used.
func (a *App) ConfigPath() string { return a.opt.ConfigPath }

// ConfigFile returns the current content of the configuration file (as last loaded successfully); it
// is empty when only defaults are used.
func (a *App) ConfigFile() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fileRaw
}

// LastReload returns the result of the last reload, or nil if there has been none.
func (a *App) LastReload() *ReloadStatus { return a.reload.Load() }

// Log hands an access log entry to the access log, the statistics and the metrics.
func (a *App) Log(e *accesslog.Entry) {
	a.accessLog.Log(e)
	a.Stats.Log(e)
	a.Metrics.Log(e)
	if a.opt.Hook != nil {
		a.opt.Hook(e)
	}
}

// AccessLogDropped is the number of access log entries dropped because the queue was full.
func (a *App) AccessLogDropped() uint64 { return a.accessLog.Dropped() }

// bootstrapAdmin creates the first administrator when the operational database has no administrators
// yet and the configuration sets initial_admin.
func (a *App) bootstrapAdmin(cfg *config.Config) error {
	ia := cfg.Admin.InitialAdmin
	if !a.Store.Writable() {
		return nil
	}
	if ia.Username == "" {
		return a.generateAdmin()
	}
	if len(a.Store.Data().Admins) > 0 {
		if ia.FromEnv {
			a.log.Warn("the password file named by RELAY_ADMIN_PASSWORD_FILE is still present, but the operational database already has administrators, so it is no longer used; delete the file (in k8s, the matching Secret)")
		} else {
			a.log.Warn("the configuration still sets admin.initial_admin, but the operational database already has administrators, so it is no longer used; remove it and its password file or environment variable")
		}
		return nil
	}
	h, err := bcrypt.GenerateFromPassword([]byte(ia.Password.Reveal()), 12)
	if err != nil {
		return err
	}
	err = a.Store.Update(func(d *store.Data) error {
		d.Admins = append(d.Admins, store.Admin{Name: ia.Username, Hash: string(h), Role: "admin", MustChange: true, Created: time.Now().UTC()})
		return nil
	})
	if err == nil {
		if ia.FromEnv {
			a.log.Info("created the first administrator from the password file named by RELAY_ADMIN_PASSWORD_FILE; the password must be changed at the first login, after which delete the file (in k8s, the matching Secret)", "username", ia.Username)
		} else {
			a.log.Info("created the first administrator from admin.initial_admin; the password must be changed at the first login, after which remove initial_admin from the configuration", "username", ia.Username)
		}
		_ = a.Store.Audit(store.AuditEntry{Actor: "system", Action: "admin.bootstrap", Target: ia.Username})
	}
	return err
}

func limitConfig(cfg *config.Config) limit.Config {
	l := cfg.Limits
	exempt, _ := config.ParsePrefixes(l.ExemptCIDRs)
	return limit.Config{
		ManifestRate: l.ManifestRate, ManifestBurst: l.ManifestBurst, HeadWeight: l.HeadWeight,
		BlobStreamsPerClient: l.BlobStreamsPerClient, MaxInflight: l.MaxInflight, MaxBlobStreams: l.MaxBlobStreams,
		TrackedClients: l.TrackedClients, AuthFailuresPerMin: l.AuthFailuresPerMin, Exempt: exempt,
	}
}

func fingerprint(parts ...any) string {
	h := sha256.New()
	for _, p := range parts {
		b, _ := json.Marshal(p)
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// build generates a Runtime from the configuration and the current operational data without putting it
// into effect: use commit to apply it, or discard when it is not used. When prev is not nil, its
// connection pool and caches are reused where possible.
func (a *App) build(cfg *config.Config, prev *Runtime) (_ *Runtime, err error) {
	data := a.Store.Data()
	now := time.Now()
	rt := &Runtime{Config: cfg, Data: data, Built: now}
	defer func() {
		if err != nil && rt.Pool != nil {
			a.discard(rt, prev)
		}
	}()

	// Client accounts: those from the configuration file win; same-named ones in the operational database are ignored
	rt.Users = map[string]*access.User{}
	if cfg.Access.UsersFile != "" {
		f, err := os.Open(cfg.Access.UsersFile) //nolint:gosec // the path is set by the operator in the configuration
		if err != nil {
			return nil, i18n.Errorf("access.users_file: %w", err)
		}
		users, err := access.ParseHtpasswd(f)
		_ = f.Close()
		if err != nil {
			return nil, i18n.Errorf("access.users_file: %w", err)
		}
		for i := range users {
			rt.Users[users[i].Name] = &users[i]
		}
	}
	for _, su := range data.Users {
		if _, dup := rt.Users[su.Name]; dup {
			a.log.Warn("client account in the operational database has the same name as one in users_file; using the configuration file", "user", su.Name)
			continue
		}
		rt.Users[su.Name] = &access.User{Name: su.Name, Hash: []byte(su.Hash), Disabled: su.Disabled, Source: "store",
			ManifestRPS: su.ManifestRate, BlobStreams: su.BlobStreams}
	}

	// IP rules and temporary bans
	allow, err := config.ParsePrefixes(cfg.Access.IPAllow)
	if err != nil {
		return nil, err
	}
	deny, err := config.ParsePrefixes(cfg.Access.IPDeny)
	if err != nil {
		return nil, err
	}
	for _, r := range data.IPRules {
		p, err := config.ParsePrefixes([]string{r.CIDR})
		if err != nil {
			a.log.Warn("ignoring malformed IP rule in the operational database", "cidr", r.CIDR)
			continue
		}
		if r.Kind == "allow" {
			allow = append(allow, p...)
		} else {
			deny = append(deny, p...)
		}
	}
	var bans []access.Ban
	for _, b := range data.Bans {
		if !now.Before(b.Until) {
			continue
		}
		p, err := netip.ParsePrefix(b.CIDR)
		if err != nil {
			continue
		}
		bans = append(bans, access.Ban{Prefix: p.Masked(), Until: b.Until, Reason: b.Reason})
	}
	rt.IPRules = access.NewIPRules(allow, deny, bans)

	// Image rules
	imgAllow := append([]string(nil), cfg.Access.Images.Allow...)
	var imgDeny []access.ImageRule
	for _, d := range cfg.Access.Images.Deny {
		imgDeny = append(imgDeny, access.ImageRule{Pattern: d.Pattern, Status: d.Status, Message: d.Message})
	}
	for _, r := range data.ImageRules {
		if r.Kind == "allow" {
			imgAllow = append(imgAllow, r.Pattern)
		} else {
			imgDeny = append(imgDeny, access.ImageRule{Pattern: r.Pattern, Status: r.Status, Message: r.Message})
		}
	}
	if rt.Images, err = access.NewImageRules(imgAllow, imgDeny); err != nil {
		return nil, err
	}

	// Upstreams: redirect hosts added in the operational database (exact host names) are appended to the allowlist
	ups := map[string]*config.Upstream{}
	var credHash []string
	for name, u := range cfg.Upstreams {
		c := *u
		c.ExtraRedirectHosts = append([]string(nil), u.ExtraRedirectHosts...)
		for _, h := range data.ExtraRedirectHosts[name] {
			c.ExtraRedirectHosts = append(c.ExtraRedirectHosts, regexp.QuoteMeta(strings.ToLower(h)))
		}
		ups[name] = &c
		if u.Credentials != nil {
			s := sha256.Sum256([]byte(u.Credentials.Password.Reveal()))
			credHash = append(credHash, name+":"+hex.EncodeToString(s[:]))
		}
	}
	ucfg := *cfg
	ucfg.Upstreams = ups
	rt.poolKey = fingerprint(ups, credHash, cfg.Proxy, cfg.Timeouts)
	if prev != nil && prev.poolKey == rt.poolKey {
		rt.Pool = prev.Pool
	} else if rt.Pool, err = upstream.NewPool(&ucfg, a.withLog(a.opt.Upstream)); err != nil {
		return nil, err
	}
	rt.cacheKey = fingerprint(cfg.Cache, cfg.Limits.MaxManifestBuffers, cfg.Timeouts.ResponseHeader)
	if prev != nil && prev.cacheKey == rt.cacheKey {
		rt.Manifests = prev.Manifests
	} else {
		rt.Manifests = manifest.New(cfg.Cache, cfg.Limits.MaxManifestBuffers, 2*cfg.Timeouts.ResponseHeader.D())
	}

	// Management API tokens
	rt.Tokens = map[[32]byte]Token{}
	for _, t := range cfg.Admin.APITokens {
		rt.Tokens[sha256.Sum256([]byte(t.Token.Reveal()))] = Token{Name: t.Name, Role: t.Role, Source: "config"}
	}
	for _, t := range data.APITokens {
		b, err := hex.DecodeString(t.Hash)
		if err != nil || len(b) != 32 {
			continue
		}
		var k [32]byte
		copy(k[:], b)
		rt.Tokens[k] = Token{Name: t.Name, Role: t.Role, Source: "store"}
	}

	trusted, err := config.ParsePrefixes(cfg.ClientIP.TrustedProxies)
	if err != nil {
		return nil, err
	}
	rt.IPs = clientip.New(trusted, cfg.ClientIP.UseXRealIP)
	rt.Mgmt.Public = cfg.Admin.Public
	if rt.Mgmt.Allow, err = config.ParsePrefixes(cfg.Admin.Allow); err != nil {
		return nil, err
	}
	if rt.Mgmt.ProxiedAllow, err = config.ParsePrefixes(cfg.Admin.ProxiedAllow); err != nil {
		return nil, err
	}
	if u, err := url.Parse(cfg.Admin.PublicURL); err == nil && cfg.Admin.PublicURL != "" {
		rt.Mgmt.PublicHost = strings.ToLower(u.Host)
	}
	rt.Router = route.New(cfg)
	rt.Site = cfg.Site
	if s := data.Site; s != nil {
		rt.Site = config.Site{Name: s.Name, Operator: s.Operator, Links: s.Links, ShowStatus: cfg.Site.ShowStatus}
	}
	wcfg := *cfg
	wcfg.Site = rt.Site
	if rt.Web, err = web.New(&wcfg, func() any { return a.StatusReport() }); err != nil {
		return nil, err
	}
	if prev != nil {
		rt.FileConfig, rt.Overrides = prev.FileConfig, prev.Overrides
	}
	return rt, nil
}

// commit puts the rt generated by build into effect: the replaced connection pool is closed later
// (requests in progress keep using the old one), and the rate limit settings are switched along with it.
func (a *App) commit(rt, prev *Runtime) {
	if prev != nil && prev.Pool != rt.Pool {
		old := prev.Pool
		time.AfterFunc(2*time.Minute, old.Close)
	}
	a.Limiter.SetConfig(limitConfig(rt.Config))
	a.rt.Store(rt)
}

// discard drops an rt generated by build that will not be put into effect: any connection pool it
// created is closed.
func (a *App) discard(rt, prev *Runtime) {
	if prev == nil || prev.Pool != rt.Pool {
		rt.Pool.Close()
	}
}

func (a *App) withLog(o upstream.Options) upstream.Options {
	if o.Log == nil {
		o.Log = a.log
	}
	return o
}

// Rebuild regenerates the Runtime after the operational data changes (the API changed accounts, rules
// and so on).
func (a *App) Rebuild() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	prev := a.rt.Load()
	rt, err := a.build(prev.Config, prev)
	if err != nil {
		return err
	}
	a.commit(rt, prev)
	return nil
}

// ErrNoConfigFile means there is no configuration file to reload.
var ErrNoConfigFile = i18n.Errorf("there is no configuration file; only the defaults are in use")

// ReloadConfig rereads the configuration file and replaces the Runtime. When the configuration has errors
// it keeps the previous settings and returns the error. The console overrides are layered on again, and
// those that conflict with the configuration file are suspended (the configuration file is the baseline,
// as on restart).
func (a *App) ReloadConfig() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := &ReloadStatus{Time: time.Now()}
	defer a.reload.Store(st)
	if a.opt.ConfigPath == "" {
		st.Err, st.Cause = ErrNoConfigFile.Error(), ErrNoConfigFile
		return ErrNoConfigFile
	}
	raw, err := os.ReadFile(a.opt.ConfigPath)
	if err != nil {
		st.Err, st.Cause = err.Error(), err
		return err
	}
	res, err := config.Parse(raw)
	if err != nil {
		st.Err, st.Cause = err.Error(), err
		a.log.Error("configuration is invalid; keeping the previous settings", "err", err)
		return err
	}
	prev := a.rt.Load()
	rt, eff, err := a.assemble(raw, res, prev)
	if err != nil {
		st.Err, st.Cause = err.Error(), err
		a.log.Error("new configuration cannot be applied; keeping the previous settings", "err", err)
		return err
	}
	st.Restart = restartNeeded(prev.Config, eff.Config)
	for _, w := range eff.Warnings {
		a.log.Warn(w.String())
	}
	if len(st.Restart) > 0 {
		a.log.Warn("these changes take effect only after a restart", "fields", strings.Join(st.Restart, ", "))
	}
	a.noteOverrides(prev.Overrides, rt.Overrides)
	a.commit(rt, prev)
	a.cfgHash = sha256.Sum256(raw)
	a.fileRaw, a.warnings = raw, eff.Warnings
	a.log.Info("configuration reloaded", "path", a.opt.ConfigPath)
	return nil
}

// restartNeeded lists the changes that a hot reload cannot apply.
func restartNeeded(old, cur *config.Config) []string {
	var out []string
	check := func(name string, x, y any) {
		if fingerprint(x) != fingerprint(y) {
			out = append(out, name)
		}
	}
	check("listen", old.Listen, cur.Listen)
	// ACME host names are always allowed by the effective configuration (entrances can be changed in
	// the console), so no restart is needed
	ot, ct := old.TLS, cur.TLS
	ot.ACME.Hosts, ct.ACME.Hosts = nil, nil
	check("tls", ot, ct)
	check("data_dir", old.DataDir, cur.DataDir)
	check("admin.store", old.Admin.Store, cur.Admin.Store)
	check("log.access", old.Log.Access, cur.Log.Access)
	check("log.level", old.Log.Level, cur.Log.Level)
	check("timeouts.read_header", old.Timeouts.ReadHeader, cur.Timeouts.ReadHeader)
	check("timeouts.idle", old.Timeouts.Idle, cur.Timeouts.Idle)
	check("timeouts.shutdown_grace", old.Timeouts.ShutdownGrace, cur.Timeouts.ShutdownGrace)
	check("proxy.buffer_size", old.Proxy.BufferSize, cur.Proxy.BufferSize)
	check("limits.tracked_clients", old.Limits.TrackedClients, cur.Limits.TrackedClients)
	return out
}

// Watch checks the configuration file every 10 seconds and reloads it when the content has changed (this
// also works for the symlink swap of a k8s ConfigMap).
func (a *App) Watch(ctx context.Context) {
	if a.opt.ConfigPath == "" {
		return
	}
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			raw, err := os.ReadFile(a.opt.ConfigPath)
			if err != nil {
				continue
			}
			a.mu.Lock()
			changed := sha256.Sum256(raw) != a.cfgHash
			a.mu.Unlock()
			if changed {
				_ = a.ReloadConfig()
			}
		}
	}
}

// Health returns the result of the last probe of each upstream.
func (a *App) Health() map[string]upstream.Health {
	a.healthMu.Lock()
	defer a.healthMu.Unlock()
	out := make(map[string]upstream.Health, len(a.health))
	for k, v := range a.health {
		out[k] = v
	}
	return out
}

// CheckUpstream probes an upstream immediately and records the result. ok is false when there is no such
// upstream.
func (a *App) CheckUpstream(ctx context.Context, name string) (h upstream.Health, ok bool) {
	u := a.Runtime().Pool.Get(name)
	if u == nil {
		return h, false
	}
	h = u.Probe(ctx)
	a.healthMu.Lock()
	if a.health == nil {
		a.health = map[string]upstream.Health{}
	}
	a.health[name] = h
	a.healthMu.Unlock()
	a.probes.Record(name, h.OK, h.Latency, h.Checked)
	return h, true
}

// ProbeHours is the hourly probe results of an upstream for the last 24 hours.
func (a *App) ProbeHours(name string) []status.Hour { return a.probes.Hours(name, time.Now()) }

// StatusReport is the content of the usage page's /status.json: the reachability of each upstream and its
// availability over the last 24 hours, plus the request success rate of this service.
func (a *App) StatusReport() status.Report {
	rt := a.Runtime()
	now := time.Now()
	in := status.Input{
		Version: version.Version, Started: a.Started, Now: now,
		Interval: rt.Config.Proxy.HealthInterval.D(), Names: rt.Pool.Names(),
		Probes: map[string]status.Probe{}, Open: map[string]bool{}, History: a.probes,
	}
	for name, h := range a.Health() {
		in.Probes[name] = status.Probe{OK: h.OK, Status: h.Status, Latency: h.Latency, Error: h.Error, Checked: h.Checked}
	}
	for _, name := range in.Names {
		if u := rt.Pool.Get(name); u != nil && u.BreakerOpen() {
			in.Open[name] = true
		}
	}
	in.Requests = requestHours(a.Stats.Series(now.Add(-status.Hours*time.Hour)), now)
	return status.Build(in)
}

// requestHours sums the per-minute counts by hour into status.Hours entries, oldest first; the last entry
// is the current hour.
func requestHours(minutes []stats.Minute, now time.Time) []status.RequestsHour {
	cur := now.Truncate(time.Hour).Unix()
	out := make([]status.RequestsHour, status.Hours)
	for i := range out {
		out[i].Start = cur - int64(status.Hours-1-i)*3600
	}
	for _, m := range minutes {
		i := status.Hours - 1 - int((cur-m.Start/3600*3600)/3600)
		if i < 0 || i >= status.Hours {
			continue
		}
		out[i].Requests += m.Requests
		out[i].Status5xx += m.Status5xx
	}
	return out
}

// WatchUpstreams probes all upstreams every proxy.health_interval, starting with one probe at start-up.
func (a *App) WatchUpstreams(ctx context.Context) {
	for {
		iv := a.Runtime().Config.Proxy.HealthInterval.D()
		if iv > 0 {
			var wg sync.WaitGroup
			for _, name := range a.Runtime().Pool.Names() {
				wg.Go(func() { a.CheckUpstream(ctx, name) })
			}
			wg.Wait()
		} else {
			iv = time.Minute // when turned off, still check every minute whether the setting has changed
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(iv):
		}
	}
}

// Close saves the statistics, finishes writing the access log, and closes the connections and the
// operational database.
func (a *App) Close() {
	a.Stats.Flush()
	if n := a.accessLog.Dropped(); n > 0 {
		a.log.Warn("access log entries were dropped while the queue was full", "count", n)
	}
	_ = a.accessLog.Close()
	if rt := a.rt.Load(); rt != nil {
		rt.Pool.Close()
	}
	_ = a.Store.Close()
}

func (a *App) registerMetrics() {
	m := a.Metrics
	m.Register("relay_manifest_cache_events_total", "Manifest cache results", "counter", func() []metrics.Sample {
		st := a.Runtime().Manifests.Stats()
		return []metrics.Sample{
			{Labels: metrics.Labels("result", "hit"), Value: float64(st.Hits)},
			{Labels: metrics.Labels("result", "miss"), Value: float64(st.Misses)},
			{Labels: metrics.Labels("result", "revalidated"), Value: float64(st.Revalidations)},
			{Labels: metrics.Labels("result", "stale"), Value: float64(st.Stale)},
			{Labels: metrics.Labels("result", "negative"), Value: float64(st.Negative)},
			{Labels: metrics.Labels("result", "bypass"), Value: float64(st.Bypass)},
		}
	})
	m.Register("relay_manifest_cache_bytes", "Bytes used by the manifest cache", "gauge", func() []metrics.Sample {
		return []metrics.Sample{{Value: float64(a.Runtime().Manifests.Stats().Bytes)}}
	})
	m.Register("relay_inflight_requests", "Requests in progress", "gauge", func() []metrics.Sample {
		return []metrics.Sample{{Value: float64(a.Limiter.Stats().Inflight)}}
	})
	m.Register("relay_blob_streams", "Layers (blobs) being relayed", "gauge", func() []metrics.Sample {
		return []metrics.Sample{{Value: float64(a.Limiter.Stats().BlobStreams)}}
	})
	m.Register("relay_rate_limited_total", "Requests refused by rate limits or concurrency caps", "counter", func() []metrics.Sample {
		return []metrics.Sample{{Value: float64(a.Limiter.Stats().Limited)}}
	})
	m.Register("relay_upstream_breaker_open", "Whether the upstream circuit breaker is open", "gauge", func() []metrics.Sample {
		var out []metrics.Sample
		for _, name := range a.Runtime().Pool.Names() {
			v := 0.0
			if a.Runtime().Pool.Get(name).BreakerOpen() {
				v = 1
			}
			out = append(out, metrics.Sample{Labels: metrics.Labels("upstream", name), Value: v})
		}
		return out
	})
	m.Register("relay_upstream_probe_up", "Whether the last upstream probe succeeded (/v2/ returned 200 or 401)", "gauge", func() []metrics.Sample {
		var out []metrics.Sample
		health := a.Health()
		for _, name := range a.Runtime().Pool.Names() {
			h, ok := health[name]
			if !ok {
				continue
			}
			v := 0.0
			if h.OK {
				v = 1
			}
			out = append(out, metrics.Sample{Labels: metrics.Labels("upstream", name), Value: v})
		}
		return out
	})
	m.Register("relay_upstream_probe_seconds", "Duration of the last upstream probe", "gauge", func() []metrics.Sample {
		var out []metrics.Sample
		health := a.Health()
		for _, name := range a.Runtime().Pool.Names() {
			if h, ok := health[name]; ok {
				out = append(out, metrics.Sample{Labels: metrics.Labels("upstream", name), Value: h.Latency.Seconds()})
			}
		}
		return out
	})
	m.Register("relay_upstream_quota_remaining", "Remaining pull quota reported by the upstream in response headers (Docker Hub's Ratelimit-Remaining)", "gauge", func() []metrics.Sample {
		var out []metrics.Sample
		for _, name := range a.Runtime().Pool.Names() {
			if q := a.Runtime().Pool.Get(name).Quota(); q != nil {
				out = append(out, metrics.Sample{Labels: metrics.Labels("upstream", name), Value: float64(q.Remaining)})
			}
		}
		return out
	})
	m.Register("relay_accesslog_dropped_total", "Access log entries dropped because the queue was full", "counter", func() []metrics.Sample {
		return []metrics.Sample{{Value: float64(a.accessLog.Dropped())}}
	})
}

// hasMessage reports whether list has a message that reads the same as m.
func hasMessage(list []i18n.Message, m i18n.Message) bool {
	return slices.ContainsFunc(list, func(x i18n.Message) bool { return x.String() == m.String() })
}

// generateAdmin creates the first administrator with a random password and logs the password once
// (Options.GenerateAdmin). The password must be changed at the first sign-in.
func (a *App) generateAdmin() error {
	if !a.opt.GenerateAdmin || len(a.Store.Data().Admins) > 0 {
		return nil
	}
	name := os.Getenv("RELAY_ADMIN_USERNAME")
	if !config.ValidAccountName(name) {
		name = "admin"
	}
	pw := rand.Text()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
	if err != nil {
		return err
	}
	if err := a.Store.Update(func(d *store.Data) error {
		if len(d.Admins) > 0 {
			return nil
		}
		d.Admins = append(d.Admins, store.Admin{Name: name, Hash: string(h), Role: "admin", MustChange: true, Created: time.Now().UTC()})
		return nil
	}); err != nil {
		return err
	}
	a.log.Warn("no administrator yet, so one was created with this generated password, shown only this once; "+
		"sign in with it and choose your own, or set one with: registry-mirrors-relay admin password",
		"username", name, "password", pw)
	_ = a.Store.Audit(store.AuditEntry{Actor: "system", Action: "admin.bootstrap", Target: name, Detail: "generated"})
	return nil
}
