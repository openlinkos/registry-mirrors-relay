package breaker

import (
	"sync"
	"testing"
	"time"
)

func TestBreaker(t *testing.T) {
	b := New(3, 10*time.Second, 40*time.Second)
	now := time.Unix(1000, 0)
	b.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		b.Allow()
		b.Done(true)
	}
	if ok, _ := b.Allow(); !ok {
		t.Fatal("requests should pass before the threshold is reached")
	}
	b.Done(true) // third failure: open
	if ok, wait := b.Allow(); ok || wait != 10*time.Second {
		t.Fatalf("an open breaker should refuse and give a wait time: %v %v", ok, wait)
	}

	now = now.Add(10 * time.Second)
	if ok, _ := b.Allow(); !ok {
		t.Fatal("after the cooldown one probe request should pass")
	}
	if ok, _ := b.Allow(); ok {
		t.Fatal("other requests should be refused while the probe is in progress")
	}
	b.Done(true) // probe failed: cooldown doubles
	if ok, wait := b.Allow(); ok || wait != 20*time.Second {
		t.Fatalf("cooldown should double after a failed probe: %v %v", ok, wait)
	}
	now = now.Add(20 * time.Second)
	b.Allow()
	b.Done(true)
	now = now.Add(40 * time.Second)
	b.Allow()
	b.Done(true)
	if _, wait := b.Allow(); wait != 40*time.Second {
		t.Fatalf("cooldown should not exceed the limit: %v", wait)
	}

	now = now.Add(40 * time.Second)
	b.Allow()
	b.Done(false) // probe succeeded: close
	if ok, _ := b.Allow(); !ok || b.Open() {
		t.Fatal("breaker should close after a successful probe")
	}
	b.Done(true)
	b.Done(true)
	if b.Open() {
		t.Fatal("failure count should restart from 0 after closing")
	}
}

func TestSuccessResetsCount(t *testing.T) {
	b := New(2, time.Second, time.Second)
	b.Done(true)
	b.Done(false)
	b.Done(true)
	if b.Open() {
		t.Fatal("a success should reset the consecutive failure count")
	}
}

func TestNil(t *testing.T) {
	b := New(0, time.Second, time.Second)
	if b != nil {
		t.Fatal("threshold 0 should return nil")
	}
	if ok, _ := b.Allow(); !ok || b.Open() {
		t.Fatal("nil should always allow")
	}
	b.Done(true)
}

func TestConcurrent(t *testing.T) {
	b := New(5, time.Millisecond, 10*time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				if ok, _ := b.Allow(); ok {
					b.Done((i+j)%3 == 0)
				}
			}
		}(i)
	}
	wg.Wait()
}
