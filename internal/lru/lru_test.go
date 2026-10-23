package lru

import (
	"sync"
	"testing"
)

func TestEvictByCount(t *testing.T) {
	c := New[string, int](2, 0)
	c.Add("a", 1, 0)
	c.Add("b", 2, 0)
	c.Get("a") // a 变成最近使用
	c.Add("c", 3, 0)
	if _, ok := c.Peek("b"); ok {
		t.Fatal("b 应被淘汰")
	}
	if _, ok := c.Peek("a"); !ok {
		t.Fatal("a 刚用过，不应被淘汰")
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d", c.Len())
	}
}

func TestEvictByWeight(t *testing.T) {
	c := New[string, string](100, 10)
	var evicted []string
	c.OnEvict(func(k, _ string) { evicted = append(evicted, k) })
	c.Add("a", "", 4)
	c.Add("b", "", 4)
	c.Add("c", "", 4) // 超过 10，淘汰 a
	if c.Weight() != 8 || len(evicted) != 1 || evicted[0] != "a" {
		t.Fatalf("weight=%d evicted=%v", c.Weight(), evicted)
	}
	if c.Add("huge", "", 11) {
		t.Fatal("单个超过上限的条目不应放入")
	}
	c.Add("b", "", 1) // 替换时重算权重
	if c.Weight() != 5 {
		t.Fatalf("替换后 weight=%d", c.Weight())
	}
	c.Remove("b")
	if c.Weight() != 4 || c.Len() != 1 {
		t.Fatalf("删除后 weight=%d len=%d", c.Weight(), c.Len())
	}
}

func TestConcurrent(t *testing.T) {
	c := New[int, int](64, 0)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				c.Add(i%100+g, i, 1)
				c.Get(i % 50)
			}
		}(g)
	}
	wg.Wait()
	if c.Len() > 64 {
		t.Fatalf("超过上限：%d", c.Len())
	}
}
