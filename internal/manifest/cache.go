// Package manifest is an in-memory manifest cache. It caches only manifests (usually a few KB each), never
// layers, and never writes to disk.
//
// A pull by tag first sends a HEAD to the upstream (Docker Hub does not count it). Once the digest is known,
// a manifest already in memory under that digest is used directly; only otherwise is it fetched once with a
// GET by digest. When the same tag is pulled repeatedly, only the first pull uses up the upstream's pull quota.
//
// Correctness:
//   - content stored by digest is immutable; its sha256 is verified before storing, and content that does
//     not match the digest is not stored and an error is returned;
//   - what is stored by tag is only the tag → digest mapping, keyed with the client's Accept, and it is
//     confirmed again with a HEAD after it expires (30 seconds by default);
//   - when the upstream fails (429, 5xx, unreachable, circuit breaker open), the expired tag mapping can still
//     be used for a while (6 hours by default);
//   - a 404 is remembered for a short time (15 seconds by default), so clients that keep requesting a missing
//     image do not hit the upstream every time.
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

// MaxSize is the size limit of a single manifest (the OCI distribution spec requires registries to support at
// least 4 MiB).
const MaxSize = 4 << 20

// Manifest is a manifest cached by digest.
type Manifest struct {
	Body      []byte
	MediaType string
	Digest    string
	Stored    time.Time
}

// TagRef is the manifest a tag points to; a HEAD can be answered from it without fetching the body.
type TagRef struct {
	Digest    string
	MediaType string
	Size      int64
	Stored    time.Time
}

// Outcome is the result of a resolution, from which the caller writes the response. Only one group of fields
// is set:
//   - Manifest: has a body (GET); a HEAD can also be answered from it.
//   - Head: metadata only, answers a HEAD.
//   - NotFound: the upstream says it does not exist (or requires authentication), answer 404.
//   - Status: the upstream returned an error status (Body may be empty).
//   - Err: network error, open circuit breaker and so on.
//   - Bypass: the cache cannot handle it (for example the upstream HEAD gives no digest, or a redirect has to
//     be passed to the client), and the caller forwards it as an ordinary request.
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
	Cache      string   // hit, miss, revalidated, stale or negative, written to the access log
	Hops       []string // redirects followed on the server side
}

// Stats holds cumulative counters.
type Stats struct {
	Hits, Misses, Revalidations, Stale, Negative, Bypass uint64
	Bytes                                                int64
	Entries                                              int
}

// Fetcher is the upstream; tests can replace it.
type Fetcher interface {
	Fetch(ctx context.Context, r upstream.Request) (*upstream.Response, error)
}

// Cache is safe for concurrent use.
type Cache struct {
	cfg      config.Cache
	digests  *lru.Cache[string, *Manifest]
	tags     *lru.Cache[string, TagRef]
	negative *lru.Cache[string, time.Time]
	sf       singleflight.Group
	sem      chan struct{} // number of manifest bodies being buffered at once
	timeout  time.Duration
	now      func() time.Time

	hits, misses, revalidations, stale, negHits, bypass atomic.Uint64
}

// New creates a cache. When cfg.MaxBytes is 0 it returns nil, and the caller should forward directly.
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

// Stats returns the counters.
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

// CanonicalAccept turns the client's Accept into a stable key: parameters removed, lower case, deduplicated
// and sorted.
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

// Request is a manifest request.
type Request struct {
	Upstream string
	Repo     string
	Ref      string // tag or digest
	Head     bool
	Accept   []string // the client's original Accept values
}

// Resolve resolves a manifest request according to the caching rules.
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
			// The upstream HEAD gives no digest (or not a sha256 one): cannot cache, forward as an ordinary request
			c.bypass.Add(1)
			return Outcome{Bypass: true}
		}
		tr = TagRef{Digest: dg, MediaType: resp.Header.Get("Content-Type"), Size: resp.ContentLength, Stored: c.now()}
		c.tags.Add(base, tr, 0)
		c.revalidations.Add(1)
		return c.fromTag(ctx, up, r, accept, tr, "revalidated", resp.Hops)
	case s == http.StatusNotFound || s == http.StatusUnauthorized:
		// Docker Hub returns 401 for repositories that do not exist: treat it as not found too
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

// fromTag: the tag has been mapped to a digest; fetch the body by digest (for a HEAD the metadata is enough).
func (c *Cache) fromTag(ctx context.Context, up Fetcher, r Request, accept string, tr TagRef, how string, hops []string) Outcome {
	if m, ok := c.getDigest(r.Upstream, r.Repo, tr.Digest); ok {
		if how == "hit" {
			c.hits.Add(1)
		}
		return Outcome{Manifest: m, Cache: how, Hops: hops}
	}
	if r.Head {
		if tr.Size < 0 {
			// Without the size a HEAD cannot be answered, and a HEAD must not cause a GET: forward as an ordinary request
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
		// The client only sent a HEAD: do not send a GET upstream for it (only GETs count against Docker Hub's
		// quota), forward as an ordinary request
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
	errTooLarge = errors.New("manifest exceeds 4 MiB")
	errMismatch = errors.New("manifest content does not match digest")
	errPass     = errors.New("upstream requires a direct connection from the client")
)

type statusError struct {
	status     int
	body       []byte
	ctype      string
	retryAfter string
}

func (e *statusError) Error() string { return "upstream returned " + http.StatusText(e.status) }

type fetched struct {
	m    *Manifest
	hops []string
}

// fetch performs one GET by digest, verifies it and stores it in the cache. Concurrent identical requests are
// merged into one that is detached from the client connection that started it: if the first client
// disconnects, the other requests waiting for the same manifest must not fail with it.
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

// IsTooLarge and IsMismatch let the caller tell apart these two cases that cannot be relayed.
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

// DefaultAccept is the Accept used to request a manifest from the upstream when the client sends none.
const DefaultAccept = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"
