# Nephtys — Outcome Roadmap

**Author:** Andrea Bozzo

**Planning model:** outcome- and dependency-driven; releases ship when their acceptance criteria are met

**Project status:** the IEEE UIC short paper is accepted and the paper-window feature freeze is lifted

This is a living direction document, not a delivery calendar. GitHub issues hold implementation scope and acceptance criteria; the public project board shows execution state. Dates are used only when an external constraint genuinely has one.

## 1. Product position

Nephtys turns heterogeneous real-time sources into uniform, durable, replayable NATS streams. Its defensible niche is a connector layer that is:

- light enough for edge deployments;
- reconfigurable in place without dropping the source connection;
- generic over source payloads;
- observable and recoverable by a solo operator;
- useful to conventional consumers and autonomous agents alike.

The controlled comparison in the companion repository processed an identical deterministic sensor workload with matched output hashes. Nephtys used roughly 19 MB tool RSS versus 110 MB for Node-RED, with materially lower CPU use. The result supports Nephtys as an evaluated systems design point, not a new algorithm.

## 2. Current state

Completed foundations:

- five connectors: WebSocket, SSE, REST polling, webhook, and gRPC;
- durable event and configuration persistence through NATS JetStream;
- runtime pipeline reconfiguration;
- WebSocket post-connect frames, including reconnect replay;
- per-stream ingest, publish, drop, latency, and dedup metrics, plus a per-stream state gauge and `health`/`last_message_at` on the stream API;
- multi-architecture GHCR images with a `docker run` quickstart and a provisioned Grafana operations dashboard (0.3.0);
- changelog discipline and reproducible examples, with `--config-check` enforced over every published example;
- a controlled Node-RED comparison and accepted systems paper.

Remaining product gaps:

- distribution stops at the container: no binary releases, checksums, or provenance;
- configuration validation is structural, not semantic: `--config-check` does not reject unknown fields or invalid middleware values, and malformed durations fall back to defaults instead of failing;
- operational recovery needs a connector supervisor and explicit restart policy;
- the generic sensor story needs a second end-to-end public consumer;
- replay is supported by JetStream but not yet taught as a first-class workflow;
- live pipeline updates are not yet persisted across process restart;
- liveness and dependency readiness are not separated for orchestrated deployments;
- persisted connector configuration has no source-agnostic secret-reference mechanism;
- the public Go module and generated gRPC API need stable import paths before v1;
- adoption and external feedback remain limited.

## 3. Strategic pillars

1. **Operational core** — an operator can run many streams and immediately see what is connected, reconnecting, stopped, or errored.
2. **Public proof** — a runnable reference deployment, replay recipe, release artifacts, and clear positioning demonstrate that the system works outside its original consumer.
3. **Stable platform** — public API paths, compatibility policy, release automation, and explicit backpressure semantics support a v1 commitment.
4. **Focused experiments** — agent-native access and ecosystem envelopes are explored without blocking core releases.

## 4. Release sequence

### Shipped — operational surface and distribution (0.3.0)

Exit criteria, all met:

- per-stream state gauge plus `health` and `last_message_at` in the stream API;
- multi-architecture GHCR image with a verified `docker run` quickstart;
- changelog and GitHub release notes;
- existing configurations remain backward-compatible.

### Current release — operational proof

Exit criteria:

- a sensor reference stack from source through JetStream to a small consumer and dashboard;
- a documented replay/backfill recipe with an integration test;
- a connector supervisor with bounded restart policy, recovery tests, and terminal state visibility.
- durable pipeline updates with restart and storage-failure tests;
- an authoritative configuration contract: strict decoding and full middleware validation, with no silent fallback for malformed explicit values;
- separate liveness and readiness probes;
- a shared connector lifecycle conformance suite with deterministic fault fixtures.

The reference deployment is a prerequisite for designing backpressure because it supplies a second consumer shape beyond Mercury.

### Flow-control release

Exit criteria:

- explicit per-stream overflow policies with a backward-compatible default and drop metrics;
- resumable SSE patterns, including `Last-Event-ID`, without source-specific helpers;
- behavior validated against both the reference deployment and Mercury-style freshness requirements.

### Stable v1 platform

Exit criteria:

- a documented event-envelope compatibility policy and optional schema version;
- canonical Go module path and publicly importable generated gRPC stubs;
- secret references and redacted diagnostics for persisted connector credentials;
- automated multi-architecture binaries, checksums, images, and release notes;
- an API-freeze review and documented SemVer commitment.

The MCP bridge and CloudEvents output are not v1 release gates.

## 5. Experiments and backlog

### Agent-native bridge

Prototype a read-only MCP surface that can list registered streams, fetch recent JetStream events, and tail subjects. Reuse existing authorization and keep it decoupled from the ingestion pipeline. Continue only if real agent clients validate the interaction model.

### CloudEvents output

Explore an opt-in CloudEvents envelope only after the core release sequence is healthy. Existing consumers must not be forced to adopt it.

### Deferred until evidence exists

| Item | Activation signal | Rationale |
|---|---|---|
| Raspberry Pi benchmark and scale sweep | Hardware plus an adopter, publication, or regression question | The protocol exists; measurement without a decision to inform is churn. |
| OpenTelemetry tracing | A deployment needs cross-service traces | Keep the default footprint small. |
| Stage-labeled latency histograms | A consumer defines an SLO | Metrics should answer an operational question. |
| Multi-instance coordination | A real load or availability profile requires it | Avoid speculative distributed coordination. |
| Additional sinks and adaptive polling | Sensor reference users identify a concrete workflow | Let the second consumer shape the interface. |
| Managed service | Sustained community adoption | Preserve an OSS-first posture. |

Per-source subscribe helpers remain out of scope; generic connectors and user-supplied frames are the intended abstraction.

## 6. Risks and controls

- **Publication obligations:** track registration, presentation, and self-archiving requirements as explicit administrative issues whenever they affect the accepted paper.
- **Single-maintainer capacity:** limit active implementation to one core item plus one small documentation or community item.
- **Experimental decay:** reassess agent and event-envelope experiments against current ecosystem behavior before implementation.
- **Scope coupling:** experiments may consume slack but may not delay operational or stable-platform exit criteria.

## 7. Planning rules

- Board states distinguish backlog, ready, active work, review, blocked work, and done.
- An issue is ready only when its problem, scope, acceptance criteria, and dependencies are explicit.
- Release milestones group outcomes; they do not carry promised dates.
- At most one core implementation issue is in progress for the primary maintainer.
- Re-plan when a second real consumer appears, an adopter reports a substantive problem, a breaking integration need emerges, or v1 ships.
