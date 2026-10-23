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

// 媒体类型
const (
	MediaIndex    = "application/vnd.oci.image.index.v1+json"
	MediaManifest = "application/vnd.oci.image.manifest.v1+json"
	MediaConfig   = "application/vnd.oci.image.config.v1+json"
	MediaLayer    = "application/vnd.oci.image.layer.v1.tar+gzip"
)

// Digest 返回内容的 sha256 digest。
func Digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// Image 是一个多架构镜像：index 指向两个平台的 manifest，每个 manifest 有一个 config 和两个层。
type Image struct {
	Repo      string
	Tag       string
	Index     []byte
	Manifests map[string][]byte // digest → manifest
	Blobs     map[string][]byte // digest → 内容
}

// IndexDigest 是 index 的 digest。
func (im *Image) IndexDigest() string { return Digest(im.Index) }

type descriptor struct {
	MediaType string            `json:"mediaType"`
	Digest    string            `json:"digest"`
	Size      int               `json:"size"`
	Platform  map[string]string `json:"platform,omitempty"`
}

// NewImage 生成一个随机内容的镜像。layerSize 是每层的字节数。
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

// Registry 是一个假的上游：registry 本身、令牌服务和存放 blob 的 CDN，可以分开放在不同主机上。
type Registry struct {
	Host     string // registry 主机，如 registry.test
	AuthHost string // 令牌服务主机；为空表示不要求令牌
	CDNHost  string // blob 重定向到这里；为空表示 registry 直接返回 blob
	// ManifestHost 不为空时，manifest 请求先 307 到这台主机（模拟 registry.k8s.io → pkg.dev）
	ManifestHost string
	Service      string

	mu        sync.Mutex
	images    map[string]*Image // repo → 镜像
	Calls     map[string]int    // "方法 主机 路径种类" → 次数
	AuthSeen  map[string]int    // 主机 → 收到带 Authorization 的请求次数
	TokenReqs []url.Values
	// Fail 可以让某个路径返回指定状态（测试错误映射），键是 "主机 路径"
	Fail map[string]FailWith
}

// FailWith 描述一次故意的失败。
type FailWith struct {
	Status int
	Body   string
	Header http.Header
}

// NewRegistry 在测试网络里启动一个假上游。
func NewRegistry(t testing.TB, n *Net, host, authHost, cdnHost string) *Registry {
	r := &Registry{Host: host, AuthHost: authHost, CDNHost: cdnHost, Service: host,
		images: map[string]*Image{}, Calls: map[string]int{}, AuthSeen: map[string]int{}, Fail: map[string]FailWith{}}
	names := []string{host}
	for _, h := range []string{authHost, cdnHost} {
		if h != "" {
			names = append(names, h)
		}
	}
	n.Serve(t, r, names...)
	return r
}

// ServeManifestHost 再启动 ManifestHost（和 registry 同一个处理器）。
func (r *Registry) ServeManifestHost(t testing.TB, n *Net, host string) {
	r.ManifestHost = host
	n.Serve(t, r, host)
}

// Add 加入一个镜像。
func (r *Registry) Add(im *Image) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.images[im.Repo] = im
}

// Count 返回某类请求的次数。
func (r *Registry) Count(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Calls[key]
}

// AuthCount 返回某台主机收到带 Authorization 的请求次数。
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
			// 带签名的地址：签名只对 GET 有效
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
		// 和 S3 一样：签名绑定了方法，HEAD 会被拒绝
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
