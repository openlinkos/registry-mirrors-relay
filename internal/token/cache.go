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

// Credential 是取令牌用的账号。
type Credential struct {
	Username, Password string
}

// Request 描述要取的令牌。
type Request struct {
	Realm   string
	Service string
	Scopes  []string
	Cred    *Credential // nil 表示匿名
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

// StatusError 是令牌服务返回的非 200 状态。
type StatusError struct{ Status int }

func (e *StatusError) Error() string { return fmt.Sprintf("令牌服务返回 %d", e.Status) }

type entry struct {
	token   string
	expires time.Time
}

// Cache 取令牌并缓存。并发的同一请求只向令牌服务发一次。
type Cache struct {
	client    *http.Client
	timeout   time.Duration
	userAgent string
	now       func() time.Time
	entries   *lru.Cache[string, entry]
	sf        singleflight.Group

	// OnCredentialRejected 在配置的账号被令牌服务拒绝时调用（之后改用匿名），可以为 nil。
	OnCredentialRejected func(realm, username string)
}

// maxTokenResponse 是令牌响应正文的上限。
const maxTokenResponse = 1 << 20

// NewCache 创建缓存。client 应当是带拨号拦截的上游客户端。
func NewCache(client *http.Client, timeout time.Duration, userAgent string, maxEntries int) *Cache {
	return &Cache{
		client:    client,
		timeout:   timeout,
		userAgent: userAgent,
		now:       time.Now,
		entries:   lru.New[string, entry](maxEntries, 0),
	}
}

// Get 返回令牌：缓存里有并且没过期就直接用，否则去取。
func (c *Cache) Get(ctx context.Context, req Request) (string, error) {
	k := req.key()
	if e, ok := c.entries.Get(k); ok && c.now().Before(e.expires) {
		return e.token, nil
	}
	// 取令牌和发起它的客户端连接脱钩：第一个客户端断开，不应让等着同一个令牌的其他请求一起失败
	v, err, _ := c.sf.Do(k, func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
		defer cancel()
		e, err := c.fetch(fctx, req)
		var se *StatusError
		if req.Cred != nil && errors.As(err, &se) && (se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden) {
			// 账号被拒绝：退回匿名，别让配置错误把拉取全部拖垮
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

// Invalidate 丢掉缓存的令牌（例如上游用它返回了 401）。
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
		return entry{}, fmt.Errorf("令牌响应不是合法的 JSON：%w", err)
	}
	tok := body.Token
	if tok == "" {
		tok = body.AccessToken
	}
	if tok == "" {
		return entry{}, errors.New("令牌响应里没有 token")
	}
	return entry{token: tok, expires: c.now().Add(ttl(body.ExpiresIn))}, nil
}

// ttl 按 expires_in 计算缓存时长，提前一段时间过期，留出网络往返的余量。规范规定缺省为 60 秒。
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
