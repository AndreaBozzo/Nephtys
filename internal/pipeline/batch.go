package pipeline

import (
	"encoding/json"
	"log/slog"
	"time"

	"nephtys/internal/domain"
)

// NewBatch creates a middleware that buffers events and flushes them
// periodically or when a maximum size is reached.
//
// The generation owns the worker goroutine's lifetime: it stops accepting into
// the buffer when the generation is cancelled and drains once the generation is
// sealed. See Generation.Retire for why those are two separate signals.
//
// It returns an error rather than falling back to defaults when cfg states a
// flush interval or batch size it cannot honour — see resolveDuration.
func NewBatch(gen *Generation, cfg *domain.BatchConfig) (Middleware, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, nil
	}

	maxSize, err := resolveCount("pipeline.batch.max_batch_size", cfg.MaxBatchSize, defaultMaxBatchSize, maxBatchSize)
	if err != nil {
		return nil, err
	}

	flushInterval, err := resolveDuration("pipeline.batch.flush_interval", cfg.FlushInterval, defaultFlushInterval)
	if err != nil {
		return nil, err
	}

	type topicEvent struct {
		topic string
		event domain.StreamEvent
	}

	// We return a middleware that acts as a passthrough to a background worker
	return func(next Handler) Handler {
		eventCh := make(chan topicEvent, maxSize)
		workerDone := gen.newWorker()

		// Background worker for batching
		go func() {
			defer close(workerDone)
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
			//
			// Its `default` branch is only sound because the caller waits for
			// the generation to be sealed first: at that point no publisher can
			// write to eventCh again, so finding it empty means it is empty
			// rather than merely empty right now.
			//
			// eventCh is created and written only within this closure and is
			// never closed, so the receive below cannot yield a zero value.
			drainAndFlush := func() {
				for {
					select {
					case te := <-eventCh:
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
				case <-gen.Done():
					// Cancellation stops new events entering the buffer, but
					// publishers that were already inside the chain may still
					// be writing to it. Sealing is the generation's statement
					// that they have all left; draining before it would strand
					// whatever arrives in between.
					<-gen.Sealed()
					drainAndFlush()
					return
				case te := <-eventCh:
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
			//
			// This deliberately runs before the cancellation check below. That
			// check exists to avoid handing an event to a worker that has
			// already drained and exited; a binary event never touches the
			// worker, so refusing it after cancellation would drop data for no
			// benefit. Downstream handlers do not observe the pipeline context.
			if event.IsBinary() {
				return next(topic, event)
			}

			// A cancelled generation has been retired — either the process is
			// shutting down or a pipeline hot-swap replaced it. Forward the
			// event downstream unbatched rather than buffering it for a worker
			// that is winding down. The batch envelope is a shape, not the
			// data: losing it beats dropping the event or reporting an ingest
			// error for a swap the operator asked for.
			//
			// This check racing with cancellation is harmless, which it was not
			// before #57. A publisher that passes it microseconds before
			// cancellation still holds the generation's read lock, so
			// retirement cannot seal — and therefore the worker cannot drain —
			// until this call returns. Whichever branch of the select below it
			// takes, the event is either buffered for a worker that will still
			// drain it or forwarded downstream here.
			if err := gen.Err(); err != nil {
				return next(topic, event)
			}

			// The Done case matters under load: publishers park here on a full
			// buffer, and cancellation releases them.
			select {
			case eventCh <- topicEvent{topic: topic, event: event}:
				return nil
			case <-gen.Done():
				return next(topic, event)
			}
		}
	}, nil
}
