// Package manifest 是 manifest 的内存缓存。只缓存 manifest（每个通常几 KB），不缓存镜像层，不写盘。
//
// 按 tag 拉取时先向上游发 HEAD（Docker Hub 不计次数），拿到 digest 后：内存里有这个 digest 就直接用，
// 没有才按 digest GET 一次。同一个 tag 被反复拉取，只有第一次消耗上游的拉取次数。
//
// 正确性：
//   - 按 digest 存的内容是不可变的，存之前校验 sha256，和 digest 不符就不存、返回错误；
//   - 按 tag 存的只是 tag → digest 的映射，键里带客户端的 Accept，过期（默认 30 秒）后重新 HEAD 确认；
//   - 上游出错（429、5xx、连不上、熔断）时，tag 映射在过期后的一段时间内（默认 6 小时）仍可以用旧的；
//   - 404 记一小段时间（默认 15 秒），反复请求不存在镜像的客户端不会每次都打到上游。
package manifest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
	"github.com/openlinkos/registry-mirrors-relay/internal/lru"
	"github.com/openlinkos/registry-mirrors-relay/internal/upstream"
)

// MaxSize 是单个 manifest 的上限（OCI distribution 规范要求 registry 至少支持 4 MiB）。
const MaxSize = 4 << 20

// Manifest 是一份按 digest 缓存的 manifest。
type Manifest struct {
	Body      []byte
	MediaType string
	Digest    string
	Stored    time.Time
}

// TagRef 是 tag 指向的 manifest，HEAD 时可以不取正文直接回答。
type TagRef struct {
	Digest    string
	MediaType string
	Size      int64
	Stored    time.Time
}

// Outcome 是一次解析的结果，调用方据此写响应。各字段只有一组有值：
//   - Manifest：有正文（GET）；HEAD 时也可以用它回答。
//   - Head：只有元数据，回答 HEAD。
//   - NotFound：上游说不存在（或要求认证），回答 404。
//   - Status：上游返回了错误状态（Body 可能为空）。
//   - Err：网络错误、熔断等。
//   - Bypass：缓存处理不了（例如上游 HEAD 不给 digest、重定向要交给客户端），调用方按普通请求转发。
type Outcome struct {
	Manifest   *Manifest
	Head       *TagRef
	NotFound   bool
	Status     int
	Body       []byte
	BodyType   string
	RetryAfter string
	Err        error
	Bypass     bool
	Cache      string   // hit、miss、revalidated、stale、negative，写进访问日志
	Hops       []string // 服务端跟随的跳转
}

// Stats 是累计的计数。
type Stats struct {
	Hits, Misses, Revalidations, Stale, Negative, Bypass uint64
	Bytes                                                int64
	Entries                                              int
}

// Fetcher 是上游，测试里可以替换。
type Fetcher interface {
	Fetch(ctx context.Context, r upstream.Request) (*upstream.Response, error)
}

// Cache 并发安全。
type Cache struct {
	cfg      config.Cache
	digests  *lru.Cache[string, *Manifest]
	tags     *lru.Cache[string, TagRef]
	negative *lru.Cache[string, time.Time]
	sf       singleflight.Group
	sem      chan struct{} // 同时在缓冲的 manifest 正文数
	timeout  time.Duration
	now      func() time.Time

	hits, misses, revalidations, stale, negHits, bypass atomic.Uint64
}

// New 创建缓存。cfg.MaxBytes 为 0 时返回 nil，调用方应直接转发。
func New(cfg config.Cache, maxBuffers int, fetchTimeout time.Duration) *Cache {
	if cfg.MaxBytes <= 0 {
		return nil
	}
	if maxBuffers < 1 {
		maxBuffers = 1
	}
	return &Cache{
		cfg:      cfg,
		digests:  lru.New[string, *Manifest](1<<20, int64(cfg.MaxBytes)),
		tags:     lru.New[string, TagRef](65536, 0),
		negative: lru.New[string, time.Time](16384, 0),
		sem:      make(chan struct{}, maxBuffers),
		timeout:  fetchTimeout,
		now:      time.Now,
	}
}

// Stats 返回计数。
func (c *Cache) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	return Stats{
		Hits: c.hits.Load(), Misses: c.misses.Load(), Revalidations: c.revalidations.Load(),
		Stale: c.stale.Load(), Negative: c.negHits.Load(), Bypass: c.bypass.Load(),
		Bytes: c.digests.Weight(), Entries: c.digests.Len(),
	}
}

// CanonicalAccept 把客户端的 Accept 整理成稳定的键：去掉参数、转小写、去重排序。
func CanonicalAccept(values []string) string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			mt := strings.ToLower(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]))
			if mt != "" && !seen[mt] {
				seen[mt] = true
				out = append(out, mt)
			}
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func isSHA256(d string) bool {
	if !strings.HasPrefix(d, "sha256:") || len(d) != 7+64 {
		return false
	}
	_, err := hex.DecodeString(d[7:])
	return err == nil && strings.ToLower(d) == d
}

// Request 是一次 manifest 请求。
type Request struct {
	Upstream string
	Repo     string
	Ref      string // tag 或 digest
	Head     bool
	Accept   []string // 客户端的 Accept 原值
}

// Resolve 按缓存规则解析一次 manifest 请求。
func (c *Cache) Resolve(ctx context.Context, up Fetcher, r Request) Outcome {
	accept := CanonicalAccept(r.Accept)
	if strings.Contains(r.Ref, ":") {
		return c.byDigest(ctx, up, r, accept)
	}
	base := r.Upstream + "\x00" + r.Repo + "\x00" + r.Ref + "\x00" + accept
	if at, ok := c.negative.Get(base); ok && c.now().Sub(at) < c.cfg.NegativeTTL.D() {
		c.negHits.Add(1)
		return Outcome{NotFound: true, Cache: "negative"}
	}
	tr, have := c.tags.Get(base)
	age := c.now().Sub(tr.Stored)
	if have && age < c.cfg.TagTTL.D() {
		return c.fromTag(ctx, up, r, accept, tr, "hit", nil)
	}
	canStale := have && age < c.cfg.TagTTL.D()+c.cfg.StaleIfError.D()

	resp, err := up.Fetch(ctx, upstream.Request{
		Method: http.MethodHead, Path: "/v2/" + r.Repo + "/manifests/" + r.Ref,
		Header: acceptHeader(r.Accept), Repo: r.Repo,
	})
	if err != nil {
		if canStale && ctx.Err() == nil {
			c.stale.Add(1)
			return c.fromTag(ctx, up, r, accept, tr, "stale", nil)
		}
		return Outcome{Err: err}
	}
	_ = resp.Body.Close()
	if resp.PassLocation != "" {
		c.bypass.Add(1)
		return Outcome{Bypass: true}
	}
	switch s := resp.StatusCode; {
	case s == http.StatusOK:
		dg := resp.Header.Get("Docker-Content-Digest")
		if !isSHA256(dg) {
			// 上游的 HEAD 不给 digest（或不是 sha256）：缓存不了，按普通请求转发
			c.bypass.Add(1)
			return Outcome{Bypass: true}
		}
		tr = TagRef{Digest: dg, MediaType: resp.Header.Get("Content-Type"), Size: resp.ContentLength, Stored: c.now()}
		c.tags.Add(base, tr, 0)
		c.revalidations.Add(1)
		return c.fromTag(ctx, up, r, accept, tr, "revalidated", resp.Hops)
	case s == http.StatusNotFound || s == http.StatusUnauthorized:
		// Docker Hub 对不存在的仓库返回 401：一样当作不存在
		c.negative.Add(base, c.now(), 0)
		return Outcome{NotFound: true, Cache: "miss", Hops: resp.Hops}
	case s == http.StatusTooManyRequests || s >= 500:
		if canStale {
			c.stale.Add(1)
			return c.fromTag(ctx, up, r, accept, tr, "stale", resp.Hops)
		}
		return Outcome{Status: s, RetryAfter: resp.Header.Get("Retry-After"), Hops: resp.Hops}
	default:
		c.bypass.Add(1)
		return Outcome{Bypass: true}
	}
}

// fromTag：tag 已经对应到 digest，再按 digest 取正文（HEAD 时有元数据就够了）。
func (c *Cache) fromTag(ctx context.Context, up Fetcher, r Request, accept string, tr TagRef, how string, hops []string) Outcome {
	if m, ok := c.getDigest(r.Upstream, r.Repo, tr.Digest); ok {
		if how == "hit" {
			c.hits.Add(1)
		}
		return Outcome{Manifest: m, Cache: how, Hops: hops}
	}
	if r.Head {
		if tr.Size < 0 {
			// 不知道大小就回答不了 HEAD，也不能为 HEAD 去 GET：按普通请求转发
			c.bypass.Add(1)
			return Outcome{Bypass: true}
		}
		if how == "hit" {
			c.hits.Add(1)
		}
		return Outcome{Head: &tr, Cache: how, Hops: hops}
	}
	out := c.fetch(ctx, up, r, tr.Digest)
	if out.Cache == "" {
		out.Cache = how
	}
	if len(out.Hops) == 0 {
		out.Hops = hops
	}
	return out
}

func (c *Cache) byDigest(ctx context.Context, up Fetcher, r Request, accept string) Outcome {
	if !isSHA256(r.Ref) {
		c.bypass.Add(1)
		return Outcome{Bypass: true}
	}
	if m, ok := c.getDigest(r.Upstream, r.Repo, r.Ref); ok {
		c.hits.Add(1)
		return Outcome{Manifest: m, Cache: "hit"}
	}
	if r.Head {
		// 客户端只发了 HEAD：不能为它向上游发 GET（GET 才计 Docker Hub 次数），按普通请求转发
		c.bypass.Add(1)
		return Outcome{Bypass: true}
	}
	return c.fetch(ctx, up, r, r.Ref)
}

func (c *Cache) getDigest(upName, repo, dg string) (*Manifest, bool) {
	k := upName + "\x00" + repo + "\x00" + dg
	m, ok := c.digests.Get(k)
	if !ok {
		return nil, false
	}
	if c.now().Sub(m.Stored) > c.cfg.DigestTTL.D() {
		c.digests.Remove(k)
		return nil, false
	}
	return m, true
}

var (
	errTooLarge = errors.New("manifest 超过 4 MiB")
	errMismatch = errors.New("manifest 的内容和 digest 不符")
	errPass     = errors.New("上游要求客户端直连")
)

type statusError struct {
	status     int
	body       []byte
	ctype      string
	retryAfter string
}

func (e *statusError) Error() string { return "上游返回 " + http.StatusText(e.status) }

type fetched struct {
	m    *Manifest
	hops []string
}

// fetch 按 digest GET 一次，校验后存入缓存。并发的同一请求合并成一次，并且和发起它的客户端连接脱钩：
// 第一个客户端断开，不应让等着同一个 manifest 的其他请求一起失败。
func (c *Cache) fetch(ctx context.Context, up Fetcher, r Request, dg string) Outcome {
	key := r.Upstream + "\x00" + r.Repo + "\x00" + dg
	v, err, _ := c.sf.Do(key, func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
		defer cancel()
		select {
		case c.sem <- struct{}{}:
			defer func() { <-c.sem }()
		case <-fctx.Done():
			return nil, fctx.Err()
		}
		resp, err := up.Fetch(fctx, upstream.Request{
			Method: http.MethodGet, Path: "/v2/" + r.Repo + "/manifests/" + dg,
			Header: acceptHeader(r.Accept), Repo: r.Repo,
		})
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.PassLocation != "" {
			return nil, errPass
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			return nil, &statusError{status: resp.StatusCode, body: b, ctype: resp.Header.Get("Content-Type"), retryAfter: resp.Header.Get("Retry-After")}
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, MaxSize+1))
		if err != nil {
			return nil, err
		}
		if len(body) > MaxSize {
			return nil, errTooLarge
		}
		sum := sha256.Sum256(body)
		if "sha256:"+hex.EncodeToString(sum[:]) != dg {
			return nil, errMismatch
		}
		m := &Manifest{Body: body, MediaType: resp.Header.Get("Content-Type"), Digest: dg, Stored: c.now()}
		c.digests.Add(key, m, int64(len(body))+256)
		return fetched{m: m, hops: resp.Hops}, nil
	})
	if err != nil {
		var se *statusError
		switch {
		case errors.Is(err, errPass):
			c.bypass.Add(1)
			return Outcome{Bypass: true}
		case errors.As(err, &se) && (se.status == http.StatusNotFound || se.status == http.StatusUnauthorized):
			return Outcome{NotFound: true, Cache: "miss"}
		case errors.As(err, &se):
			return Outcome{Status: se.status, Body: se.body, BodyType: se.ctype, RetryAfter: se.retryAfter, Cache: "miss"}
		}
		return Outcome{Err: err, Cache: "miss"}
	}
	c.misses.Add(1)
	f := v.(fetched)
	return Outcome{Manifest: f.m, Cache: "miss", Hops: f.hops}
}

// IsTooLarge、IsMismatch 让调用方区分这两种不能转发的情况。
func IsTooLarge(err error) bool { return errors.Is(err, errTooLarge) }
func IsMismatch(err error) bool { return errors.Is(err, errMismatch) }

func acceptHeader(values []string) http.Header {
	h := http.Header{}
	a := strings.Join(values, ", ")
	if len(a) > 4096 {
		a = a[:4096]
	}
	if a == "" {
		a = DefaultAccept
	}
	h.Set("Accept", a)
	return h
}

// DefaultAccept 是客户端没带 Accept 时向上游要 manifest 用的值。
const DefaultAccept = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"
