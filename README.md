<div align="center">
  <h1>Nephtys</h1>
  <p><strong>Real-time data stream connector for the data economy</strong></p>
</div>

---

Nephtys ingests live data streams (WebSocket, webhooks, gRPC), normalizes events into a standard format, and publishes them to an internal NATS broker. It exposes a REST API for dynamic stream management and is designed to stand alone or integrate with [Ceres](https://github.com/AndreaBozzo/Ceres) and [Ares](https://github.com/AndreaBozzo/Ares) through the [Pantheon](https://github.com/AndreaBozzo/Pantheon) gateway.

> *Named after the Egyptian goddess of the night, rivers, and protector of the dead — she watches over the streams that flow in the dark.*

## Architecture

```
cmd/nephtys/          Entrypoint — wires config, broker, pipeline, server
internal/
  domain/             Core models — StreamEvent, StreamSourceConfig, SourceStatus
  connector/          StreamSource interface + implementations (WebSocket, ...)
  broker/             NATS connection wrapper + typed publish
  pipeline/           Middleware chain (filtering, enrichment, dedup — extensible)
  server/             REST API — stream management + health check
  config/             Environment-based configuration
```

All connectors implement the `StreamSource` interface and are decoupled from the broker through a `publish` callback wired through the pipeline.

## Prerequisites

- **Go** 1.23+
- **Docker** (for NATS)

## Quick Start

```bash
# Start NATS
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
    "topic": "nephtys.stream.crypto.btc"
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
| `sse` | 🔜 Planned | Server-Sent Events |
| `webhook` | 🔜 Planned | Inbound HTTP webhooks |
| `grpc` | 🔜 Planned | gRPC streaming |

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

## Related Projects

- **[Ceres](https://github.com/AndreaBozzo/Ceres)** — Semantic search engine for open data portals
- **[Ares](https://github.com/AndreaBozzo/Ares)** — Web scraper with LLM-powered structured data extraction
- **[Pantheon](https://github.com/AndreaBozzo/Pantheon)** — Unified TypeScript API gateway

## License

Apache-2.0 — see [LICENSE](LICENSE).
