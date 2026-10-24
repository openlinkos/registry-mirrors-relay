// Package app 把配置和运营数据组装成一份不可变的 Runtime，并在配置文件或运营数据变化时整体替换（热重载）。
// 后台对配置的修改存在运营数据库里，叠加在配置文件之上，见 overrides.go。
//
// 替换时尽量保留状态：上游和代理设置没变就沿用原来的连接池（令牌缓存随之保留），缓存设置没变就沿用 manifest 缓存；
// 限流计数、口令校验缓存、统计一直保留。监听地址、TLS、运营数据的存储方式这类改动要重启才能生效，会在日志里提示。
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// Token 是一个管理 API 令牌的身份。
type Token struct {
	Name   string
	Role   string // admin | viewer
	Source string // config | store
}

// Runtime 是某一时刻生效的全部设置。不可变，请求开始时取一次，整个请求都用它。
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
	Tokens    map[[32]byte]Token // SHA-256(令牌) → 身份
	Data      store.Data         // 运营数据的快照（API 列表用）
	Site      config.Site        // 生效的站点信息：后台设置过就用它，否则用配置文件的
	Mgmt      MgmtPolicy         // 管理入口（后台、API、指标）的访问范围
	Built     time.Time

	// FileConfig 是只按配置文件解析的配置（不含后台的修改），Config 是叠加了后台修改的、生效的配置
	FileConfig *config.Config
	Overrides  []OverrideState // 后台对配置的修改和它们的状态

	poolKey, cacheKey string
}

// MgmtPolicy 是管理入口的访问范围，由 mgmt 包使用。
type MgmtPolicy struct {
	Public       bool           // 后台和 API 对所有地址开放（admin.public）
	Allow        []netip.Prefix // 直连时允许的对端（admin.allow）
	ProxiedAllow []netip.Prefix // 经代理时允许的客户端（admin.proxied_allow）
	PublicHost   string         // admin.public_url 的 Host，经代理访问时只认它
}

// Options 主要给测试用。
type Options struct {
	ConfigPath string // 为空表示只用默认值，不监视文件
	Upstream   upstream.Options
	Log        *slog.Logger
	Store      store.Store // 为 nil 时按配置打开
	// Hook 在每条访问记录写出时调用（测试里用来检查记录的内容）
	Hook func(*accesslog.Entry)
}

// App 持有跨越重载的状态。
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
	mu      sync.Mutex // 串行化重载和后台对配置的修改；下面三项也由它保护
	cfgHash [32]byte
	fileRaw []byte // 配置文件的内容，后台修改配置时叠加在它上面
	// warnings 是生效的配置的提醒，后台保存修改时只报新增的
	warnings []string
	reload   atomic.Pointer[ReloadStatus]

	healthMu sync.Mutex
	health   map[string]upstream.Health
	probes   *status.History // 每小时的探测结果，给用法页的服务状态
}

// ReloadStatus 是最近一次重载的结果。
type ReloadStatus struct {
	Time    time.Time
	Err     string
	Restart []string // 需要重启才能生效的改动
}

// New 组装 App。res 是配置文件 raw 已经通过校验的解析结果；运营数据库里后台对配置的修改在这里叠加上去。
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
		return nil, fmt.Errorf("访问日志：%w", err)
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
		if !slices.Contains(res.Warnings, w) {
			a.log.Warn(w)
		}
	}
	a.noteOverrides(nil, rt.Overrides)
	a.commit(rt, nil)
	a.warnings = eff.Warnings
	a.registerMetrics()
	a.registerOverrideMetrics()
	return a, nil
}

// Runtime 返回当前生效的设置。
func (a *App) Runtime() *Runtime { return a.rt.Load() }

// ConfigPath 是配置文件的路径，只用默认值时为空。
func (a *App) ConfigPath() string { return a.opt.ConfigPath }

// ConfigFile 返回配置文件现在的内容（最近一次成功加载的），只用默认值时为空。
func (a *App) ConfigFile() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fileRaw
}

// LastReload 返回最近一次重载的结果，没有重载过时为 nil。
func (a *App) LastReload() *ReloadStatus { return a.reload.Load() }

// Log 把一条访问记录同时交给访问日志、统计和指标。
func (a *App) Log(e *accesslog.Entry) {
	a.accessLog.Log(e)
	a.Stats.Log(e)
	a.Metrics.Log(e)
	if a.opt.Hook != nil {
		a.opt.Hook(e)
	}
}

// AccessLogDropped 是访问日志因队列满而丢弃的条数。
func (a *App) AccessLogDropped() uint64 { return a.accessLog.Dropped() }

// bootstrapAdmin：运营数据库里还没有管理员、配置里写了 initial_admin 时，创建第一个管理员。
func (a *App) bootstrapAdmin(cfg *config.Config) error {
	ia := cfg.Admin.InitialAdmin
	if ia.Username == "" || !a.Store.Writable() {
		return nil
	}
	if len(a.Store.Data().Admins) > 0 {
		if ia.FromEnv {
			a.log.Warn("RELAY_ADMIN_PASSWORD_FILE 指向的口令文件还在，但运营数据库里已经有管理员，它不会再被使用；请删掉这个文件（k8s 里是对应的 Secret）")
		} else {
			a.log.Warn("配置里还有 admin.initial_admin，但运营数据库里已经有管理员，它不会再被使用；请把它和对应的口令文件或环境变量删掉")
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
			a.log.Info("已按 RELAY_ADMIN_PASSWORD_FILE 指向的口令文件创建第一个管理员；第一次登录后必须改口令，之后请删掉这个文件（k8s 里是对应的 Secret）", "username", ia.Username)
		} else {
			a.log.Info("已按 admin.initial_admin 创建第一个管理员；第一次登录后必须改口令，之后请把 initial_admin 从配置里删掉", "username", ia.Username)
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

// build 由配置和当前的运营数据生成 Runtime，还没有生效：生效用 commit，不用了用 discard。
// prev 不为 nil 时尽量沿用其中的连接池和缓存。
func (a *App) build(cfg *config.Config, prev *Runtime) (_ *Runtime, err error) {
	data := a.Store.Data()
	now := time.Now()
	rt := &Runtime{Config: cfg, Data: data, Built: now}
	defer func() {
		if err != nil && rt.Pool != nil {
			a.discard(rt, prev)
		}
	}()

	// 客户端账号：配置文件里的优先，运营数据库里同名的忽略
	rt.Users = map[string]*access.User{}
	if cfg.Access.UsersFile != "" {
		f, err := os.Open(cfg.Access.UsersFile) //nolint:gosec // 路径由运维在配置里指定
		if err != nil {
			return nil, fmt.Errorf("access.users_file：%w", err)
		}
		users, err := access.ParseHtpasswd(f)
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("access.users_file：%w", err)
		}
		for i := range users {
			rt.Users[users[i].Name] = &users[i]
		}
	}
	for _, su := range data.Users {
		if _, dup := rt.Users[su.Name]; dup {
			a.log.Warn("运营数据库里的账号和 users_file 重名，以配置文件为准", "user", su.Name)
			continue
		}
		rt.Users[su.Name] = &access.User{Name: su.Name, Hash: []byte(su.Hash), Disabled: su.Disabled, Source: "store",
			ManifestRPS: su.ManifestRate, BlobStreams: su.BlobStreams}
	}

	// IP 规则和临时封禁
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
			a.log.Warn("运营数据库里的 IP 规则写法不对，忽略", "cidr", r.CIDR)
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

	// 镜像规则
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

	// 上游：运营数据库里加的重定向主机（精确主机名）追加到白名单
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

	// 管理 API 令牌
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

// commit 让 build 生成的 rt 生效：换下来的连接池稍后关掉（进行中的请求继续用旧的），限流设置跟着换。
func (a *App) commit(rt, prev *Runtime) {
	if prev != nil && prev.Pool != rt.Pool {
		old := prev.Pool
		time.AfterFunc(2*time.Minute, old.Close)
	}
	a.Limiter.SetConfig(limitConfig(rt.Config))
	a.rt.Store(rt)
}

// discard 丢掉 build 生成、但不再生效的 rt：它新建的连接池关掉。
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

// Rebuild 在运营数据变化后（API 修改了账号、规则等）重新生成 Runtime。
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

// ErrNoConfigFile 表示没有配置文件可以重载。
var ErrNoConfigFile = errors.New("没有配置文件，只在用默认值")

// ReloadConfig 重新读取配置文件并替换 Runtime。配置有错时保留原来的设置并返回错误。
// 后台对配置的修改重新叠加上去，和配置文件冲突的暂停（配置文件是基准，和重启时一样）。
func (a *App) ReloadConfig() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := &ReloadStatus{Time: time.Now()}
	defer a.reload.Store(st)
	if a.opt.ConfigPath == "" {
		st.Err = ErrNoConfigFile.Error()
		return ErrNoConfigFile
	}
	raw, err := os.ReadFile(a.opt.ConfigPath)
	if err != nil {
		st.Err = err.Error()
		return err
	}
	res, err := config.Parse(raw)
	if err != nil {
		st.Err = err.Error()
		a.log.Error("配置有误，保留原来的设置", "err", err)
		return err
	}
	prev := a.rt.Load()
	rt, eff, err := a.assemble(raw, res, prev)
	if err != nil {
		st.Err = err.Error()
		a.log.Error("新配置无法生效，保留原来的设置", "err", err)
		return err
	}
	st.Restart = restartNeeded(prev.Config, eff.Config)
	for _, w := range eff.Warnings {
		a.log.Warn(w)
	}
	if len(st.Restart) > 0 {
		a.log.Warn("这些改动要重启才能生效", "fields", strings.Join(st.Restart, ", "))
	}
	a.noteOverrides(prev.Overrides, rt.Overrides)
	a.commit(rt, prev)
	a.cfgHash = sha256.Sum256(raw)
	a.fileRaw, a.warnings = raw, eff.Warnings
	a.log.Info("已重新加载配置", "path", a.opt.ConfigPath)
	return nil
}

// restartNeeded 列出热重载管不到的改动。
func restartNeeded(old, cur *config.Config) []string {
	var out []string
	check := func(name string, x, y any) {
		if fingerprint(x) != fingerprint(y) {
			out = append(out, name)
		}
	}
	check("listen", old.Listen, cur.Listen)
	// ACME 的域名随时按生效的配置放行（入口可以在后台改），不用重启
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

// Watch 每 10 秒检查一次配置文件，内容变了就重载（也适用于 k8s ConfigMap 的符号链接替换）。
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

// Health 返回各上游最近一次探测的结果。
func (a *App) Health() map[string]upstream.Health {
	a.healthMu.Lock()
	defer a.healthMu.Unlock()
	out := make(map[string]upstream.Health, len(a.health))
	for k, v := range a.health {
		out[k] = v
	}
	return out
}

// CheckUpstream 立即探测一个上游并记下结果。没有这个上游时 ok 为 false。
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

// ProbeHours 是一个上游近 24 小时每小时的探测结果。
func (a *App) ProbeHours(name string) []status.Hour { return a.probes.Hours(name, time.Now()) }

// StatusReport 是用法页 /status.json 的内容：各上游的连通性和近 24 小时的可用率，以及本服务的请求成功率。
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

// requestHours 把每分钟的计数按小时汇总成 status.Hours 项，旧的在前，最后一项是当前小时。
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

// WatchUpstreams 按 proxy.health_interval 探测所有上游，启动时先探测一次。
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
			iv = time.Minute // 关着时也每分钟看一次设置有没有改
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(iv):
		}
	}
}

// Close 存下统计、写完访问日志、关闭连接和运营数据库。
func (a *App) Close() {
	a.Stats.Flush()
	if n := a.accessLog.Dropped(); n > 0 {
		a.log.Warn("访问日志队列满时丢弃过记录", "count", n)
	}
	_ = a.accessLog.Close()
	if rt := a.rt.Load(); rt != nil {
		rt.Pool.Close()
	}
	_ = a.Store.Close()
}

func (a *App) registerMetrics() {
	m := a.Metrics
	m.Register("relay_manifest_cache_events_total", "manifest 缓存的结果", "counter", func() []metrics.Sample {
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
	m.Register("relay_manifest_cache_bytes", "manifest 缓存占用的字节数", "gauge", func() []metrics.Sample {
		return []metrics.Sample{{Value: float64(a.Runtime().Manifests.Stats().Bytes)}}
	})
	m.Register("relay_inflight_requests", "正在处理的请求数", "gauge", func() []metrics.Sample {
		return []metrics.Sample{{Value: float64(a.Limiter.Stats().Inflight)}}
	})
	m.Register("relay_blob_streams", "正在转发的镜像层数", "gauge", func() []metrics.Sample {
		return []metrics.Sample{{Value: float64(a.Limiter.Stats().BlobStreams)}}
	})
	m.Register("relay_rate_limited_total", "被限流或超过并发上限而拒绝的请求数", "counter", func() []metrics.Sample {
		return []metrics.Sample{{Value: float64(a.Limiter.Stats().Limited)}}
	})
	m.Register("relay_upstream_breaker_open", "上游熔断器是否打开", "gauge", func() []metrics.Sample {
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
	m.Register("relay_upstream_probe_up", "上游最近一次探测是否正常（/v2/ 返回 200 或 401）", "gauge", func() []metrics.Sample {
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
	m.Register("relay_upstream_probe_seconds", "上游最近一次探测的耗时", "gauge", func() []metrics.Sample {
		var out []metrics.Sample
		health := a.Health()
		for _, name := range a.Runtime().Pool.Names() {
			if h, ok := health[name]; ok {
				out = append(out, metrics.Sample{Labels: metrics.Labels("upstream", name), Value: h.Latency.Seconds()})
			}
		}
		return out
	})
	m.Register("relay_upstream_quota_remaining", "上游在响应头里报告的剩余拉取额度（Docker Hub 的 Ratelimit-Remaining）", "gauge", func() []metrics.Sample {
		var out []metrics.Sample
		for _, name := range a.Runtime().Pool.Names() {
			if q := a.Runtime().Pool.Get(name).Quota(); q != nil {
				out = append(out, metrics.Sample{Labels: metrics.Labels("upstream", name), Value: float64(q.Remaining)})
			}
		}
		return out
	})
	m.Register("relay_accesslog_dropped_total", "访问日志因队列满而丢弃的条数", "counter", func() []metrics.Sample {
		return []metrics.Sample{{Value: float64(a.accessLog.Dropped())}}
	})
}
