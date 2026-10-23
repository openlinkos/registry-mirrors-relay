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

// fakeUp 模拟上游：tags 里是 tag → 正文；calls 记录 "方法 路径" 的次数。
type fakeUp struct {
	mu      sync.Mutex
	tags    map[string][]byte
	calls   map[string]int
	fail    error // 不为 nil 时所有请求返回这个错误
	status  int   // 不为 0 时所有请求返回这个状态
	noDig   bool  // HEAD 不给 Docker-Content-Digest
	corrupt bool  // GET 返回的内容和 digest 不符
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
		t.Fatalf("第一次：%+v", o)
	}
	if up.count("GET") != 1 || up.count("HEAD") != 1 {
		t.Fatalf("第一次应 HEAD 1 次、按 digest GET 1 次：%v", up.calls)
	}
	// tag 还新鲜：完全不访问上游
	o = c.Resolve(context.Background(), up, req)
	if o.Cache != "hit" || up.count("HEAD") != 1 || up.count("GET") != 1 {
		t.Fatalf("tag 新鲜时应命中：%+v %v", o, up.calls)
	}
	// tag 过期：只 HEAD 确认，digest 没变就不 GET
	*now = now.Add(31 * time.Second)
	o = c.Resolve(context.Background(), up, req)
	if o.Cache != "revalidated" || up.count("HEAD") != 2 || up.count("GET") != 1 {
		t.Fatalf("过期后只应 HEAD：%+v %v", o, up.calls)
	}
	// 按 digest 拉：命中
	o = c.Resolve(context.Background(), up, Request{Upstream: "docker.io", Repo: "library/x", Ref: digestOf(body)})
	if o.Cache != "hit" || up.count("GET") != 1 {
		t.Fatalf("按 digest 应命中：%+v", o)
	}
	// 不同仓库不共用缓存
	o = c.Resolve(context.Background(), up, Request{Upstream: "docker.io", Repo: "library/y", Ref: digestOf(body)})
	if o.Cache != "miss" {
		t.Fatalf("缓存键应包含仓库：%+v", o)
	}
}

func TestHeadNeverGets(t *testing.T) {
	c, up, body, _ := newCache(t)
	o := c.Resolve(context.Background(), up, Request{Upstream: "u", Repo: "r", Ref: "v1", Head: true, Accept: accept})
	if o.Head == nil || o.Head.Digest != digestOf(body) || o.Head.Size != int64(len(body)) {
		t.Fatalf("HEAD 应只用元数据回答：%+v", o)
	}
	o = c.Resolve(context.Background(), up, Request{Upstream: "u", Repo: "r", Ref: digestOf(body), Head: true})
	if !o.Bypass {
		t.Fatalf("按 digest 的 HEAD 没命中时应转发 HEAD，而不是 GET：%+v", o)
	}
	if up.count("GET") != 0 {
		t.Fatalf("客户端只发 HEAD 时不能向上游 GET：%v", up.calls)
	}
}

func TestAcceptIsPartOfTagKey(t *testing.T) {
	c, up, _, _ := newCache(t)
	c.Resolve(context.Background(), up, Request{Upstream: "u", Repo: "r", Ref: "v1", Accept: accept})
	c.Resolve(context.Background(), up, Request{Upstream: "u", Repo: "r", Ref: "v1", Accept: []string{"application/vnd.docker.distribution.manifest.v2+json"}})
	if up.count("HEAD") != 2 {
		t.Fatalf("Accept 不同应分别确认：%v", up.calls)
	}
	if CanonicalAccept([]string{"B;q=0.5, a", "a"}) != "a,b" {
		t.Fatal("CanonicalAccept 应去参数、去重、排序、小写")
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
		t.Fatalf("上游连不上时应返回旧内容：%+v", o)
	}
	up.fail, up.status = nil, http.StatusTooManyRequests
	if o = c.Resolve(context.Background(), up, req); o.Cache != "stale" {
		t.Fatalf("429 时应返回旧内容：%+v", o)
	}
	// 超过 stale_if_error（默认 6 小时）就不再用旧的
	*now = now.Add(7 * time.Hour)
	o = c.Resolve(context.Background(), up, req)
	if o.Status != http.StatusTooManyRequests || o.RetryAfter != "7" {
		t.Fatalf("太旧时应把上游的错误交给客户端：%+v", o)
	}
}

func TestNegativeCache(t *testing.T) {
	c, up, _, now := newCache(t)
	req := Request{Upstream: "u", Repo: "r", Ref: "missing", Accept: accept}
	for i := 0; i < 5; i++ {
		if o := c.Resolve(context.Background(), up, req); !o.NotFound {
			t.Fatalf("应返回不存在：%+v", o)
		}
	}
	if up.count("HEAD") != 1 {
		t.Fatalf("15 秒内重复请求不存在的镜像，只应问上游一次：%v", up.calls)
	}
	*now = now.Add(16 * time.Second)
	c.Resolve(context.Background(), up, req)
	if up.count("HEAD") != 2 {
		t.Fatal("过期后应重新确认")
	}
}

func TestDigestMismatchNotCached(t *testing.T) {
	c, up, body, _ := newCache(t)
	up.corrupt = true
	o := c.Resolve(context.Background(), up, Request{Upstream: "u", Repo: "r", Ref: digestOf(body)})
	if !IsMismatch(o.Err) || c.Stats().Entries != 0 {
		t.Fatalf("内容和 digest 不符时应报错且不缓存：%+v", o)
	}
}

func TestHeadWithoutDigestBypasses(t *testing.T) {
	c, up, _, _ := newCache(t)
	up.noDig = true
	if o := c.Resolve(context.Background(), up, Request{Upstream: "u", Repo: "r", Ref: "v1", Accept: accept}); !o.Bypass {
		t.Fatalf("上游 HEAD 不给 digest 时应转发：%+v", o)
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
		t.Fatalf("并发的同一 manifest 应只 GET 一次：失败 %d，%v", bad.Load(), up.calls)
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
		t.Fatalf("应按字节数淘汰：%+v", st)
	}
	if New(config.Cache{}, 1, time.Second) != nil {
		t.Fatal("max_bytes 为 0 时应关闭缓存")
	}
}
