// Package config defines the configuration of registry-mirrors-relay: YAML parsing, built-in upstreams, defaults
// and validation.
//
// Configuration comes in two kinds. The static configuration here is governed by the file (suited to GitOps).
// Operational data such as client accounts and rules can be written here too, or edited in the admin console and
// stored in the operational database; the two are merged.
package config

// Config is the complete static configuration. See Defaults for the default values of its fields.
type Config struct {
	// DataDir holds the operational database and ACME certificates, never any image data.
	DataDir   string               `yaml:"data_dir"`
	Listen    Listen               `yaml:"listen"`
	TLS       TLS                  `yaml:"tls"`
	ClientIP  ClientIP             `yaml:"client_ip"`
	Site      Site                 `yaml:"site"`
	Entrances []Entrance           `yaml:"entrances"`
	Upstreams map[string]*Upstream `yaml:"-"` // merged by Load on top of the built-in upstreams, see mergeUpstreams
	Access    Access               `yaml:"access"`
	Limits    Limits               `yaml:"limits"`
	Cache     Cache                `yaml:"cache"`
	Timeouts  Timeouts             `yaml:"timeouts"`
	Proxy     Proxy                `yaml:"proxy"`
	Admin     Admin                `yaml:"admin"`
	Log       Log                  `yaml:"log"`
	Metrics   Metrics              `yaml:"metrics"`
}

// Listen holds the listen addresses; an empty address means that listener is off.
type Listen struct {
	HTTP  string `yaml:"http"`  // pull entrance (plain HTTP), for use behind a CDN or reverse proxy
	HTTPS string `yaml:"https"` // pull entrance (TLS), used when tls.mode is not off
	// Admin is a separate management port: the admin console, the admin API and metrics are served here and no
	// longer on the pull port. When empty (the default) they share the port with the pull entrance. Who may reach
	// them is set by admin.public and admin.allow.
	Admin string `yaml:"admin"`
}

// TLS decides where the certificates for the HTTPS entrance come from.
type TLS struct {
	Mode     string `yaml:"mode"` // off | files | acme
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	ACME     ACME   `yaml:"acme"`
}

// ACME obtains certificates automatically with HTTP-01 or TLS-ALPN-01; DNS-01 is not supported.
type ACME struct {
	Email        string   `yaml:"email"`
	CacheDir     string   `yaml:"cache_dir"`     // default <data_dir>/acme
	Hosts        []string `yaml:"hosts"`         // default: every domain named in the entrances
	DirectoryURL string   `yaml:"directory_url"` // default: Let's Encrypt production
}

// ClientIP decides how the real client IP is determined.
type ClientIP struct {
	// TrustedProxies are the only sources whose X-Forwarded-For is trusted; the first untrusted address from the
	// right is taken. Entries may be CIDRs, single IPs, or the keywords private_ranges and loopback. Empty by
	// default: no source is trusted and the TCP source address is used.
	TrustedProxies []string `yaml:"trusted_proxies"`
	UseXRealIP     bool     `yaml:"use_x_real_ip"` // prefer X-Real-IP when the request comes from a trusted proxy
}

// Site is the site information shown on the usage page and in the admin console.
type Site struct {
	Name     string `yaml:"name"`
	Operator string `yaml:"operator"`
	// Brand is a brand of the OpenLinkOS design system (ui.Brands), for OpenLinkOS and LinkOS services: its
	// colours and marks on the usage page and in the console. Empty is the neutral look.
	Brand string            `yaml:"brand"`
	Links map[string]string `yaml:"links"` // home, status, terms, privacy, abuse, source
	// ShowStatus shows the service status on the usage page (each upstream's reachability and availability over
	// the last 24 hours), with the data from /status.json. It holds only what is safe to publish: no raw errors,
	// addresses, quotas or request counts.
	ShowStatus bool `yaml:"show_status"`
}

// Entrance is a pull entrance, matched by Host.
//   - With Upstream: the entrance always forwards to that upstream (one subdomain per upstream).
//   - Without Upstream: a single-domain entrance that picks the upstream by path prefix (/v2/ghcr.io/...) or by
//     containerd's ns parameter, and uses DefaultUpstream when there is neither (that is what Docker's
//     registry-mirrors sends).
type Entrance struct {
	Name            string   `yaml:"name"`
	Hosts           []string `yaml:"hosts"` // the Host values to match (without port); "*" matches any Host
	Upstream        string   `yaml:"upstream"`
	DefaultUpstream string   `yaml:"default_upstream"`
	Auth            string   `yaml:"auth"` // none | basic
	// Page is the usage page shown when a browser opens this entrance. public (the default): anyone can see it,
	// and with auth: basic only pulls need a sign-in; login: the page needs a sign-in too (only together with
	// auth: basic); off: no usage page.
	Page string `yaml:"page"`
}

// Upstream is an upstream registry. Its name (the map key) is how it is written in image references, such as
// docker.io or ghcr.io.
type Upstream struct {
	Registry      string   `yaml:"registry"`       // the host actually contacted, such as registry-1.docker.io
	Aliases       []string `yaml:"aliases"`        // names also recognised in path prefixes and ns
	LibraryPrefix bool     `yaml:"library_prefix"` // add library/ to single-segment names (Docker Hub)
	// RedirectHosts are the redirect target hosts the server may follow (regular expressions, automatically
	// anchored with ^(?: )$, case-insensitive).
	RedirectHosts []string `yaml:"redirect_hosts"`
	// ExtraRedirectHosts is appended to RedirectHosts. When overriding a built-in upstream, writing this saves
	// copying the built-in allowlist again.
	ExtraRedirectHosts []string `yaml:"extra_redirect_hosts"`
	// TokenRealmHosts are the token service hosts that may be contacted (regular expressions). When the realm
	// given by the upstream is not among them, no token is fetched.
	TokenRealmHosts  []string     `yaml:"token_realm_hosts"`
	UnlistedRedirect string       `yaml:"unlisted_redirect"` // redirects off the allowlist: pass (to the client as is) | deny (return 502)
	Credentials      *Credentials `yaml:"credentials"`
	Disabled         bool         `yaml:"disabled"`
	// TLSCAFile is an extra CA (PEM) to trust, for proxying a registry that uses a private CA. The system CAs
	// remain trusted.
	TLSCAFile string `yaml:"tls_ca_file"`
	// AllowPrivateNetwork lets this upstream connect to private network addresses (refused by default). Turn it
	// on only to proxy an internal registry or for load testing.
	AllowPrivateNetwork bool `yaml:"allow_private_network"`
}

// Credentials is the upstream account used when fetching tokens, to raise the quota on upstreams such as Docker
// Hub. With an account configured, every client can also pull the private repositories the account can read, so
// public_only: true must be written to confirm it.
type Credentials struct {
	Username   string `yaml:"username"`
	Password   Secret `yaml:"password"`
	RealmHost  string `yaml:"realm_host"`  // send the account only to this token service host
	PublicOnly bool   `yaml:"public_only"` // confirms that the account can read only public repositories
}

// Access is access control. What is written here is merged with what is edited in the admin console.
type Access struct {
	UsersFile string     `yaml:"users_file"` // htpasswd file, bcrypt only
	IPAllow   []string   `yaml:"ip_allow"`   // when non-empty, only these addresses are allowed
	IPDeny    []string   `yaml:"ip_deny"`
	Images    ImageRules `yaml:"images"`
}

// ImageRules are the image lists. Names are written in full, with the upstream: docker.io/library/nginx,
// ghcr.io/owner/**. * matches any characters within one segment, ** matches any number of segments.
type ImageRules struct {
	Allow []string    `yaml:"allow"` // when non-empty, only matching images are allowed
	Deny  []ImageRule `yaml:"deny"`
}

// ImageRule is a deny rule.
type ImageRule struct {
	Pattern string `yaml:"pattern"`
	Status  int    `yaml:"status"` // 403 or 451, default 403
	Message string `yaml:"message"`
}

// Limits are the rate limits and concurrency caps. Clients are counted by sign-in user name, or by IP when not
// signed in (IPv6 by /64).
type Limits struct {
	ManifestRate         float64  `yaml:"manifest_rate"`  // manifest requests per second per client, 0 means no limit
	ManifestBurst        int      `yaml:"manifest_burst"` // allowed burst
	HeadWeight           float64  `yaml:"head_weight"`    // how many requests one HEAD counts as
	BlobStreamsPerClient int      `yaml:"blob_streams_per_client"`
	MaxInflight          int      `yaml:"max_inflight"`
	MaxBlobStreams       int      `yaml:"max_blob_streams"`
	MaxManifestBuffers   int      `yaml:"max_manifest_buffers"`
	TrackedClients       int      `yaml:"tracked_clients"` // how many clients the rate limit table remembers at most (LRU)
	AuthFailuresPerMin   int      `yaml:"auth_failures_per_min"`
	ExemptCIDRs          []string `yaml:"exempt_cidrs"`
}

// Cache is the in-memory manifest cache; it holds only manifests, never layers.
type Cache struct {
	MaxBytes     Size     `yaml:"max_bytes"` // 0 turns the cache off
	DigestTTL    Duration `yaml:"digest_ttl"`
	TagTTL       Duration `yaml:"tag_ttl"`
	StaleIfError Duration `yaml:"stale_if_error"` // how long stale content may still be served when the upstream fails
	NegativeTTL  Duration `yaml:"negative_ttl"`   // how long a 404 is remembered
}

// Timeouts are the various timeouts. There is no overall write timeout: a large layer may take a long time to
// download, as long as it keeps making progress.
type Timeouts struct {
	ReadHeader       Duration `yaml:"read_header"`
	Idle             Duration `yaml:"idle"`
	WriteIdle        Duration `yaml:"write_idle"` // disconnect when writing to the client stalls for this long
	Dial             Duration `yaml:"dial"`
	TLSHandshake     Duration `yaml:"tls_handshake"`
	ResponseHeader   Duration `yaml:"response_header"`
	UpstreamReadIdle Duration `yaml:"upstream_read_idle"` // give up when reading from the upstream stalls for this long
	Token            Duration `yaml:"token"`
	ShutdownGrace    Duration `yaml:"shutdown_grace"`
}

// Proxy holds the details of forwarding behaviour.
type Proxy struct {
	UserAgent               string `yaml:"user_agent"` // User-Agent sent to upstreams, default registry-mirrors-relay/<version>
	MaxIdleConnsPerHost     int    `yaml:"max_idle_conns_per_host"`
	MaxRedirects            int    `yaml:"max_redirects"`
	BufferSize              Size   `yaml:"buffer_size"`               // forwarding buffer per stream, default 16KiB
	ExposeUpstreamRatelimit bool   `yaml:"expose_upstream_ratelimit"` // pass the upstream's Ratelimit-* headers on to clients
	CDNCacheDigests         bool   `yaml:"cdn_cache_digests"`         // let CDNs cache by-digest responses for a long time
	// BlobHTTP2 sends layers over HTTP/2 as well. The default is HTTP/1.1: HTTP/2 accepts up to several MB of
	// not-yet-read data per stream in advance, so with slow clients memory grows linearly with concurrency; with
	// HTTP/1.1, TCP provides back-pressure and each stream takes only one forwarding buffer in the process.
	// Manifests and tokens still use HTTP/2.
	BlobHTTP2 bool `yaml:"blob_http2"`
	// Circuit breaker: after BreakerFailures consecutive connection failures or timeouts, requests fail at once
	// for BreakerCooldown, after which one request is let through as a probe. If the probe fails too, the
	// cooldown doubles, up to 1 minute. BreakerFailures 0 means no circuit breaker.
	BreakerFailures int      `yaml:"breaker_failures"`
	BreakerCooldown Duration `yaml:"breaker_cooldown"`
	// HealthInterval is the interval at which each upstream's /v2/ is probed (not counted as pulls); the results
	// are shown in the admin console, the metrics and the usage page's /status.json, and do not affect readyz. 0
	// turns probing off.
	HealthInterval Duration `yaml:"health_interval"`
}

// Admin is the admin console.
type Admin struct {
	// PublicURL is the console's canonical address (optional), used for cookies, origin checks and
	// proxied_allow.
	PublicURL string `yaml:"public_url"`
	// Public opens the admin console and the admin API to every address (the default), including through a CDN
	// or reverse proxy, protected by sign-in: bcrypt passwords, sign-in rate limits, CSRF checks and
	// re-authentication for sensitive actions. With false, only addresses in Allow and ProxiedAllow get an
	// answer. Metrics and pprof have no sign-in, are not affected by it, and always follow Allow and ProxiedAllow.
	Public bool `yaml:"public"`
	// Allow lists the peer addresses let in on a direct connection (no proxy headers in the request). It applies
	// to metrics, pprof, and the console and API when public: false. Defaults to loopback and private_ranges, so
	// kubectl port-forward, SSH tunnels and in-cluster Prometheus all work, while requests through a CDN or
	// reverse proxy are treated as non-existent (404). An empty list turns it off.
	Allow []string `yaml:"allow"`
	// ProxiedAllow lists the client addresses let in through a reverse proxy (taken from a trusted proxy's
	// X-Forwarded-For), for the same uses as Allow. Empty by default; using it also requires public_url and
	// client_ip.trusted_proxies.
	ProxiedAllow []string `yaml:"proxied_allow"`
	// ConfigEdit allows changing the configuration in the console (entrances, upstreams, rate limits, cache and
	// more, stored in the operational database on top of the configuration file). On by default; with false,
	// console overrides never take effect and can only be reset, and the configuration file alone decides the
	// configuration (suited to strict GitOps).
	ConfigEdit   bool         `yaml:"config_edit"`
	Store        Store        `yaml:"store"`
	InitialAdmin InitialAdmin `yaml:"initial_admin"`
	// APITokens are admin API tokens written in the configuration (suited to automation and k8s Secrets). They
	// can also be created in the console and stored in the operational database.
	APITokens []APIToken `yaml:"api_tokens"`
}

// APIToken is an admin API token, sent in Authorization: Bearer with requests.
type APIToken struct {
	Name  string `yaml:"name"`
	Token Secret `yaml:"token"` // at least 32 characters
	Role  string `yaml:"role"`  // admin (read-write) | viewer (read-only)
}

// Store is how operational data is stored.
type Store struct {
	Driver string `yaml:"driver"` // bolt (single instance, writable) | none (read-only, all from the config file; for multi-replica k8s)
	Path   string `yaml:"path"`   // default <data_dir>/state.db
}

// InitialAdmin creates the first administrator while the operational database has none. This administrator must
// change the password after the first sign-in, so the password in the configuration works only once; afterwards
// initial_admin and its secret should be deleted. With read-only operational data (store: none), it is the
// console's only administrator, and its password can only be changed in the configuration.
type InitialAdmin struct {
	Username string `yaml:"username"`
	Password Secret `yaml:"password"`
	FromEnv  bool   `yaml:"-"` // from RELAY_ADMIN_PASSWORD_FILE, not the configuration file
}

// Log holds the logging settings.
type Log struct {
	Level  string    `yaml:"level"` // debug | info | warn | error
	Access AccessLog `yaml:"access"`
}

// AccessLog is the access log, with the same fields as Caddy's JSON access log.
type AccessLog struct {
	Output  string `yaml:"output"` // stdout | file | off
	Path    string `yaml:"path"`
	MaxSize Size   `yaml:"max_size"` // files rotate by size
	Keep    int    `yaml:"keep"`     // how many old files to keep
}

// Metrics covers Prometheus metrics and the debug endpoints, served on the same entrance as the admin console
// (and likewise restricted by admin.allow).
type Metrics struct {
	Enabled bool `yaml:"enabled"` // serve /metrics; off by default
	Pprof   bool `yaml:"pprof"`   // also serve /debug/pprof/; needs enabled
}
