![Nephtys Logo](https://raw.githubusercontent.com/AndreaBozzo/Nephtys/main/docs/assets/logo.png)

<div align="center">
  <h1>Nephtys</h1>
  <p><strong>Real-time data stream connector for the data economy</strong></p>
  <p>
    <a href="https://github.com/AndreaBozzo/Nephtys/actions"><img src="https://github.com/AndreaBozzo/Nephtys/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
    <a href="https://codecov.io/gh/AndreaBozzo/Nephtys"><img src="https://codecov.io/gh/AndreaBozzo/Nephtys/branch/main/graph/badge.svg" alt="Codecov"></a>
    <a href="https://github.com/AndreaBozzo/Nephtys/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue.svg" alt="License"></a>
    <a href="https://discord.gg/fztdKSPXSz"><img src="https://img.shields.io/discord/1469399961987711161?color=5865F2&logo=discord&logoColor=white&label=Discord" alt="Discord"></a>
  </p>
</div>

---

Nephtys ingests live data streams (WebSocket, webhooks, Server-Sent Events, gRPC), normalizes events into a standard format, and publishes them to NATS JetStream with durable persistence. It exposes a REST API for dynamic stream management and is designed as a standalone service or as part of a larger data processing ecosystem.

> *Named after the Egyptian goddess of the night, rivers, and protector of the dead — she watches over the streams that flow in the dark.*

## Table of Contents

- [Why Nephtys?](#why-nephtys)
- [Use Cases](#use-cases)
- [Key Features](#key-features)
- [Performance](#performance)
- [Architecture](#architecture)
- [Quick Start](#quick-start)
- [Usage Examples](#usage)
- [REST API Reference](#rest-api)
- [Configuration](#configuration)
- [Supported Connectors](#supported-connectors)
- [Pipeline Middlewares](#pipeline-middlewares)
- [Persistence](#persistence)
- [Development](#development)
- [Contributing](#contributing)
- [Citation](#citation)
- [License](#license)

---

## Why Nephtys?

Nephtys is the conceptual sibling of [Ceres](https://github.com/AndreaBozzo/Ceres) and [Ares](https://github.com/AndreaBozzo/Ares) — built with the same philosophy of creating robust, open tooling for data ingestion, but aimed at a distinctly different domain.

- **Ceres** harvests open data portals.
- **Ares** scrapes and extracts structured data from the web.
- **Nephtys** captures data *in motion*.

Where batch jobs fail to provide the immediacy required by algorithmic trading, live monitoring, or real-time ML pipelines, Nephtys steps in to ensure no event is missed, dropping it securely into your reliable, local NATS infrastructure.

Beyond its core design, Nephtys has proven effective at feeding real-time data to AI agents and autonomous systems. The clean normalization and durable JetStream delivery give agents a stable substrate for observing and acting on live environments.

## Use Cases

Nephtys is intentionally generic — it doesn't care what kind of stream you point it at, as long as it's real-time. Common profiles:

- **Urban sensor streams** *(primary framing of the [UIC 2026 paper](#citation))* — air-quality monitors, weather stations, traffic counters, smart-city telemetry. Typically polled (REST) or pushed (SSE/MQTT-over-WS), low-to-moderate event rate, durable archive needed.
- **Market data** — exchange WebSocket feeds for trades, depth, order book snapshots. High event rate, sequencing matters, used by downstream trading agents (e.g. [Mercury](https://github.com/AndreaBozzo/Mercury)).
- **Public event streams** — Wikimedia recent-changes, GitHub events, transit feeds. SSE or WebSocket, useful for monitoring and ML pipelines.
- **AI agent telemetry** — feeding live observations to autonomous agents that need a normalized, durable event substrate.

See [`docs/examples/`](docs/examples/) for runnable configurations covering each profile.

## Key Features

- **Real-time ingestion** across WebSocket, SSE, REST polling, gRPC, and webhook sources.
- **Durable persistence** via NATS JetStream — both event payloads and stream configurations survive restarts.
- **Configurable pipelines** — filter, transform, deduplicate, enrich, threshold, and batch payloads on the fly via JSON config.
- **No extra infrastructure** — runs alongside NATS; no separate database, cache, or coordination service required.
- **Self-healing pull connectors** — `websocket` and `sse` reconnect with exponential backoff; `rest_poller` retries on the next tick. (Inbound `webhook` and `grpc` sources delegate retry to the upstream client — see [Supported Connectors](#supported-connectors).)
- **Edge-friendly footprint** — single Go binary, low memory, suitable for resource-constrained deployments.

## Performance

From the peer-reviewed evaluation for the [UIC 2026 paper](#citation): Nephtys and Node-RED 5.0.1 processed the **same deterministic 12,000-event sensor workload** (identical pipeline semantics, matched accepted-event hashes, three interleaved trials, NATS JetStream sink for both). Same output — a fraction of the footprint:

| Metric (mean ± SD, 3 trials) | Nephtys | Node-RED 5.0.1 |
|---|---|---|
| Tool RSS | **19.1 ± 0.1 MB** | 109.6 ± 0.4 MB |
| Tool + NATS RSS | **27.2 ± 0.1 MB** | 117.3 ± 0.4 MB |
| CPU (100% = 1 logical core) | **0.03 ± 0.02%** | 0.31 ± 0.08% |
| Bandwidth reduction (bytes / messages) | 67.3% / 98.7% | 67.3% / 98.7% |

Both systems achieved identical filtering results; end-to-end p95 latency was equivalent (batch-window dominated).

The same protocol repeated on **real edge hardware** — a Raspberry Pi 5 (4 GB), both tools native — reproduced this, with the gap widening as Node-RED costs 17% more resident memory on ARM64 while Nephtys is essentially unchanged:

| Metric (mean ± SD, 3 trials, Raspberry Pi 5) | Nephtys | Node-RED 5.0.1 |
|---|---|---|
| Tool RSS | **19.5 ± 0.1 MB** | 128.5 ± 0.4 MB |
| Tool + NATS RSS | **38.9 ± 0.1 MB** | 147.1 ± 0.5 MB |
| CPU (100% = 1 logical core) | **0.32 ± 0.00%** | 0.72 ± 0.01% |
| Wall power (whole board, at the socket) | 3.610 ± 0.005 W | 3.584 ± 0.014 W |

Every slot produced the same event-sequence hash and the same 67.3% / 98.7% reduction as on x86-64, and the SoC never throttled. **Note the negative result:** wall power is *indistinguishable* — Nephtys measured 0.7% higher, below the meter's resolution. The Pi's ~3.0 W idle floor dominates at 40 events/s, so the small footprint buys **memory headroom for co-located workloads, not lower power**. Anyone sizing an edge deployment on energy grounds should measure at their own load.

Scope: single node, one workload profile per platform — full protocol, raw counters, recorded deviations, and scripts in the [companion repository](https://github.com/AndreaBozzo/uic2026-nephtys/), summarized in [`docs/benchmarks/`](docs/benchmarks/).

## Architecture

Nephtys orchestrates independent connectors through a unified pipeline that outputs directly to highly available JetStream constructs.

```mermaid
flowchart LR
    subgraph Data Sources
        WS[WebSocket]
        Poll[REST Poller]
        Web[Webhook]
        SSE[SSE]
        GRPC[gRPC]
    end

    subgraph Nephtys Node
        Connector[Connector Interface]
        Pipeline[Middleware Pipeline]
        JS_C[(KV Store Configs)]
        
        Connector --> |Raw Events| Pipeline
    end

    subgraph NATS Broker
        JS[(JetStream Persistence)]
    end

    WS --> Connector
    Poll --> Connector
    Web --> Connector
    SSE --> Connector
    GRPC --> Connector

    Pipeline --> |Normalized Events| JS
    JS_C -.->|State Auto-rebuild| Connector
```

## Quick Start

### Run the published image

No toolchain required — multi-architecture images (`linux/amd64`, `linux/arm64`) are published to GHCR:

```bash
# A shared network so Nephtys can resolve NATS by name on every platform.
# Idempotent, so the block is safe to re-run.
docker network create nephtys 2>/dev/null || true

docker run -d --name nats --network nephtys -p 4222:4222 \
  nats:alpine --jetstream --store_dir /data

docker run --rm --name nephtys --network nephtys -p 3002:3002 \
  -e NATS_URL=nats://nats:4222 \
  ghcr.io/andreabozzo/nephtys:edge
```

`edge` tracks `main`; released versions are tagged `0.3.0`, `0.3`, and `latest`. The image runs as a non-root user and contains only the statically linked binary.

```bash
docker run --rm ghcr.io/andreabozzo/nephtys:edge --version
```

### Build from source

**Prerequisites:** **Go** 1.25+ and **Docker** (to rapidly provision NATS).

```bash
# Clone the repository
git clone https://github.com/AndreaBozzo/Nephtys.git
cd Nephtys

# Start NATS with JetStream, Prometheus, and a pre-provisioned Grafana
docker compose up -d

# Configure environment — `make run` exports this; the binary itself reads
# only the environment, so any other launcher needs these exported directly
cp .env.example .env

# Run Nephtys
make run
```

Set `NEPHTYS_ADMIN_TOKEN` in `.env` before the examples below. Leaving it unset is a valid production choice — it disables the stream-management routes entirely — but it means the first `POST /v1/streams` you try answers `403`, not `201`.

## Usage

Start your local Nephtys instance with `make run`. The REST API listens on `:3002` (by default) and connects to NATS at `:4222`.

If `NEPHTYS_ADMIN_TOKEN` is set, stream-management routes require `Authorization: Bearer <token>`. If it is unset, those protected endpoints intentionally return `403`, so the header examples below apply only when auth is enabled.

More runnable examples live in [`docs/examples/`](docs/examples/), including a Wikimedia SSE stream and a Binance WebSocket stream.

### 1. Register a sensor stream (REST poller)

Polls the public Open-Meteo weather API every 60 seconds and publishes normalized events to NATS. No API key required.

```bash
curl -X POST http://localhost:3002/v1/streams \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $NEPHTYS_ADMIN_TOKEN" \
  -d @docs/examples/sensor_rest_poller.json
```

This publishes to subject `nephtys.stream.sensors.weather.bologna`. Tap it from a separate terminal:

```bash
nats sub "nephtys.stream.sensors.>"
```

### 2. Register a market-data stream (WebSocket)

```bash
curl -X POST http://localhost:3002/v1/streams \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $NEPHTYS_ADMIN_TOKEN" \
  -d '{
    "id": "binance_btc",
    "kind": "websocket",
    "url": "wss://stream.binance.com:9443/ws/btcusdt@trade",
    "topic": "nephtys.stream.crypto.btc",
    "pipeline": {
      "filter": { "match_types": ["trade"] },
      "transform": { "mapping": { "price": "p", "qty": "q", "symbol": "s" } },
      "dedup": { "enabled": true, "ttl": "1m" },
      "enrich": { "tags": { "env": "prod" } }
    }
  }'
```

### 3. Verify active streams

```bash
curl \
  -H "Authorization: Bearer $NEPHTYS_ADMIN_TOKEN" \
  http://localhost:3002/v1/streams
```

### 4. Remove a stream

```bash
# Gracefully stops the worker and removes the persisted configuration
curl -X DELETE http://localhost:3002/v1/streams/binance_btc \
  -H "Authorization: Bearer $NEPHTYS_ADMIN_TOKEN"
```

### WebSocket metadata inference

For JSON WebSocket payloads, Nephtys infers useful envelope metadata without forcing an exchange-specific schema into the connector:
- `e` becomes the event `type`
- `E` becomes the envelope `timestamp`
- `seq`, `u`, `lastUpdateId`, or `t` become the envelope `seq`

This keeps the connector generic while still giving downstream consumers enough sequencing information to build local state safely.

### Binary payloads

When a connector emits raw binary data, Nephtys publishes it directly to NATS instead of JSON-encoding it first. The message carries:
- `Content-Type` header, for example `application/vnd.apache.arrow.stream`
- `X-Nephtys-Seq` header when sequence metadata is available

## REST API

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Health check (Verifies internal NATS connectivity) |
| `GET` | `/v1/streams` | List active streams with connector status, health, and last-message time |
| `POST` | `/v1/streams` | Register, save, and start a new stream |
| `DELETE` | `/v1/streams/{id}` | Halt stream ingest and remove it from configuration |
| `PUT` | `/v1/streams/{id}/pipeline` | Update a running stream pipeline, durably |

## Configuration

Control the global behavior of the instance via environment variables.

| Variable | Default | Description |
|----------|---------|-------------|
| `NATS_URL` | `nats://localhost:4222` | Broker endpoint address |
| `NEPHTYS_PORT` | `3002` | Port for the management REST API |
| `NEPHTYS_ADMIN_TOKEN` | unset | Optional bearer token for stream-management endpoints |
| `NEPHTYS_LOG_LEVEL` | `info` | Operational logging verbosity (`debug`, `info`, `warn`, `error`) |

Each `GET /v1/streams` item includes `status`, derived `health` (`healthy`, `degraded`, or `errored`), and `last_message_at` once the source has emitted an event. Prometheus exposes the same connector state as the one-hot `nephtys_stream_state{stream_id,state}` gauge.

### Metrics and the operations dashboard

`GET /metrics` serves Prometheus text format. Every Nephtys series carries the `nephtys_` prefix; `go_*`, `process_*` and `promhttp_*` are the standard collectors.

| Metric | Type | Labels |
|---|---|---|
| `nephtys_stream_state` | gauge (one-hot) | `stream_id`, `state` |
| `nephtys_events_ingested_total` | counter | `stream_id` |
| `nephtys_events_published_total` | counter | `stream_id` |
| `nephtys_events_dropped_by_pipeline_total` | counter | `stream_id`, `middleware` |
| `nephtys_bytes_ingested_total` | counter | `stream_id` |
| `nephtys_bytes_published_total` | counter | `stream_id` |
| `nephtys_event_processing_duration_seconds` | histogram | `stream_id` |
| `nephtys_dedup_cache_size` | gauge | `stream_id` |
| `nephtys_dedup_cache_capacity` | gauge | `stream_id` |
| `nephtys_dedup_cache_evictions_total` | counter | `stream_id` |

`docker compose up -d` brings up NATS, Prometheus and a Grafana that is **already provisioned** — the datasource and the *Nephtys — Operations* dashboard are mounted from [`deploy/grafana/`](deploy/grafana/), so there is nothing to import by hand:

| Service | URL | Notes |
|---|---|---|
| Grafana | <http://localhost:3000/d/nephtys-ops> | login `admin` / `admin` |
| Prometheus | <http://localhost:9090> | scrapes Nephtys every 5s |

The dashboard shows per-stream state, ingest/publish event and byte rates, drops broken down by middleware, processing-latency quantiles, and dedup cache saturation against configured capacity. Its `Instance` and `Stream` variables scope every panel.

Prometheus scrapes two targets — `host.docker.internal:3002` for a binary run with `make run`, and `nephtys:3002` for the optional in-compose service — so whichever way you run Nephtys, one target is up and the other reports down. To run Nephtys inside the stack from the published image instead of on the host:

```bash
make docker-up-full     # docker compose --profile nephtys up -d
```

The dashboard JSON is committed and reviewed as source, so Grafana is configured not to persist UI edits: use "Save As" to experiment, and change [`nephtys-ops.json`](deploy/grafana/dashboards/nephtys-ops.json) to make a change stick.

### CLI flags

```bash
nephtys --version                                   # print version (VCS-stamped) and exit
nephtys --config-check docs/examples/sensor.json    # validate a stream config and exit (0 = ok, 1 = invalid)
cat config.json | nephtys --config-check -          # same, from stdin (useful in CI)
```

### Configuration validation

`--config-check` applies exactly the rules `POST /v1/streams` applies — same decoder, same validator — so a config CI accepts is one the running service accepts. The contract it enforces:

- **Unknown fields are errors.** A misspelled `flush_intervl` is rejected rather than dropped, and so is content after the JSON object.
- **An omitted value takes its documented default; a stated-but-invalid one is an error.** `dedup.ttl`, `batch.flush_interval`, and `rest_poller.interval` used to swallow a parse failure and fall back — `"5 m"` ran at the 1s default with nothing in the logs. Absent still means "no opinion"; present-and-unparseable now fails.
- **Configuration that cannot do anything is rejected**: an enabled `threshold` with no `path`, an empty `match_types` / `mapping` / `tags`, and a connector block belonging to a different `kind` than the stream's.
- **The two counts that size an allocation are bounded at both ends.** `dedup.cache_size` preallocates its LRU map and `batch.max_batch_size` sizes the batch worker's channel buffer, so both are capped (1,000,000 and 100,000) as well as required to be positive — a stray zero should be a rejected config, not an out-of-memory kill. Both ceilings sit well above any supported workload; a batch near the upper one would already exceed NATS' 1 MB default `max_payload`.
- **Errors name the offending JSON path**, e.g. `pipeline.batch.flush_interval: "1 sec" is not a valid duration`.

The same rules apply to `PUT /v1/streams/{id}/pipeline`, which returns 400 on anything `--config-check` rejects, and to configs restored from JetStream at startup — a persisted config the current validator rejects is skipped with a warning rather than started.

## Supported Connectors

| Kind | Direction | Reconnect | Description | Config Keys |
|------|-----------|-----------|-------------|-------------|
| `websocket` | Outbound (pull) | Auto, exp. backoff | Standard WebSocket. | `url` |
| `rest_poller` | Outbound (pull) | N/A (next tick) | Periodically requests JSON from REST APIs at given intervals. | `url`, `interval` |
| `sse` | Outbound (pull) | Auto, exp. backoff | Standard Server-Sent Events bindings. | `url` |
| `webhook` | Inbound (push) | Client's responsibility | Local HTTP server receiving inbound webhooks. | `port`, `path`, `auth_token` |
| `grpc` | Inbound (push) | Client's responsibility | gRPC server accepting client-streaming pushes. | `port` |

**Reconnect semantics.** Pull connectors (`websocket`, `sse`) reconnect transparently with exponential backoff (1s → 30s) on transient failures; `rest_poller` simply retries on the next tick. Push connectors (`webhook`, `grpc`) do not "reconnect" — they accept whatever the upstream client sends, so retry-on-failure is the *client's* responsibility. If the local HTTP/gRPC server itself fails (rare), the stream enters an error state and must be removed and re-registered.

## Pipeline Middlewares

Pipelines are declared inline on stream registration as JSON. Each middleware is optional; they run in a fixed order (filter → transform → dedup → enrich → threshold → batch) before the event is published.

- **Filter** — drops events whose `type` doesn't match `match_types`. `type` is the envelope field the connector sets, not a field inside the payload: SSE uses the `event:` frame name, WebSocket infers it from a top-level `e` key, and everything else falls back to a per-connector default. A `match_types` listing values that only ever appear inside the payload matches nothing and drops the whole stream.
- **Transform** — remaps fields in the JSON payload using dot-notation paths.
- **Dedup** — short-window LRU deduplication on FNV-1a hashes of the event body. Per-stream and in-memory; state is not shared across instances and does not survive restart. Bounded by `cache_size` (default 1000). `ttl` (default `1m`) is enforced lazily: an entry is fresh until the TTL has elapsed since it was last seen, and treated as expired only when its hash is checked again past that window — stale entries that are never re-seen remain in the LRU until evicted by capacity. Size `cache_size` for at least one full TTL window of expected unique payloads.
- **Enrich** — adds static tags to outgoing events.
- **Threshold** — emits only when a numeric path changes by at least a configured delta (useful for sensor anomaly filtering).
- **Batch** — buffers events into bounded batches before publishing. Flushes on `max_batch_size`, on `flush_interval`, and once more when the stream is stopped or its pipeline is replaced, so buffered events are not discarded. An event that arrives mid-swap, from a publisher still holding the retired pipeline, is published on its own rather than batched or rejected: the batch envelope is a shape, the event is data. See [pipeline replacement](#pipeline-replacement) for why that final flush cannot miss an event.

**Binary payloads and the pipeline.** Events whose `content_type` is anything other than `application/json` carry their bytes outside the JSON envelope. Dedup hashes those bytes, so distinct binary frames are treated as distinct. Batch cannot aggregate them into its JSON array envelope without discarding them, so it passes them through individually — on a stream that mixes text and binary frames, binary events may therefore overtake JSON events still sitting in the batch buffer. Transform, threshold, and enrich operate on JSON payloads and pass binary events through untouched; filter matches on `type` and works for both.

### Pipeline replacement

`PUT /v1/streams/{id}/pipeline` replaces a running stream's pipeline without dropping the source connection or losing an event.

A replacement is built first and installed by pointing publishers at it; only then is the outgoing pipeline retired. Retirement is a handshake rather than a cancellation, because publishers reach a pipeline through an unsynchronised pointer and an arbitrary number of them may be mid-call when the swap happens:

1. the outgoing pipeline stops accepting events into buffers, which also releases any publisher parked on a full one;
2. it waits for the publishers already inside it to finish, and seals at that moment;
3. its buffering middlewares then drain and flush, and the swap is not reported complete until they have.

The ordering matters because step 3 is only sound after step 2. A worker that drains as soon as it is cancelled can empty and abandon its buffer while a publisher is still on its way in, and an event that lands there afterwards is never flushed and never reported — it fails no publish and increments no counter. Sealing is what rules that out.

The cost is one read-lock pair per event on the publish path, measured at roughly 24 ns with no additional allocations — about 1% of a filter→transform→dedup→enrich chain. `make bench` reports it as `BenchmarkGenerationOverhead`.

The same handshake runs on shutdown and on stream removal, so a buffered batch is flushed before the process exits rather than racing it.

**A pipeline update is durable.** `200 OK` means the replacement is both running and stored, so a restart resumes the stream on the updated pipeline rather than the one it was registered with. The stored config is written *before* the swap, under the same lock that guards registration and removal, which is what keeps the running pipeline and the stored one from describing different streams:

- If the config store rejects the write, the swap does not happen. The stream keeps running its previous pipeline and the endpoint answers `503 Service Unavailable` — the request was valid and the stream exists, so retrying is the right response. See [Persistence](#persistence) for what a `503` does and does not promise about the store itself.
- The update replaces the `pipeline` block only. `kind`, `url`, `topic` and the connector block are carried over from the registered config untouched.
- There is no ephemeral mode. A pipeline that should not outlive the process does not currently have a way to say so; if you need one, open an issue rather than relying on the update being forgotten.

## Persistence

Nephtys uses NATS JetStream for both event durability and configuration state — no separate database is required.

- **Event payloads** are written to JetStream with a 72h default retention (configurable on the broker).
- **Stream configurations** are stored in a JetStream KV bucket and reloaded on startup, so registered streams survive restarts. The stored config is the *effective* one: `POST /v1/streams` writes it, and every accepted `PUT /v1/streams/{id}/pipeline` amends it, so what restarts is what was last running. A persisted config the current validator rejects is skipped with a warning rather than started.
- **A management call that cannot be persisted does not take effect.** `POST /v1/streams`, `PUT /v1/streams/{id}/pipeline` and `DELETE /v1/streams/{id}` all answer `503` rather than applying a change the store did not accept. For `DELETE` that means the stream is left *running*: tearing it down while its config survives would bring it back at the next restart, which is the same divergence in the other direction.
- **`503` means the change was not applied locally, not that the store is untouched.** A JetStream request that times out has an unknown outcome — it may still be applied after the client has given up, in which case the next restart acts on it. Nephtys does not currently reconcile that; it is tracked in [#65](https://github.com/AndreaBozzo/Nephtys/issues/65). Treat a `503` as "retry, then verify with `GET /v1/streams`".

## Development

```bash
make help            # List available targets
make build           # Build the binary
make test            # Run the test suite
make fmt             # Format the code (gofmt)
make vet             # Run go vet
make check-examples  # Validate every docs/examples/*.json with --config-check
make all             # Run fmt + vet + test (the standard pre-commit cycle)
```

### Docker Management

```bash
make docker-build # Build the production Docker image (add VERSION=v0.3.0 to stamp a release)
make docker-up    # Start NATS JetStream for local development
make docker-down  # Stop and remove the local containers
```

## Contributing

Contributions and issues are welcome. See [CONTRIBUTING.md](docs/CONTRIBUTING.md) for setup instructions and the contribution workflow.

## Citation

If you use Nephtys in your research, please cite the following accepted short paper:

> **Andrea Bozzo. "Nephtys: Lightweight Edge Connector for Bandwidth-Efficient Ingestion of Urban Sensor Streams". IEEE UIC 2026.**

Companion repository and evaluation material: [AndreaBozzo/uic2026-nephtys](https://github.com/AndreaBozzo/uic2026-nephtys/)

### BibTeX

```bibtex
@inproceedings{bozzo2026nephtys,
  author    = {Bozzo, Andrea},
  title     = {Nephtys: Lightweight Edge Connector for Bandwidth-Efficient Ingestion of Urban Sensor Streams},
  booktitle = {IEEE International Conference on Ubiquitous Intelligence and Computing (UIC)},
  year      = {2026},
  note      = {Accepted Short Paper}
}
```

## License

Nephtys is open-source software, freely available under the [Apache-2.0 License](LICENSE).
