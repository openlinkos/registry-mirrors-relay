// Package clientip extracts the client's real IP from a request.
//
// X-Forwarded-For is trusted only when it comes from a configured proxy, and the first untrusted
// address is taken scanning from right to left: the rightmost entry was added by the proxy nearest to
// us, while the leftmost can be anything the client chooses to write (for example private addresses
// added by a corporate network proxy). This matches Caddy's trusted_proxies_strict.
package clientip

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Resolver is immutable and is replaced as a whole when the configuration is reloaded.
type Resolver struct {
	trusted    []netip.Prefix
	useXRealIP bool
}

// New creates a Resolver. When trusted is empty no proxy is trusted and the TCP source address is
// used directly.
func New(trusted []netip.Prefix, useXRealIP bool) *Resolver {
	return &Resolver{trusted: trusted, useXRealIP: useXRealIP}
}

// Trusted reports whether a is a trusted proxy.
func (r *Resolver) Trusted(a netip.Addr) bool { return r.isTrusted(a) }

func (r *Resolver) isTrusted(a netip.Addr) bool {
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Remote parses RemoteAddr (the TCP source address).
func Remote(req *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	return parse(host)
}

// parse strips the IPv6 zone and turns IPv4-mapped addresses back into IPv4. It returns the zero value
// when parsing fails.
func parse(s string) netip.Addr {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		// X-Forwarded-For occasionally carries a port or square brackets
		if ap, err2 := netip.ParseAddrPort(strings.TrimSpace(s)); err2 == nil {
			a = ap.Addr()
		} else {
			return netip.Addr{}
		}
	}
	return a.WithZone("").Unmap()
}

// proxyHeaders are the request headers that proxies, CDNs and load balancers add (in canonical form).
// Clients connecting directly do not send them.
var proxyHeaders = []string{
	"Forwarded", "Via", "X-Real-Ip", "Cf-Connecting-Ip", "True-Client-Ip", "Fastly-Client-Ip",
	"X-Client-Ip", "X-Cluster-Client-Ip", "X-Envoy-External-Address", "X-Original-Forwarded-For",
}

// Proxied reports whether the request came through a proxy: it carries any X-Forwarded-* header or
// one of the headers above, or it is HTTP/1.0 (nginx's default proxy_pass uses HTTP/1.0 for upstream
// fetches and does not add X-Forwarded-For). The client may also write these headers itself, so they
// are only ever used to refuse, never to allow.
func Proxied(req *http.Request) bool {
	if req.ProtoMajor == 1 && req.ProtoMinor == 0 {
		return true
	}
	for k := range req.Header {
		if strings.HasPrefix(k, "X-Forwarded-") {
			return true
		}
	}
	for _, h := range proxyHeaders {
		if _, ok := req.Header[h]; ok {
			return true
		}
	}
	return false
}

// Forwarded returns the client address passed on by a trusted proxy: the TCP peer is a trusted proxy
// and the address was actually taken from X-Real-IP (with use_x_real_ip) or X-Forwarded-For.
// Otherwise ok is false.
func (r *Resolver) Forwarded(req *http.Request) (client netip.Addr, ok bool) {
	remote := Remote(req)
	if !remote.IsValid() || !r.isTrusted(remote) {
		return netip.Addr{}, false
	}
	if r.useXRealIP && parse(req.Header.Get("X-Real-IP")).IsValid() {
		c, _ := r.ClientIP(req)
		return c, true
	}
	if len(req.Header.Values("X-Forwarded-For")) == 0 {
		return netip.Addr{}, false
	}
	c, _ := r.ClientIP(req)
	return c, c != remote
}

// ClientIP returns the client IP and the TCP source address.
func (r *Resolver) ClientIP(req *http.Request) (client, remote netip.Addr) {
	remote = Remote(req)
	if !remote.IsValid() || !r.isTrusted(remote) {
		return remote, remote
	}
	if r.useXRealIP {
		if a := parse(req.Header.Get("X-Real-IP")); a.IsValid() {
			return a, remote
		}
	}
	// Multiple X-Forwarded-For lines are joined in order, equivalent to one comma-separated line
	var hops []string
	for _, v := range req.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	client = remote
	for i := len(hops) - 1; i >= 0; i-- {
		a := parse(hops[i])
		if !a.IsValid() {
			// Malformed value: stop looking further left, as anything to the left is untrustworthy
			return client, remote
		}
		client = a
		if !r.isTrusted(a) {
			return client, remote
		}
	}
	// Every hop is a trusted proxy: use the leftmost one
	return client, remote
}
