package clientip

import (
	"net/http/httptest"
	"testing"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
)

func resolver(t *testing.T, xrealip bool, trusted ...string) *Resolver {
	t.Helper()
	ps, err := config.ParsePrefixes(trusted)
	if err != nil {
		t.Fatal(err)
	}
	return New(ps, xrealip)
}

func TestClientIP(t *testing.T) {
	cdn := resolver(t, false, "10.0.0.0/8", "203.0.113.0/24")
	cases := []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"不受信任的来源忽略 XFF", "198.51.100.9:5000", []string{"1.2.3.4"}, "198.51.100.9"},
		{"经 CDN", "10.0.0.1:5000", []string{"198.51.100.23, 203.0.113.5"}, "198.51.100.23"},
		{"客户端自己带的内网地址在最左边", "10.0.0.1:5000", []string{"192.168.32.20, 192.168.32.12, 198.51.100.23"}, "198.51.100.23"},
		{"客户端伪造的值不能越过真实地址", "10.0.0.1:5000", []string{"8.8.8.8, 198.51.100.23"}, "198.51.100.23"},
		{"多行 XFF", "10.0.0.1:5000", []string{"9.9.9.9", "198.51.100.23, 203.0.113.5"}, "198.51.100.23"},
		{"全是受信任的代理", "10.0.0.1:5000", []string{"10.1.1.1, 10.2.2.2"}, "10.1.1.1"},
		{"写坏的值", "10.0.0.1:5000", []string{"8.8.8.8, garbage, 203.0.113.5"}, "203.0.113.5"},
		{"没有 XFF", "10.0.0.1:5000", nil, "10.0.0.1"},
		{"IPv6 和 zone", "[fe80::1%eth0]:5000", nil, "fe80::1"},
		{"IPv4 映射地址", "[::ffff:10.0.0.7]:5000", []string{"2001:db8::1"}, "2001:db8::1"},
		{"带端口的 XFF", "10.0.0.1:5000", []string{"[2001:db8::2]:443"}, "2001:db8::2"},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = c.remote
		for _, v := range c.xff {
			req.Header.Add("X-Forwarded-For", v)
		}
		got, _ := cdn.ClientIP(req)
		if got.String() != c.want {
			t.Errorf("%s：期望 %s，实际 %s", c.name, c.want, got)
		}
	}
}

func TestNoTrustedProxies(t *testing.T) {
	r := resolver(t, false)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got, _ := r.ClientIP(req); got.String() != "127.0.0.1" {
		t.Fatalf("没有配置可信代理时应忽略 XFF：%s", got)
	}
}

func TestXRealIP(t *testing.T) {
	r := resolver(t, true, "loopback")
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-IP", "198.51.100.7")
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got, _ := r.ClientIP(req); got.String() != "198.51.100.7" {
		t.Fatalf("应优先用 X-Real-IP：%s", got)
	}
	req.RemoteAddr = "198.51.100.1:1"
	if got, _ := r.ClientIP(req); got.String() != "198.51.100.1" {
		t.Fatalf("来源不受信任时不看 X-Real-IP：%s", got)
	}
}

func FuzzClientIP(f *testing.F) {
	f.Add("10.0.0.1:1", "1.2.3.4, 10.0.0.2")
	f.Add("[::1]:1", "garbage,,, ")
	r := New(nil, false)
	cdn := resolver(&testing.T{}, false, "private_ranges")
	f.Fuzz(func(t *testing.T, remote, xff string) {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remote
		req.Header.Set("X-Forwarded-For", xff)
		r.ClientIP(req)
		got, rem := cdn.ClientIP(req)
		if rem.IsValid() && !cdn.isTrusted(rem) && got != rem {
			t.Fatalf("来源不受信任时客户端 IP 必须等于来源：%s %s", got, rem)
		}
	})
}
