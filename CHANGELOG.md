# Changelog

All notable changes to Nephtys are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
(pre-1.0: minor versions may contain breaking changes).

## [Unreleased]

### Added
- Multi-architecture container images published to `ghcr.io/andreabozzo/nephtys` for `linux/amd64` and `linux/arm64`. `main` publishes `edge`; a `vX.Y.Z` tag publishes the full version, the `X.Y` series, and moves `latest`. Every build also carries an immutable short-sha tag and OCI source/license/revision/version labels. Pull requests build the image in a separate job that holds no package-write scope and no registry credential. The README now leads with an image-based quickstart that needs no Go toolchain. (#24)
- `make check-examples` validates every config in `docs/examples/` with `--config-check`, and CI runs it. The rule that each example must stay loadable was documented but unenforced; the target also fails when the directory matches no JSON at all, so a rename cannot make the check vacuously pass. (#41)
- Per-stream operational visibility: one-hot `nephtys_stream_state{stream_id,state}` metric plus additive `health` and `last_message_at` fields from `GET /v1/streams`. (#11)
- Optional `websocket.on_connect_send` field (string or list of strings) on WebSocket stream configs: frames are sent verbatim after every successful handshake, including reconnects. Unlocks sources that require a subscribe/auth frame. Trading-style and sensor-style examples in `docs/examples/`. (#10)
- Outcome-based roadmap covering the operational core, public proof, stable platform, and focused experiments without calendar commitments.
- README comparison section citing the controlled Node-RED benchmark from the UIC 2026 camera-ready evaluation.
- Agent-telemetry example stream config in `docs/examples/`.
- `docs/benchmarks/` with a reusable wall-power characterization harness (Shelly meter sampling, exact-rate SSE load generator) and preliminary Raspberry Pi 5 power-vs-throughput results. Explicitly scoped as exploratory — the definitive controlled comparison lives in the companion `uic2026-nephtys` repo.
- Definitive Raspberry Pi 5 edge comparison against Node-RED 5.0.1 in `docs/benchmarks/`: 19.51 ± 0.07 MB vs 128.47 ± 0.44 MB tool RSS (6.59×) at identical byte/message reduction and matching event-sequence hashes, with no throttling. Wall power was indistinguishable between the two systems (3.610 ± 0.005 W vs 3.584 ± 0.014 W), so no energy advantage is claimed: the board's ~3.0 W idle floor dominates a 40 events/s workload.

### Changed
- **Breaking (observability):** every Prometheus metric now carries the `nephtys_` prefix. Unprefixed names like `events_ingested_total` were near-certain to collide in a shared Prometheus, and the Prometheus naming guidelines call for a single-word application prefix. Label sets, help text and bucket boundaries are unchanged — this is a rename only. Old and new names are not dual-emitted: pre-1.0 the compatibility promise does not extend to metric names, and duplicate series are expensive on an edge-targeted binary. Update scrape rules, alerts and dashboards using the mapping below. (#39)

  | Old | New |
  | --- | --- |
  | `events_ingested_total` | `nephtys_events_ingested_total` |
  | `events_dropped_by_pipeline_total` | `nephtys_events_dropped_by_pipeline_total` |
  | `events_published_total` | `nephtys_events_published_total` |
  | `bytes_ingested_total` | `nephtys_bytes_ingested_total` |
  | `bytes_published_total` | `nephtys_bytes_published_total` |
  | `event_processing_duration_seconds` | `nephtys_event_processing_duration_seconds` |
  | `dedup_cache_size` | `nephtys_dedup_cache_size` |
  | `dedup_cache_evictions_total` | `nephtys_dedup_cache_evictions_total` |
  | `nephtys_stream_state` | unchanged |

  Go runtime and process collector series (`go_*`, `process_*`, `promhttp_*`) keep their standard names.
- The Docker image is hardened ahead of publishing it. It now runs as `nonroot` (uid 65532) instead of root, stamps the version through a `VERSION` build arg so `docker run … --version` reports a release rather than a commit hash, and cross-compiles with `--platform=$BUILDPLATFORM` plus `GOARCH=$TARGETARCH` so the arm64 leg of a multi-arch build no longer runs the Go toolchain under QEMU emulation. A new `.dockerignore` cuts the build context from ~13 MB to the ~350 KB the build actually needs. (#40)
- Logs are now emitted through an explicit `slog` text handler on stderr, so records carry `time=`/`level=`/`msg=` keys instead of the Go standard-logger prefix. Field names and destination are otherwise unchanged. (#38)
- Citation updated to reflect the accepted status of the IEEE UIC 2026 short paper.

### Fixed
- `NEPHTYS_LOG_LEVEL` now actually controls logging verbosity. It was documented in the README and `.env.example` and loaded into the runtime config, but never applied, so the process always ran at `info` and `debug` was silently a no-op. Unrecognized values fall back to `info` with a single warning naming the offending value rather than failing startup. (#38)
- Binary payloads are no longer corrupted by the pipeline. Binary events carry their bytes on a separate field and leave the JSON payload empty, which two middlewares read unconditionally: dedup hashed the empty payload, giving every binary event the same hash and dropping all but the first as a duplicate; batch built its JSON array from the same empty payload, discarding the binary data and its content type entirely. Dedup now hashes the event body, and batch passes non-JSON events through individually rather than aggregating them. (#37)
- The batch middleware now drains its buffer before exiting, so events it has already accepted are no longer silently discarded when a stream is stopped or its pipeline is hot-swapped. It also reports a cancelled pipeline to the caller instead of accepting events a departed worker will never flush, and logs rather than ignores a failed batch marshal. (#37)
- Compose `nats` service now uses the `nats:alpine` image so its `wget`-based healthcheck actually runs; the default distroless image has no `wget` or shell, leaving the container perpetually `unhealthy` despite a working server.

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
