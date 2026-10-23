// Package upstream 负责和上游 registry 打交道：共用的连接池、拨号拦截、服务端取令牌、跟随重定向。
package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/breaker"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/token"
	"github.com/openlinkos/registry-mirrors-relay/internal/version"
)

// Options 主要给测试用：替换拨号、信任测试 CA、放行本地地址。
type Options struct {
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	RootCAs     *x509.CertPool
	AllowAddr   func(netip.Addr) bool
	Log         *slog.Logger
}

// Pool 是所有上游。一般的上游共用一个连接池和令牌缓存；配了私有 CA 或允许内网的上游单独一套。
type Pool struct {
	ups        map[string]*Upstream
	transports []*http.Transport
	log        *slog.Logger
}

// client 是一套连接池加令牌缓存。http 走 HTTP/2，给 manifest、令牌这类小请求；blob 给镜像层，默认只用 HTTP/1.1。
type client struct {
	http   *http.Client
	blob   *http.Client
	tokens *token.Cache
}

// Upstream 是一个上游。
type Upstream struct {
	Name     string
	Registry string

	redirectRe   *regexp.Regexp
	realmRe      *regexp.Regexp
	unlistedDeny bool
	maxRedirects int
	userAgent    string
	cred         *token.Credential
	credRealm    string
	client       *client
	breaker      *breaker.Breaker
	log          *slog.Logger

	mu         sync.Mutex
	challenges map[string]token.Challenge // 主机 → 它的 Bearer 质询（只用 realm、service）
}

// NewPool 由配置创建所有启用的上游。
func NewPool(cfg *config.Config, opt Options) (*Pool, error) {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	ua := cfg.Proxy.UserAgent
	if ua == "" {
		ua = version.UserAgent()
	}
	p := &Pool{ups: map[string]*Upstream{}, log: log}
	shared := p.newClient(cfg, opt, ua, opt.AllowAddr, opt.RootCAs)

	names := make([]string, 0, len(cfg.Upstreams))
	for n := range cfg.Upstreams {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		c := cfg.Upstreams[name]
		if c.Disabled {
			continue
		}
		redirectRe, err := config.CompileHostPatterns(append(append([]string{}, c.RedirectHosts...), c.ExtraRedirectHosts...))
		if err != nil {
			return nil, fmt.Errorf("上游 %s：%w", name, err)
		}
		realmRe, err := config.CompileHostPatterns(c.TokenRealmHosts)
		if err != nil {
			return nil, fmt.Errorf("上游 %s：%w", name, err)
		}
		cl := shared
		if c.AllowPrivateNetwork || c.TLSCAFile != "" {
			roots, err := rootsWith(opt.RootCAs, c.TLSCAFile)
			if err != nil {
				return nil, fmt.Errorf("上游 %s：%w", name, err)
			}
			allow := opt.AllowAddr
			if c.AllowPrivateNetwork {
				allow = func(netip.Addr) bool { return true }
			}
			cl = p.newClient(cfg, opt, ua, allow, roots)
		}
		u := &Upstream{
			Name: name, Registry: c.Registry,
			redirectRe: redirectRe, realmRe: realmRe,
			unlistedDeny: c.UnlistedRedirect == "deny",
			maxRedirects: cfg.Proxy.MaxRedirects,
			userAgent:    ua,
			client:       cl,
			breaker:      breaker.New(cfg.Proxy.BreakerFailures, cfg.Proxy.BreakerCooldown.D(), time.Minute),
			log:          log,
			challenges:   map[string]token.Challenge{},
		}
		if cr := c.Credentials; cr != nil {
			u.cred = &token.Credential{Username: cr.Username, Password: cr.Password.Reveal()}
			u.credRealm = cr.RealmHost
		}
		p.ups[name] = u
	}
	return p, nil
}

// rootsWith 返回系统 CA（测试时是 base）再加上 caFile 里的证书。
func rootsWith(base *x509.CertPool, caFile string) (*x509.CertPool, error) {
	if caFile == "" {
		return base, nil
	}
	var roots *x509.CertPool
	if base != nil {
		roots = base.Clone()
	} else {
		var err error
		if roots, err = x509.SystemCertPool(); err != nil {
			roots = x509.NewCertPool()
		}
	}
	pem, err := os.ReadFile(caFile) //nolint:gosec // 路径由运维在配置里指定
	if err != nil {
		return nil, err
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s 里没有可用的 PEM 证书", caFile)
	}
	return roots, nil
}

func (p *Pool) newClient(cfg *config.Config, opt Options, ua string, allow func(netip.Addr) bool, roots *x509.CertPool) *client {
	t := cfg.Timeouts
	dialer := &net.Dialer{Timeout: t.Dial.D(), KeepAlive: 30 * time.Second, ControlContext: guardControl(allow)}
	dial := dialer.DialContext
	if opt.DialContext != nil {
		dial = opt.DialContext
	}
	tr := &http.Transport{
		Proxy:                 nil, // 不读 HTTP_PROXY：代理会绕过拨号拦截
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, NextProtos: []string{"h2", "http/1.1"}},
		TLSHandshakeTimeout:   t.TLSHandshake.D(),
		ResponseHeaderTimeout: t.ResponseHeader.D(),
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   cfg.Proxy.MaxIdleConnsPerHost,
		DisableCompression:    true, // 字节原样透传，长度和 digest 不变
		ReadBufferSize:        64 << 10,
		WriteBufferSize:       16 << 10,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: 30 * time.Second, // 连接空闲 30 秒发一次 ping，及时发现断掉的 HTTP/2 连接
			PingTimeout:     15 * time.Second,
		},
	}
	p.transports = append(p.transports, tr)
	// 重定向自己处理：要检查目标、去掉 Authorization、记录每一跳
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	hc := &http.Client{Transport: tr, CheckRedirect: noRedirect}
	bc := hc
	if !cfg.Proxy.BlobHTTP2 {
		bt := tr.Clone()
		bt.ForceAttemptHTTP2 = false
		bt.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{} // 非 nil 的空表：不用 HTTP/2
		bt.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, NextProtos: []string{"http/1.1"}}
		bt.HTTP2 = nil
		bt.ReadBufferSize, bt.WriteBufferSize = 0, 0 // 用默认的 4 KiB；大块读取会绕过它直接读连接
		p.transports = append(p.transports, bt)
		bc = &http.Client{Transport: bt, CheckRedirect: noRedirect}
	}
	tc := token.NewCache(hc, t.Token.D(), ua, 4096)
	tc.OnCredentialRejected = func(realm, user string) {
		p.log.Warn("上游账号被令牌服务拒绝，改用匿名", "realm", realm, "username", user)
	}
	return &client{http: hc, blob: bc, tokens: tc}
}

// Get 返回上游，没有时返回 nil。
func (p *Pool) Get(name string) *Upstream { return p.ups[name] }

// Names 返回所有上游的名字，按字母排序。
func (p *Pool) Names() []string {
	out := make([]string, 0, len(p.ups))
	for n := range p.ups {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Close 关闭空闲连接。
func (p *Pool) Close() {
	for _, tr := range p.transports {
		tr.CloseIdleConnections()
	}
}

// BreakerOpen 表示这个上游的熔断器是否打开。
func (u *Upstream) BreakerOpen() bool { return u.breaker.Open() }

// Request 是发给上游的一个请求。
type Request struct {
	Method string // GET 或 HEAD
	Path   string // /v2/...
	Query  url.Values
	Header http.Header // 已按白名单过滤：Accept、Range、If-Range
	Repo   string      // 上游的仓库名，用来生成令牌的 scope
	Blob   bool        // 镜像层：用 blob 连接池
}

// Response 是上游的最终响应。Body 由调用方关闭。
type Response struct {
	*http.Response
	Hops []string // 服务端跟随过的每一跳的主机
	// PassLocation 不为空时，表示遇到了白名单外（或超过跳数）的重定向，应原样交给客户端
	PassLocation string
	// Probed 表示 HEAD 跟随重定向时改成了 GET Range: bytes=0-0（对象存储的签名绑定了方法，直接 HEAD 会 403），
	// 调用方应按 Content-Range 里的总长度回答 HEAD，并丢弃正文。
	Probed bool
}

// RedirectError 表示白名单外的重定向，且上游配置为 deny。
type RedirectError struct{ Host string }

func (e *RedirectError) Error() string { return "上游重定向到白名单外的主机 " + e.Host }

// CircuitOpenError 表示上游的熔断器打开着，请求没有发出去。
type CircuitOpenError struct {
	Upstream   string
	RetryAfter time.Duration
}

func (e *CircuitOpenError) Error() string {
	return "上游 " + e.Upstream + " 连续失败，暂停请求"
}

// ErrNoLocation 表示上游返回了重定向却没有 Location。
var ErrNoLocation = errors.New("上游的重定向没有 Location")

func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// Fetch 发出请求，按需取令牌，并在服务端跟随白名单内的重定向。
func (u *Upstream) Fetch(ctx context.Context, r Request) (*Response, error) {
	target := &url.URL{Scheme: "https", Host: u.Registry, Path: r.Path}
	if len(r.Query) > 0 {
		target.RawQuery = r.Query.Encode()
	}
	req, err := u.newRequest(ctx, r.Method, target, r.Header)
	if err != nil {
		return nil, err
	}
	if ok, wait := u.breaker.Allow(); !ok {
		return nil, &CircuitOpenError{Upstream: u.Name, RetryAfter: wait}
	}
	resp, err := u.do(ctx, req, r.Repo, true, r.Blob) //nolint:bodyclose // 重定向时在循环里 discard，否则随 Response 交给调用方关闭
	switch {
	case err != nil && ctx.Err() != nil:
		u.breaker.Cancel() // 客户端自己走了，说明不了上游的情况
	case err != nil:
		u.breaker.Done(true)
	default:
		u.breaker.Done(resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout)
	}
	if err != nil {
		return nil, err
	}
	var hops []string
	method := r.Method
	probed := false
	hopHeader := r.Header
	for isRedirect(resp.StatusCode) {
		loc := resp.Header.Get("Location")
		code := resp.StatusCode
		prev := resp.Request.URL
		discard(resp)
		if loc == "" {
			return nil, ErrNoLocation
		}
		next, err := prev.Parse(loc) // 相对地址按返回它的那一跳解析
		if err != nil {
			return nil, fmt.Errorf("上游的 Location 不合法：%w", err)
		}
		if len(hops) >= u.maxRedirects || !u.redirectAllowed(next) {
			if u.unlistedDeny {
				return nil, &RedirectError{Host: next.Host}
			}
			return &Response{Response: &http.Response{StatusCode: code, Header: http.Header{}, Body: http.NoBody}, Hops: hops, PassLocation: next.String()}, nil
		}
		hops = append(hops, strings.ToLower(next.Hostname()))
		if code == http.StatusSeeOther {
			method = http.MethodGet
		}
		if method == http.MethodHead {
			method, probed = http.MethodGet, true
			hopHeader = hopHeader.Clone()
			hopHeader.Set("Range", "bytes=0-0")
			hopHeader.Del("If-Range")
		}
		req, err = u.newRequest(ctx, method, next, hopHeader)
		if err != nil {
			return nil, err
		}
		// 跟随重定向时不主动带令牌：CDN 和对象存储的地址自带签名。对方要求认证时再按它的质询取
		resp, err = u.do(ctx, req, r.Repo, false, r.Blob) //nolint:bodyclose // 同上
		if err != nil {
			return nil, err
		}
	}
	return &Response{Response: resp, Hops: hops, Probed: probed}, nil
}

func (u *Upstream) newRequest(ctx context.Context, method string, target *url.URL, h http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range h {
		req.Header[k] = append([]string(nil), vs...)
	}
	req.Header.Set("User-Agent", u.userAgent)
	return req, nil
}

// do 发一次请求；上游返回 Bearer 质询时取令牌再试一次。primary 表示这是发给 registry 本身的请求：
// 只有它会主动带上已知的令牌，也只有它可能用上配置的账号。
func (u *Upstream) do(ctx context.Context, req *http.Request, repo string, primary, blob bool) (*http.Response, error) {
	hc := u.client.http
	if blob {
		hc = u.client.blob
	}
	host := strings.ToLower(req.URL.Host)
	var attached *token.Request
	if primary {
		if ch, ok := u.challenge(host); ok {
			tr := u.tokenRequest(ch, "repository:"+repo+":pull", true)
			if tok, err := u.client.tokens.Get(ctx, tr); err == nil {
				req.Header.Set("Authorization", "Bearer "+tok)
				attached = &tr
			}
		}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	var ch *token.Challenge
	for _, c := range token.ParseChallenges(resp.Header.Values("WWW-Authenticate")) {
		if c.Scheme == "bearer" {
			c := c
			ch = &c
			break
		}
	}
	if ch == nil {
		return resp, nil
	}
	if !u.realmAllowed(ch.Realm()) {
		u.log.Warn("上游给出的令牌服务不在白名单内，不去取令牌", "upstream", u.Name, "realm", ch.Realm())
		return resp, nil
	}
	discard(resp)
	u.setChallenge(host, *ch)
	if attached != nil {
		u.client.tokens.Invalidate(*attached)
	}
	scope := ch.Scope()
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	tok, err := u.client.tokens.Get(ctx, u.tokenRequest(*ch, scope, primary))
	if err != nil {
		return nil, fmt.Errorf("取令牌：%w", err)
	}
	retry := req.Clone(ctx)
	retry.Header.Set("Authorization", "Bearer "+tok)
	return hc.Do(retry)
}

func (u *Upstream) tokenRequest(ch token.Challenge, scope string, mayUseCred bool) token.Request {
	tr := token.Request{Realm: ch.Realm(), Service: ch.Service(), Scopes: []string{scope}}
	if mayUseCred && u.cred != nil {
		if ru, err := url.Parse(ch.Realm()); err == nil && strings.EqualFold(ru.Hostname(), u.credRealm) {
			tr.Cred = u.cred
		}
	}
	return tr
}

func (u *Upstream) challenge(host string) (token.Challenge, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	ch, ok := u.challenges[host]
	return ch, ok
}

func (u *Upstream) setChallenge(host string, ch token.Challenge) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.challenges) >= 64 {
		clear(u.challenges) // 正常只有几台主机，超过说明上游在变，清空重来
	}
	// 只记 realm 和 service：scope 是那一次请求的仓库，不能用到别的仓库上
	u.challenges[host] = token.Challenge{Scheme: ch.Scheme, Params: map[string]string{"realm": ch.Realm(), "service": ch.Service()}}
}

// redirectAllowed：https、443 端口、没有 userinfo、不是 IP 字面量，主机在白名单内。
func (u *Upstream) redirectAllowed(next *url.URL) bool {
	return hostAllowed(next, u.redirectRe)
}

func (u *Upstream) realmAllowed(realm string) bool {
	ru, err := url.Parse(realm)
	return err == nil && hostAllowed(ru, u.realmRe)
}

func hostAllowed(t *url.URL, re *regexp.Regexp) bool {
	if t.Scheme != "https" || t.User != nil || (t.Port() != "" && t.Port() != "443") {
		return false
	}
	h := strings.TrimSuffix(strings.ToLower(t.Hostname()), ".")
	if h == "" {
		return false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return false
	}
	return re.MatchString(h)
}

// discard 读掉并关闭不再需要的响应正文（最多 64 KiB），让连接可以复用。
func discard(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
