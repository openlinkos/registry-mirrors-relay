package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/openlinkos/registry-mirrors-relay/internal/lru"
)

// Credential is the account used to obtain tokens.
type Credential struct {
	Username, Password string
}

// Request describes the token to obtain.
type Request struct {
	Realm   string
	Service string
	Scopes  []string
	Cred    *Credential // nil means anonymous
}

func (r Request) key() string {
	scopes := append([]string(nil), r.Scopes...)
	sort.Strings(scopes)
	who := "anonymous"
	if r.Cred != nil {
		who = "user:" + r.Cred.Username
	}
	return r.Realm + "\x00" + r.Service + "\x00" + strings.Join(scopes, " ") + "\x00" + who
}

// StatusError is a non-200 status returned by the token service.
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return fmt.Sprintf("token service returned %d", e.Status) }

type entry struct {
	token   string
	expires time.Time
}

// Cache obtains tokens and caches them. Concurrent identical requests reach the token service only
// once.
type Cache struct {
	client    *http.Client
	timeout   time.Duration
	userAgent string
	now       func() time.Time
	entries   *lru.Cache[string, entry]
	sf        singleflight.Group

	// OnCredentialRejected is called when the token service rejects the configured account (anonymous
	// access is used after that); it may be nil.
	OnCredentialRejected func(realm, username string)
}

// maxTokenResponse is the size limit for the token response body.
const maxTokenResponse = 1 << 20

// NewCache creates a cache. client should be the upstream client with the dial interception.
func NewCache(client *http.Client, timeout time.Duration, userAgent string, maxEntries int) *Cache {
	return &Cache{
		client:    client,
		timeout:   timeout,
		userAgent: userAgent,
		now:       time.Now,
		entries:   lru.New[string, entry](maxEntries, 0),
	}
}

// Get returns a token: a cached, unexpired one is used directly, otherwise a new one is obtained.
func (c *Cache) Get(ctx context.Context, req Request) (string, error) {
	k := req.key()
	if e, ok := c.entries.Get(k); ok && c.now().Before(e.expires) {
		return e.token, nil
	}
	// Obtaining the token is decoupled from the client connection that triggered it: the first client
	// disconnecting should not make other requests waiting for the same token fail with it
	v, err, _ := c.sf.Do(k, func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
		defer cancel()
		e, err := c.fetch(fctx, req)
		var se *StatusError
		if req.Cred != nil && errors.As(err, &se) && (se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden) {
			// Account rejected: fall back to anonymous, so a configuration error does not break every pull
			if c.OnCredentialRejected != nil {
				c.OnCredentialRejected(req.Realm, req.Cred.Username)
			}
			anon := req
			anon.Cred = nil
			e, err = c.fetch(fctx, anon)
		}
		if err != nil {
			return "", err
		}
		c.entries.Add(k, e, 0)
		return e.token, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// Invalidate discards the cached token (for example when the upstream returned 401 for it).
func (c *Cache) Invalidate(req Request) { c.entries.Remove(req.key()) }

func (c *Cache) fetch(ctx context.Context, req Request) (entry, error) {
	u, err := url.Parse(req.Realm)
	if err != nil {
		return entry{}, err
	}
	q := u.Query()
	if req.Service != "" {
		q.Set("service", req.Service)
	}
	for _, s := range req.Scopes {
		q.Add("scope", s)
	}
	u.RawQuery = q.Encode()
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return entry{}, err
	}
	hr.Header.Set("User-Agent", c.userAgent)
	hr.Header.Set("Accept", "application/json")
	if req.Cred != nil {
		hr.SetBasicAuth(req.Cred.Username, req.Cred.Password)
	}
	resp, err := c.client.Do(hr)
	if err != nil {
		return entry{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return entry{}, &StatusError{Status: resp.StatusCode}
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenResponse)).Decode(&body); err != nil {
		return entry{}, fmt.Errorf("token response is not valid JSON: %w", err)
	}
	tok := body.Token
	if tok == "" {
		tok = body.AccessToken
	}
	if tok == "" {
		return entry{}, errors.New("token response contains no token")
	}
	return entry{token: tok, expires: c.now().Add(ttl(body.ExpiresIn))}, nil
}

// ttl computes the cache duration from expires_in, expiring a little early to leave a margin for the
// network round trip. The specification sets the default to 60 seconds.
func ttl(expiresIn int) time.Duration {
	if expiresIn <= 0 {
		expiresIn = 60
	}
	d := time.Duration(expiresIn) * time.Second
	margin := max(10*time.Second, d/10)
	if d-margin < time.Second {
		return time.Second
	}
	return d - margin
}
