<div align="center">
  <img src="docs/assets/logo.png" alt="Nephtys Logo" width="800" height="auto"/>
  <h1>Nephtys</h1>
  <p><strong>Real-time data stream connector for the data economy</strong></p>
  <p>
    <a href="https://github.com/AndreaBozzo/Nephtys/actions"><img src="https://github.com/AndreaBozzo/Nephtys/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
    <a href="https://github.com/AndreaBozzo/Nephtys/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue.svg" alt="License"></a>
    <a href="https://discord.gg/fztdKSPXSz"><img src="https://img.shields.io/discord/1469399961987711161?color=5865F2&logo=discord&logoColor=white&label=Discord" alt="Discord"></a>
  </p>
</div>

---

Nephtys ingests live data streams (WebSocket, webhooks, gRPC), normalizes events into a standard format, and publishes them to NATS JetStream with durable persistence. It exposes a REST API for dynamic stream management and is designed as a standalone service or as part of a larger data ecosystem.

> *Named after the Egyptian goddess of the night, rivers, and protector of the dead — she watches over the streams that flow in the dark.*

Conceptual sibling of [Ceres](https://github.com/AndreaBozzo/Ceres) and [Ares](https://github.com/AndreaBozzo/Ares) — same philosophy, different domain. Where Ceres harvests open data portals and Ares scrapes structured data from the web, Nephtys captures data *in motion*.

## Architecture

```
cmd/nephtys/          Entrypoint — wires config, broker, pipeline, server
internal/
  domain/             Core models — StreamEvent, StreamSourceConfig, SourceStatus
  connector/          StreamSource interface + implementations (WebSocket, ...)
  broker/             NATS JetStream wrapper + durable event streams
  pipeline/           Per-stream middleware chain (filtering, enrichment, dedup — extensible)
  server/             REST API — stream management + health check
  store/              JetStream KV — persistent stream config across restarts
  config/             Environment-based configuration
```

All connectors implement the `StreamSource` interface and are decoupled from the broker through a `publish` callback wired through the pipeline.

## Tech Stack

| Component | Technology |
|-----------|------------|
| Language | Go 1.23+ |
| Broker | NATS with JetStream |
| Persistence | JetStream KV (stream configs) + Streams (event data) |
| REST API | Go stdlib `net/http` |

## Quick Start

### Prerequisites

- **Go** 1.23+
- **Docker** (for NATS)

### Setup

```bash
# Clone the repository
git clone https://github.com/AndreaBozzo/Nephtys.git
cd Nephtys

# Start NATS with JetStream
docker compose up -d

# Configure environment
cp .env.example .env

# Run Nephtys
make run
```

## Usage

### Start the server

```bash
make run
# REST API on :3000, connected to NATS on :4222
```

### Register a stream dynamically

```bash
curl -X POST http://localhost:3000/v1/streams \
  -H "Content-Type: application/json" \
  -d '{
    "id": "binance_btc",
    "kind": "websocket",
    "url": "wss://stream.binance.com:9443/ws/btcusdt@trade",
    "topic": "nephtys.stream.crypto.btc",
    "pipeline": {
      "filter": { "match_types": ["trade"] },
      "transform": { "mapping": { "price": "data.p", "qty": "data.q" } },
      "dedup": { "enabled": true, "ttl": "1m" },
      "enrich": { "tags": { "env": "prod" } }
    }
  }'
```

### Register a REST Poller stream

```bash
curl -X POST http://localhost:3000/v1/streams \
  -H "Content-Type: application/json" \
  -d '{
    "id": "weather_data",
    "kind": "rest_poller",
    "url": "https://api.open-meteo.com/v1/forecast?latitude=52.52&longitude=13.41&current_weather=true",
    "topic": "nephtys.stream.weather",
    "rest_poller": {
      "interval": "5m",
      "method": "GET"
    }
  }'
```

### Register a gRPC stream

```bash
curl -X POST http://localhost:3000/v1/streams \
  -H "Content-Type: application/json" \
  -d '{
    "id": "internal_telemetry",
    "kind": "grpc",
    "topic": "nephtys.stream.telemetry",
    "grpc": {
      "port": "50051"
    }
  }'
```

### List active streams

```bash
curl http://localhost:3000/v1/streams
```

### Remove a stream

```bash
curl -X DELETE http://localhost:3000/v1/streams/binance_btc
```

### Health check

```bash
curl http://localhost:3000/health
```

## REST API

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Health check (NATS connectivity) |
| `GET` | `/v1/streams` | List active streams and their status |
| `POST` | `/v1/streams` | Register and start a new stream |
| `DELETE` | `/v1/streams/{id}` | Stop and remove a stream |

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `NATS_URL` | `nats://localhost:4222` | NATS broker URL |
| `NEPHTYS_PORT` | `3000` | REST API port |
| `NEPHTYS_LOG_LEVEL` | `info` | Log level |

## Supported Connectors

| Kind | Status | Description |
|------|--------|-------------|
| `websocket` | ✅ Ready | WebSocket with auto-reconnect and exponential backoff |
| `rest_poller` | ✅ Ready | Periodically poll REST APIs at a configured interval |
| `sse` | 🔜 Planned | Server-Sent Events |
| `webhook` | ✅ Ready | Inbound HTTP webhooks |
| `grpc` | ✅ Ready | gRPC client streaming (high-throughput, low-latency) |

## Pipeline Middlewares

Nephtys supports per-stream configurable middlewares to process events before they are published to NATS:

- **Filter**: Drop events that don't match specific types.
- **Transform**: Pluck variables from deeply nested JSON payloads and flatten them to root using dot-notation maps.
- **Dedup**: Prevent publishing duplicate messages within a time window using FNV-64a payload hashing and an LRU cache.
- **Enrich**: Inject static tags into JSON object payloads.

Middlewares are configured dynamically within the JSON payload when registering a stream via the API.

## Persistence

Nephtys uses NATS JetStream for all persistence — no additional database required.

- **Event data** — Durably stored in JetStream streams with configurable retention (default: 72h)
- **Stream configs** — Persisted in a JetStream KV bucket, automatically restored on restart
- **Zero extra infrastructure** — JetStream is built into NATS

## Development

```bash
make help       # Show all targets
make build      # Build binary
make test       # Run tests
make fmt        # Format code
make vet        # Static analysis
make all        # fmt + vet + test
```

## Docker

```bash
# Build the image
make docker-build

# Start NATS for local development
make docker-up

# Stop NATS
make docker-down
```

## Contributing

Contributions are welcome! See [CONTRIBUTING.md](docs/CONTRIBUTING.md) for setup instructions and guidelines.

## License

Apache-2.0 — see [LICENSE](LICENSE).
