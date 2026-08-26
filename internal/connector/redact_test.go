package connector

import (
	"strings"
	"testing"
)

// TestRedactURL covers what a connector is allowed to say about the endpoint
// that failed. These error strings reach GET /v1/streams through a stream's
// last_error, and endpoint URLs routinely carry a token.
func TestRedactURL(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		absent string
	}{
		{
			name:   "query string is dropped",
			in:     "wss://gateway.example.com/feed?apikey=TOPSECRET",
			want:   "wss://gateway.example.com/feed",
			absent: "TOPSECRET",
		},
		{
			name:   "userinfo is dropped",
			in:     "https://user:hunter2@api.example.com/events",
			want:   "https://api.example.com/events",
			absent: "hunter2",
		},
		{
			name:   "fragment is dropped",
			in:     "http://example.com/stream#token",
			want:   "http://example.com/stream",
			absent: "#token",
		},
		{
			name: "a plain endpoint survives intact",
			in:   "ws://example.com:8080/live",
			want: "ws://example.com:8080/live",
		},
		{
			// Nothing parseable to redact: say nothing rather than guess which
			// part of it might be a secret.
			name:   "unparseable input is not echoed",
			in:     "http://[::1",
			want:   "the configured endpoint",
			absent: "[::1",
		},
		{
			name:   "a bare token is not echoed",
			in:     "TOPSECRET",
			want:   "the configured endpoint",
			absent: "TOPSECRET",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactURL(tt.in)
			if got != tt.want {
				t.Errorf("redactURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if tt.absent != "" && strings.Contains(got, tt.absent) {
				t.Errorf("redactURL(%q) = %q, which still leaks %q", tt.in, got, tt.absent)
			}
		})
	}
}
