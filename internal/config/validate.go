package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// CompileHostPatterns 把主机名正则合成一个，加上锚点并且不区分大小写。
// 匹配时请先把主机名转成小写、去掉末尾的点。
func CompileHostPatterns(patterns []string) (*regexp.Regexp, error) {
	if len(patterns) == 0 {
		return regexp.MustCompile(`^\b$`), nil // 什么都不匹配
	}
	parts := make([]string, len(patterns))
	for i, p := range patterns {
		if _, err := regexp.Compile(p); err != nil {
			return nil, fmt.Errorf("正则 %q：%w", p, err)
		}
		parts[i] = "(?:" + p + ")"
	}
	return regexp.Compile(`(?i)^(?:` + strings.Join(parts, "|") + `)$`)
}

// ParsePrefixes 解析 CIDR、单个 IP，以及关键字 private_ranges、loopback。
func ParsePrefixes(items []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, raw := range items {
		s := strings.TrimSpace(raw)
		switch s {
		case "private_ranges":
			for _, p := range []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "::1/128", "fc00::/7"} {
				out = append(out, netip.MustParsePrefix(p))
			}
			continue
		case "loopback":
			out = append(out, netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128"))
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("%q 不是 IP 或 CIDR", raw)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

var (
	upstreamNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(:[0-9]{1,5})?$`)
	hostRe         = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
)

func validate(c *Config, res *Result) []error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	warn := func(format string, a ...any) { res.Warnings = append(res.Warnings, fmt.Sprintf(format, a...)) }

	// 监听地址
	for name, addr := range map[string]string{
		"listen.http": c.Listen.HTTP, "listen.https": c.Listen.HTTPS,
		"listen.admin": c.Listen.Admin, "listen.metrics": c.Listen.Metrics,
	} {
		if addr == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			bad("%s %q 写法不对，应写成 :8080 或 127.0.0.1:9090", name, addr)
		}
	}
	if c.Listen.HTTP == "" && c.Listen.HTTPS == "" && !hasPortEntrance(c) {
		bad("listen.http 和 listen.https 都没开，也没有按端口区分的入口，客户端无处可连")
	}

	// TLS
	switch c.TLS.Mode {
	case "off":
		if c.Listen.HTTPS != "" {
			bad("listen.https 已设置，但 tls.mode 是 off")
		}
	case "files":
		if c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
			bad("tls.mode 是 files 时要写 tls.cert_file 和 tls.key_file")
		}
		if c.Listen.HTTPS == "" {
			bad("tls.mode 是 files 时要设置 listen.https")
		}
	case "acme":
		if c.Listen.HTTPS == "" {
			bad("tls.mode 是 acme 时要设置 listen.https（通常是 :443）")
		}
		if len(c.TLS.ACME.Hosts) == 0 {
			bad("tls.mode 是 acme 时要有明确的域名：在入口的 hosts 里写域名，或写 tls.acme.hosts")
		}
		for _, h := range c.TLS.ACME.Hosts {
			if h == "*" || !hostRe.MatchString(h) {
				bad("tls.acme.hosts 里的 %q 不是可以申请证书的域名", h)
			}
		}
		if c.Listen.HTTP == "" {
			warn("tls.mode 是 acme 但 listen.http 没开：只能用 TLS-ALPN-01 验证，需要 listen.https 在公网 443 端口")
		}
	default:
		bad("tls.mode 只能是 off、files、acme，现在是 %q", c.TLS.Mode)
	}

	// 客户端 IP
	trusted, err := ParsePrefixes(c.ClientIP.TrustedProxies)
	if err != nil {
		bad("client_ip.trusted_proxies：%v", err)
	}
	for _, p := range trusted {
		if p.Bits() == 0 {
			bad("client_ip.trusted_proxies 里的 %s 信任了所有地址，客户端 IP 可以随意伪造", p)
		}
	}
	if len(c.ClientIP.TrustedProxies) == 0 && c.Listen.HTTP != "" {
		warn("client_ip.trusted_proxies 为空：客户端 IP 取 TCP 来源地址。放在 CDN 或反向代理之后时，请写上它们的回源地址段")
	}

	// 上游
	names := make([]string, 0, len(c.Upstreams))
	for n := range c.Upstreams {
		names = append(names, n)
	}
	sort.Strings(names)
	aliasOwner := map[string]string{}
	for _, n := range names {
		u := c.Upstreams[n]
		if !upstreamNameRe.MatchString(n) {
			bad("上游名 %q 应该是镜像地址里的 registry 写法，如 docker.io、ghcr.io（小写，至少有一个点）", n)
		}
		if u.Registry == "" {
			bad("上游 %s 没有写 registry", n)
		} else if !upstreamNameRe.MatchString(u.Registry) && !hostRe.MatchString(u.Registry) {
			bad("上游 %s 的 registry %q 应该是主机名，不带 https:// 和路径", n, u.Registry)
		}
		for _, a := range append([]string{n}, u.Aliases...) {
			if other, ok := aliasOwner[a]; ok && other != n {
				bad("名字 %q 同时属于上游 %s 和 %s", a, other, n)
			}
			aliasOwner[a] = n
		}
		if _, err := CompileHostPatterns(append(append([]string{}, u.RedirectHosts...), u.ExtraRedirectHosts...)); err != nil {
			bad("上游 %s 的重定向白名单：%v", n, err)
		}
		if _, err := CompileHostPatterns(u.TokenRealmHosts); err != nil {
			bad("上游 %s 的 token_realm_hosts：%v", n, err)
		}
		if len(u.TokenRealmHosts) == 0 {
			warn("上游 %s 没有写 token_realm_hosts：它要求令牌时会拉取失败", n)
		}
		if u.UnlistedRedirect != "pass" && u.UnlistedRedirect != "deny" {
			bad("上游 %s 的 unlisted_redirect 只能是 pass 或 deny", n)
		}
		if cr := u.Credentials; cr != nil && !u.Disabled {
			if cr.Username == "" || !cr.Password.IsSet() {
				bad("上游 %s 的 credentials 要写 username 和 password", n)
			}
			if cr.RealmHost == "" {
				bad("上游 %s 的 credentials 要写 realm_host：只把账号发给这个令牌服务", n)
			}
			if !cr.PublicOnly {
				bad("上游 %s 配了账号：这个账号能读的私有仓库也会被所有客户端拉到。确认它只能读公开仓库后，写 public_only: true", n)
			} else {
				warn("上游 %s 配了账号：请确认它是只能读公开仓库的令牌（例如 Docker Hub 的 Public Repo Read-only），并且不属于任何组织", n)
			}
			if cr.Password.Value != "" {
				warn("上游 %s 的口令直接写在配置里，建议改用 env 或 file", n)
			}
		}
	}

	// 入口
	type key struct{ listen, host string }
	seen := map[key]string{}
	if len(c.Entrances) == 0 {
		bad("至少要有一个入口（entrances）")
	}
	for _, e := range c.Entrances {
		where := fmt.Sprintf("入口 %s", e.Name)
		if len(e.Hosts) == 0 && e.Listen == "" {
			bad("%s 要写 hosts 或 listen", where)
		}
		if e.Listen != "" {
			if _, _, err := net.SplitHostPort(e.Listen); err != nil {
				bad("%s 的 listen %q 写法不对", where, e.Listen)
			}
			if e.Listen == c.Listen.HTTP || e.Listen == c.Listen.HTTPS {
				bad("%s 的 listen %q 和 listen.http/https 相同：按端口区分的入口要用单独的端口", where, e.Listen)
			}
		}
		hosts := e.Hosts
		if len(hosts) == 0 {
			hosts = []string{"*"}
		}
		for _, h := range hosts {
			if h != "*" && !hostRe.MatchString(h) {
				bad("%s 的 host %q 不是合法的域名（不要带端口和协议）", where, h)
			}
			k := key{e.Listen, h}
			if other, ok := seen[k]; ok {
				bad("%s 和 %s 都匹配 %q", other, where, h)
			}
			seen[k] = where
		}
		for _, ref := range []struct{ field, name string }{{"upstream", e.Upstream}, {"default_upstream", e.DefaultUpstream}} {
			if ref.name == "" {
				continue
			}
			u, ok := c.Upstreams[ref.name]
			switch {
			case !ok:
				bad("%s 的 %s %q 不是已配置的上游", where, ref.field, ref.name)
			case u.Disabled:
				bad("%s 的 %s %q 已停用", where, ref.field, ref.name)
			}
		}
		if e.Upstream != "" && e.DefaultUpstream != "" {
			bad("%s 同时写了 upstream 和 default_upstream：固定上游的入口不需要默认上游", where)
		}
		switch e.Auth {
		case "none":
		case "basic":
			if c.Access.UsersFile == "" && c.Admin.Store.Driver == "none" {
				warn("%s 要求登录，但没有 access.users_file，运营数据也是只读的：没有账号能登录", where)
			}
		default:
			bad("%s 的 auth 只能是 none 或 basic", where)
		}
	}

	// 访问控制
	if _, err := ParsePrefixes(c.Access.IPAllow); err != nil {
		bad("access.ip_allow：%v", err)
	}
	if _, err := ParsePrefixes(c.Access.IPDeny); err != nil {
		bad("access.ip_deny：%v", err)
	}
	if _, err := ParsePrefixes(c.Limits.ExemptCIDRs); err != nil {
		bad("limits.exempt_cidrs：%v", err)
	}
	for _, p := range c.Access.Images.Allow {
		if err := checkImagePattern(p, c); err != nil {
			bad("access.images.allow：%v", err)
		}
	}
	for i := range c.Access.Images.Deny {
		r := &c.Access.Images.Deny[i]
		if err := checkImagePattern(r.Pattern, c); err != nil {
			bad("access.images.deny：%v", err)
		}
		if r.Status == 0 {
			r.Status = 403
		}
		if r.Status != 403 && r.Status != 451 {
			bad("access.images.deny 里 %q 的 status 只能是 403 或 451", r.Pattern)
		}
	}

	// 限流、缓存、超时
	l := c.Limits
	for name, v := range map[string]float64{
		"limits.manifest_rate": l.ManifestRate, "limits.head_weight": l.HeadWeight,
		"limits.manifest_burst": float64(l.ManifestBurst), "limits.blob_streams_per_client": float64(l.BlobStreamsPerClient),
		"limits.max_inflight": float64(l.MaxInflight), "limits.max_blob_streams": float64(l.MaxBlobStreams),
		"limits.max_manifest_buffers": float64(l.MaxManifestBuffers), "limits.tracked_clients": float64(l.TrackedClients),
		"limits.auth_failures_per_min": float64(l.AuthFailuresPerMin),
	} {
		if v < 0 {
			bad("%s 不能是负数", name)
		}
	}
	if l.ManifestRate > 0 && l.ManifestBurst < 1 {
		bad("limits.manifest_rate 大于 0 时，limits.manifest_burst 至少为 1")
	}
	if l.TrackedClients == 0 {
		bad("limits.tracked_clients 至少为 1")
	}
	if c.Cache.MaxBytes > 0 && c.Cache.TagTTL > c.Cache.DigestTTL {
		bad("cache.tag_ttl 不能比 cache.digest_ttl 长")
	}
	if c.Proxy.MaxRedirects < 0 || c.Proxy.MaxRedirects > 10 {
		bad("proxy.max_redirects 应在 0 到 10 之间")
	}
	if c.Proxy.BufferSize < 4<<10 || c.Proxy.BufferSize > 1<<20 {
		bad("proxy.buffer_size 应在 4KiB 到 1MiB 之间")
	}
	if c.Timeouts.WriteIdle.D() <= 0 || c.Timeouts.UpstreamReadIdle.D() <= 0 || c.Timeouts.ReadHeader.D() <= 0 {
		bad("timeouts.write_idle、upstream_read_idle、read_header 都要大于 0")
	}

	// 管理后台
	switch c.Admin.Store.Driver {
	case "bolt", "none":
	default:
		bad("admin.store.driver 只能是 bolt 或 none")
	}
	if c.Admin.PublicURL != "" {
		u, err := url.Parse(c.Admin.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
			bad("admin.public_url 应写成 https://admin.example.com 这样，不带路径")
		}
	} else if c.Listen.Admin != "" && !isLoopbackListen(c.Listen.Admin) {
		warn("listen.admin 不只监听本机，但没有写 admin.public_url：后台只接受用 127.0.0.1 或 localhost 访问")
	}
	if c.Admin.InitialAdmin.Username != "" && !c.Admin.InitialAdmin.Password.IsSet() {
		bad("admin.initial_admin 写了 username，也要写 password")
	}

	// 日志
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		bad("log.level 只能是 debug、info、warn、error")
	}
	switch c.Log.Access.Output {
	case "stdout", "off":
	case "file":
		if c.Log.Access.Path == "" {
			bad("log.access.output 是 file 时要写 log.access.path")
		}
		if c.Log.Access.MaxSize < 1<<20 {
			bad("log.access.max_size 至少 1MiB")
		}
	default:
		bad("log.access.output 只能是 stdout、file、off")
	}
	return errs
}

func hasPortEntrance(c *Config) bool {
	for _, e := range c.Entrances {
		if e.Listen != "" {
			return true
		}
	}
	return false
}

func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
}

// checkImagePattern 要求规则写全上游，并且上游是已配置的，避免写成 library/nginx 却以为禁掉了。
func checkImagePattern(p string, c *Config) error {
	reg, rest, ok := strings.Cut(p, "/")
	if !ok || rest == "" {
		return fmt.Errorf("%q 要写全上游，如 docker.io/library/nginx、ghcr.io/owner/**", p)
	}
	if reg != "*" {
		if _, ok := c.Upstreams[strings.ToLower(reg)]; !ok {
			return fmt.Errorf("%q 的上游 %s 没有配置", p, reg)
		}
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == "" {
			return fmt.Errorf("%q 里有空的段", p)
		}
		if strings.Contains(seg, "**") && seg != "**" {
			return fmt.Errorf("%q：** 要单独作为一段", p)
		}
	}
	return nil
}

// Port 返回监听地址里的端口号，解析失败时返回 0。
func Port(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}
