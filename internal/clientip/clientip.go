// Package clientip 从请求里取出客户端的真实 IP。
//
// 只信任配置的代理发来的 X-Forwarded-For，并且从右往左取第一个不受信任的地址：
// 最右边是离我们最近的代理加的，最左边是客户端自己可以随便写的（例如公司内网代理加的内网地址）。
// 这和 Caddy 的 trusted_proxies_strict 一致。
package clientip

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Resolver 是不可变的，配置重载时整体替换。
type Resolver struct {
	trusted    []netip.Prefix
	useXRealIP bool
}

// New 创建 Resolver。trusted 为空时不信任任何代理，直接用 TCP 来源地址。
func New(trusted []netip.Prefix, useXRealIP bool) *Resolver {
	return &Resolver{trusted: trusted, useXRealIP: useXRealIP}
}

// Trusted 判断 a 是不是受信任的代理。
func (r *Resolver) Trusted(a netip.Addr) bool { return r.isTrusted(a) }

func (r *Resolver) isTrusted(a netip.Addr) bool {
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Remote 解析 RemoteAddr（TCP 来源地址）。
func Remote(req *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	return parse(host)
}

// parse 去掉 IPv6 的 zone，把 IPv4 映射地址还原成 IPv4。解析失败返回零值。
func parse(s string) netip.Addr {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		// X-Forwarded-For 里偶尔会带端口或方括号
		if ap, err2 := netip.ParseAddrPort(strings.TrimSpace(s)); err2 == nil {
			a = ap.Addr()
		} else {
			return netip.Addr{}
		}
	}
	return a.WithZone("").Unmap()
}

// proxyHeaders 是各种代理、CDN、负载均衡会加的请求头（规范化后的写法）。客户端直连时不会带它们。
var proxyHeaders = []string{
	"Forwarded", "Via", "X-Real-Ip", "Cf-Connecting-Ip", "True-Client-Ip", "Fastly-Client-Ip",
	"X-Client-Ip", "X-Cluster-Client-Ip", "X-Envoy-External-Address", "X-Original-Forwarded-For",
}

// Proxied 判断请求是不是经过了代理：带任何 X-Forwarded-* 或上面的头，或者是 HTTP/1.0
// （nginx 默认的 proxy_pass 用 HTTP/1.0 回源，而且不加 X-Forwarded-For）。
// 这些头也可能是客户端自己写的，所以只用来「拒绝」，从不用来「放行」。
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

// Forwarded 返回受信任的代理传来的客户端地址：TCP 对端是受信任的代理，并且地址确实取自
// X-Real-IP（use_x_real_ip 时）或 X-Forwarded-For。不满足时 ok 为 false。
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

// ClientIP 返回客户端 IP 和 TCP 来源地址。
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
	// 多行 X-Forwarded-For 按顺序拼起来，等同于一行逗号分隔
	var hops []string
	for _, v := range req.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	client = remote
	for i := len(hops) - 1; i >= 0; i-- {
		a := parse(hops[i])
		if !a.IsValid() {
			// 写坏的值：不再往左看，左边的内容不可信
			return client, remote
		}
		client = a
		if !r.isTrusted(a) {
			return client, remote
		}
	}
	// 每一跳都是受信任的代理：用最左边的那个
	return client, remote
}
