package config

import (
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/openlinkos/registry-mirrors-relay/internal/i18n"
	"github.com/openlinkos/registry-mirrors-relay/internal/ui"
)

// CompileHostPatterns joins the host name patterns into one anchored, case-insensitive expression.
// Before matching, convert the host name to lower case and strip any trailing dot.
func CompileHostPatterns(patterns []string) (*regexp.Regexp, error) {
	if len(patterns) == 0 {
		return regexp.MustCompile(`^\b$`), nil // matches nothing
	}
	parts := make([]string, len(patterns))
	for i, p := range patterns {
		if _, err := regexp.Compile(p); err != nil {
			return nil, i18n.Errorf("pattern %q: %w", p, err)
		}
		parts[i] = "(?:" + p + ")"
	}
	return regexp.Compile(`(?i)^(?:` + strings.Join(parts, "|") + `)$`)
}

// ParsePrefixes parses CIDRs, single IPs and the keywords private_ranges and loopback.
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
			return nil, i18n.Errorf("%q is not an IP address or CIDR range", raw)
		}
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// MaxACMEHosts is the maximum number of domains ACME obtains certificates for: entrances can be changed in the
// console, so their number needs a cap to avoid hitting the certificate issuance rate limits.
const MaxACMEHosts = 100

var (
	// accountRe is the syntax of account and token names (shared by client accounts, console administrators and
	// API tokens)
	accountRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	upstreamNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(:[0-9]{1,5})?$`)
	hostRe         = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
)

func validate(c *Config, res *Result) []error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, i18n.Errorf(format, a...)) }
	warn := func(format string, a ...any) { res.Warnings = append(res.Warnings, i18n.M(format, a...)) }

	// Listen addresses
	for name, addr := range map[string]string{
		"listen.http": c.Listen.HTTP, "listen.https": c.Listen.HTTPS, "listen.admin": c.Listen.Admin,
	} {
		if addr == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			bad("%s %q is not a valid address; write it like :8080 or 127.0.0.1:9090", name, addr)
		}
	}
	if c.Listen.HTTP == "" && c.Listen.HTTPS == "" {
		bad("neither listen.http nor listen.https is set, so clients have nowhere to connect")
	}
	if c.Listen.Admin != "" && (c.Listen.Admin == c.Listen.HTTP || c.Listen.Admin == c.Listen.HTTPS) {
		bad("listen.admin uses the same address as the pull entrance; to share the pull port, leave listen.admin empty")
	}

	// TLS
	switch c.TLS.Mode {
	case "off":
		if c.Listen.HTTPS != "" {
			bad("listen.https is set but tls.mode is off")
		}
	case "files":
		if c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
			bad("tls.mode files needs tls.cert_file and tls.key_file")
		}
		if c.Listen.HTTPS == "" {
			bad("tls.mode files needs listen.https")
		}
	case "acme":
		if c.Listen.HTTPS == "" {
			bad("tls.mode acme needs listen.https (usually :443)")
		}
		if len(c.TLS.ACME.Hosts) == 0 {
			bad("tls.mode acme needs explicit host names: name them in the entrances' hosts or in tls.acme.hosts")
		}
		if len(c.TLS.ACME.Hosts) > MaxACMEHosts {
			bad("tls.mode acme requests certificates for at most %d host names, but there are %d", MaxACMEHosts, len(c.TLS.ACME.Hosts))
		}
		for _, h := range c.TLS.ACME.Hosts {
			if h == "*" || !hostRe.MatchString(h) {
				bad("%q in tls.acme.hosts is not a host name a certificate can be issued for", h)
			}
		}
		if c.Listen.HTTP == "" {
			warn("tls.mode is acme but listen.http is off: only the TLS-ALPN-01 challenge works, which needs listen.https on public port 443")
		}
	default:
		bad("tls.mode must be off, files or acme, not %q", c.TLS.Mode)
	}

	// Client IP
	trusted, err := ParsePrefixes(c.ClientIP.TrustedProxies)
	if err != nil {
		bad("client_ip.trusted_proxies: %v", err)
	}
	for _, p := range trusted {
		if p.Bits() == 0 {
			bad("%s in client_ip.trusted_proxies trusts every address, so client IPs can be forged at will", p)
		}
	}
	if len(c.ClientIP.TrustedProxies) == 0 && c.Listen.HTTP != "" {
		warn("client_ip.trusted_proxies is empty: the client IP is the TCP peer address. Behind a CDN or reverse proxy, list the address ranges it connects from")
	}

	// Upstreams
	names := make([]string, 0, len(c.Upstreams))
	for n := range c.Upstreams {
		names = append(names, n)
	}
	sort.Strings(names)
	aliasOwner := map[string]string{}
	for _, n := range names {
		u := c.Upstreams[n]
		if !upstreamNameRe.MatchString(n) {
			bad("upstream name %q must be written as the registry in image names, such as docker.io or ghcr.io (lowercase, with at least one dot)", n)
		}
		if u.Registry == "" {
			bad("upstream %s has no registry", n)
		} else if !upstreamNameRe.MatchString(u.Registry) && !hostRe.MatchString(u.Registry) {
			bad("upstream %s: registry %q must be a host name, without https:// or a path", n, u.Registry)
		}
		for _, a := range append([]string{n}, u.Aliases...) {
			if other, ok := aliasOwner[a]; ok && other != n {
				bad("name %q belongs to both upstream %s and %s", a, other, n)
			}
			aliasOwner[a] = n
		}
		if _, err := CompileHostPatterns(append(append([]string{}, u.RedirectHosts...), u.ExtraRedirectHosts...)); err != nil {
			bad("upstream %s: redirect allowlist: %v", n, err)
		}
		if _, err := CompileHostPatterns(u.TokenRealmHosts); err != nil {
			bad("upstream %s: token_realm_hosts: %v", n, err)
		}
		if len(u.TokenRealmHosts) == 0 {
			warn("upstream %s has no token_realm_hosts: pulls fail when it asks for a token", n)
		}
		if u.UnlistedRedirect != "pass" && u.UnlistedRedirect != "deny" {
			bad("upstream %s: unlisted_redirect must be pass or deny", n)
		}
		if u.AllowPrivateNetwork {
			warn("upstream %s may connect to private addresses: do this only for a private registry or for testing", n)
		}
		if u.TLSCAFile != "" {
			if _, err := os.Stat(u.TLSCAFile); err != nil {
				bad("upstream %s: tls_ca_file: %v", n, err)
			}
		}
		if cr := u.Credentials; cr != nil && !u.Disabled {
			if cr.Username == "" || !cr.Password.IsSet() {
				bad("upstream %s: credentials need a username and a password", n)
			}
			if cr.RealmHost == "" {
				bad("upstream %s: credentials need a realm_host, the only token service the account is sent to", n)
			}
			if !cr.PublicOnly {
				bad("upstream %s has credentials: every client could pull the private repositories this account can read. Once you are sure it can read only public repositories, set public_only: true", n)
			} else {
				warn("upstream %s has credentials: make sure it is a token that can read only public repositories (such as Docker Hub's Public Repo Read-only) and belongs to no organisation", n)
			}
			if cr.Password.Value != "" {
				warn("upstream %s: the password is written in the configuration; use env or file instead", n)
			}
		}
	}

	// Entrances
	seen := map[string]i18n.Message{}
	if len(c.Entrances) == 0 {
		bad("at least one entrance is needed (entrances)")
	}
	for _, e := range c.Entrances {
		where := i18n.M("entrance %s", e.Name)
		if len(e.Hosts) == 0 {
			bad(`%s needs hosts ("*" means any host name)`, where)
		}
		for _, h := range e.Hosts {
			if h != "*" && !hostRe.MatchString(h) {
				bad("%s: host %q is not a valid host name (leave out the port and scheme)", where, h)
			}
			if other, ok := seen[h]; ok {
				bad("%s and %s both match %q", other, where, h)
			}
			seen[h] = where
		}
		for _, ref := range []struct{ field, name string }{{"upstream", e.Upstream}, {"default_upstream", e.DefaultUpstream}} {
			if ref.name == "" {
				continue
			}
			u, ok := c.Upstreams[ref.name]
			switch {
			case !ok:
				bad("%s: %s %q is not a configured upstream", where, ref.field, ref.name)
			case u.Disabled:
				bad("%s: %s %q is disabled", where, ref.field, ref.name)
			}
		}
		if e.Upstream != "" && e.DefaultUpstream != "" {
			bad("%s sets both upstream and default_upstream; an entrance with a fixed upstream needs no default upstream", where)
		}
		switch e.Auth {
		case "none":
		case "basic":
			if c.Access.UsersFile == "" && c.Admin.Store.Driver == "none" {
				warn("%s requires sign-in, but there is no access.users_file and the operational data is read-only: no account can sign in", where)
			}
		default:
			bad("%s: auth must be none or basic", where)
		}
		switch e.Page {
		case "public", "off":
		case "login":
			if e.Auth != "basic" {
				bad("%s: page: login needs auth: basic; without accounts nobody could see the usage page", where)
			}
		default:
			bad("%s: page must be public, login or off", where)
		}
	}

	if c.Site.Brand != "" && !ui.IsBrand(c.Site.Brand) {
		var ids []string
		for _, b := range ui.Brands() {
			ids = append(ids, b.ID)
		}
		bad("site.brand %q is not a brand of the design system; leave it empty, or use one of: %s", c.Site.Brand, strings.Join(ids, ", "))
	}

	// Access control
	if _, err := ParsePrefixes(c.Access.IPAllow); err != nil {
		bad("access.ip_allow: %v", err)
	}
	if _, err := ParsePrefixes(c.Access.IPDeny); err != nil {
		bad("access.ip_deny: %v", err)
	}
	if _, err := ParsePrefixes(c.Limits.ExemptCIDRs); err != nil {
		bad("limits.exempt_cidrs: %v", err)
	}
	for _, p := range c.Access.Images.Allow {
		if err := checkImagePattern(p, c); err != nil {
			bad("access.images.allow: %v", err)
		}
	}
	for i := range c.Access.Images.Deny {
		r := &c.Access.Images.Deny[i]
		if err := checkImagePattern(r.Pattern, c); err != nil {
			bad("access.images.deny: %v", err)
		}
		if r.Status == 0 {
			r.Status = 403
		}
		if r.Status != 403 && r.Status != 451 {
			bad("access.images.deny: the status of %q must be 403 or 451", r.Pattern)
		}
	}

	// Rate limits, cache, timeouts
	l := c.Limits
	for name, v := range map[string]float64{
		"limits.manifest_rate": l.ManifestRate, "limits.head_weight": l.HeadWeight,
		"limits.manifest_burst": float64(l.ManifestBurst), "limits.blob_streams_per_client": float64(l.BlobStreamsPerClient),
		"limits.max_inflight": float64(l.MaxInflight), "limits.max_blob_streams": float64(l.MaxBlobStreams),
		"limits.max_manifest_buffers": float64(l.MaxManifestBuffers), "limits.tracked_clients": float64(l.TrackedClients),
		"limits.auth_failures_per_min": float64(l.AuthFailuresPerMin),
	} {
		if v < 0 {
			bad("%s cannot be negative", name)
		}
	}
	if l.ManifestRate > 0 && l.ManifestBurst < 1 {
		bad("limits.manifest_burst must be at least 1 when limits.manifest_rate is above 0")
	}
	if l.TrackedClients == 0 {
		bad("limits.tracked_clients must be at least 1")
	}
	if c.Cache.MaxBytes > 0 && c.Cache.TagTTL > c.Cache.DigestTTL {
		bad("cache.tag_ttl cannot be longer than cache.digest_ttl")
	}
	if c.Proxy.BreakerFailures < 0 || (c.Proxy.BreakerFailures > 0 && c.Proxy.BreakerCooldown.D() <= 0) {
		bad("proxy.breaker_failures cannot be negative, and when it is above 0, proxy.breaker_cooldown must be too")
	}
	if c.Proxy.MaxRedirects < 0 || c.Proxy.MaxRedirects > 10 {
		bad("proxy.max_redirects must be between 0 and 10")
	}
	if c.Proxy.BufferSize < 4<<10 || c.Proxy.BufferSize > 1<<20 {
		bad("proxy.buffer_size must be between 4KiB and 1MiB")
	}
	if c.Timeouts.WriteIdle.D() <= 0 || c.Timeouts.UpstreamReadIdle.D() <= 0 || c.Timeouts.ReadHeader.D() <= 0 {
		bad("timeouts.write_idle, upstream_read_idle and read_header must all be above 0")
	}

	// Admin console
	switch c.Admin.Store.Driver {
	case "bolt", "none":
	default:
		bad("admin.store.driver must be bolt or none")
	}
	if c.Admin.PublicURL != "" {
		u, err := url.Parse(c.Admin.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
			bad("admin.public_url must look like https://admin.example.com, without a path")
		} else if u.Scheme == "http" && !isLoopbackListen(net.JoinHostPort(u.Hostname(), "80")) {
			warn("admin.public_url uses http: console passwords and sessions would cross the network in clear text; put it behind an https reverse proxy")
		}
	}
	// Who may reach the management entrance (console, API, metrics)
	for _, l := range []struct {
		name string
		list []string
	}{{"admin.allow", c.Admin.Allow}, {"admin.proxied_allow", c.Admin.ProxiedAllow}} {
		ps, err := ParsePrefixes(l.list)
		if err != nil {
			bad("%s: %v", l.name, err)
		}
		for _, p := range ps {
			if p.Bits() == 0 {
				warn("%s: %s allows every address, so only passwords protect the console", l.name, p)
			}
		}
	}
	if len(c.Admin.Allow) == 0 && len(c.Admin.ProxiedAllow) == 0 {
		if c.Admin.Public {
			warn("admin.allow is empty: nobody can reach metrics and pprof")
		} else {
			warn("admin.allow is empty: the console, the admin API and metrics are closed to everyone")
		}
	}
	if c.Admin.Public && c.TLS.Mode == "off" && len(trusted) == 0 && !strings.HasPrefix(c.Admin.PublicURL, "https://") {
		warn("the console is open to every address (admin.public), but the service has no HTTPS and no reverse proxy is configured " +
			"(client_ip.trusted_proxies): passwords would cross the network in clear text. Put it behind HTTPS, or set " +
			"admin.public: false to keep it to the local machine and the LAN")
	}
	if len(c.Admin.ProxiedAllow) > 0 {
		if c.Admin.PublicURL == "" {
			bad("admin.proxied_allow needs admin.public_url: proxied access is accepted only on that host name")
		}
		if len(trusted) == 0 {
			bad("admin.proxied_allow needs client_ip.trusted_proxies: only client addresses passed on by a trusted proxy can be believed")
		}
	}
	if c.Metrics.Pprof && !c.Metrics.Enabled {
		warn("metrics.pprof has no effect unless metrics.enabled is on")
	}
	seenTok := map[string]bool{}
	for i := range c.Admin.APITokens {
		tk := &c.Admin.APITokens[i]
		if tk.Name == "" || seenTok[tk.Name] {
			bad("admin.api_tokens item %d: the name must be set and unique", i+1)
		}
		seenTok[tk.Name] = true
		if tk.Role == "" {
			tk.Role = "viewer"
		}
		if tk.Role != "admin" && tk.Role != "viewer" {
			bad("admin.api_tokens: the role of %s must be admin or viewer", tk.Name)
		}
		if !tk.Token.IsSet() {
			bad("admin.api_tokens: %s has no token", tk.Name)
		}
	}
	if c.Admin.InitialAdmin.Username != "" && !c.Admin.InitialAdmin.Password.IsSet() {
		bad("admin.initial_admin has a username but no password")
	}
	if c.Admin.InitialAdmin.Password.Value != "" {
		warn("the admin.initial_admin password is written in the configuration: configuration files are often copied, committed or printed, so use file (a Docker or Kubernetes secret) instead. The password must be changed at the first sign-in; remove initial_admin afterwards")
	}
	if c.Admin.InitialAdmin.Username != "" && !accountRe.MatchString(c.Admin.InitialAdmin.Username) {
		bad("admin.initial_admin.username may contain only letters, digits, dots, underscores and hyphens, up to 64 characters")
	}

	// Logs
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		bad("log.level must be debug, info, warn or error")
	}
	switch c.Log.Access.Output {
	case "stdout", "off":
	case "file":
		if c.Log.Access.Path == "" {
			bad("log.access.output file needs log.access.path")
		}
		if c.Log.Access.MaxSize < 1<<20 {
			bad("log.access.max_size must be at least 1MiB")
		}
	default:
		bad("log.access.output must be stdout, file or off")
	}
	return errs
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

// checkImagePattern requires a rule to name its upstream in full, and the upstream to be configured, so that
// nobody writes library/nginx believing it is denied.
func checkImagePattern(p string, c *Config) error {
	reg, rest, ok := strings.Cut(p, "/")
	if !ok || rest == "" {
		return i18n.Errorf("%q must start with the upstream, such as docker.io/library/nginx or ghcr.io/owner/**", p)
	}
	if reg != "*" {
		if _, ok := c.Upstreams[strings.ToLower(reg)]; !ok {
			return i18n.Errorf("%q: upstream %s is not configured", p, reg)
		}
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == "" {
			return i18n.Errorf("%q has an empty segment", p)
		}
		if strings.Contains(seg, "**") && seg != "**" {
			return i18n.Errorf("%q: ** must be a segment of its own", p)
		}
	}
	return nil
}

// Port returns the port number in a listen address, or 0 if it cannot be parsed.
func Port(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// ValidAccountName reports whether s is a valid name for a client account, administrator or token.
func ValidAccountName(s string) bool { return accountRe.MatchString(s) }
