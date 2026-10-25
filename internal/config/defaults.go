package config

import "time"

// DefaultUpstream is the upstream a single-domain entrance uses when there is no path prefix and no ns.
// Docker's registry-mirrors applies only to Docker Hub and sends requests without a prefix, hence docker.io.
const DefaultUpstream = "docker.io"

// BuiltinUpstreams returns the five built-in upstreams. An upstream of the same name in the configuration file
// overrides fields on top of these.
//
// The redirect allowlists follow the destinations actually observed (2026-10, matching upstreams.caddy in
// registry-mirrors-caddy). New hosts are handed to the client as is by default; redirect1..3 in the access log
// and the console's Upstreams page list them, so the allowlists can be extended from there.
func BuiltinUpstreams() map[string]*Upstream {
	return map[string]*Upstream{
		// Docker Hub: blobs redirect to *.docker.com, such as production.cloudfront.docker.com, or to Cloudflare R2
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
		// gcr.io: served by Artifact Registry; blobs are relative redirects on the same host (/artifacts-downloads/...)
		"gcr.io": {
			Registry:        "gcr.io",
			RedirectHosts:   []string{`gcr\.io`, `[a-z0-9-]+-docker\.pkg\.dev`, `storage\.googleapis\.com`},
			TokenRealmHosts: []string{`gcr\.io`, `[a-z0-9-]+-docker\.pkg\.dev`},
		},
		// ghcr.io: blobs redirect to pkg-containers.githubusercontent.com
		"ghcr.io": {
			Registry:        "ghcr.io",
			RedirectHosts:   []string{`ghcr\.io`, `pkg-containers\.githubusercontent\.com`},
			TokenRealmHosts: []string{`ghcr\.io`},
		},
		// registry.k8s.io: manifests redirect to <region>-docker.pkg.dev; blobs redirect to cdn.registry.k8s.io,
		// S3 buckets in the various AWS regions, or pkg.dev. Where exactly depends on this server's egress IP.
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
		// quay.io: blobs redirect to signed cdnNN.quay.io URLs, or (for AWS addresses in the same region) straight to S3
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

// Defaults returns all default values. The configuration file only needs to set what differs from them.
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
		// one single-domain entrance matching any Host by default: right after docker run, pulls work at IP:port
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
			BufferSize:          16 << 10, // in benchmarks 16 KiB beat 32 KiB: less memory, slightly more throughput (docs/benchmark.md)
			BreakerFailures:     5,
			BreakerCooldown:     Duration(10 * time.Second),
			HealthInterval:      Duration(time.Minute),
		},
		Admin: Admin{Store: Store{Driver: "bolt"}, Public: true, Allow: []string{"loopback", "private_ranges"}, ConfigEdit: true},
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
