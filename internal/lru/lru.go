// Package lru is a bounded LRU cache that can be limited by both entry count and weight (for example
// bytes). It is safe for concurrent use.
package lru

import (
	"container/list"
	"sync"
)

type entry[K comparable, V any] struct {
	key    K
	val    V
	weight int64
}

// Cache has O(1) Add and Get.
type Cache[K comparable, V any] struct {
	mu        sync.Mutex
	ll        *list.List
	items     map[K]*list.Element
	maxItems  int
	maxWeight int64 // 0 means no weight limit
	weight    int64
	onEvict   func(K, V)
}

// New creates a cache. maxItems is at least 1; when maxWeight is 0 only the entry count is limited.
func New[K comparable, V any](maxItems int, maxWeight int64) *Cache[K, V] {
	if maxItems < 1 {
		maxItems = 1
	}
	return &Cache[K, V]{ll: list.New(), items: map[K]*list.Element{}, maxItems: maxItems, maxWeight: maxWeight}
}

// OnEvict sets the callback for evictions (it is called with the lock held, so the callback must not
// use this cache again).
func (c *Cache[K, V]) OnEvict(f func(K, V)) { c.onEvict = f }

// Get returns the value and marks it as most recently used.
func (c *Cache[K, V]) Get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*entry[K, V]).val, true
	}
	var zero V
	return zero, false
}

// Peek returns the value without changing the order.
func (c *Cache[K, V]) Peek(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		return el.Value.(*entry[K, V]).val, true
	}
	var zero V
	return zero, false
}

// Add inserts or replaces an entry. A single entry whose weight exceeds the limit is not added and Add
// returns false.
func (c *Cache[K, V]) Add(k K, v V, weight int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.maxWeight > 0 && weight > c.maxWeight {
		return false
	}
	if el, ok := c.items[k]; ok {
		e := el.Value.(*entry[K, V])
		c.weight += weight - e.weight
		e.val, e.weight = v, weight
		c.ll.MoveToFront(el)
	} else {
		c.items[k] = c.ll.PushFront(&entry[K, V]{key: k, val: v, weight: weight})
		c.weight += weight
	}
	for c.ll.Len() > c.maxItems || (c.maxWeight > 0 && c.weight > c.maxWeight) {
		c.removeElement(c.ll.Back())
	}
	return true
}

// Remove deletes an entry.
func (c *Cache[K, V]) Remove(k K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.removeElement(el)
	}
}

func (c *Cache[K, V]) removeElement(el *list.Element) {
	e := el.Value.(*entry[K, V])
	c.ll.Remove(el)
	delete(c.items, e.key)
	c.weight -= e.weight
	if c.onEvict != nil {
		c.onEvict(e.key, e.val)
	}
}

// Len is the number of entries.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Weight is the current total weight.
func (c *Cache[K, V]) Weight() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.weight
}
