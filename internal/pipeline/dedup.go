package pipeline

import (
	"container/list"
	"hash/fnv"
	"sync"
	"time"

	"nephtys/internal/domain"
)

// NewDedup creates a deduplication middleware based on payload hashing.
func NewDedup(cfg *domain.DedupConfig) Middleware {
	if cfg == nil || !cfg.Enabled {
		return nil
	}

	ttl := 1 * time.Minute
	if cfg.TTL != "" {
		if parsed, err := time.ParseDuration(cfg.TTL); err == nil {
			ttl = parsed
		}
	}

	cacheSize := cfg.CacheSize
	if cacheSize <= 0 {
		cacheSize = 1000
	}

	// LRU cache components
	type entry struct {
		hash uint64
		ts   time.Time
	}
	ll := list.New()
	cache := make(map[uint64]*list.Element, cacheSize)
	var mu sync.Mutex

	return func(event domain.StreamEvent) (domain.StreamEvent, bool) {
		// Calculate FNV-1a hash of the payload
		h := fnv.New64a()
		h.Write(event.Payload)
		hash := h.Sum64()

		mu.Lock()
		defer mu.Unlock()

		now := time.Now()

		// Check if seen
		if elem, exists := cache[hash]; exists {
			ent := elem.Value.(*entry)
			if now.Sub(ent.ts) <= ttl {
				// Move to front (recently used)
				ll.MoveToFront(elem)
				return event, false // Duplicate, drop it
			}
			// Expired: technically we can just update it below, but let's remove it first
			ll.Remove(elem)
			delete(cache, hash)
		}

		// Map cleanup if it gets too large (LRU eviction)
		if len(cache) >= cacheSize {
			oldest := ll.Back()
			if oldest != nil {
				ll.Remove(oldest)
				delete(cache, oldest.Value.(*entry).hash)
			}
		}

		// Mark as seen
		elem := ll.PushFront(&entry{hash: hash, ts: now})
		cache[hash] = elem
		return event, true
	}
}
