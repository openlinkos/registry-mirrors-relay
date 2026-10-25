package lru

import (
	"sync"
	"testing"
)

func TestEvictByCount(t *testing.T) {
	c := New[string, int](2, 0)
	c.Add("a", 1, 0)
	c.Add("b", 2, 0)
	c.Get("a") // a becomes most recently used
	c.Add("c", 3, 0)
	if _, ok := c.Peek("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := c.Peek("a"); !ok {
		t.Fatal("a was just used and should not have been evicted")
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
	c.Add("c", "", 4) // exceeds 10, evicts a
	if c.Weight() != 8 || len(evicted) != 1 || evicted[0] != "a" {
		t.Fatalf("weight=%d evicted=%v", c.Weight(), evicted)
	}
	if c.Add("huge", "", 11) {
		t.Fatal("a single entry over the limit should not be added")
	}
	c.Add("b", "", 1) // replacing recalculates the weight
	if c.Weight() != 5 {
		t.Fatalf("after replace weight=%d", c.Weight())
	}
	c.Remove("b")
	if c.Weight() != 4 || c.Len() != 1 {
		t.Fatalf("after remove weight=%d len=%d", c.Weight(), c.Len())
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
		t.Fatalf("over the limit: %d", c.Len())
	}
}
