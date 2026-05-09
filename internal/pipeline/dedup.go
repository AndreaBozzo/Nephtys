package pipeline

import (
	"container/list"
	"hash/fnv"
	"sync"
	"time"

	"nephtys/internal/domain"
	"nephtys/internal/telemetry"
)

// NewDedup creates a deduplication middleware based on payload hashing.
//
// Semantics: per-stream, in-memory, FNV-1a payload hash, LRU-bounded by
// cfg.CacheSize (default 1000). Entries expire after cfg.TTL (default 1m).
// State does not survive process restart and is not shared across instances —
// dedup is a "best-effort within this process and within this window" facility,
// not a distributed exactly-once guarantee. If the stream's unique-payload
// rate exceeds cfg.CacheSize entries within cfg.TTL, the LRU silently evicts
// older hashes and the effective dedup window collapses to the last
// cfg.CacheSize events. Operators with high-cardinality streams should size
// CacheSize for at least one full TTL window of expected unique payloads.
func NewDedup(streamID string, cfg *domain.DedupConfig) Middleware {
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

	return func(next Handler) Handler {
		return func(topic string, event domain.StreamEvent) error {
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
					telemetry.EventsDropped.WithLabelValues(streamID, "dedup").Inc()
					return nil // Duplicate, drop it
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
					telemetry.DedupCacheEvictions.WithLabelValues(streamID).Inc()
				}
			}

			// Mark as seen
			elem := ll.PushFront(&entry{hash: hash, ts: now})
			cache[hash] = elem
			telemetry.DedupCacheSize.WithLabelValues(streamID).Set(float64(len(cache)))
			return next(topic, event)
		}
	}
}
