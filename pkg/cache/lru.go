// Package cache provides a small concurrency-safe LRU cache.
package cache

import (
	"container/list"
	"sync"
)

// LRU holds at most capacity entries and evicts the least recently used one.
// Get and Add both count as a use.
type LRU[K comparable, V any] struct {
	capacity int

	mu    sync.Mutex
	items map[K]*list.Element
	order *list.List
}

type entry[K comparable, V any] struct {
	key   K
	value V
}

// NewLRU returns a cache holding at most capacity entries; a non-positive
// capacity is treated as 1.
func NewLRU[K comparable, V any](capacity int) *LRU[K, V] {
	if capacity <= 0 {
		capacity = 1
	}
	return &LRU[K, V]{capacity: capacity, items: make(map[K]*list.Element), order: list.New()}
}

// Get returns the cached value and marks it most recently used.
func (c *LRU[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.order.MoveToFront(element)
	return element.Value.(*entry[K, V]).value, true
}

// Add stores value under key, replacing any existing value, and evicts the
// least recently used entries beyond capacity.
func (c *LRU[K, V]) Add(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.items[key]; ok {
		element.Value.(*entry[K, V]).value = value
		c.order.MoveToFront(element)
		return
	}
	c.items[key] = c.order.PushFront(&entry[K, V]{key: key, value: value})
	for c.order.Len() > c.capacity {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*entry[K, V]).key)
	}
}

// Len reports the number of cached entries.
func (c *LRU[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
