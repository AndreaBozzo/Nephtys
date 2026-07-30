package pipeline

import (
	"testing"

	"nephtys/internal/domain"
)

// The constructors below return an error rather than silently defaulting a
// malformed value. These helpers keep tests that exercise *behavior* free of
// that error handling; the error paths themselves are covered in validate_test.go.

// mustBatch builds a batch middleware against gen. Pass a generation from
// newGeneration when the test drives retirement itself.
func mustBatch(tb testing.TB, gen *Generation, cfg *domain.BatchConfig) Middleware {
	tb.Helper()
	mw, err := NewBatch(gen, cfg)
	if err != nil {
		tb.Fatalf("NewBatch: %v", err)
	}
	return mw
}

func mustDedup(tb testing.TB, streamID string, cfg *domain.DedupConfig) Middleware {
	tb.Helper()
	mw, err := NewDedup(streamID, cfg)
	if err != nil {
		tb.Fatalf("NewDedup: %v", err)
	}
	return mw
}

// mustGeneration builds a full generation and retires it when the test ends, so
// no test leaks a batch worker.
func mustGeneration(tb testing.TB, streamID string, cfg *domain.PipelineConfig, publish Handler) *Generation {
	tb.Helper()
	gen, err := NewGeneration(streamID, cfg, publish)
	if err != nil {
		tb.Fatalf("NewGeneration: %v", err)
	}
	tb.Cleanup(gen.Retire)
	return gen
}
