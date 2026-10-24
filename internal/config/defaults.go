package config

import "time"

// DefaultUpstream 是单域名入口在没有路径前缀和 ns 时使用的上游。
// Docker 的 registry-mirrors 只对 Docker Hub 生效，发来的请求不带前缀，所以默认是 docker.io。
const DefaultUpstream = "docker.io"

// BuiltinUpstreams 返回内置的五个上游。配置文件里同名的上游会在此基础上覆盖字段。
//
// 重定向白名单按实际观察到的去向整理（2026-10，与 registry-mirrors-caddy 的 upstreams.caddy 一致）。
// 新出现的主机默认原样交给客户端，访问日志里的 redirect1..3 和后台的「上游」页会列出来，可据此补充。
func BuiltinUpstreams() map[string]*Upstream {
	return map[string]*Upstream{
		// Docker Hub：blob 重定向到 production.cloudfront.docker.com 等 *.docker.com，或 Cloudflare R2
		"docker.io": {
			Registry:      "registry-1.docker.io",
			Aliases:       []string{"index.docker.io", "registry-1.docker.io"},
			LibraryPrefix: true,
			RedirectHosts: []string{
				`registry-1\.docker\.io`,
				`(?:[a-z0-9-]+\.)+docker\.com`,
				`docker-images-prod\.[0-9a-f]+\.r2\.cloudflarestorage\.com`,
			},
			TokenRealmHosts: []string{`auth\.docker\.io`},
		},
		// gcr.io：由 Artifact Registry 提供，blob 是同主机的相对重定向（/artifacts-downloads/...）
		"gcr.io": {
			Registry:        "gcr.io",
			RedirectHosts:   []string{`gcr\.io`, `[a-z0-9-]+-docker\.pkg\.dev`, `storage\.googleapis\.com`},
			TokenRealmHosts: []string{`gcr\.io`, `[a-z0-9-]+-docker\.pkg\.dev`},
		},
		// ghcr.io：blob 重定向到 pkg-containers.githubusercontent.com
		"ghcr.io": {
			Registry:        "ghcr.io",
			RedirectHosts:   []string{`ghcr\.io`, `pkg-containers\.githubusercontent\.com`},
			TokenRealmHosts: []string{`ghcr\.io`},
		},
		// registry.k8s.io：manifest 重定向到 <区域>-docker.pkg.dev；blob 重定向到 cdn.registry.k8s.io、
		// AWS 各区域的 S3 桶，或 pkg.dev。具体去向取决于本服务器的出口 IP。
		"registry.k8s.io": {
			Registry: "registry.k8s.io",
			RedirectHosts: []string{
				`registry\.k8s\.io`,
				`cdn\.registry\.k8s\.io`,
				`[a-z0-9-]+-docker\.pkg\.dev`,
				`[a-z0-9-]+\.s3\.dualstack\.[a-z0-9-]+\.amazonaws\.com`,
				`storage\.googleapis\.com`,
			},
			TokenRealmHosts: []string{`registry\.k8s\.io`, `[a-z0-9-]+-docker\.pkg\.dev`},
		},
		// quay.io：blob 重定向到带签名的 cdnNN.quay.io，或（同区域的 AWS 地址）直接到 S3
		"quay.io": {
			Registry: "quay.io",
			RedirectHosts: []string{
				`quay\.io`,
				`cdn[0-9]*\.quay\.io`,
				`(?:quayio-production-s3\.)?s3(?:\.dualstack)?(?:\.[a-z0-9-]+)?\.amazonaws\.com`,
			},
			TokenRealmHosts: []string{`quay\.io`},
		},
	}
}

// Defaults 返回全部默认值。配置文件只需要写和默认值不同的部分。
func Defaults() *Config {
	return &Config{
		DataDir: "/var/lib/registry-mirrors-relay",
		Listen: Listen{
			HTTP: ":8080",
		},
		TLS: TLS{Mode: "off"},
		Site: Site{
			Name:       "Registry Mirrors Relay",
			Links:      map[string]string{},
			ShowStatus: true,
		},
		// 默认一个单域名入口，匹配任意 Host：docker run 起来就能用 IP:端口 拉取
		Entrances: []Entrance{{Name: "default", Hosts: []string{"*"}}},
		Limits: Limits{
			ManifestRate:         2,
			ManifestBurst:        120,
			HeadWeight:           0.25,
			BlobStreamsPerClient: 16,
			MaxInflight:          4096,
			MaxBlobStreams:       1024,
			MaxManifestBuffers:   64,
			TrackedClients:       65536,
			AuthFailuresPerMin:   10,
		},
		Cache: Cache{
			MaxBytes:     64 << 20,
			DigestTTL:    Duration(24 * time.Hour),
			TagTTL:       Duration(30 * time.Second),
			StaleIfError: Duration(6 * time.Hour),
			NegativeTTL:  Duration(15 * time.Second),
		},
		Timeouts: Timeouts{
			ReadHeader:       Duration(10 * time.Second),
			Idle:             Duration(120 * time.Second),
			WriteIdle:        Duration(60 * time.Second),
			Dial:             Duration(10 * time.Second),
			TLSHandshake:     Duration(10 * time.Second),
			ResponseHeader:   Duration(30 * time.Second),
			UpstreamReadIdle: Duration(60 * time.Second),
			Token:            Duration(15 * time.Second),
			ShutdownGrace:    Duration(30 * time.Second),
		},
		Proxy: Proxy{
			MaxIdleConnsPerHost: 64,
			MaxRedirects:        3,
			BufferSize:          16 << 10, // 压测里 16 KiB 比 32 KiB 省内存，吞吐也略高，见 docs/benchmark.md
			BreakerFailures:     5,
			BreakerCooldown:     Duration(10 * time.Second),
			HealthInterval:      Duration(time.Minute),
		},
		Admin: Admin{Store: Store{Driver: "bolt"}, Allow: []string{"loopback", "private_ranges"}},
		Log: Log{
			Level: "info",
			Access: AccessLog{
				Output:  "stdout",
				MaxSize: 100 << 20,
				Keep:    10,
			},
		},
	}
}
