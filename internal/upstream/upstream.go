// Package upstream deals with upstream registries: shared connection pools, dial guarding, server-side token
// fetching and redirect following.
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
	"sync/atomic"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/breaker"
	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/token"
	"github.com/openlinkos/registry-mirrors-relay/internal/version"
)

// Options are mainly for tests: replace dialling, trust a test CA, allow local addresses.
type Options struct {
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	RootCAs     *x509.CertPool
	AllowAddr   func(netip.Addr) bool
	Log         *slog.Logger
}

// Pool holds all upstreams. Ordinary upstreams share one connection pool and token cache; upstreams with a
// private CA or allowed to reach private networks get their own.
type Pool struct {
	ups        map[string]*Upstream
	transports []*http.Transport
	log        *slog.Logger
}

// client is a connection pool plus token cache. http uses HTTP/2, for small requests such as manifests and
// tokens; blob is for layers and uses only HTTP/1.1 by default.
type client struct {
	http   *http.Client
	blob   *http.Client
	tokens *token.Cache
}

// Upstream is an upstream.
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
	challenges map[string]token.Challenge // host → its Bearer challenge (only realm and service are used)
	quota      atomic.Pointer[Quota]
}

// NewPool creates all enabled upstreams from the configuration.
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
			return nil, fmt.Errorf("upstream %s: %w", name, err)
		}
		realmRe, err := config.CompileHostPatterns(c.TokenRealmHosts)
		if err != nil {
			return nil, fmt.Errorf("upstream %s: %w", name, err)
		}
		cl := shared
		if c.AllowPrivateNetwork || c.TLSCAFile != "" {
			roots, err := rootsWith(opt.RootCAs, c.TLSCAFile)
			if err != nil {
				return nil, fmt.Errorf("upstream %s: %w", name, err)
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

// rootsWith returns the system CAs (base in tests) plus the certificates in caFile.
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
	pem, err := os.ReadFile(caFile) //nolint:gosec // the path is set by the operator in the configuration
	if err != nil {
		return nil, err
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no usable PEM certificates in %s", caFile)
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
		Proxy:                 nil, // do not read HTTP_PROXY: a proxy would bypass the dial guard
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, NextProtos: []string{"h2", "http/1.1"}},
		TLSHandshakeTimeout:   t.TLSHandshake.D(),
		ResponseHeaderTimeout: t.ResponseHeader.D(),
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   cfg.Proxy.MaxIdleConnsPerHost,
		DisableCompression:    true, // pass bytes through unchanged so length and digest stay the same
		ReadBufferSize:        64 << 10,
		WriteBufferSize:       16 << 10,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: 30 * time.Second, // ping after 30 seconds idle to detect broken HTTP/2 connections early
			PingTimeout:     15 * time.Second,
		},
	}
	p.transports = append(p.transports, tr)
	// Handle redirects ourselves: check the target, drop Authorization, record every hop
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	hc := &http.Client{Transport: tr, CheckRedirect: noRedirect}
	bc := hc
	if !cfg.Proxy.BlobHTTP2 {
		bt := tr.Clone()
		bt.ForceAttemptHTTP2 = false
		bt.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{} // non-nil empty map: no HTTP/2
		bt.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, NextProtos: []string{"http/1.1"}}
		bt.HTTP2 = nil
		bt.ReadBufferSize, bt.WriteBufferSize = 0, 0 // use the default 4 KiB; large reads bypass it and read the connection directly
		p.transports = append(p.transports, bt)
		bc = &http.Client{Transport: bt, CheckRedirect: noRedirect}
	}
	tc := token.NewCache(hc, t.Token.D(), ua, 4096)
	tc.OnCredentialRejected = func(realm, user string) {
		p.log.Warn("upstream credentials rejected by token service, falling back to anonymous", "realm", realm, "username", user)
	}
	return &client{http: hc, blob: bc, tokens: tc}
}

// Get returns the upstream, or nil if there is none.
func (p *Pool) Get(name string) *Upstream { return p.ups[name] }

// Names returns the names of all upstreams in alphabetical order.
func (p *Pool) Names() []string {
	out := make([]string, 0, len(p.ups))
	for n := range p.ups {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Close closes idle connections.
func (p *Pool) Close() {
	for _, tr := range p.transports {
		tr.CloseIdleConnections()
	}
}

// BreakerOpen reports whether this upstream's circuit breaker is open.
func (u *Upstream) BreakerOpen() bool { return u.breaker.Open() }

// Request is a request sent to the upstream.
type Request struct {
	Method string // GET or HEAD
	Path   string // /v2/...
	Query  url.Values
	Header http.Header // already filtered by allowlist: Accept, Range, If-Range
	Repo   string      // repository name on the upstream, used to build the token scope
	Blob   bool        // layer: use the blob connection pool
}

// Response is the upstream's final response. The caller closes Body.
type Response struct {
	*http.Response
	Hops []string // host of every hop followed on the server side
	// When PassLocation is not empty, a redirect outside the allowlist (or beyond the hop limit) was met and
	// should be passed to the client as is
	PassLocation string
	// Probed means a HEAD following a redirect was turned into GET Range: bytes=0-0 (object storage signatures
	// are bound to the method, so a plain HEAD gets 403); the caller should answer the HEAD with the total
	// length in Content-Range and discard the body.
	Probed bool
}

// RedirectError means a redirect outside the allowlist, with the upstream configured to deny it.
type RedirectError struct{ Host string }

func (e *RedirectError) Error() string {
	return "upstream redirected to host not in allowlist: " + e.Host
}

// CircuitOpenError means the upstream's circuit breaker is open and the request was not sent.
type CircuitOpenError struct {
	Upstream   string
	RetryAfter time.Duration
}

func (e *CircuitOpenError) Error() string {
	return "upstream " + e.Upstream + " failed repeatedly, requests paused"
}

// ErrNoLocation means the upstream returned a redirect without a Location.
var ErrNoLocation = errors.New("upstream redirect has no Location")

func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// Fetch sends the request, fetches tokens as needed and follows allowlisted redirects on the server side.
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
	resp, err := u.do(ctx, req, r.Repo, true, r.Blob) //nolint:bodyclose // discarded in the loop on redirect, otherwise handed to the caller with Response to close
	switch {
	case err != nil && ctx.Err() != nil:
		u.breaker.Cancel() // the client left on its own; that says nothing about the upstream
	case err != nil:
		u.breaker.Done(true)
	default:
		u.breaker.Done(resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout)
	}
	if err != nil {
		return nil, err
	}
	u.observeQuota(resp.Header)
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
		next, err := prev.Parse(loc) // a relative address is resolved against the hop that returned it
		if err != nil {
			return nil, fmt.Errorf("invalid upstream Location: %w", err)
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
		// Do not attach a token when following a redirect: CDN and object storage URLs carry their own signature.
		// If the other side asks for authentication, fetch one according to its challenge
		resp, err = u.do(ctx, req, r.Repo, false, r.Blob) //nolint:bodyclose // as above
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

// do sends one request; when the upstream returns a Bearer challenge it fetches a token and tries once more.
// primary means the request goes to the registry itself: only such requests attach a known token up front,
// and only they may use the configured account.
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
		u.log.Warn("token service from upstream not in allowlist, not fetching a token", "upstream", u.Name, "realm", ch.Realm())
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
		return nil, fmt.Errorf("fetch token: %w", err)
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
		clear(u.challenges) // normally there are only a few hosts; more means the upstream is changing, so start over
	}
	// Record only realm and service: the scope is for that request's repository and must not be used for others
	u.challenges[host] = token.Challenge{Scheme: ch.Scheme, Params: map[string]string{"realm": ch.Realm(), "service": ch.Service()}}
}

// redirectAllowed: https, port 443, no userinfo, not an IP literal, and the host is in the allowlist.
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

// discard reads and closes a response body that is no longer needed (up to 64 KiB) so the connection can be
// reused.
func discard(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
