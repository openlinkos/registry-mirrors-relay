package route

import (
	"net/url"
	"strings"
	"testing"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testRouter(t *testing.T) *Router {
	t.Helper()
	res, err := config.Parse([]byte(`
entrances:
  - hosts: [mirror.example.com]
  - hosts: [ghcr.example.com]
    upstream: ghcr.io
  - hosts: [gcr.example.com]
    upstream: gcr.io
  - hosts: [quay.example.com]
    upstream: quay.io
  - hosts: ["*"]
`))
	if err != nil {
		t.Fatal(err)
	}
	return New(res.Config)
}

func parse(t *testing.T, r *Router, e *Entrance, raw string) (Target, *Error) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return r.Parse(e, u)
}

func TestEntranceLookup(t *testing.T) {
	r := testRouter(t)
	for host, want := range map[string]string{
		"ghcr.example.com":       "ghcr.example.com",
		"GHCR.example.com.:8443": "ghcr.example.com",
		"mirror.example.com":     "mirror.example.com",
		"10.0.0.1:8080":          "*",
		"[::1]:8080":             "*",
	} {
		e := r.Entrance(host)
		if e == nil || e.Name != want {
			t.Errorf("%s: want entrance %s, got %+v", host, want, e)
		}
	}
	if e := r.Entrance("quay.example.com"); e == nil || e.Upstream != "quay.io" {
		t.Fatalf("fixed-upstream entrance: %+v", e)
	}
	res, err := config.Parse([]byte("entrances: [{hosts: [only.example.com]}]"))
	if err != nil {
		t.Fatal(err)
	}
	if e := New(res.Config).Entrance("other.example.com"); e != nil {
		t.Fatal("should return nil when no entrance matches")
	}
}

func TestParseAllInOne(t *testing.T) {
	r := testRouter(t)
	e := r.Entrance("mirror.example.com")
	cases := []struct {
		path, up, repo, client string
		kind                   Kind
	}{
		{"/v2/nginx/manifests/latest", "docker.io", "library/nginx", "nginx", KindManifest},
		{"/v2/library/nginx/manifests/1.27", "docker.io", "library/nginx", "library/nginx", KindManifest},
		{"/v2/bitnami/redis/blobs/" + digest, "docker.io", "bitnami/redis", "bitnami/redis", KindBlob},
		{"/v2/ghcr.io/owner/image/manifests/v1", "ghcr.io", "owner/image", "ghcr.io/owner/image", KindManifest},
		{"/v2/docker.io/nginx/manifests/latest", "docker.io", "library/nginx", "docker.io/nginx", KindManifest},
		{"/v2/index.docker.io/library/nginx/tags/list", "docker.io", "library/nginx", "index.docker.io/library/nginx", KindTags},
		{"/v2/gcr.io/google.com/cloudsdktool/cloud-sdk/manifests/slim", "gcr.io", "google.com/cloudsdktool/cloud-sdk", "gcr.io/google.com/cloudsdktool/cloud-sdk", KindManifest},
		{"/v2/registry.k8s.io/pause/manifests/3.10", "registry.k8s.io", "pause", "registry.k8s.io/pause", KindManifest},
		{"/v2/owner/image/manifests/v1?ns=ghcr.io", "ghcr.io", "owner/image", "owner/image", KindManifest},
		{"/v2/nginx/manifests/latest?ns=docker.io", "docker.io", "library/nginx", "nginx", KindManifest},
		{"/v2/a/manifests/b/manifests/c", "docker.io", "a/manifests/b", "a/manifests/b", KindManifest},
		{"/v2/x/referrers/" + digest + "?artifactType=application%2Fvnd.example", "docker.io", "library/x", "x", KindReferrers},
	}
	for _, c := range cases {
		tg, err := parse(t, r, e, c.path)
		if err != nil {
			t.Errorf("%s: %v", c.path, err)
			continue
		}
		if tg.Upstream != c.up || tg.Repo != c.repo || tg.Client != c.client || tg.Kind != c.kind {
			t.Errorf("%s: got %+v", c.path, tg)
		}
	}
	tg, _ := parse(t, r, e, "/v2/x/referrers/"+digest+"?artifactType=application%2Fvnd.example&ns=docker.io&evil=1")
	if tg.Query.Encode() != "artifactType=application%2Fvnd.example" {
		t.Fatalf("only artifactType should be forwarded: %s", tg.Query.Encode())
	}
}

func TestParseFixedEntrance(t *testing.T) {
	r := testRouter(t)
	e := r.Entrance("gcr.example.com")
	// A fixed-upstream entrance does not treat the first segment as a registry: google.com/... on gcr.io is a
	// repository
	tg, err := parse(t, r, e, "/v2/google.com/cloudsdktool/cloud-sdk/manifests/slim")
	if err != nil || tg.Upstream != "gcr.io" || tg.Repo != "google.com/cloudsdktool/cloud-sdk" {
		t.Fatalf("got %+v %v", tg, err)
	}
	// gcr.io has no library prefix
	if tg, _ := parse(t, r, e, "/v2/distroless/manifests/x"); tg.Repo != "distroless" {
		t.Fatalf("gcr.io should not get library/: %+v", tg)
	}
	// containerd has configured this entrance as a Docker Hub mirror
	if _, err := parse(t, r, e, "/v2/library/nginx/manifests/latest?ns=docker.io"); err == nil || err.Status != 404 {
		t.Fatalf("a mismatched ns should return 404: %v", err)
	}
	if _, err := parse(t, r, e, "/v2/distroless/static/manifests/x?ns=gcr.io"); err != nil {
		t.Fatalf("a matching ns should be allowed: %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	r := testRouter(t)
	e := r.Entrance("mirror.example.com")
	for path, status := range map[string]int{
		"/v2/_catalog":                                     404,
		"/v2/x/blobs/uploads/":                             404,
		"/v2/x/blobs/uploads/abc":                          404,
		"/v2/example.org/x/manifests/latest":               404, // upstream not configured, no fallback to Docker Hub
		"/v2/x/manifests/latest?ns=example.org":            404,
		"/v2/ghcr.io/x/manifests/v1?ns=quay.io":            404,
		"/v2/x/blobs/latest":                               400,
		"/v2/x/manifests/bad:tag":                          400,
		"/v2/UPPER/manifests/latest":                       400,
		"/v2/a//b/manifests/latest":                        400,
		"/v2/a/../b/manifests/latest":                      400,
		"/v2/a%2Fb/manifests/latest":                       400,
		"/v2/" + strings.Repeat("a", 300) + "/manifests/x": 400,
		"/v2/x/manifests/" + strings.Repeat("t", 129):      400,
		"/v1/search":                                       404,
	} {
		_, err := parse(t, r, e, path)
		if err == nil || err.Status != status {
			t.Errorf("%s: want %d, got %v", path, status, err)
		}
	}
	if tg, err := parse(t, r, e, "/v2/"); err != nil || tg.Kind != KindPing {
		t.Fatalf("/v2/ should be a ping: %+v %v", tg, err)
	}
}

func TestLinkRewrite(t *testing.T) {
	r := testRouter(t)
	e := r.Entrance("mirror.example.com")
	tg, _ := parse(t, r, e, "/v2/docker.io/nginx/tags/list?n=2")
	if tg.Query.Get("n") != "2" {
		t.Fatalf("n should be forwarded: %v", tg.Query)
	}
	got := tg.RewriteLink(`</v2/library/nginx/tags/list?last=1.27&n=2>; rel="next"`)
	if got != `</v2/docker.io/nginx/tags/list?last=1.27&n=2>; rel="next"` {
		t.Fatalf("wrong Link rewrite: %s", got)
	}
	if tg.RewriteLink(`<https://evil.example/v2/other/tags/list>; rel="next"`) != "" {
		t.Fatal("a Link pointing elsewhere should be dropped")
	}
	tg2, _ := parse(t, r, e, "/v2/x/tags/list?n=99999")
	if tg2.Query.Get("n") != "1000" {
		t.Fatalf("n should be capped at 1000: %v", tg2.Query)
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"/v2/", "/v2/nginx/manifests/latest", "/v2/ghcr.io/a/b/blobs/" + digest, "/v2/a/tags/list?n=1", "/v2/%2e%2e/x"} {
		f.Add(s)
	}
	res, err := config.Parse(nil)
	if err != nil {
		f.Fatal(err)
	}
	r := New(res.Config)
	e := r.Entrance("x")
	f.Fuzz(func(t *testing.T, raw string) {
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		tg, perr := r.Parse(e, u)
		if perr != nil || tg.Kind == KindPing {
			return
		}
		p := tg.UpstreamPath()
		if !strings.HasPrefix(p, "/v2/") {
			t.Fatalf("upstream path is not under /v2/: %q → %q", raw, p)
		}
		for _, seg := range strings.Split(p[1:], "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Fatalf("upstream path has an unsafe segment: %q → %q", raw, p)
			}
		}
		if _, ok := r.Upstream(tg.Upstream); !ok {
			t.Fatalf("upstream is not in the configuration: %q → %q", raw, tg.Upstream)
		}
	})
}
