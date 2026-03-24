package pipeline

import (
	"encoding/json"
	"sync"
	"time"

	"nephtys/internal/domain"
)

// NewBatch creates a middleware that buffers events and flushes them
// periodically or when a maximum size is reached.
func NewBatch(cfg *domain.BatchConfig) Middleware {
	if cfg == nil || !cfg.Enabled {
		return nil
	}

	maxSize := cfg.MaxBatchSize
	if maxSize <= 0 {
		maxSize = 100
	}

	flushInterval := 1 * time.Second
	if cfg.FlushInterval != "" {
		if parsed, err := time.ParseDuration(cfg.FlushInterval); err == nil {
			flushInterval = parsed
		}
	}

	return func(next Handler) Handler {
		var mu sync.Mutex
		var batch []domain.StreamEvent
		var timer *time.Timer
		var lastTopic string

		flushLocked := func() {
			if len(batch) == 0 {
				if timer != nil {
					timer.Stop()
					timer = nil
				}
				return
			}
			events := batch
			batch = nil
			topic := lastTopic
			if timer != nil {
				timer.Stop()
				timer = nil
			}

			// Unlock to call next securely
			mu.Unlock()

			payloads := make([]json.RawMessage, len(events))
			for i, e := range events {
				payloads[i] = e.Payload
			}
			arrayPayload, _ := json.Marshal(payloads)
			batchedEvent := domain.StreamEvent{
				Source:    events[0].Source,
				Type:      events[0].Type + "_batch",
				Timestamp: time.Now().UnixMilli(),
				Payload:   arrayPayload,
			}
			_ = next(topic, batchedEvent)

			// Relock since caller expects lock on return
			mu.Lock()
		}

		return func(topic string, event domain.StreamEvent) error {
			mu.Lock()
			defer mu.Unlock()

			batch = append(batch, event)
			lastTopic = topic

			if len(batch) >= maxSize {
				flushLocked()
				return nil
			}

			if timer == nil {
				timer = time.AfterFunc(flushInterval, func() {
					mu.Lock()
					flushLocked()
					mu.Unlock()
				})
			}

			return nil
		}
	}
}
