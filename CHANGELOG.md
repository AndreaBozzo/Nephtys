# Changelog

All notable changes to Nephtys are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
(pre-1.0: minor versions may contain breaking changes).

## [Unreleased]

### Added
- Post-acceptance roadmap (Aug 2026 – Jan 2027) with three-pillar direction: ops core, visibility, agent-native frontier.
- README comparison section citing the controlled Node-RED benchmark from the UIC 2026 camera-ready evaluation.
- Agent-telemetry example stream config in `docs/examples/`.

### Changed
- Citation updated to reflect the accepted status of the IEEE UIC 2026 short paper.

### Security
- Bumped `golang.org/x/net` 0.54.0 → 0.56.0 and the Go modules dependency group (moderate advisory).

## [0.2.0] — 2026-05-10

### Added
- Content-type-aware event publishing with binary payload passthrough (`Content-Type` and `X-Nephtys-Seq` headers on NATS messages).
- WebSocket binary message support with proper payload wrapping, and metadata inference for JSON payloads (`e` → type, `E` → timestamp, `seq`/`u`/`lastUpdateId`/`t` → sequence).
- `group_by` support in the threshold middleware; array path extraction in the transform middleware.
- Operator CLI flags: `--version` (VCS-stamped) and `--config-check <file|->` for CI-friendly stream config validation.
- Per-stream Prometheus metrics: ingest/publish counters (events and bytes), per-middleware drop counters, ingest→publish latency histogram, dedup cache size gauge and eviction counter.
- Benchmarks for the publish path (`internal/broker`) and pipeline chains (`internal/pipeline`).
- `docs/examples/` with runnable sensor (REST poller, SSE) and crypto (WebSocket) stream configs; README use-case section framing sensors first.
- `golangci-lint` configuration and CI job; test coverage lifted above 70% with generated code excluded from Codecov.

### Changed
- More robust `EnsureStream` handling of stream updates vs. creation.
- Reconnect semantics documented per connector class (pull connectors auto-reconnect with exponential backoff; push connectors delegate retry to the upstream client).
- NATS server bumped to 1.50.0.

### Fixed
- Staticcheck findings on JSON path traversal.

## [0.1.0] — 2026-03-26

Initial public release.

### Added
- Connector framework with five sources: WebSocket, REST poller, Server-Sent Events, inbound webhook, and gRPC client-streaming.
- Middleware pipeline declared per stream as JSON: filter, transform, dedup (LRU + TTL), enrich, threshold, batch.
- NATS JetStream persistence for event payloads (72 h default retention) and stream configurations (KV bucket with auto-restore on startup).
- REST management API (`/health`, `GET/POST /v1/streams`, `DELETE /v1/streams/{id}`, `PUT /v1/streams/{id}/pipeline`) with optional bearer-token auth.
- Prometheus metrics endpoint, initial Grafana/Prometheus compose stack and image renderer service.
- CI (gofmt, go vet, race-enabled tests), Codecov integration, Apache-2.0 license, contribution and governance docs.

[Unreleased]: https://github.com/AndreaBozzo/Nephtys/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/AndreaBozzo/Nephtys/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/AndreaBozzo/Nephtys/releases/tag/v0.1.0
