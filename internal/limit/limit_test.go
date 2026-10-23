package limit

import (
	"net/netip"
	"sync"
	"testing"
	"time"
)

var ip = netip.MustParseAddr("198.51.100.7")

func TestKey(t *testing.T) {
	if Key("alice", ip) != "user:alice" || Key("", ip) != "ip:198.51.100.7" {
		t.Fatal("IPv4 和用户名的键不对")
	}
	a := Key("", netip.MustParseAddr("2001:db8:1:2:aaaa::1"))
	b := Key("", netip.MustParseAddr("2001:db8:1:2:bbbb::9"))
	if a != b || a != "ip:2001:db8:1:2::/64" {
		t.Fatalf("同一个 /64 应共用一个键：%s %s", a, b)
	}
	if Key("", netip.MustParseAddr("::ffff:198.51.100.7")) != "ip:198.51.100.7" {
		t.Fatal("IPv4 映射地址应还原")
	}
}

func TestManifestBucket(t *testing.T) {
	l := New(Config{ManifestRate: 2, ManifestBurst: 4, HeadWeight: 0.25, TrackedClients: 10})
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		if ok, _ := l.AllowManifest("k", ip, false, Override{}); !ok {
			t.Fatalf("突发内第 %d 次应放行", i+1)
		}
	}
	ok, wait := l.AllowManifest("k", ip, false, Override{})
	if ok || wait < time.Second {
		t.Fatalf("超过突发应拒绝并给出等待：%v %v", ok, wait)
	}
	now = now.Add(500 * time.Millisecond) // 补回 1 个
	if ok, _ := l.AllowManifest("k", ip, false, Override{}); !ok {
		t.Fatal("补回令牌后应放行")
	}
	now = now.Add(125 * time.Millisecond) // 补回 0.25 个：够一次 HEAD
	if ok, _ := l.AllowManifest("k", ip, true, Override{}); !ok {
		t.Fatal("HEAD 只扣 0.25")
	}
	if ok, _ := l.AllowManifest("other", ip, false, Override{}); !ok {
		t.Fatal("不同客户端互不影响")
	}
	// 单独放宽的账号
	for i := 0; i < 50; i++ {
		if ok, _ := l.AllowManifest("user:ci", ip, false, Override{ManifestRate: 10}); !ok {
			t.Fatalf("放宽的账号第 %d 次应放行", i+1)
		}
	}
	// 豁免的地址
	l.SetConfig(Config{ManifestRate: 2, ManifestBurst: 1, TrackedClients: 10, Exempt: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}})
	for i := 0; i < 10; i++ {
		if ok, _ := l.AllowManifest("k", ip, false, Override{}); !ok {
			t.Fatal("豁免的地址不限流")
		}
	}
	if l.Stats().Limited == 0 {
		t.Fatal("应记下被限流的次数")
	}
}

func TestTrackedClientsBounded(t *testing.T) {
	l := New(Config{ManifestRate: 1, ManifestBurst: 1, TrackedClients: 100})
	for i := 0; i < 1000; i++ {
		l.AllowManifest(Key("", netip.AddrFrom4([4]byte{198, 51, byte(i >> 8), byte(i)})), ip, false, Override{})
	}
	if n := l.Stats().TrackedClients; n != 100 {
		t.Fatalf("记住的客户端数应受上限约束：%d", n)
	}
}

func TestBlobStreams(t *testing.T) {
	l := New(Config{BlobStreamsPerClient: 2, MaxBlobStreams: 3, TrackedClients: 10})
	r1, ok1, _ := l.AcquireBlob("a", ip, Override{})
	r2, ok2, _ := l.AcquireBlob("a", ip, Override{})
	_, ok3, reason := l.AcquireBlob("a", ip, Override{})
	if !ok1 || !ok2 || ok3 || reason != "client" {
		t.Fatalf("每个客户端最多 2 个：%v %v %v %s", ok1, ok2, ok3, reason)
	}
	r4, ok4, _ := l.AcquireBlob("b", ip, Override{})
	_, ok5, reason := l.AcquireBlob("c", ip, Override{})
	if !ok4 || ok5 || reason != "global" {
		t.Fatalf("全局最多 3 个：%v %v %s", ok4, ok5, reason)
	}
	r1()
	r1() // 重复释放无害
	if _, ok, _ := l.AcquireBlob("c", ip, Override{}); !ok {
		t.Fatal("释放后应能再占")
	}
	r2()
	r4()
	if st := l.Stats(); st.BlobStreams != 1 {
		t.Fatalf("流计数：%d", st.BlobStreams)
	}
	l.mu.Lock()
	n := len(l.streams)
	l.mu.Unlock()
	if n != 1 {
		t.Fatalf("流都结束的客户端应从表里删掉：%d", n)
	}
}

func TestInflight(t *testing.T) {
	l := New(Config{MaxInflight: 10, TrackedClients: 1})
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, rejected := 0, 0
	release := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, good := l.AcquireInflight()
			mu.Lock()
			if good {
				ok++
			} else {
				rejected++
			}
			mu.Unlock()
			if good {
				<-release
				r()
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if ok != 10 || rejected != 10 || l.Stats().Inflight != 0 {
		t.Fatalf("ok=%d rejected=%d inflight=%d", ok, rejected, l.Stats().Inflight)
	}
}

func TestAuthFailures(t *testing.T) {
	l := New(Config{AuthFailuresPerMin: 3, TrackedClients: 10})
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if ok, _ := l.AuthAllowed(ip); !ok {
			t.Fatalf("第 %d 次应允许尝试", i+1)
		}
		l.AuthFailed(ip)
	}
	if ok, wait := l.AuthAllowed(ip); ok || wait < time.Second {
		t.Fatal("失败 3 次后应拒绝")
	}
	now = now.Add(20 * time.Second) // 每分钟补回 3 次：20 秒补回 1 次
	if ok, _ := l.AuthAllowed(ip); !ok {
		t.Fatal("补回后应允许")
	}
	if ok, _ := l.AuthAllowed(netip.MustParseAddr("198.51.100.8")); !ok {
		t.Fatal("别的 IP 不受影响")
	}
}
