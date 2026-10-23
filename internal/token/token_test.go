package token

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseChallenges(t *testing.T) {
	cases := []struct {
		in     []string
		scheme string
		params map[string]string
		n      int
	}{
		{[]string{`Bearer realm="https://auth.docker.io/token",service="registry.docker.io"`}, "bearer",
			map[string]string{"realm": "https://auth.docker.io/token", "service": "registry.docker.io"}, 1},
		{[]string{`Bearer realm="https://gcr.io/v2/token",service=gcr.io`}, "bearer",
			map[string]string{"realm": "https://gcr.io/v2/token", "service": "gcr.io"}, 1},
		{[]string{`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:user/image:pull"`}, "bearer",
			map[string]string{"scope": "repository:user/image:pull"}, 1},
		{[]string{`Bearer realm="https://x/token", scope="repository:a:pull,push"`}, "bearer",
			map[string]string{"scope": "repository:a:pull,push"}, 1},
		{[]string{`Basic realm="r", Bearer realm="https://x/t",service="s"`}, "basic", map[string]string{"realm": "r"}, 2},
		{[]string{`bearer REALM="https://x/t"`, `Basic realm="a \"quoted\" b"`}, "bearer", map[string]string{"realm": "https://x/t"}, 2},
		{[]string{`Bearer realm="https://us-west2-docker.pkg.dev/v2/token"`}, "bearer",
			map[string]string{"realm": "https://us-west2-docker.pkg.dev/v2/token"}, 1},
	}
	for _, c := range cases {
		got := ParseChallenges(c.in)
		if len(got) != c.n {
			t.Errorf("%v：期望 %d 个质询，实际 %+v", c.in, c.n, got)
			continue
		}
		if got[0].Scheme != c.scheme {
			t.Errorf("%v：scheme %q", c.in, got[0].Scheme)
		}
		for k, v := range c.params {
			if got[0].Params[k] != v {
				t.Errorf("%v：%s = %q，期望 %q", c.in, k, got[0].Params[k], v)
			}
		}
	}
	if q := ParseChallenges([]string{`Basic realm="a \"quoted\" b"`}); q[0].Params["realm"] != `a "quoted" b` {
		t.Fatalf("转义：%q", q[0].Params["realm"])
	}
	_ = ParseChallenges([]string{`=,,"`, `Bearer realm="unterminated`, ``})
}

func tokenServer(t *testing.T, calls *atomic.Int32, check func(r *http.Request) int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if code := check(r); code != 200 {
			w.WriteHeader(code)
			return
		}
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"tok-` + r.URL.Query().Get("scope") + `","expires_in":300}`))
	}))
}

func TestCacheAndSingleflight(t *testing.T) {
	var calls atomic.Int32
	srv := tokenServer(t, &calls, func(r *http.Request) int {
		if r.URL.Query().Get("service") != "svc" {
			return 400
		}
		return 200
	})
	defer srv.Close()
	c := NewCache(srv.Client(), 5*time.Second, "test", 16)
	req := Request{Realm: srv.URL + "/token", Service: "svc", Scopes: []string{"repository:a:pull"}}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := c.Get(context.Background(), req)
			if err != nil || tok != "tok-repository:a:pull" {
				t.Errorf("Get：%q %v", tok, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("并发的同一请求应只取一次，实际 %d 次", calls.Load())
	}
	// 过期后重新取
	base := time.Now()
	c.now = func() time.Time { return base.Add(290 * time.Second) }
	if _, err := c.Get(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("过期（300s 减去 30s 余量）后应重新取，实际 %d 次", calls.Load())
	}
	c.Invalidate(req)
	_, _ = c.Get(context.Background(), req)
	if calls.Load() != 3 {
		t.Fatal("Invalidate 后应重新取")
	}
}

func TestLeaderCancelDoesNotFailFollowers(t *testing.T) {
	var calls atomic.Int32
	srv := tokenServer(t, &calls, func(*http.Request) int { return 200 })
	defer srv.Close()
	c := NewCache(srv.Client(), 5*time.Second, "test", 16)
	req := Request{Realm: srv.URL + "/token", Scopes: []string{"repository:b:pull"}}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 2)
	go func() { _, err := c.Get(ctx, req); errc <- err }()
	time.Sleep(5 * time.Millisecond)
	go func() { _, err := c.Get(context.Background(), req); errc <- err }()
	cancel()
	for i := 0; i < 2; i++ {
		if err := <-errc; err != nil {
			t.Fatalf("第一个请求取消不应让取令牌失败：%v", err)
		}
	}
}

func TestCredentialFallback(t *testing.T) {
	var calls atomic.Int32
	srv := tokenServer(t, &calls, func(r *http.Request) int {
		if _, _, ok := r.BasicAuth(); ok {
			return 401
		}
		return 200
	})
	defer srv.Close()
	c := NewCache(srv.Client(), 5*time.Second, "test", 16)
	var rejected string
	c.OnCredentialRejected = func(_, user string) { rejected = user }
	tok, err := c.Get(context.Background(), Request{Realm: srv.URL, Scopes: []string{"s"}, Cred: &Credential{"bot", "bad"}})
	if err != nil || tok != "tok-s" || rejected != "bot" {
		t.Fatalf("账号被拒绝后应退回匿名：%q %v %q", tok, err, rejected)
	}
}

func TestFetchErrors(t *testing.T) {
	var calls atomic.Int32
	srv := tokenServer(t, &calls, func(*http.Request) int { return 503 })
	defer srv.Close()
	c := NewCache(srv.Client(), 5*time.Second, "test", 16)
	_, err := c.Get(context.Background(), Request{Realm: srv.URL})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 503 {
		t.Fatalf("应返回 StatusError 503：%v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"nope":1}`)) }))
	defer bad.Close()
	if _, err := NewCache(bad.Client(), time.Second, "t", 4).Get(context.Background(), Request{Realm: bad.URL}); err == nil {
		t.Fatal("没有 token 字段应报错")
	}
}

func TestTTL(t *testing.T) {
	for in, want := range map[int]time.Duration{0: 50 * time.Second, 300: 270 * time.Second, 3600: 3240 * time.Second, 5: time.Second} {
		if got := ttl(in); got != want {
			t.Errorf("ttl(%d) = %v，期望 %v", in, got, want)
		}
	}
}
