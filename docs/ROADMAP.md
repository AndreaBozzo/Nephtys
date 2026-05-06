# Nephtys — Roadmap (May–Jul 2026)

**Author:** Andrea Bozzo
**Last reviewed:** 2026-05-06
**Horizon:** ~3 months
**Status of project:** UIC short paper under peer review (~1 month remaining). Code is stable; this window is for **focused, additive evolution** that keeps the paper's claims true and, where possible, strengthens them.

---

## 1. Operating principles for this window

1. **Keep the paper true, don't freeze the code.** The paper frames Nephtys as a generic real-time stream connector for both *market data and sensor streams* with a NATS JetStream backend, opaque-payload pipelines, and a small REST/gRPC control surface. Changes that uphold or *strengthen* those claims are welcome; changes that contradict them aren't. "Stable" means trustworthy under review, not untouchable.
2. **Sensor-class consumers are first-class.** Crypto is the loud consumer today (Mercury), but every design choice should pass the "would a sensor stream operator find this reasonable?" check. Sensor scenarios — irregular rates, lossy upstreams, long reconnects, low-cardinality high-volume telemetry — are explicit design constraints, not edge cases.
3. **Generic-first discipline.** Every change must answer "would this still make sense for a non-crypto consumer?" If no, defer. Mercury's needs are *informative*, not authoritative.
4. **80/20 split with Mercury.** Roughly 80% of solo evening time goes to Mercury (live-flip path), 20% to Nephtys (this document). One Nephtys theme per month, plus opportunistic quick wins (§4) that fit in 30–60 min slots.
5. **Public API surface evolves additively, not destructively.** Adding optional fields, additive endpoints, and new metrics is fine. Renaming, removing, or restructuring REST/gRPC types is off-limits until camera-ready — a reviewer comparing the paper's described API to the repo must see consistency.
6. **Document deferrals, don't silently drop them.** Items that would compromise generic-ness or expand scope go to §7 with rationale, so they're recoverable post-review.

---

## 2. Current state (verified 2026-05-06, against code at HEAD `8b2ce07`)

**What works well:**
- Generic stream connector abstraction (`internal/connector/`): WebSocket, REST poller, SSE, webhook, gRPC sources — all framework-agnostic.
- NATS JetStream backend with content-type-aware publishing ([broker/nats.go](../internal/broker/nats.go)).
- StreamEvent carries a `Seq` field; `X-Nephtys-Seq` header published when upstream provides one. Mercury falls back to NATS `metadata.sequence` otherwise.
- Pipeline DSL (filter / transform / dedup / enrich / threshold) operates on opaque payloads.
- Bearer-token admin auth ([config/config.go](../internal/config/config.go), [server/auth.go](../internal/server/auth.go)).
- **Stream config persistence works**: registration & deletion roundtrip through a JetStream KV bucket ([store/](../internal/store/), wired in [cmd/nephtys/main.go:38-50](../cmd/nephtys/main.go#L38-L50)). `StreamManager.Restore()` ([server/manager.go:119-149](../internal/server/manager.go#L119-L149)) re-registers persisted streams on startup.
- **Per-stream Prometheus metrics already wired** ([telemetry/metrics.go](../internal/telemetry/metrics.go)): `events_ingested_total{stream_id}`, `events_published_total{stream_id}`, `events_dropped_by_pipeline_total{stream_id, middleware}`, `bytes_ingested_total{stream_id}`, `bytes_published_total{stream_id}`. `/metrics` endpoint exposed via `promhttp.Handler()` ([server/server.go:70](../internal/server/server.go#L70)) and reachable without admin auth (Prometheus-scrape-friendly).
- Full CI + Codecov + Grafana renderer.
- Production consumer: Mercury (crypto trading agent). Paper framing is **urban sensor streams**; sensor consumers are claimed but not yet demonstrated end-to-end.

**Known limitations carried into this window:**
- **L1.** `UpdatePipeline` ([server/manager.go:151-184](../internal/server/manager.go#L151-L184)) is **intentionally transient** — the in-code comment frames pipeline hot-swaps as Dynamic Context Adaptation, deliberately not persisted. Whether this is correct is a *design question*, not a bug; treat as open question for §6.
- **L2.** WebSocket connector has **no post-connect hook** ([connector/websocket.go](../internal/connector/websocket.go)) for sending auth/subscribe frames. Works for URL-subscribed sources (Binance, simple sensor WS endpoints) but blocks any venue or sensor gateway requiring an explicit subscribe/auth frame.
- **L3.** Per-stream connector *state* is not exposed — neither in the REST `GET /v1/streams` response nor as a Prometheus gauge. Operators get throughput metrics but no per-stream "is it alive / reconnecting / errored" view. Counters exist; the state gauge is the gap.
- **L4.** No documented sensor example in repo. The paper's primary framing (urban sensor streams) has no end-to-end demonstration in `docs/` or `README.md`. The README shows only a crypto WebSocket example.
- **L5.** Single-instance only — no horizontal-scaling story (out of scope for this window; paper does not claim it).
- **L6.** "Generic" is a *claim* until a non-crypto consumer ships. Today Mercury is the only end-to-end consumer; a sensor demo would convert the claim to a demonstrated property.

---

## 3. Three-month plan (the themed work)

One theme per month. Each is small, additive, and chosen because it's valuable to *both* trading and sensor consumers.

### Month 1 (May 2026) — Sensor demonstration in repo (fixes L4) ✅ landed 2026-05-06

**Goal:** make the paper's primary framing (urban sensor streams) self-evident from the repo, end-to-end.

**Status:** Done in this window. [`docs/examples/`](examples/) holds three runnable configs (`sensor_rest_poller.json`, `sensor_sse.json`, `crypto_websocket.json`); `README.md` now leads its Usage section with the sensor example and includes a "Use cases" block that frames sensors first. L4 closed.

**Why this first:** The paper's title is *"Lightweight Edge Connector for Bandwidth-Efficient Ingestion of Urban Sensor Streams,"* but today the README and `docs/` show only a crypto WebSocket example. A reviewer reading the repo sees a trading-shaped project with a sensor-shaped abstract. Closing that gap is the highest-leverage *paper-positive* move in this window: it's documentation + example configs (no code change), it strengthens every reviewer touchpoint, and it costs less than any code-change month. The previously planned "pipeline durability" theme is no longer needed at this level — stream-config persistence already works (see §2); only `UpdatePipeline` itself is intentionally transient, which is a design question, not a fix to schedule.

**Scope:**
- Create `docs/examples/` with at least 3 minimal stream configs:
  - `crypto_websocket.json` — current Binance trade WS (parity with README example).
  - `sensor_rest_poller.json` — REST poller against a public open-data sensor API (e.g. air quality, weather, transit).
  - `sensor_sse.json` — SSE example to show the connector beyond polling.
- Add a "Sensor stream example" walkthrough section to `README.md` paralleling the existing "WebSocket Stream" section: register → verify → describe what the operator sees.
- Add a `## Use cases` block to `README.md` that names sensor and market-data consumers explicitly, framed in the paper's order (sensors first).
- Smoke-test each example config locally against `make run`.

**Out of scope:**
- Code changes (this is documentation-only by design).
- Live demo deployments / hosted examples.
- Tutorial videos or blog content.

**Estimated effort:** ~3 hrs.

**Success criteria:**
- A reviewer landing on the README sees a sensor example in the first scroll.
- Each `docs/examples/*.json` config validates against `validateStreamConfig` ([server/handlers.go:131](../internal/server/handlers.go#L131)).
- No code change touches the public API.

---

### Month 2 (June 2026) — WebSocket post-connect hook (fixes L2)

**Goal:** unlock auth/subscribe-frame sources without compromising generic-ness.

**Why this:** real-world WebSocket sources require sending a subscribe or auth message after connect — Coinbase and Kraken on the trading side, but **also** many sensor and IoT gateways (LoRaWAN bridges, MQTT-over-WS endpoints, building-management systems, scientific data feeds). Today Nephtys can't reach them. The fix is a single optional config field — additive, generic, and scrupulously framework-agnostic.

**Scope:**
- Add optional `on_connect_send` field to WebSocket source config (string or list of strings, sent verbatim after handshake).
- Implement send-after-handshake logic in [connector/websocket.go](../internal/connector/websocket.go).
- Re-send on reconnect (so the field is durable across connection drops).
- Test: mock WS server verifies message receipt; reconnect flow re-sends.
- Documentation example in `docs/`: at least one trading-style and one sensor-style example (e.g. an IoT gateway requiring an auth frame).

**Out of scope:**
- Templating / variable substitution in the message (would invite consumer-specific use; defer).
- Auth header schemes beyond what already works via static headers.
- Per-source helper builders (Binance subscribe, Coinbase subscribe, MQTT connect packet, etc.) — those belong in *consumers*, not in Nephtys.

**Estimated effort:** ~4 hrs.

**Success criteria:**
- Test passes against a mock WS server requiring a subscribe frame.
- Field is *optional*; existing WS configs unchanged.
- No new dependency.
- Docs show both a trading and a sensor use case.

---

### Month 3 (July 2026) — Per-stream connector-state visibility (closes L3)

**Goal:** finish the operational surface so an operator running 50 sensor streams can answer "is stream X alive, reconnecting, or errored?" at a glance.

**Why this:** `/metrics` already exists ([server/server.go:70](../internal/server/server.go#L70)) and per-stream throughput counters are wired ([telemetry/metrics.go](../internal/telemetry/metrics.go) — `events_ingested_total`, `events_published_total`, `bytes_*_total`, `events_dropped_by_pipeline_total`). What's missing is per-stream **state**: counters tell you *how much* is flowing, not *whether the connector is healthy*. For sensor operators with dozens of streams, this is the gap between "I have metrics" and "I have ops visibility." Surface state in two places (REST list response + Prometheus gauge) so both human operators and dashboards can use it. **This month is the explicit drop-target if Months 1–2 overrun or the paper review extends.**

**Scope:**
- Add a Prometheus gauge: `nephtys_stream_state{stream_id, state="connected|reconnecting|errored|stopped"}` driven by `connector.StreamSource.Status()` (already available in [server/manager.go:194](../internal/server/manager.go#L194)).
- Extend `StreamInfo` ([server/manager.go:48-52](../internal/server/manager.go#L48-L52)) returned by `GET /v1/streams` with additive fields: `last_message_at` (RFC3339 timestamp) and `health` (string: `healthy|degraded|errored`). Existing clients ignore unknown fields, so this stays additive and paper-safe.
- Hook `last_message_at` updates into the existing `instrumentedPublish` path in [server/manager.go:263-269](../internal/server/manager.go#L263-L269).
- One unit test for the new gauge; one for the extended `StreamInfo`.
- Documentation update describing the new fields and gauge.

**Out of scope:**
- OpenTelemetry / tracing (paper deliberately avoids OTel; revisit post-review).
- Latency histograms (defer until a consumer with a real SLO needs them).
- Per-pipeline-stage metrics (would couple metrics to pipeline DSL internals).

**Estimated effort:** ~5 hrs.

**Success criteria:**
- `curl localhost:3002/metrics` returns valid Prometheus text.
- Metric names follow Prometheus conventions and are venue-agnostic (no `binance_*`, no `crypto_*`, no `sensor_*` either — pure stream-level naming).
- Existing Grafana renderer dashboard renders something useful.

---

## 4. Quick wins (opportunistic, fit in 30–60 min slots)

Pick one whenever there's a leftover slot. All are additive, sensor-friendly, and paper-safe.

Note: Q1 and Q6 below are folded into Month 1 (§3) because they form the core of the sensor-demonstration deliverable. They're listed here only for cross-referencing.

| # | Quick win | Cost | Status | Why it's a win |
|---|---|---|---|---|
| Q1 | Sensor-stream walkthrough in `README.md` | 30 min | Folded into M1 | Strengthens paper framing in the most-read file. |
| Q2 | `--version` / `--config-check` flags on the `nephtys` binary | 45 min | Open | Operator QoL; useful in downstream CI. |
| Q3 | `last_message_at` on `StreamInfo` (extends `GET /v1/streams`) | 45 min | Folded into M3 | Liveness signal any operator wants. |
| Q4 | `health` field on `StreamInfo` (driven by connector state) | 30 min | Folded into M3 | Companion to Q3; additive. |
| Q5 | `golangci-lint` config + CI job | 45 min | Open | Catches drift; cheaper before review than after. |
| Q6 | `docs/examples/` with crypto WS + sensor REST poller + sensor SSE configs | 1 hr | Folded into M1 | Concrete proof-of-genericness. |
| Q7 | `CHANGELOG.md` + first release tag | 30 min | Open | Useful discipline once a 2nd consumer adopts. |
| Q8 | Benchmark for publish path (`BenchmarkPublishOpaque` in `broker/`) | 1 hr | Open | Lets the paper cite real numbers if reviewers ask. |

**Rule of thumb:** if a quick win starts to grow past ~1 hr or starts touching public types, stop and re-scope it as a themed month.

**Open items (Q2, Q5, Q7, Q8) priority:** Q5 first (catches drift cheap), then Q8 (paper-defensive), then Q7 (release hygiene), then Q2 (nice-to-have).

---

## 5. The "do not touch" list (review window)

These would be reasonable changes in any other window, but they are **off-limits until camera-ready** because they could invalidate paper claims or invite reviewer churn:

- ❌ **Schema/version envelope on StreamEvent** — would change wire format and contradict the paper's documented schema.
- ❌ **Backpressure policy** — single-consumer profile can't drive a sound design; deferred until a 2nd consumer profile exists.
- ❌ **gRPC API redesign** — even if cleaner, breaks the paper's described surface.
- ❌ **Renaming public types/fields** — same reason.
- ❌ **Multi-instance / clustering** — large scope; paper does not claim it; expanding now invites "why isn't this in the paper" questions.
- ❌ **Tracing / OpenTelemetry** — paper's framing explicitly avoids imposing OTel; revisit post-review.
- ❌ **Consumer-specific helpers** (Binance subscribe builder, depth normalizer, MQTT connect-packet builder, etc.) — paper's core thesis is genericness; any leak undermines it.

If a reviewer *requests* any of these, that's a different conversation — handle it as a paper revision, not a roadmap change.

---

## 6. Post-review window (Aug 2026 onward — preview, not commitment)

Once the paper is through, the constraints lift and the priorities reorder. Likely candidates, in rough order:

1. **Land a 2nd consumer profile, sensor-side.** Even a small demo (open-data poller, public weather/air-quality feed, a Raspberry Pi telemetry stream) that exercises Nephtys end-to-end. Turns "generic" from a claim into a demonstrated property and unlocks honest design conversations for backpressure, schema versioning, and multi-tenancy.
2. **Backpressure policy** — once two consumer profiles exist, the design has a real anchor.
3. **Schema/version envelope** — major version bump territory. Worth it once there are real consumers to migrate.
4. **Latency histograms** — driven by a consumer with an SLO (Mercury's live-flip path qualifies once it's flipped; sensor SLAs would also drive it).
5. **Multi-instance scaling** — depends on actual load profile; premature today.

These are not commitments. They are the shape of the conversation when the review window closes.

---

## 7. Deferred items (with rationale)

| Item | Deferred until | Rationale |
|---|---|---|
| Pipeline config versioning/history | Post-review | Adds API surface; current "last write wins" is sufficient for one operator. |
| Per-source subscribe helpers (any source) | Never (in Nephtys) | Lives in consumer, not connector. Generic-ness is non-negotiable. |
| OTel tracing | Post-review | Paper deliberately avoids imposing OTel. |
| Latency histograms | When a consumer has an SLO | Premature without a consumer driving the requirement. |
| Backpressure policy | 2nd consumer profile exists | Single profile can't drive a sound design. |
| Schema/version envelope | Post-review + 2nd consumer | Major version event; needs migration story. |
| Multi-instance / clustering | Post-review | Paper does not claim it; scope-expanding during review. |
| Auth schemes beyond bearer token | When asked | YAGNI; current token auth covers Mercury and any local-dev consumer. |

---

## 8. Working agreement with Mercury

Mercury is the loudest consumer in this window but **not** a steering input.

- Mercury improvements that *would* benefit from a Nephtys change get logged here (as "deferred") and revisited post-review unless they are also genuinely generic (i.e. would help a sensor consumer too).
- Mercury's M1/M2/M3 plan ([Mercury ROADMAP.md](../../Mercury/ROADMAP.md)) does not depend on any item in this Nephtys roadmap. The two committed Mercury-relevant Nephtys items (Months 1–2 above) are **independently valuable** to a generic operator; their alignment with Mercury is incidental.
- If Mercury hits a hard blocker that requires a Nephtys change not in this plan, treat it as an exception requiring an explicit decision, not an in-flight scope expansion.

---

## 9. Cadence and review

- **Monthly check-in** (last evening of each month): did the planned theme land? If not, why? Do not roll unfinished work forward without an honest reason.
- **Time budget:** ~4 hours/month on the themed work (20% of ~5 hrs/week × 4 weeks). Quick wins (§4) come from leftover/odd slots, not the main budget.
- **Buffer strategy:** Month 3 (Prometheus surface) is the explicit drop-target if the paper review extends or Months 1–2 overrun. Use it; don't pre-allocate it.
- **Re-plan trigger:** paper accepted, paper rejected, or any "do not touch" item proves to be load-bearing for Mercury's live flip. In any of those cases, this document is rewritten, not patched.
