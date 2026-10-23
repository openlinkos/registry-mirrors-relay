// Package config 定义 registry-mirrors-relay 的配置：YAML 解析、内置上游、默认值和校验。
//
// 配置分两类：这里的静态配置以文件为准（适合 GitOps）；客户端账号、规则等运营数据可以写在这里，
// 也可以在管理后台里编辑，存进运营数据库，两边合并生效。
package config

// Config 是完整的静态配置。字段的默认值见 Defaults。
type Config struct {
	// DataDir 存放运营数据库和 ACME 证书，不存放任何镜像数据。
	DataDir   string               `yaml:"data_dir"`
	Listen    Listen               `yaml:"listen"`
	TLS       TLS                  `yaml:"tls"`
	ClientIP  ClientIP             `yaml:"client_ip"`
	Site      Site                 `yaml:"site"`
	Entrances []Entrance           `yaml:"entrances"`
	Upstreams map[string]*Upstream `yaml:"-"` // 由 Load 在内置上游的基础上合并，见 mergeUpstreams
	Access    Access               `yaml:"access"`
	Limits    Limits               `yaml:"limits"`
	Cache     Cache                `yaml:"cache"`
	Timeouts  Timeouts             `yaml:"timeouts"`
	Proxy     Proxy                `yaml:"proxy"`
	Admin     Admin                `yaml:"admin"`
	Log       Log                  `yaml:"log"`
	Metrics   Metrics              `yaml:"metrics"`
}

// Listen 是各个监听地址，留空表示不开。
type Listen struct {
	HTTP    string `yaml:"http"`    // 拉取入口（明文），放在 CDN 或反向代理之后时用
	HTTPS   string `yaml:"https"`   // 拉取入口（TLS），tls.mode 不为 off 时用
	Admin   string `yaml:"admin"`   // 管理后台，默认只在本机
	Metrics string `yaml:"metrics"` // Prometheus 指标，默认只在本机
}

// TLS 决定 HTTPS 入口的证书来源。
type TLS struct {
	Mode     string `yaml:"mode"` // off | files | acme
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	ACME     ACME   `yaml:"acme"`
}

// ACME 用 HTTP-01 或 TLS-ALPN-01 自动申请证书，不支持 DNS-01。
type ACME struct {
	Email        string   `yaml:"email"`
	CacheDir     string   `yaml:"cache_dir"`     // 默认 <data_dir>/acme
	Hosts        []string `yaml:"hosts"`         // 默认取所有入口里写明的域名
	DirectoryURL string   `yaml:"directory_url"` // 默认 Let's Encrypt 正式环境
}

// ClientIP 决定怎样得到客户端的真实 IP。
type ClientIP struct {
	// TrustedProxies 只信任这些来源发来的 X-Forwarded-For，从右往左取第一个不受信任的地址。
	// 可以写 CIDR、单个 IP，或者关键字 private_ranges、loopback。默认为空：不信任任何来源，直接用 TCP 来源地址。
	TrustedProxies []string `yaml:"trusted_proxies"`
	UseXRealIP     bool     `yaml:"use_x_real_ip"` // 来自受信任代理时，优先用 X-Real-IP
}

// Site 是用法页和管理后台上显示的站点信息。
type Site struct {
	Name     string            `yaml:"name"`
	Operator string            `yaml:"operator"`
	Links    map[string]string `yaml:"links"` // home、status、terms、privacy、abuse、source
}

// Entrance 是一个拉取入口。
//   - 写了 Upstream：这个入口固定转发到该上游（每个上游一个子域名，或每个上游一个端口）。
//   - 没写 Upstream：单域名入口，按路径前缀（/v2/ghcr.io/...）、containerd 的 ns 参数选上游，
//     都没有时用 DefaultUpstream（Docker 的 registry-mirrors 发来的就是这种请求）。
type Entrance struct {
	Name            string   `yaml:"name"`
	Hosts           []string `yaml:"hosts"`  // 匹配的 Host（不含端口），"*" 匹配任意 Host
	Listen          string   `yaml:"listen"` // 按端口区分时的独立监听地址，如 ":5003"；留空表示用 listen.http/https
	Upstream        string   `yaml:"upstream"`
	DefaultUpstream string   `yaml:"default_upstream"`
	Auth            string   `yaml:"auth"` // none | basic
}

// Upstream 是一个上游 registry。名字（map 的键）是镜像地址里的写法，如 docker.io、ghcr.io。
type Upstream struct {
	Registry      string   `yaml:"registry"`       // 实际访问的主机，如 registry-1.docker.io
	Aliases       []string `yaml:"aliases"`        // 路径前缀和 ns 里也认这些名字
	LibraryPrefix bool     `yaml:"library_prefix"` // 只有一段的名字补 library/（Docker Hub）
	// RedirectHosts 是允许服务端跟随的重定向目标主机（正则，自动加 ^(?: )$ 锚点，不区分大小写）。
	RedirectHosts []string `yaml:"redirect_hosts"`
	// ExtraRedirectHosts 追加到 RedirectHosts 后面。覆盖内置上游时，写这个就不用把内置的白名单再抄一遍。
	ExtraRedirectHosts []string `yaml:"extra_redirect_hosts"`
	// TokenRealmHosts 是允许访问的令牌服务主机（正则）。上游给出的 realm 不在其中时，不去取令牌。
	TokenRealmHosts  []string     `yaml:"token_realm_hosts"`
	UnlistedRedirect string       `yaml:"unlisted_redirect"` // 白名单外的重定向：pass（原样交给客户端）| deny（返回 502）
	Credentials      *Credentials `yaml:"credentials"`
	Disabled         bool         `yaml:"disabled"`
	// TLSCAFile 额外信任这个 CA（PEM），代理用私有 CA 的 registry 时用。系统自带的 CA 仍然信任。
	TLSCAFile string `yaml:"tls_ca_file"`
	// AllowPrivateNetwork 允许这个上游连接内网地址（默认拒绝）。只在代理内网 registry 或压测时打开。
	AllowPrivateNetwork bool `yaml:"allow_private_network"`
}

// Credentials 是取令牌时使用的上游账号，用来提高 Docker Hub 等上游的额度。
// 配了账号，这个账号能读的私有仓库也会被所有客户端拉到，所以必须写 public_only: true 确认。
type Credentials struct {
	Username   string `yaml:"username"`
	Password   Secret `yaml:"password"`
	RealmHost  string `yaml:"realm_host"`  // 只把账号发给这个令牌服务主机
	PublicOnly bool   `yaml:"public_only"` // 确认这个账号只能读公开仓库
}

// Access 是访问控制。这里写的内容和管理后台里编辑的合并生效。
type Access struct {
	UsersFile string     `yaml:"users_file"` // htpasswd 文件，只接受 bcrypt
	IPAllow   []string   `yaml:"ip_allow"`   // 非空时只允许这些地址
	IPDeny    []string   `yaml:"ip_deny"`
	Images    ImageRules `yaml:"images"`
}

// ImageRules 是镜像名单。名字写全，带上游：docker.io/library/nginx、ghcr.io/owner/**。
// * 匹配一段中的任意字符，** 匹配任意多段。
type ImageRules struct {
	Allow []string    `yaml:"allow"` // 非空时只允许匹配的镜像
	Deny  []ImageRule `yaml:"deny"`
}

// ImageRule 是一条禁止规则。
type ImageRule struct {
	Pattern string `yaml:"pattern"`
	Status  int    `yaml:"status"` // 403 或 451，默认 403
	Message string `yaml:"message"`
}

// Limits 是限流和并发上限。客户端按登录用户名计，没登录时按 IP 计（IPv6 按 /64）。
type Limits struct {
	ManifestRate         float64  `yaml:"manifest_rate"`  // 每个客户端每秒的 manifest 请求数，0 表示不限
	ManifestBurst        int      `yaml:"manifest_burst"` // 允许的突发数
	HeadWeight           float64  `yaml:"head_weight"`    // 一次 HEAD 按几次算
	BlobStreamsPerClient int      `yaml:"blob_streams_per_client"`
	MaxInflight          int      `yaml:"max_inflight"`
	MaxBlobStreams       int      `yaml:"max_blob_streams"`
	MaxManifestBuffers   int      `yaml:"max_manifest_buffers"`
	TrackedClients       int      `yaml:"tracked_clients"` // 限流表最多记住多少个客户端（LRU）
	AuthFailuresPerMin   int      `yaml:"auth_failures_per_min"`
	ExemptCIDRs          []string `yaml:"exempt_cidrs"`
}

// Cache 是 manifest 的内存缓存，只放 manifest，不放镜像层。
type Cache struct {
	MaxBytes     Size     `yaml:"max_bytes"` // 0 表示关闭
	DigestTTL    Duration `yaml:"digest_ttl"`
	TagTTL       Duration `yaml:"tag_ttl"`
	StaleIfError Duration `yaml:"stale_if_error"` // 上游出错时还能用旧内容多久
	NegativeTTL  Duration `yaml:"negative_ttl"`   // 404 记多久
}

// Timeouts 是各项超时。没有总的写超时：大层可以下载很久，只要一直有进展。
type Timeouts struct {
	ReadHeader       Duration `yaml:"read_header"`
	Idle             Duration `yaml:"idle"`
	WriteIdle        Duration `yaml:"write_idle"` // 向客户端写数据时，多久没有进展就断开
	Dial             Duration `yaml:"dial"`
	TLSHandshake     Duration `yaml:"tls_handshake"`
	ResponseHeader   Duration `yaml:"response_header"`
	UpstreamReadIdle Duration `yaml:"upstream_read_idle"` // 读上游数据时，多久没有进展就放弃
	Token            Duration `yaml:"token"`
	ShutdownGrace    Duration `yaml:"shutdown_grace"`
}

// Proxy 是转发行为的细节。
type Proxy struct {
	UserAgent               string `yaml:"user_agent"` // 发往上游的 User-Agent，默认 registry-mirrors-relay/<版本>
	MaxIdleConnsPerHost     int    `yaml:"max_idle_conns_per_host"`
	MaxRedirects            int    `yaml:"max_redirects"`
	BufferSize              Size   `yaml:"buffer_size"`               // 每个流的转发缓冲，默认 16KiB
	ExposeUpstreamRatelimit bool   `yaml:"expose_upstream_ratelimit"` // 把上游的 Ratelimit-* 头转给客户端
	CDNCacheDigests         bool   `yaml:"cdn_cache_digests"`         // 按 digest 的响应允许 CDN 长期缓存
	// BlobHTTP2 让镜像层也走 HTTP/2。默认走 HTTP/1.1：HTTP/2 会替每个流先收下最多几 MB 还没被读走的数据，
	// 客户端慢时内存随并发线性增长；HTTP/1.1 由 TCP 做背压，进程里每个流只占一块转发缓冲。manifest、令牌仍走 HTTP/2。
	BlobHTTP2 bool `yaml:"blob_http2"`
	// 熔断：连续 BreakerFailures 次连不上或超时，就在 BreakerCooldown 内直接失败，之后放一个请求试探。
	// 试探仍失败时冷却时间加倍，最长 1 分钟。BreakerFailures 为 0 表示不熔断。
	BreakerFailures int      `yaml:"breaker_failures"`
	BreakerCooldown Duration `yaml:"breaker_cooldown"`
}

// Admin 是管理后台。
type Admin struct {
	// PublicURL 是从浏览器访问后台的地址，用于 Cookie 和来源检查。留空时只接受回环地址访问。
	PublicURL    string       `yaml:"public_url"`
	Store        Store        `yaml:"store"`
	InitialAdmin InitialAdmin `yaml:"initial_admin"`
}

// Store 是运营数据的存放方式。
type Store struct {
	Driver string `yaml:"driver"` // bolt（单实例可写）| none（只读，全部来自配置文件；k8s 多副本时用）
	Path   string `yaml:"path"`   // 默认 <data_dir>/state.db
}

// InitialAdmin 在运营数据库里还没有管理员时，用来创建第一个管理员。
type InitialAdmin struct {
	Username string `yaml:"username"`
	Password Secret `yaml:"password"`
}

// Log 是日志设置。
type Log struct {
	Level  string    `yaml:"level"` // debug | info | warn | error
	Access AccessLog `yaml:"access"`
}

// AccessLog 是访问日志，字段和 Caddy 的 JSON 访问日志一致。
type AccessLog struct {
	Output  string `yaml:"output"` // stdout | file | off
	Path    string `yaml:"path"`
	MaxSize Size   `yaml:"max_size"` // 文件按大小轮转
	Keep    int    `yaml:"keep"`     // 保留几个旧文件
}

// Metrics 是指标和调试接口。
type Metrics struct {
	Pprof bool `yaml:"pprof"` // 在指标监听上开 /debug/pprof
}
