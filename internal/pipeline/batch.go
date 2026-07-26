package pipeline

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"nephtys/internal/domain"
)

// NewBatch creates a middleware that buffers events and flushes them
// periodically or when a maximum size is reached.
// The provided context controls the lifetime of the background worker goroutine.
func NewBatch(ctx context.Context, cfg *domain.BatchConfig) Middleware {
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

	type topicEvent struct {
		topic string
		event domain.StreamEvent
	}

	// We return a middleware that acts as a passthrough to a background worker
	return func(next Handler) Handler {
		eventCh := make(chan topicEvent, maxSize)

		// Background worker for batching
		go func() {
			var batch []domain.StreamEvent
			var lastTopic string
			ticker := time.NewTicker(flushInterval)
			defer ticker.Stop()

			flush := func() {
				if len(batch) == 0 {
					return
				}

				payloads := make([]json.RawMessage, len(batch))
				for i, e := range batch {
					payloads[i] = e.Payload
				}
				arrayPayload, err := json.Marshal(payloads)
				if err != nil {
					slog.Error("batch marshal failed, dropping batch", "topic", lastTopic, "size", len(batch), "error", err)
					batch = batch[:0]
					return
				}

				batchedEvent := domain.StreamEvent{
					Source:    batch[0].Source,
					Type:      batch[0].Type + "_batch",
					Timestamp: time.Now().UnixMilli(),
					Payload:   arrayPayload,
				}

				// Using the last seen topic for the batch
				if err := next(lastTopic, batchedEvent); err != nil {
					slog.Error("batch flush failed", "topic", lastTopic, "error", err)
				}
				batch = batch[:0] // Reset batch, keeping allocated capacity
			}

			// drainAndFlush empties the buffered channel into the current
			// batch before flushing. Events already accepted by the handler
			// were reported to the source as published; discarding them on
			// shutdown or a pipeline hot-swap would be silent data loss.
			drainAndFlush := func() {
				for {
					select {
					case te, ok := <-eventCh:
						// A closed channel is always ready; without this guard
						// the loop would append zero-value events forever.
						if !ok {
							flush()
							return
						}
						batch = append(batch, te.event)
						lastTopic = te.topic
						if len(batch) >= maxSize {
							flush()
						}
					default:
						flush()
						return
					}
				}
			}

			for {
				select {
				case <-ctx.Done():
					drainAndFlush()
					return
				case te, ok := <-eventCh:
					if !ok {
						drainAndFlush()
						return // Channel closed, terminate worker
					}
					batch = append(batch, te.event)
					lastTopic = te.topic
					if len(batch) >= maxSize {
						flush()
						ticker.Reset(flushInterval)
					}
				case <-ticker.C:
					flush()
				}
			}
		}()

		// The returned handler just pushes to the channel
		return func(topic string, event domain.StreamEvent) error {
			// Binary events cannot be aggregated: the batch envelope is a JSON
			// array built from Payload, which is nil for them, and it carries
			// no ContentType. Batching one would discard its Data entirely, so
			// pass it straight through instead. The cost is that binary events
			// may overtake JSON events still sitting in the buffer on a stream
			// that mixes both — preferable to losing them.
			if event.IsBinary() {
				return next(topic, event)
			}

			// Prefer reporting a closed pipeline over handing an event to a
			// channel whose worker has already drained and exited.
			if err := ctx.Err(); err != nil {
				return err
			}

			select {
			case eventCh <- topicEvent{topic: topic, event: event}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}
