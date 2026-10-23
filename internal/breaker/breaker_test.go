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
		t.Fatal("未达阈值前应放行")
	}
	b.Done(true) // 第三次失败，打开
	if ok, wait := b.Allow(); ok || wait != 10*time.Second {
		t.Fatalf("打开后应拒绝并给出等待时间：%v %v", ok, wait)
	}

	now = now.Add(10 * time.Second)
	if ok, _ := b.Allow(); !ok {
		t.Fatal("冷却后应放一个试探请求")
	}
	if ok, _ := b.Allow(); ok {
		t.Fatal("试探进行中，其他请求应被拒绝")
	}
	b.Done(true) // 试探失败，冷却加倍
	if ok, wait := b.Allow(); ok || wait != 20*time.Second {
		t.Fatalf("试探失败后冷却应加倍：%v %v", ok, wait)
	}
	now = now.Add(20 * time.Second)
	b.Allow()
	b.Done(true)
	now = now.Add(40 * time.Second)
	b.Allow()
	b.Done(true)
	if _, wait := b.Allow(); wait != 40*time.Second {
		t.Fatalf("冷却不应超过上限：%v", wait)
	}

	now = now.Add(40 * time.Second)
	b.Allow()
	b.Done(false) // 试探成功，关闭
	if ok, _ := b.Allow(); !ok || b.Open() {
		t.Fatal("试探成功后应关闭")
	}
	b.Done(true)
	b.Done(true)
	if b.Open() {
		t.Fatal("关闭后失败计数应从 0 重新开始")
	}
}

func TestSuccessResetsCount(t *testing.T) {
	b := New(2, time.Second, time.Second)
	b.Done(true)
	b.Done(false)
	b.Done(true)
	if b.Open() {
		t.Fatal("成功应清零连续失败计数")
	}
}

func TestNil(t *testing.T) {
	b := New(0, time.Second, time.Second)
	if b != nil {
		t.Fatal("阈值为 0 应返回 nil")
	}
	if ok, _ := b.Allow(); !ok || b.Open() {
		t.Fatal("nil 应始终放行")
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
