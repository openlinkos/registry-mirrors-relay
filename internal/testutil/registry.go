package testutil

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Media types
const (
	MediaIndex    = "application/vnd.oci.image.index.v1+json"
	MediaManifest = "application/vnd.oci.image.manifest.v1+json"
	MediaConfig   = "application/vnd.oci.image.config.v1+json"
	MediaLayer    = "application/vnd.oci.image.layer.v1.tar+gzip"
)

// Digest returns the sha256 digest of the content.
func Digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// Image is a multi-architecture image: the index points to manifests for two platforms, and each
// manifest has one config and two layers.
type Image struct {
	Repo      string
	Tag       string
	Index     []byte
	Manifests map[string][]byte // digest → manifest
	Blobs     map[string][]byte // digest → content
}

// IndexDigest is the digest of the index.
func (im *Image) IndexDigest() string { return Digest(im.Index) }

type descriptor struct {
	MediaType string            `json:"mediaType"`
	Digest    string            `json:"digest"`
	Size      int               `json:"size"`
	Platform  map[string]string `json:"platform,omitempty"`
}

// NewImage generates an image with random content. layerSize is the size of each layer in bytes.
func NewImage(t testing.TB, repo, tag string, layerSize int) *Image {
	t.Helper()
	im := &Image{Repo: repo, Tag: tag, Manifests: map[string][]byte{}, Blobs: map[string][]byte{}}
	var manifests []descriptor
	for _, arch := range []string{"amd64", "arm64"} {
		cfg := []byte(fmt.Sprintf(`{"architecture":%q,"os":"linux","rootfs":{"type":"layers"}}`, arch))
		im.Blobs[Digest(cfg)] = cfg
		var layers []descriptor
		for i := 0; i < 2; i++ {
			b := make([]byte, layerSize)
			if _, err := rand.Read(b); err != nil {
				t.Fatal(err)
			}
			im.Blobs[Digest(b)] = b
			layers = append(layers, descriptor{MediaType: MediaLayer, Digest: Digest(b), Size: len(b)})
		}
		m, _ := json.Marshal(map[string]any{
			"schemaVersion": 2, "mediaType": MediaManifest,
			"config": descriptor{MediaType: MediaConfig, Digest: Digest(cfg), Size: len(cfg)},
			"layers": layers,
		})
		im.Manifests[Digest(m)] = m
		manifests = append(manifests, descriptor{MediaType: MediaManifest, Digest: Digest(m), Size: len(m),
			Platform: map[string]string{"os": "linux", "architecture": arch}})
	}
	im.Index, _ = json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": MediaIndex, "manifests": manifests})
	return im
}

// Registry is a fake upstream: the registry itself, a token service and a CDN that stores blobs,
// which can be placed on separate hosts.
type Registry struct {
	Host     string // registry host, e.g. registry.test
	AuthHost string // token service host; empty means no token is required
	CDNHost  string // blobs are redirected here; empty means the registry returns blobs directly
	// When ManifestHost is set, manifest requests are first sent with a 307 to this host (simulating
	// registry.k8s.io → pkg.dev)
	ManifestHost string
	Service      string

	mu        sync.Mutex
	images    map[string]*Image // repo → image
	Calls     map[string]int    // "method host path-kind" → count
	AuthSeen  map[string]int    // host → number of requests received with Authorization
	Protos    map[string]string // "host path-kind" → protocol of the latest request (HTTP/1.1, HTTP/2.0)
	TokenReqs []url.Values
	// Fail makes a path return a given status (to test error mapping); the key is "host path"
	Fail map[string]FailWith
}

// FailWith describes a deliberate failure.
type FailWith struct {
	Status int
	Body   string
	Header http.Header
}

// NewRegistry starts a fake upstream in the test network.
func NewRegistry(t testing.TB, n *Net, host, authHost, cdnHost string) *Registry {
	r := &Registry{Host: host, AuthHost: authHost, CDNHost: cdnHost, Service: host,
		images: map[string]*Image{}, Calls: map[string]int{}, AuthSeen: map[string]int{}, Protos: map[string]string{}, Fail: map[string]FailWith{}}
	names := []string{host}
	for _, h := range []string{authHost, cdnHost} {
		if h != "" {
			names = append(names, h)
		}
	}
	n.Serve(t, r, names...)
	return r
}

// ServeManifestHost also starts ManifestHost (with the same handler as the registry).
func (r *Registry) ServeManifestHost(t testing.TB, n *Net, host string) {
	r.ManifestHost = host
	n.Serve(t, r, host)
}

// Add adds an image.
func (r *Registry) Add(im *Image) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.images[im.Repo] = im
}

// Count returns the number of requests of a kind.
func (r *Registry) Count(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Calls[key]
}

// Proto returns the protocol used by the latest request of a kind on a host.
func (r *Registry) Proto(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Protos[key]
}

// AuthCount returns the number of requests with Authorization that a host received.
func (r *Registry) AuthCount(host string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.AuthSeen[host]
}

var (
	itemRe = regexp.MustCompile(`^/v2/(.+)/(manifests|blobs)/([^/]+)$`)
	tagsRe = regexp.MustCompile(`^/v2/(.+)/tags/list$`)
)

func ociError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"errors":[{"code":%q,"message":"test"}]}`, code)
}

func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	host := strings.Split(req.Host, ":")[0]
	kind := "other"
	switch {
	case host == r.AuthHost && req.URL.Path == "/token":
		kind = "token"
	case req.URL.Path == "/v2/":
		kind = "ping"
	case strings.Contains(req.URL.Path, "/manifests/"):
		kind = "manifest"
	case strings.Contains(req.URL.Path, "/blobs/"):
		kind = "blob"
	case strings.HasSuffix(req.URL.Path, "/tags/list"):
		kind = "tags"
	}
	r.mu.Lock()
	r.Calls[req.Method+" "+host+" "+kind]++
	r.Protos[host+" "+kind] = req.Proto
	if req.Header.Get("Authorization") != "" {
		r.AuthSeen[host]++
	}
	fail, failing := r.Fail[host+" "+req.URL.Path]
	r.mu.Unlock()
	if failing {
		for k, v := range fail.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(fail.Status)
		_, _ = w.Write([]byte(fail.Body))
		return
	}

	switch {
	case kind == "token":
		r.mu.Lock()
		r.TokenReqs = append(r.TokenReqs, req.URL.Query())
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		scopes := req.URL.Query()["scope"]
		sort.Strings(scopes)
		fmt.Fprintf(w, `{"token":%q,"expires_in":300}`, "tok|"+strings.Join(scopes, " "))
		return
	case host == r.CDNHost:
		r.serveCDN(w, req)
		return
	}

	if req.URL.Path == "/v2/" {
		if r.AuthHost != "" && !r.authorized(req, "") {
			r.challenge(w, "")
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	var repo, what, ref string
	if m := itemRe.FindStringSubmatch(req.URL.Path); m != nil {
		repo, what, ref = m[1], m[2], m[3]
	} else if m := tagsRe.FindStringSubmatch(req.URL.Path); m != nil {
		repo, what = m[1], "tags"
	} else {
		ociError(w, http.StatusNotFound, "NAME_UNKNOWN")
		return
	}
	if what == "manifests" && r.ManifestHost != "" && host == r.Host {
		w.Header().Set("Location", "https://"+r.ManifestHost+req.URL.Path)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	if r.AuthHost != "" && !r.authorized(req, repo) {
		r.challenge(w, repo)
		return
	}
	r.mu.Lock()
	im := r.images[repo]
	r.mu.Unlock()
	if im == nil {
		ociError(w, http.StatusNotFound, "NAME_UNKNOWN")
		return
	}
	switch what {
	case "manifests":
		var body []byte
		ctype := MediaManifest
		switch {
		case ref == im.Tag || ref == im.IndexDigest():
			body, ctype = im.Index, MediaIndex
		default:
			body = im.Manifests[ref]
		}
		if body == nil {
			ociError(w, http.StatusNotFound, "MANIFEST_UNKNOWN")
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Docker-Content-Digest", Digest(body))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Set-Cookie", "tracking=1")
		w.Header().Set("Docker-Ratelimit-Source", "198.51.100.200")
		w.Header().Set("Ratelimit-Remaining", "99;w=3600")
		if req.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(body)
	case "blobs":
		b := im.Blobs[ref]
		if b == nil {
			ociError(w, http.StatusNotFound, "BLOB_UNKNOWN")
			return
		}
		if r.CDNHost != "" {
			// Signed URL: the signature is valid only for GET
			w.Header().Set("Location", "https://"+r.CDNHost+"/blobs/"+ref+"?sig=s3cr3t&method=GET")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Docker-Content-Digest", ref)
		http.ServeContent(w, req, "", time.Unix(0, 0), bytes.NewReader(b))
	case "tags":
		n, _ := strconv.Atoi(req.URL.Query().Get("n"))
		tags := []string{im.Tag, "a", "b"}
		if n > 0 && n < len(tags) {
			w.Header().Set("Link", fmt.Sprintf(`</v2/%s/tags/list?last=%s&n=%d>; rel="next"`, repo, tags[n-1], n))
			tags = tags[:n]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": tags})
	}
}

func (r *Registry) serveCDN(w http.ResponseWriter, req *http.Request) {
	if req.URL.Query().Get("sig") != "s3cr3t" || req.Method != http.MethodGet {
		// As with S3: the signature is bound to the method, so HEAD is refused
		w.WriteHeader(http.StatusForbidden)
		return
	}
	d := strings.TrimPrefix(req.URL.Path, "/blobs/")
	r.mu.Lock()
	var b []byte
	for _, im := range r.images {
		if v, ok := im.Blobs[d]; ok {
			b = v
		}
	}
	r.mu.Unlock()
	if b == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "binary/octet-stream")
	w.Header().Set("Set-Cookie", "cdn=1")
	http.ServeContent(w, req, "", time.Unix(0, 0), bytes.NewReader(b))
}

func (r *Registry) authorized(req *http.Request, repo string) bool {
	h := req.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer tok|") {
		return false
	}
	if repo == "" {
		return true
	}
	return strings.Contains(h, "repository:"+repo+":pull")
}

func (r *Registry) challenge(w http.ResponseWriter, repo string) {
	c := fmt.Sprintf(`Bearer realm="https://%s/token",service="%s"`, r.AuthHost, r.Service)
	if repo != "" {
		c += fmt.Sprintf(`,scope="repository:%s:pull"`, repo)
	}
	w.Header().Set("WWW-Authenticate", c)
	ociError(w, http.StatusUnauthorized, "UNAUTHORIZED")
}
