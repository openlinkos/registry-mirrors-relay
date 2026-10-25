package clientip

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
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
		{"untrusted source ignores XFF", "198.51.100.9:5000", []string{"1.2.3.4"}, "198.51.100.9"},
		{"via CDN", "10.0.0.1:5000", []string{"198.51.100.23, 203.0.113.5"}, "198.51.100.23"},
		{"client's own private addresses on the left", "10.0.0.1:5000", []string{"192.168.32.20, 192.168.32.12, 198.51.100.23"}, "198.51.100.23"},
		{"forged client values cannot get past the real address", "10.0.0.1:5000", []string{"8.8.8.8, 198.51.100.23"}, "198.51.100.23"},
		{"multiple XFF lines", "10.0.0.1:5000", []string{"9.9.9.9", "198.51.100.23, 203.0.113.5"}, "198.51.100.23"},
		{"all trusted proxies", "10.0.0.1:5000", []string{"10.1.1.1, 10.2.2.2"}, "10.1.1.1"},
		{"malformed value", "10.0.0.1:5000", []string{"8.8.8.8, garbage, 203.0.113.5"}, "203.0.113.5"},
		{"no XFF", "10.0.0.1:5000", nil, "10.0.0.1"},
		{"IPv6 and zone", "[fe80::1%eth0]:5000", nil, "fe80::1"},
		{"IPv4-mapped address", "[::ffff:10.0.0.7]:5000", []string{"2001:db8::1"}, "2001:db8::1"},
		{"XFF with port", "10.0.0.1:5000", []string{"[2001:db8::2]:443"}, "2001:db8::2"},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = c.remote
		for _, v := range c.xff {
			req.Header.Add("X-Forwarded-For", v)
		}
		got, _ := cdn.ClientIP(req)
		if got.String() != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestNoTrustedProxies(t *testing.T) {
	r := resolver(t, false)
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got, _ := r.ClientIP(req); got.String() != "127.0.0.1" {
		t.Fatalf("XFF should be ignored when no trusted proxy is configured: %s", got)
	}
}

func TestXRealIP(t *testing.T) {
	r := resolver(t, true, "loopback")
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Real-IP", "198.51.100.7")
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got, _ := r.ClientIP(req); got.String() != "198.51.100.7" {
		t.Fatalf("X-Real-IP should take precedence: %s", got)
	}
	req.RemoteAddr = "198.51.100.1:1"
	if got, _ := r.ClientIP(req); got.String() != "198.51.100.1" {
		t.Fatalf("X-Real-IP should be ignored when the source is untrusted: %s", got)
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
			t.Fatalf("client IP must equal the source when the source is untrusted: %s %s", got, rem)
		}
	})
}

func TestProxied(t *testing.T) {
	if Proxied(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Fatal("a direct connection does not count as proxied")
	}
	for _, h := range []string{
		"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "Forwarded", "Via", "X-Real-IP",
		"CF-Connecting-IP", "True-Client-IP", "Fastly-Client-IP", "X-Client-IP", "X-Cluster-Client-IP",
		"X-Envoy-External-Address", "X-Original-Forwarded-For",
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set(h, "198.51.100.7")
		if !Proxied(r) {
			t.Errorf("a request with %s should count as proxied", h)
		}
	}
	old := httptest.NewRequest(http.MethodGet, "/", nil)
	old.Proto, old.ProtoMajor, old.ProtoMinor = "HTTP/1.0", 1, 0
	if !Proxied(old) {
		t.Fatal("HTTP/1.0 should count as proxied")
	}
}

func TestForwarded(t *testing.T) {
	r := resolver(t, false, "10.0.0.0/8")
	req := func(remote string, hdr map[string]string) *http.Request {
		q := httptest.NewRequest(http.MethodGet, "/", nil)
		q.RemoteAddr = remote
		for k, v := range hdr {
			q.Header.Set(k, v)
		}
		return q
	}
	if c, ok := r.Forwarded(req("10.0.0.1:5000", map[string]string{"X-Forwarded-For": "198.51.100.7"})); !ok || c != netip.MustParseAddr("198.51.100.7") {
		t.Fatalf("address passed on by a trusted proxy: %v %v", c, ok)
	}
	for name, q := range map[string]*http.Request{
		"untrusted peer":    req("192.0.2.1:5000", map[string]string{"X-Forwarded-For": "198.51.100.7"}),
		"Forwarded only":    req("10.0.0.1:5000", map[string]string{"Forwarded": "for=198.51.100.7"}),
		"Via only":          req("10.0.0.1:5000", map[string]string{"Via": "1.1 proxy"}),
		"use_x_real_ip off": req("10.0.0.1:5000", map[string]string{"X-Real-IP": "198.51.100.7"}),
		"no headers at all": req("10.0.0.1:5000", nil),
	} {
		if _, ok := r.Forwarded(q); ok {
			t.Errorf("%s: should not get a client address", name)
		}
	}
	xr := resolver(t, true, "10.0.0.0/8")
	if c, ok := xr.Forwarded(req("10.0.0.1:5000", map[string]string{"X-Real-IP": "198.51.100.8"})); !ok || c != netip.MustParseAddr("198.51.100.8") {
		t.Fatalf("with use_x_real_ip, X-Real-IP should be used: %v %v", c, ok)
	}
	if !r.Trusted(netip.MustParseAddr("10.9.9.9")) || r.Trusted(netip.MustParseAddr("192.0.2.1")) {
		t.Fatal("Trusted")
	}
}
