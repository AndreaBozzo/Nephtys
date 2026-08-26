# Changelog

All notable changes to Nephtys are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
(pre-1.0: minor versions may contain breaking changes).

## [Unreleased]

### Added
- Per-stream restart policy and a supervisor to run it. A stream may now carry an optional `restart` block — `max_attempts`, `initial_backoff`, `max_backoff`, `factor`, `reset_after` — validated by `--config-check` like every other part of the config. (#15)

  The supervisor is the only retry loop in the process. Connectors used to own their own: `websocket` and `sse` retried forever on a hardcoded 1s→30s ladder, `rest_poller` retried on the next tick, and the push connectors did not retry at all. A restart policy bolted onto that would have reached only the push connectors, because `Start` on a pull connector never returned except on cancellation — the three that actually retried would have kept an unconfigurable policy of their own. So `StreamSource` now runs one session and returns, and the ladder and the attempt budget live in the manager.

  Defaults are per kind and reproduce the previous behaviour exactly: unlimited attempts on the same 1s→30s ladder for `websocket` and `sse`; nothing for `rest_poller`, which has no session to lose; and no restart for `webhook` and `grpc`, where a lost listener has always been terminal. Configuring `restart` for a push connector is now possible and is opt-in.

  The attempt budget is earned back by staying up for `reset_after`, not by connecting. The old loops reset their counter the moment a dial succeeded, which is harmless under an unbounded ladder and a bug under a bounded one: a source that accepts and drops immediately resets the counter every cycle and retries forever, never reaching a state anything can alert on.

  A stream that spends its budget goes terminal and **stays registered**: `status: error`, `health: errored`, `nephtys_stream_state{state="errored"}`, with new `restart_count`, `last_error` and `last_error_at` fields on `GET /v1/streams` and its config still stored. New counter `nephtys_stream_restarts_total{stream_id}`.
- `docs/LIFECYCLE.md` documents the whole lifecycle in one place — the state machine, the failure contract, the concurrency invariants — and the README links to it from Architecture, Supported Connectors, and a new Stream Lifecycle section.

### Fixed
- **Breaking (API status codes):** `201 Created` now means the connector started. `POST /v1/streams` persisted the config, launched the source in a goroutine and answered `201` without waiting, so the response certified only that a goroutine had been created. A webhook stream whose port was already taken answered `201`, reported `status: running` / `health: healthy` for the moment before its bind failed, and then sat in an error state that the caller had no reason to look for. (#59)

  Registration now blocks on admission: the id is free, the port is claimed, the pipeline builds, the listener binds, and the config is written — all of it local, deterministic, and bounded — before the request is answered. It does not wait for an upstream connection, which is remote and may take arbitrarily long, so the `201` body carries a new `state` field (`connecting` or `running`) beside the existing `status: "started"`.

  What changed in what you get back:

  | Case | Before | After |
  | --- | --- | --- |
  | `POST /v1/streams` on a port another stream holds | `201 Created`, stream dies asynchronously | `409 Conflict` naming the port and the holding stream |
  | `POST /v1/streams` on a port held by another process | `201 Created`, stream dies asynchronously | `409 Conflict` with the bind error |
  | `POST /v1/streams` whose source cannot acquire its resources | `201 Created` | `409 Conflict` with the reason |
  | `POST /v1/streams` succeeding | `201 Created` | `201 Created`, plus a `state` field |

  Nothing is left behind by a failed registration: no persisted config, no claimed port, no half-installed stream. Because resources are now acquired before the config is written, the compensating "delete the config we just wrote" path is gone.

  Two connectors changed shape to make this possible. `WebhookSource` set its status to running *before* calling `ListenAndServe`; `GrpcSource` already bound its listener synchronously and it made no difference, because the error was returned into a goroutine and logged. Both now bind in `Open`, before anything reports anything. Connectors no longer carry a status at all — the manager owns lifecycle state, which removes the class rather than the two instances of it.
- Restore no longer drops streams it cannot start while keeping their configs. A persisted config that failed validation or could not be built was skipped with a warning and left in the KV bucket, so the stream existed in storage, was absent from `GET /v1/streams`, and announced itself only in one line of boot output. Such a stream is now registered in a terminal `error` state carrying the reason, so it is visible, explicable, and removable. Restore also admits in sorted id order, so when two persisted streams claim one port the same one wins on every restart instead of it depending on map iteration.
- **Breaking (API status codes):** a pipeline update is now durable, and a management call that cannot be persisted no longer reports success. `PUT /v1/streams/{id}/pipeline` swapped the live handler and deliberately left the JetStream KV copy of the config alone, so a change the API had answered `200 OK` for was silently reverted by the next restart — the stream came back on the pipeline it was registered with, with nothing recording that an update had ever been accepted. (#28)

  The manager now keeps each stream's *effective* config — the registered one, amended by every accepted pipeline update — and writes it before the swap, under the same lock that guards registration and removal. Ordering it that way is what makes the two states inseparable rather than merely usually equal: if the store rejects the write, no swap happens, the stream keeps running the pipeline the store still describes, and the caller is told. Only the `pipeline` block is replaced; `kind`, `url`, `topic` and the connector block are carried over untouched. There is no ephemeral mode — a pipeline update means the same thing whether or not the process survives the hour.

  `DELETE /v1/streams/{id}` had the same divergence pointing the other way: the config delete happened last and a failure was only logged, so a removal that reported `200 OK` could be undone by a restart bringing the stream back. The delete now happens before any teardown, so a store that refuses it leaves the stream running rather than half-removed.

  What changed in what you get back:

  | Case | Before | After |
  | --- | --- | --- |
  | `POST /v1/streams` whose config cannot be persisted | `409 Conflict` | `503 Service Unavailable` |
  | `PUT .../pipeline` whose config cannot be persisted | `200 OK`, reverted on restart | `503 Service Unavailable`, not applied |
  | `DELETE /v1/streams/{id}` whose config cannot be deleted | `200 OK`, stream returns on restart | `503 Service Unavailable`, stream still running |
  | duplicate stream id on `POST` | `409 Conflict` | `409 Conflict` (unchanged) |
  | unknown stream id on `PUT`/`DELETE` | `404 Not Found` | `404 Not Found` (unchanged) |

  `409` previously covered both a duplicate id and a store that would not accept the write, which left a client unable to tell "you already have this stream" from "try again". A caller that treats any non-2xx as fatal is unaffected; one that branches on the status should treat `503` as retryable. No response body field changed.

  One limit is worth stating: `503` means the change was not applied *locally*, not that the store is untouched. A JetStream request that times out has an unknown outcome and may be applied after the client has given up — verified by hand against a broker restart, where a `503`'d `DELETE` landed anyway once NATS returned. Reconciling that is #65.
- The `linux/arm64` image was not an arm64 image. Every multi-architecture build since #24 cross-compiled both legs to amd64 and published one of them under an arm64 manifest entry, so `docker run` on a Raspberry Pi — the platform the edge story is built on — failed with `exec /nephtys: exec format error`. Pulling `ghcr.io/andreabozzo/nephtys:edge` on aarch64 and reading the ELF header confirmed an x86-64 executable behind an `arm64`-labelled manifest.

  The cause was one character of Dockerfile: `ARG TARGETARCH=amd64`. `TARGETOS` and `TARGETARCH` are *predefined* build args, and giving a predefined platform arg a default makes the default win rather than act as a fallback — so `GOARCH` was `amd64` on both legs while buildx labelled the images by target platform regardless. Both defaults are removed; BuildKit supplies the values, including for a plain `docker build`, which is what the defaults were meant to protect.

  CI could not have caught this. The publish smoke test asserted that the manifest *listed* `linux/arm64`, which it did — the label was never the problem. Both the pull-request and publish jobs now read the ELF header of each leg's binary and fail if it does not match the architecture its manifest entry claims. Verified end to end: the corrected image runs on a Raspberry Pi 5 at 19.1 MB RSS, ingests, hot-swaps its pipeline, and restores it across a restart.
- `make run` now exports `.env` before starting the binary. The quick start has always said `cp .env.example .env`, but nothing ever read that file — there is no dotenv loader in the binary and `make run` was a bare `go run` — so the documented setup step was a no-op. Following the quick start verbatim therefore left `NEPHTYS_ADMIN_TOKEN` unset, and the first `POST /v1/streams` answered `403 Admin endpoints are disabled` with nothing pointing at the cause. `.env.example` and the README now also state that the file is a `make run` convenience: the binary reads the process environment only, so containers and systemd units still supply these directly.
- The compose stack no longer ships a Prometheus target that is DOWN by design. `prometheus.yml` listed both `nephtys:3002` (the in-compose service) and `host.docker.internal:3002` (a host-run binary), but the in-compose service sits behind a profile the default `docker compose up -d` does not enable — so the default path, which is the one the quick start documents, always showed one red target. A row that is red by design is worse than no row: it teaches an operator that red on that page carries no information.

  Both modes bind port 3002 on the host — the in-compose service publishes `3002:3002` — so a single `host.docker.internal:3002` target reaches whichever one is running, and the two cannot be up simultaneously anyway because they would contend for that port. DOWN now means what it says: nothing is serving metrics. Verified against all three states (in-compose service, host binary, neither). The Grafana dashboard is unaffected — its "Instance" variable is derived from the metrics rather than from this file.

  DNS service discovery for the in-compose name was tried first and rejected: on a network whose upstream resolver answers unknown names with a wildcard address — an ISP router did exactly this during testing — it invents a target pointing at 127.0.0.1 inside the Prometheus container, which is worse than the problem it solves.
- `.gitattributes` pins every text file to LF in the working tree as well as in the repository. Under `core.autocrlf=true` — the usual Windows default — the repository stored LF and checked CRLF out, which left `gofmt -l .` reporting all 56 Go files as unformatted: the one command that says whether your code is formatted said nothing, and `make fmt` rewrote the whole tree. `CLAUDE.md` documented this as a Windows quirk to work around; it was a setting, and `.gitattributes` is where it belongs, since it applies per repository rather than per contributor.

  No file content changed — the index was already all-LF, so `git add --renormalize .` is a no-op and only the checkout differs. `docs/assets/logo.png` is marked binary and keeps the `0d 0a` bytes in its PNG header. Existing Windows clones need one re-checkout to pick this up: `git rm -r --cached . && git reset --hard` on a clean tree.
- A pipeline hot-swap can no longer strand an event. Retiring a pipeline generation cancelled a context while publishers reached that generation through an unsynchronised atomic pointer, so retirement and use were not ordered against each other. A publisher that passed the batch middleware's cancellation check microseconds before the swap could land its event in the buffer *after* the worker's final drain had already returned, where nothing would ever pick it up. The event was never flushed and never reported — it failed no publish and incremented no counter, which is why the loss was silent. #56 made the swap lossless for every case reachable in practice; this closes the residual window rather than narrowing it. (#57)

  A generation is now a first-class object that owns its own retirement, and retiring one is a three-step handshake instead of a cancellation: it stops accepting into buffers (releasing any publisher parked on a full one), waits for the publishers already inside it and seals at that moment, and only then releases its buffering middlewares to drain. `Retire` returns once they have, so a completed swap means every event the outgoing generation accepted has reached the broker. The same handshake runs on stream removal and shutdown, where it also closes a smaller gap: a buffered batch could previously lose a race with process exit.

  The guarantee costs one read-lock pair per event on the publish path — roughly 24 ns with no additional allocations, about 1% of a filter→transform→dedup→enrich chain (`make bench`, `BenchmarkGenerationOverhead`). `#16`'s `overflow_policy` will revisit the same send path; the buffer semantics are now stated in one place for it to build on.

### Changed
- **Breaking (configuration):** stream configuration is now validated authoritatively, so configs a previous version accepted may be rejected. `--config-check` previously shared neither the decoder nor the full rule set of `POST /v1/streams`: an empty `id` and an unsupported `kind` such as `"kafka"` both exited 0 from the CLI while the API refused them, and `pipeline` was never inspected at all. Both paths now go through one decoder and one validator. What changed in what is accepted (#55):

  - Unknown and misspelled fields are errors instead of being silently dropped — `"flush_intervl": "5m"` used to validate clean and run the 1s default. Content after the JSON object is rejected too.
  - A malformed *explicit* value is an error rather than a silent fall back to the default. `dedup.ttl`, `batch.flush_interval`, and `rest_poller.interval` each swallowed their `ParseDuration` error, so `"5 m"` flushed 300× more often than the author intended with nothing in the logs. An omitted field still takes its documented default; the asymmetry is the point. `resolveDuration` in `internal/pipeline` is now the only place these are parsed.
  - `rest_poller.interval` is checked at config time. It was validated nowhere and failed inside `Start()`, so a stream registered with `201 Created` and its connector died immediately afterwards.
  - Configuration that cannot do anything is rejected: an enabled `threshold` with no `path` (which built no middleware at all while the stream reported healthy), an empty `filter.match_types` / `transform.mapping` / `enrich.tags`, a non-upper-case or unknown `rest_poller.method`, and a connector block belonging to a different `kind` than the stream declares.
  - `dedup.cache_size` and `batch.max_batch_size` are now bounded above (1,000,000 and 100,000) as well as required to be positive. Both size an allocation directly — the dedup LRU map is preallocated and `max_batch_size` sizes the batch worker's channel buffer — so an operator-supplied value with a stray extra zero could exhaust memory on a binary whose whole claim is a ~19 MB footprint. Flagged by CodeQL as `go/uncontrolled-allocation-size` once the surrounding validation made the dataflow visible; the unbounded allocation itself predates this change.
  - `PUT /v1/streams/{id}/pipeline` validates its body and returns 400. The one endpoint that changes behavior on a live stream previously ran no validation whatsoever.
  - Configs restored from the JetStream KV bucket at startup are re-validated and skipped with a warning if they fail. Restore was otherwise the one path that could start a stream from configuration the current validator rejects.

  Validation errors name the offending JSON path (`pipeline.batch.flush_interval: …`). No JSON field was renamed or removed — the change is in what is accepted. Every config in `docs/examples/` still passes, and `go test` now checks that alongside `make check-examples`.
- `docs/ROADMAP.md` transitioned past 0.3.0: the multi-architecture image, ops dashboard, per-stream state surface, and example enforcement moved from "remaining gaps" into completed foundations, and the configuration-contract gap was recorded in their place.

### Fixed
- Pipeline hot-swap no longer fails events that arrive mid-replacement. `UpdatePipeline` cancelled the running pipeline before building its replacement, so every event ingested during the rebuild reached a batch worker whose context was already cancelled and came back to the source as `context canceled`. The replacement generation is now built and installed first, and the previous one retired afterwards. A retired batch generation also publishes stragglers individually instead of rejecting them — including publishers parked on its full buffer, which it now releases — so a swap under sustained load neither drops events nor reports ingest errors.

## [0.3.0] — 2026-07-26

Operational visibility and distribution: Nephtys now ships as a multi-architecture container image, exposes a coherent `nephtys_`-namespaced metrics surface, and comes with a provisioned Grafana operations dashboard that works from `docker compose up` with no manual setup.

**Upgrading from 0.2.0:** every Prometheus metric was renamed to carry the `nephtys_` prefix. Scrape rules, alerts and dashboards referring to the old names need updating — see the mapping table under Changed.

### Added
- Provisioned Grafana operations dashboard. `docker compose up -d` now yields a Grafana with the Prometheus datasource and a *Nephtys — Operations* dashboard already present — no manual import. Panels cover per-stream state, ingest/publish event and byte rates, drops broken down by middleware, processing-latency quantiles, and dedup cache saturation against configured capacity, all scoped by `Instance` and `Stream` variables. The dashboard JSON lives in [`deploy/grafana/`](deploy/grafana/) and is mounted read-only, so it is reviewed as source rather than exported by hand from a running instance. (#42)
- Optional `nephtys` compose service running the published GHCR image, behind the `nephtys` profile (`make docker-up-full`). The default `make docker-up` is unchanged, so the edit/`make run` loop still works as before. Prometheus now lists both the in-compose and host-run targets, and its `host.docker.internal` target resolves on Linux too via a `host-gateway` alias, retiring the "does not resolve without Docker Desktop" caveat. (#42)
- `nephtys_dedup_cache_capacity` gauge, reporting each stream's configured dedup `cache_size`. Cache occupancy was already exposed but capacity was not, so saturation could not be computed from metrics alone. (#42)
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
- Unregistering a stream now removes all of its Prometheus series, not just `nephtys_stream_state`. Its counters, gauges and latency histogram previously survived for the lifetime of the process, so cardinality grew with every registration and dashboards kept charting streams that no longer existed. Relatedly, a pipeline hot-swap that drops the dedup middleware now clears the dedup cache gauges instead of leaving them frozen at the departed middleware's last values. (#42)
- Grafana no longer fails to start in the compose stack. Recent Grafana releases refuse to start the rendering service while `renderer_token` is left at its built-in default, so the container exited during boot and `docker compose up` produced no Grafana at all. The stack now sets a matching token on both Grafana and the image renderer, overridable via `GF_RENDERER_TOKEN`. (#42)
- The Wikimedia SSE example (`docs/examples/sensor_sse.json`) published nothing. Its filter listed `match_types: ["edit", "new"]` — values from the payload's own `type` field — but `filter` matches the envelope `type` the connector sets, which for SSE is the `event:` frame name (`message` for every Wikimedia frame). The filter therefore dropped 100% of events while the stream still reported healthy. The filter is removed from the example, and both the README and `docs/examples/README.md` now state what `match_types` actually matches. (#42)
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

[Unreleased]: https://github.com/AndreaBozzo/Nephtys/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/AndreaBozzo/Nephtys/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/AndreaBozzo/Nephtys/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/AndreaBozzo/Nephtys/releases/tag/v0.1.0
