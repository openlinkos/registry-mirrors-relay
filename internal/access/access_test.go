package access

import (
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func hash(t *testing.T, pw string) []byte {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestParseHtpasswd(t *testing.T) {
	h := string(hash(t, "pw"))
	users, err := ParseHtpasswd(strings.NewReader("# 注释\n\nalice:" + h + "\nbob:" + strings.Replace(h, "$2a$", "$2y$", 1) + "\n"))
	if err != nil || len(users) != 2 || users[0].Name != "alice" || users[0].Source != "config" {
		t.Fatalf("%v %v", users, err)
	}
	for _, bad := range []string{
		"alice:{SHA}W6ph5Mm5Pz8GgiULbPgzG37mj9g=",
		"alice:$apr1$abc$def",
		"alice:plaintext",
		"nocolon",
		"alice:" + h + "\nalice:" + h,
	} {
		if _, err := ParseHtpasswd(strings.NewReader(bad)); err == nil {
			t.Errorf("应拒绝：%q", bad)
		}
	}
}

func TestVerifyAndCache(t *testing.T) {
	a := NewAuthenticator()
	u := &User{Name: "alice", Hash: hash(t, "correct horse")}
	ok, err := a.Verify(u, "correct horse")
	if !ok || err != nil {
		t.Fatal("正确的口令应通过")
	}
	if ok, _ := a.Verify(u, "wrong"); ok {
		t.Fatal("错误的口令不应通过")
	}
	if ok, _ := a.Verify(nil, "x"); ok {
		t.Fatal("不存在的用户不应通过")
	}
	if ok, _ := a.Verify(&User{Name: "alice", Hash: u.Hash, Disabled: true}, "correct horse"); ok {
		t.Fatal("停用的用户不应通过")
	}
	// 缓存命中：不再跑 bcrypt。把信号量占满，命中缓存仍能通过
	for i := 0; i < cap(a.sem); i++ {
		a.sem <- struct{}{}
	}
	if ok, err := a.Verify(u, "correct horse"); !ok || err != nil {
		t.Fatalf("缓存命中时不需要 bcrypt：%v %v", ok, err)
	}
	if _, err := a.Verify(u, "other"); !errors.Is(err, ErrBusy) {
		t.Fatalf("bcrypt 名额用完时应返回 ErrBusy：%v", err)
	}
	// 换了哈希，缓存自然失效
	u2 := &User{Name: "alice", Hash: hash(t, "correct horse")}
	if _, err := a.Verify(u2, "correct horse"); !errors.Is(err, ErrBusy) {
		t.Fatal("哈希变了不应命中旧缓存")
	}
}

func TestIPRules(t *testing.T) {
	p := netip.MustParsePrefix
	now := time.Unix(1792744703, 0)
	r := NewIPRules([]netip.Prefix{p("10.0.0.0/8"), p("2001:db8::/32")}, []netip.Prefix{p("10.9.0.0/16")},
		[]Ban{{Prefix: p("10.1.2.3/32"), Until: now.Add(time.Hour)}, {Prefix: p("10.4.0.0/16"), Until: now.Add(-time.Second)}})
	for ip, want := range map[string]string{
		"10.0.0.1": "", "10.9.1.1": "ip_deny", "10.1.2.3": "banned", "10.4.0.1": "", "8.8.8.8": "ip_allow",
		"2001:db8::1": "", "::ffff:10.0.0.1": "",
	} {
		ok, reason := r.Decide(netip.MustParseAddr(ip), now)
		if reason != want || ok != (want == "") {
			t.Errorf("%s：期望 %q，实际 %v %q", ip, want, ok, reason)
		}
	}
	if ok, _ := (*IPRules)(nil).Decide(netip.MustParseAddr("1.1.1.1"), now); !ok {
		t.Fatal("nil 规则应放行")
	}
}

func TestImagePatterns(t *testing.T) {
	cases := map[string]map[string]bool{
		"ghcr.io/evil/**":         {"ghcr.io/evil/x": true, "ghcr.io/evil/x/y": true, "ghcr.io/evil": true, "ghcr.io/evilcorp/x": false, "docker.io/evil/x": false},
		"*/library/nginx":         {"docker.io/library/nginx": true, "quay.io/library/nginx": true, "docker.io/library/nginx2": false},
		"docker.io/bitnami/*":     {"docker.io/bitnami/redis": true, "docker.io/bitnami/a/b": false},
		"docker.io/*/miner*":      {"docker.io/x/miner": true, "docker.io/x/minerd": true, "docker.io/x/y/miner": false},
		"gcr.io/google.com/**":    {"gcr.io/google.com/cloudsdktool/cloud-sdk": true},
		"Docker.io/Library/Nginx": {"docker.io/library/nginx": true},
	}
	for p, want := range cases {
		re, err := CompileImagePattern(p)
		if err != nil {
			t.Fatalf("%s：%v", p, err)
		}
		for name, w := range want {
			if got := re.MatchString(name); got != w {
				t.Errorf("%s 匹配 %s：期望 %v，实际 %v（%s）", p, name, w, got, re)
			}
		}
	}
	for _, bad := range []string{"nginx", "**/x", "docker.io//x"} {
		if _, err := CompileImagePattern(bad); err == nil {
			t.Errorf("应拒绝 %q", bad)
		}
	}
}

func TestImageRules(t *testing.T) {
	r, err := NewImageRules([]string{"docker.io/library/*", "ghcr.io/**"}, []ImageRule{{Pattern: "ghcr.io/evil/**", Status: 451, Message: "投诉屏蔽"}})
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, _, _ := r.Decide("docker.io/library/nginx"); !ok {
		t.Fatal("白名单内应放行")
	}
	if ok, st, msg, rule := r.Decide("ghcr.io/evil/x"); ok || st != 451 || msg != "投诉屏蔽" || rule != "ghcr.io/evil/**" {
		t.Fatalf("黑名单优先：%v %d %q %q", ok, st, msg, rule)
	}
	if ok, st, _, _ := r.Decide("quay.io/x/y"); ok || st != 403 {
		t.Fatal("有白名单时，不在白名单里的应拒绝")
	}
}

func TestVerifyConcurrent(t *testing.T) {
	a := NewAuthenticator()
	u := &User{Name: "u", Hash: hash(t, "pw")}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = a.Verify(u, "pw") }()
	}
	wg.Wait()
}
