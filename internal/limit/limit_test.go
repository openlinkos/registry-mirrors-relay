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
		t.Fatal("wrong keys for IPv4 and user name")
	}
	a := Key("", netip.MustParseAddr("2001:db8:1:2:aaaa::1"))
	b := Key("", netip.MustParseAddr("2001:db8:1:2:bbbb::9"))
	if a != b || a != "ip:2001:db8:1:2::/64" {
		t.Fatalf("addresses in the same /64 should share one key: %s %s", a, b)
	}
	if Key("", netip.MustParseAddr("::ffff:198.51.100.7")) != "ip:198.51.100.7" {
		t.Fatal("IPv4-mapped address should be unmapped")
	}
}

func TestManifestBucket(t *testing.T) {
	l := New(Config{ManifestRate: 2, ManifestBurst: 4, HeadWeight: 0.25, TrackedClients: 10})
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		if ok, _ := l.AllowManifest("k", ip, false, Override{}); !ok {
			t.Fatalf("request %d within the burst should pass", i+1)
		}
	}
	ok, wait := l.AllowManifest("k", ip, false, Override{})
	if ok || wait < time.Second {
		t.Fatalf("exceeding the burst should be refused with a wait time: %v %v", ok, wait)
	}
	now = now.Add(500 * time.Millisecond) // refills 1 token
	if ok, _ := l.AllowManifest("k", ip, false, Override{}); !ok {
		t.Fatal("request should pass after tokens are refilled")
	}
	now = now.Add(125 * time.Millisecond) // refills 0.25 tokens: enough for one HEAD
	if ok, _ := l.AllowManifest("k", ip, true, Override{}); !ok {
		t.Fatal("HEAD should cost only 0.25")
	}
	if ok, _ := l.AllowManifest("other", ip, false, Override{}); !ok {
		t.Fatal("different clients should not affect each other")
	}
	// account with a raised limit
	for i := 0; i < 50; i++ {
		if ok, _ := l.AllowManifest("user:ci", ip, false, Override{ManifestRate: 10}); !ok {
			t.Fatalf("request %d from the account with a raised limit should pass", i+1)
		}
	}
	// exempt addresses
	l.SetConfig(Config{ManifestRate: 2, ManifestBurst: 1, TrackedClients: 10, Exempt: []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}})
	for i := 0; i < 10; i++ {
		if ok, _ := l.AllowManifest("k", ip, false, Override{}); !ok {
			t.Fatal("exempt addresses should not be rate limited")
		}
	}
	if l.Stats().Limited == 0 {
		t.Fatal("the number of limited requests should be recorded")
	}
}

func TestTrackedClientsBounded(t *testing.T) {
	l := New(Config{ManifestRate: 1, ManifestBurst: 1, TrackedClients: 100})
	for i := 0; i < 1000; i++ {
		l.AllowManifest(Key("", netip.AddrFrom4([4]byte{198, 51, byte(i >> 8), byte(i)})), ip, false, Override{})
	}
	if n := l.Stats().TrackedClients; n != 100 {
		t.Fatalf("the number of remembered clients should be bounded: %d", n)
	}
}

func TestBlobStreams(t *testing.T) {
	l := New(Config{BlobStreamsPerClient: 2, MaxBlobStreams: 3, TrackedClients: 10})
	r1, ok1, _ := l.AcquireBlob("a", ip, Override{})
	r2, ok2, _ := l.AcquireBlob("a", ip, Override{})
	_, ok3, reason := l.AcquireBlob("a", ip, Override{})
	if !ok1 || !ok2 || ok3 || reason != "client" {
		t.Fatalf("at most 2 per client: %v %v %v %s", ok1, ok2, ok3, reason)
	}
	r4, ok4, _ := l.AcquireBlob("b", ip, Override{})
	_, ok5, reason := l.AcquireBlob("c", ip, Override{})
	if !ok4 || ok5 || reason != "global" {
		t.Fatalf("at most 3 globally: %v %v %s", ok4, ok5, reason)
	}
	r1()
	r1() // releasing twice is harmless
	if _, ok, _ := l.AcquireBlob("c", ip, Override{}); !ok {
		t.Fatal("a slot should be available again after release")
	}
	r2()
	r4()
	if st := l.Stats(); st.BlobStreams != 1 {
		t.Fatalf("stream count: %d", st.BlobStreams)
	}
	l.mu.Lock()
	n := len(l.streams)
	l.mu.Unlock()
	if n != 1 {
		t.Fatalf("clients whose streams have all ended should be removed from the table: %d", n)
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
			t.Fatalf("attempt %d should be allowed", i+1)
		}
		l.AuthFailed(ip)
	}
	if ok, wait := l.AuthAllowed(ip); ok || wait < time.Second {
		t.Fatal("should refuse after 3 failures")
	}
	now = now.Add(20 * time.Second) // 3 refilled per minute: 1 every 20 seconds
	if ok, _ := l.AuthAllowed(ip); !ok {
		t.Fatal("should allow after refill")
	}
	if ok, _ := l.AuthAllowed(netip.MustParseAddr("198.51.100.8")); !ok {
		t.Fatal("other IPs should not be affected")
	}
}
