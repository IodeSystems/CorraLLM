// Package cache is the in-process cache. Per the codex there is NO external
// cache service — this lives in the process address space.
package cache

import "sync"

// Cache is a simple in-process key/value cache with no external dependency.
type Cache struct {
	mu sync.RWMutex
	m  map[string]any
}

// New builds an empty in-process cache.
func New() *Cache { return &Cache{m: map[string]any{}} }

// Get returns the cached value for key.
func (c *Cache) Get(key string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[key]
	return v, ok
}

// Set stores value under key.
func (c *Cache) Set(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = value
}
