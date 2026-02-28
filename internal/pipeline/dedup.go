package pipeline

import (
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

	// Simple hash cache
	cache := make(map[uint64]time.Time, cacheSize)
	var mu sync.Mutex

	return func(event domain.StreamEvent) (domain.StreamEvent, bool) {
		// Calculate FNV-1a hash of the payload
		h := fnv.New64a()
		h.Write(event.Payload)
		hash := h.Sum64()

		mu.Lock()
		defer mu.Unlock()

		now := time.Now()

		// Map cleanup if it gets too large (simple approach)
		if len(cache) >= cacheSize {
			for k, t := range cache {
				if now.Sub(t) > ttl {
					delete(cache, k)
				}
			}
			// If still too large, just clear it to prevent memory leaks in this simple implementation
			if len(cache) >= cacheSize {
				cache = make(map[uint64]time.Time, cacheSize)
			}
		}

		// Check if seen
		if seenAt, exists := cache[hash]; exists {
			if now.Sub(seenAt) <= ttl {
				return event, false // Duplicate, drop it
			}
		}

		// Mark as seen
		cache[hash] = now
		return event, true
	}
}
