// Package lru is a small least-recently-used cache with a cost budget,
// so memory stays bounded on small devices.
package lru

import (
	"container/list"
	"sync"
)

// Cache holds values up to a total cost; the least recently used go first.
type Cache[K comparable, V any] struct {
	mu    sync.Mutex
	max   int
	used  int
	cost  func(V) int
	order *list.List
	items map[K]*list.Element
}

type entry[K comparable, V any] struct {
	key  K
	val  V
	cost int
}

// New returns a cache that keeps at most maxCost; cost gives each value's
// cost (nil counts every value as 1, making maxCost an entry count).
func New[K comparable, V any](maxCost int, cost func(V) int) *Cache[K, V] {
	if cost == nil {
		cost = func(V) int { return 1 }
	}

	return &Cache[K, V]{mu: sync.Mutex{}, max: maxCost, used: 0, cost: cost, order: list.New(), items: map[K]*list.Element{}}
}

// Get returns a value and marks it recently used.
func (c *Cache[K, V]) Get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.items[k]; ok {
		c.order.MoveToFront(e)

		en, _ := e.Value.(*entry[K, V])

		return en.val, true
	}

	var zero V

	return zero, false
}

// Put stores a value; one that alone exceeds the budget is not stored.
func (c *Cache[K, V]) Put(k K, v V) {
	cost := c.cost(v)
	if c.max <= 0 || cost > c.max {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if e, ok := c.items[k]; ok {
		en, _ := e.Value.(*entry[K, V])
		c.used += cost - en.cost
		en.val, en.cost = v, cost
		c.order.MoveToFront(e)
	} else {
		c.items[k] = c.order.PushFront(&entry[K, V]{key: k, val: v, cost: cost})
		c.used += cost
	}

	for c.used > c.max {
		last := c.order.Back()
		en, _ := last.Value.(*entry[K, V])
		c.order.Remove(last)
		delete(c.items, en.key)
		c.used -= en.cost
	}
}

// Len is the number of entries.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.items)
}
