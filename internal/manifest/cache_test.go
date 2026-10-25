package manifest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/upstream"
)

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// fakeUp simulates an upstream: tags maps tag → body; calls counts "method path".
type fakeUp struct {
	mu      sync.Mutex
	tags    map[string][]byte
	calls   map[string]int
	fail    error // when not nil, every request returns this error
	status  int   // when not 0, every request returns this status
	noDig   bool  // HEAD gives no Docker-Content-Digest
	corrupt bool  // GET returns content that does not match the digest
	delay   time.Duration
}

func (f *fakeUp) count(k string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls[k] }

func (f *fakeUp) Fetch(ctx context.Context, r upstream.Request) (*upstream.Response, error) {
	f.mu.Lock()
	f.calls[r.Method+" "+r.Path]++
	f.calls[r.Method]++
	fail, status, delay := f.fail, f.status, f.delay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if fail != nil {
		return nil, fail
	}
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: http.NoBody, ContentLength: -1}
	if status != 0 {
		resp.StatusCode = status
		resp.Header.Set("Retry-After", "7")
		return &upstream.Response{Response: resp}, nil
	}
	ref := r.Path[strings.LastIndex(r.Path, "/")+1:]
	var body []byte
	for tag, b := range f.tags {
		if ref == tag || ref == digestOf(b) {
			body = b
		}
	}
	if body == nil {
		resp.StatusCode = 404
		return &upstream.Response{Response: resp}, nil
	}
	resp.Header.Set("Content-Type", "application/vnd.oci.image.index.v1+json")
	if !f.noDig || r.Method != http.MethodHead {
		resp.Header.Set("Docker-Content-Digest", digestOf(body))
	}
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodGet {
		if f.corrupt {
			body = append([]byte("x"), body...)
		}
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
	}
	return &upstream.Response{Response: resp}, nil
}

func newCache(t *testing.T) (*Cache, *fakeUp, []byte, *time.Time) {
	t.Helper()
	res, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := New(res.Config.Cache, 4, 5*time.Second)
	now := time.Unix(1792744703, 0)
	c.now = func() time.Time { return now }
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`)
	return c, &fakeUp{tags: map[string][]byte{"v1": body}, calls: map[string]int{}}, body, &now
}

var accept = []string{"application/vnd.oci.image.index.v1+json"}

func TestSecondPullCostsNoGet(t *testing.T) {
	c, up, body, now := newCache(t)
	req := Request{Upstream: "docker.io", Repo: "library/x", Ref: "v1", Accept: accept}
	o := c.Resolve(context.Background(), up, req)
	if o.Manifest == nil || string(o.Manifest.Body) != string(body) || o.Cache != "miss" {
		t.Fatalf("first: %+v", o)
	}
	if up.count("GET") != 1 || up.count("HEAD") != 1 {
		t.Fatalf("the first pull should HEAD once and GET by digest once: %v", up.calls)
	}
	// The tag is still fresh: no upstream access at all
	o = c.Resolve(context.Background(), up, req)
	if o.Cache != "hit" || up.count("HEAD") != 1 || up.count("GET") != 1 {
		t.Fatalf("a fresh tag should hit: %+v %v", o, up.calls)
	}
	// The tag has expired: only HEAD to confirm, no GET if the digest is unchanged
	*now = now.Add(31 * time.Second)
	o = c.Resolve(context.Background(), up, req)
	if o.Cache != "revalidated" || up.count("HEAD") != 2 || up.count("GET") != 1 {
		t.Fatalf("after expiry there should only be a HEAD: %+v %v", o, up.calls)
	}
	// Pull by digest: hit
	o = c.Resolve(context.Background(), up, Request{Upstream: "docker.io", Repo: "library/x", Ref: digestOf(body)})
	if o.Cache != "hit" || up.count("GET") != 1 {
		t.Fatalf("by digest should hit: %+v", o)
	}
	// Different repositories do not share the cache
	o = c.Resolve(context.Background(), up, Request{Upstream: "docker.io", Repo: "library/y", Ref: digestOf(body)})
	if o.Cache != "miss" {
		t.Fatalf("the cache key should include the repository: %+v", o)
	}
}

func TestHeadNeverGets(t *testing.T) {
	c, up, body, _ := newCache(t)
	o := c.Resolve(context.Background(), up, Request{Upstream: "u", Repo: "r", Ref: "v1", Head: true, Accept: accept})
	if o.Head == nil || o.Head.Digest != digestOf(body) || o.Head.Size != int64(len(body)) {
		t.Fatalf("a HEAD should be answered from metadata only: %+v", o)
	}
	o = c.Resolve(context.Background(), up, Request{Upstream: "u", Repo: "r", Ref: digestOf(body), Head: true})
	if !o.Bypass {
		t.Fatalf("a HEAD by digest that misses should be forwarded as a HEAD, not a GET: %+v", o)
	}
	if up.count("GET") != 0 {
		t.Fatalf("must not GET from the upstream when the client only sends HEAD: %v", up.calls)
	}
}

func TestAcceptIsPartOfTagKey(t *testing.T) {
	c, up, _, _ := newCache(t)
	c.Resolve(context.Background(), up, Request{Upstream: "u", Repo: "r", Ref: "v1", Accept: accept})
	c.Resolve(context.Background(), up, Request{Upstream: "u", Repo: "r", Ref: "v1", Accept: []string{"application/vnd.docker.distribution.manifest.v2+json"}})
	if up.count("HEAD") != 2 {
		t.Fatalf("different Accept values should be confirmed separately: %v", up.calls)
	}
	if CanonicalAccept([]string{"B;q=0.5, a", "a"}) != "a,b" {
		t.Fatal("CanonicalAccept should strip parameters, deduplicate, sort and lower-case")
	}
}

func TestStaleIfError(t *testing.T) {
	c, up, body, now := newCache(t)
	req := Request{Upstream: "u", Repo: "r", Ref: "v1", Accept: accept}
	c.Resolve(context.Background(), up, req)
	*now = now.Add(time.Hour)
	up.fail = errors.New("connection refused")
	o := c.Resolve(context.Background(), up, req)
	if o.Cache != "stale" || o.Manifest == nil || string(o.Manifest.Body) != string(body) {
		t.Fatalf("should return stale content when the upstream is unreachable: %+v", o)
	}
	up.fail, up.status = nil, http.StatusTooManyRequests
	if o = c.Resolve(context.Background(), up, req); o.Cache != "stale" {
		t.Fatalf("should return stale content on 429: %+v", o)
	}
	// Beyond stale_if_error (6 hours by default) stale content is no longer used
	*now = now.Add(7 * time.Hour)
	o = c.Resolve(context.Background(), up, req)
	if o.Status != http.StatusTooManyRequests || o.RetryAfter != "7" {
		t.Fatalf("when too old, the upstream error should be passed to the client: %+v", o)
	}
}

func TestNegativeCache(t *testing.T) {
	c, up, _, now := newCache(t)
	req := Request{Upstream: "u", Repo: "r", Ref: "missing", Accept: accept}
	for i := 0; i < 5; i++ {
		if o := c.Resolve(context.Background(), up, req); !o.NotFound {
			t.Fatalf("should return not found: %+v", o)
		}
	}
	if up.count("HEAD") != 1 {
		t.Fatalf("repeated requests for a missing image within 15 seconds should ask the upstream only once: %v", up.calls)
	}
	*now = now.Add(16 * time.Second)
	c.Resolve(context.Background(), up, req)
	if up.count("HEAD") != 2 {
		t.Fatal("should confirm again after expiry")
	}
}

func TestDigestMismatchNotCached(t *testing.T) {
	c, up, body, _ := newCache(t)
	up.corrupt = true
	o := c.Resolve(context.Background(), up, Request{Upstream: "u", Repo: "r", Ref: digestOf(body)})
	if !IsMismatch(o.Err) || c.Stats().Entries != 0 {
		t.Fatalf("content that does not match the digest should fail and not be cached: %+v", o)
	}
}

func TestHeadWithoutDigestBypasses(t *testing.T) {
	c, up, _, _ := newCache(t)
	up.noDig = true
	if o := c.Resolve(context.Background(), up, Request{Upstream: "u", Repo: "r", Ref: "v1", Accept: accept}); !o.Bypass {
		t.Fatalf("should forward when the upstream HEAD gives no digest: %+v", o)
	}
}

func TestConcurrentFetchCoalesced(t *testing.T) {
	c, up, body, _ := newCache(t)
	up.delay = 50 * time.Millisecond
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := c.Resolve(context.Background(), up, Request{Upstream: "u", Repo: "r", Ref: digestOf(body)})
			if o.Manifest == nil {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 || up.count("GET") != 1 {
		t.Fatalf("concurrent requests for the same manifest should GET only once: failed %d, %v", bad.Load(), up.calls)
	}
}

func TestByteLimit(t *testing.T) {
	res, _ := config.Parse([]byte("cache:\n  max_bytes: 4096\n"))
	c := New(res.Config.Cache, 4, time.Second)
	up := &fakeUp{tags: map[string][]byte{}, calls: map[string]int{}}
	for i := 0; i < 10; i++ {
		b := []byte(strings.Repeat(strconv.Itoa(i), 1000))
		up.tags["t"+strconv.Itoa(i)] = b
		c.Resolve(context.Background(), up, Request{Upstream: "u", Repo: "r", Ref: digestOf(b)})
	}
	if st := c.Stats(); st.Bytes > 4096 || st.Entries > 3 {
		t.Fatalf("should evict by byte count: %+v", st)
	}
	if New(config.Cache{}, 1, time.Second) != nil {
		t.Fatal("max_bytes 0 should disable the cache")
	}
}
