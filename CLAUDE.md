# CLAUDE.md

Nephtys is a lightweight (~19 MB RSS) real-time stream connector: it ingests WebSocket / SSE / REST-polled / webhook / gRPC sources, runs events through per-stream JSON-configured pipelines, and publishes normalized events to NATS JetStream. Single Go binary, no database — JetStream holds both events and stream configs. Peer-validated by an accepted IEEE UIC 2026 short paper (companion repo: `AndreaBozzo/uic2026-nephtys`).

## Commands

```bash
make all          # fmt + vet + test — the standard pre-commit cycle
make test         # go test -race ./...
make run          # run locally (needs NATS: make docker-up first)
go run ./cmd/nephtys --config-check <file|->   # validate a stream config (exit 0/1)
```

CI additionally runs `golangci-lint`. `.gitattributes` pins the working tree to LF on every platform, so `gofmt -l` is trustworthy on Windows too — it used to flag all 56 files under `core.autocrlf=true`. A checkout made before that file existed still has CRLF; `git rm -r --cached . && git reset --hard` on a clean tree fixes it.

## Architecture map

- `cmd/nephtys/` — entry point, CLI flags, wiring
- `internal/connector/` — one file per source kind implementing `StreamSource` (`source.go`); pull sources (websocket, sse, rest_poller) self-reconnect, push sources (webhook, grpc) don't
- `internal/pipeline/` — middleware chain: filter → transform → dedup → enrich → threshold → batch; opaque payloads, built by `builder.go`
- `internal/server/` — REST API (`:3002`), `manager.go` owns stream lifecycle (`StreamManager`), optional bearer auth
- `internal/broker/` — JetStream publishing (content-type aware, binary passthrough)
- `internal/store/` — stream-config persistence in a JetStream KV bucket, restored on startup
- `internal/telemetry/` — per-stream Prometheus metrics
- `internal/domain/event.go` — `StreamEvent` envelope + all config structs (JSON tags = API surface)
- `proto/` — gRPC definitions (stubs currently generated into `internal/grpc/` — public path move is planned, issue #19)

## Hard rules

- **Generic-first is non-negotiable**: no source-specific logic in Nephtys (no Binance/MQTT/venue helpers — those live in consumers). Every feature must make sense for a non-crypto, non-sensor consumer too.
- **API surface evolves additively**: never rename/remove JSON fields on `internal/domain` config structs or REST responses; add optional fields instead.
- New connector config fields must be validated by `--config-check` and covered by an example in `docs/examples/` (each example must pass `--config-check`).
- Update `CHANGELOG.md` (Keep a Changelog format) with any user-visible change.

## Direction

`docs/ROADMAP.md` is authoritative (currently: Aug 2026 – Jan 2027, three pillars — ops core, visibility, agent-native frontier). Work is tracked on GitHub project board 4 and milestone-tagged issues. The frontier bet is an MCP bridge exposing streams to agents, with Claude Code as the primary target client.
