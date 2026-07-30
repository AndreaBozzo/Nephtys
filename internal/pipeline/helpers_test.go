package pipeline

import (
	"context"
	"testing"

	"nephtys/internal/domain"
)

// The constructors below return an error rather than silently defaulting a
// malformed value. These helpers keep tests that exercise *behavior* free of
// that error handling; the error paths themselves are covered in validate_test.go.

func mustBatch(tb testing.TB, ctx context.Context, cfg *domain.BatchConfig) Middleware {
	tb.Helper()
	mw, err := NewBatch(ctx, cfg)
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

func mustBuild(tb testing.TB, ctx context.Context, streamID string, cfg *domain.PipelineConfig) *Pipeline {
	tb.Helper()
	pipe, err := BuildFromConfig(ctx, streamID, cfg)
	if err != nil {
		tb.Fatalf("BuildFromConfig: %v", err)
	}
	return pipe
}
