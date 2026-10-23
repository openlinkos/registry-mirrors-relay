// Package lru 是一个带上限的 LRU 缓存，可以按条数和权重（例如字节数）同时限制。并发安全。
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

// Cache 的 Add、Get 都是 O(1)。
type Cache[K comparable, V any] struct {
	mu        sync.Mutex
	ll        *list.List
	items     map[K]*list.Element
	maxItems  int
	maxWeight int64 // 0 表示不按权重限制
	weight    int64
	onEvict   func(K, V)
}

// New 创建缓存。maxItems 至少为 1；maxWeight 为 0 时只按条数限制。
func New[K comparable, V any](maxItems int, maxWeight int64) *Cache[K, V] {
	if maxItems < 1 {
		maxItems = 1
	}
	return &Cache[K, V]{ll: list.New(), items: map[K]*list.Element{}, maxItems: maxItems, maxWeight: maxWeight}
}

// OnEvict 设置淘汰时的回调（在持锁时调用，回调里不要再操作这个缓存）。
func (c *Cache[K, V]) OnEvict(f func(K, V)) { c.onEvict = f }

// Get 取值并把它移到最近使用。
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

// Peek 取值，不改变顺序。
func (c *Cache[K, V]) Peek(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		return el.Value.(*entry[K, V]).val, true
	}
	var zero V
	return zero, false
}

// Add 放入或替换。权重超过上限的单个条目不放入，返回 false。
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

// Remove 删除一项。
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

// Len 是条数。
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Weight 是当前的总权重。
func (c *Cache[K, V]) Weight() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.weight
}
