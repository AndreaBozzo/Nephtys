# Nephtys — Roadmap (Aug 2026 – Jan 2027)

**Author:** Andrea Bozzo
**Last reviewed:** 2026-07-18
**Horizon:** 6 months, with a 12-month North Star
**Status of project:** UIC 2026 short paper **accepted**; camera-ready submitted (deadline 2026-07-31). The paper-window freeze (§5 of the previous roadmap) is lifted. This document supersedes the May–Jul 2026 roadmap, per its own re-plan trigger.

---

## 1. What changed since the last roadmap

1. **The paper was accepted.** The core idea — a lightweight, dynamically reconfigurable edge connector with durable NATS JetStream persistence — is now peer-validated. Positioning per the camera-ready: an *evaluated systems design point*, not a new algorithm.
2. **The controlled Node-RED comparison landed** (companion repo, `camera-ready-2026` branch): identical 12,000-event deterministic workload, matched output hashes, and Nephtys at roughly **1/3–1/6 of the resources** — tool RSS 19.1 MB vs 109.6 MB, tool+NATS RSS 27.2 MB vs 117.3 MB, CPU 0.03% vs 0.31%. This is the headline number for all positioning work.
3. **No further benchmarking is planned for now.** The Raspberry Pi protocol and orchestration scripts exist in the companion repo but stay blocked until hardware is available and there's a reason to run them. Conference attendance is undecided (cost); see §8 Risks.
4. **The freeze list is gone.** Schema envelope, gRPC restructuring, backpressure, OTel — all previously "do not touch" — are now schedulable on their merits.

**Carried-over gaps (verified against code, 2026-07-18):**

- **G1** *(was L2)* — WebSocket connector still has no post-connect hook (`on_connect_send`). Planned as June's theme; never landed. Blocks any source requiring a subscribe/auth frame.
- **G2** *(was L3)* — Per-stream connector *state* still not exposed: `StreamInfo` carries only `id` + `status` snapshot; no `nephtys_stream_state` Prometheus gauge, no `last_message_at`, no `health`. Planned as July's theme; not landed.
- **G3** *(was Q7)* — No `CHANGELOG.md`; tags `v0.1.0`/`v0.2.0` exist but no release notes, no binaries, no published container image.
- **G4** *(was L6)* — Still exactly one production consumer (Mercury). "Generic" was demonstrated to reviewers via configs and the Node-RED comparison, but no second end-to-end consumer exists in the wild.
- **G5** — Adoption is near-zero (1 star, 0 forks, no external issues). The bottleneck for the next 6 months is *visibility and adoption*, not features.

---

## 2. Landscape read (July 2026) — where the frontier is

- **NATS is having its moment.** NVIDIA's DSX AI-factory reference architecture put its event bus on NATS (May 2026); Synadia's messaging is now explicitly "AI at the edge with NATS"; NATS 2.14 is bringing batch ingest and message scheduling. Nephtys is a *NATS-native connector layer* — there is no entrenched "Kafka Connect of NATS", and that gap is Nephtys's wedge.
- **Agents are the new stream consumer.** MCP passed ~97M monthly SDK downloads; the emerging pattern is sensors and live feeds exposed to agents as streaming context. Nephtys already claims "AI agent telemetry" as a use case; making that claim *concrete* (an MCP-facing bridge over JetStream subjects) is the highest-upside frontier bet available at our size.
- **The lightweight-edge field is crowded but adjacent, not overlapping.** Fluent Bit owns edge *log* collection; Vector owns high-throughput observability pipelines; Zenoh owns microcontroller-class pub/sub; Redpanda Connect (128 MiB binary, streams-mode REST API) is the closest functional competitor but is Kafka-ecosystem-centric and an order of magnitude heavier than Nephtys's ~19 MB RSS. EdgeX (4.0 LTS) and Node-RED (5.0, June 2026) remain the integration-platform heavyweights the paper already positions against.
- **Nephtys's defensible niche**, sharpened by the review process: *the connector layer that turns heterogeneous real-time sources into uniform, durable, replayable NATS streams — light enough for the edge, reconfigurable in place without dropping the source connection, and generic over payloads.* Every roadmap item below either hardens that niche or makes it visible.

## 3. North Star (12 months)

Nephtys is the obvious first result when someone searches "ingest real-time streams into NATS." The same ~20 MB binary that feeds a trading agent feeds an air-quality dashboard and feeds an autonomous agent's context window. v1.0 is cut, the API is stable, releases ship multi-arch binaries and images, and at least one consumer that isn't Mercury exists in public.

Three pillars, in priority order:

1. **P1 — Finish the operational core** (G1, G2, supervision): a solo operator can run 50 streams and know at a glance what's alive.
2. **P2 — Prove it, publicly**: a runnable reference deployment, replay story, and the acceptance announcement converting the paper into visibility.
3. **P3 — Frontier bet: agent-native streaming**: MCP bridge + CloudEvents opt-in, riding the NATS+AI wave while it's cresting.

---

## 4. Six-month plan

One theme per month, sized for the ~20% evening-time budget (Mercury keeps ~80%). Quick wins (§5) fill odd slots.

### Month 1 (Aug 2026) — v0.3.0: the unfinished paper-window themes

- **WebSocket post-connect hook** (G1): optional `on_connect_send` (string or list), sent verbatim after handshake and re-sent on reconnect. Test against mock WS server. ~4 h.
- **Per-stream state visibility** (G2): `nephtys_stream_state{stream_id,state}` gauge driven by `StreamSource.Status()`; additive `last_message_at` + `health` fields on `StreamInfo`. ~5 h.
- Merge open dependabot PR; tag **v0.3.0** with a first `CHANGELOG.md` (starts G3).
- **Published container image**: GHCR workflow (linux/amd64 + arm64) so `docker run ghcr.io/andreabozzo/nephtys` becomes the 30-second quickstart. Split from the v1.0 release-automation work — the Dockerfile exists; only the publish pipeline is missing. ~1 h.

### Month 2 (Sep 2026) — Announce & reference deployment, part 1

- **Visibility push** (G5): announcement of acceptance (README badge already done) — short blog/LinkedIn post, submit Nephtys to the NATS monthly newsletter, add GitHub topics/social preview, seed `good-first-issue` labels. ~3 h, disproportionate return.
- **Sensor reference deployment (A1), skeleton**: `docker compose` stack — Nephtys → NATS → tiny consumer → DuckDB → Grafana dashboard, against one reliably-up open feed (OpenAQ or Sensor.Community, already exercised in the paper). Start here; finish in Month 3.

### Month 3 (Oct 2026) — v0.4.0: reference deployment done + replay

- Finish A1; it becomes the artifact linked from the README ("does it actually work for sensors? — here").
- **Stream replay (A4)**: document the JetStream replay recipe (`nats sub --start-sequence`, consumer-from-time) with example consumer code + one integration test. Mostly docs.
- **Connector supervisor (B3)**: per-stream `restart: {max_attempts, backoff}` so a permanently failed `Start()` no longer kills a stream silently. Pairs naturally with the M1 state gauge.

### Month 4 (Nov 2026) — Backpressure + SSE hook (the "second consumer shape" work)

- **Backpressure policy (A2)**: per-stream `overflow_policy: drop_oldest | drop_newest | block`, default = current behavior. The reference deployment plus Mercury now give two consumer shapes to anchor the design.
- **SSE post-connect generalization (B5)**: `Last-Event-ID` / auth-header patterns that static headers can't express.

### Month 5 (Dec 2026) — v1.0 preparation

- **Schema/version envelope on `StreamEvent` (B4)**: optional additive `schema_version`; migration notes for Mercury.
- **Public gRPC stub path (B6)**: move generated stubs out of `internal/` to an importable `github.com/AndreaBozzo/Nephtys/proto/nephtys/v1`.
- **Release automation**: GoReleaser (or equivalent) — multi-arch binaries incl. `linux/arm64` (the edge claim deserves an ARM artifact). The GHCR image ships in Month 1; this step may unify the two pipelines without regressing published tags.

### Month 6 (Jan 2027) — v1.0.0 + frontier prototype

- **v1.0.0 cut**: API-freeze review, SemVer commitment documented, CHANGELOG discipline locked in.
- **MCP bridge prototype (frontier)**: a small sidecar (separate binary or `nephtys mcp` subcommand) exposing registered streams as MCP resources — list streams, tail a subject, fetch last-N events via JetStream. Prototype scope: read-only, no auth beyond the existing token. This is the "Nephtys feeds agents" claim made runnable, and the single best story for riding the 2026 agent wave. **Primary target client: Claude Code** (the maintainer's primary agent companion; Codex second) — success looks like `claude mcp add nephtys` giving an agent live stream awareness with zero extra config.
- **CloudEvents opt-in output (D3)** if slack remains; otherwise it moves to the next horizon.

---

## 5. Quick wins (30–60 min slots)

| # | Quick win | Cost | Status | Why |
|---|---|---|---|---|
| W1 | Merge dependabot `golang.org/x/net` PR | 10 min | ✅ 2026-07-18 | Open security-relevant bump. |
| W2 | GitHub repo topics + description (social-preview image still manual) | 20 min | ✅ 2026-07-18 | Zero-cost discoverability (G5). |
| W3 | `CHANGELOG.md` back-filled for v0.1.0/v0.2.0 | 30 min | ✅ 2026-07-18 | Prerequisite for v0.3.0 tag. |
| W4 | arXiv/self-hosted preprint link once IEEE policy allows | 30 min | Open (blocked on camera-ready/IEEE policy) | Citable artifact drives adoption. |
| W5 | Issue templates + `good-first-issue` seeding | 45 min | ✅ (templates pre-existed; issues seeded) | Lowers first-contributor friction. |
| W6 | README "Performance" section citing the controlled Node-RED numbers | 45 min | ✅ 2026-07-18 | The 1/3–1/6 resource story belongs in the most-read file. |
| W7 | `docs/examples/` config for an agent-telemetry stream | 45 min | ✅ 2026-07-18 (`agent_telemetry_webhook.json`, validated via `--config-check`) | Fourth use-case profile becomes concrete. |

---

## 6. Versioning & release discipline

- **v0.3.0** (Aug): G1 + G2 + CHANGELOG. **v0.4.0** (Oct): reference deployment, replay, supervisor. **v0.5.0** (Nov): backpressure, SSE hook. **v1.0.0** (Jan 2027): schema envelope, public proto path, release automation, API freeze.
- From v0.3.0 on: every tag gets release notes; from v1.0.0 on: SemVer, additive-only within major.

## 7. Deferred (with rationale)

| Item | Deferred until | Rationale |
|---|---|---|
| Raspberry Pi / edge-device benchmark | Hardware + a concrete need (paper revision, blog, or adopter ask) | Protocol & scripts ready in companion repo; no bench spend for now (author decision, Jul 2026). |
| Scale sweep (20/50/100 streams) | Same as above | Camera-ready explicitly lists it as future work. |
| OTel tracing (B2) | Post-v1.0, opt-in | Unchanged; nobody has asked. |
| Stage-labeled latency histograms (B1) | A consumer with an SLO | Unchanged. |
| Multi-instance / clustering (C3) | Real load profile from a real adopter | Unchanged. |
| CSV/Parquet sink (C1), adaptive polling (C2), gRPC plugins (C4) | Sensor adoption signals | Need A1 feedback to shape. |
| Per-source subscribe helpers | **Never (in Nephtys)** | Generic-ness stays non-negotiable. |
| Managed/SaaS anything (D4) | Real community adoption | OSS-first posture unchanged. |

## 8. Risks

- **R1 — IEEE registration/presentation.** Not attending in person is a cost decision, but IEEE conferences typically require at least one author registration and presentation for the paper to enter the proceedings (no-show policies vary). **Action: check UIC 2026's registration requirement and remote-presentation options before the registration deadline — this can silently void the acceptance.**
- **R2 — Single maintainer, split attention.** The 80/20 Mercury split is unchanged; every month here is sized accordingly. If a month slips, the drop-target order is: frontier prototype → backpressure → reference-deployment polish. G1/G2 do not slip a third time.
- **R3 — Frontier bet decay.** The MCP/agent wave is fast-moving; if by Dec 2026 the ecosystem has converged on a different streaming-context pattern, re-scope Month 6 rather than shipping into a dead pattern.

## 9. Cadence

- Monthly check-in (last evening of the month): did the theme land? Unfinished work moves forward only with a written reason.
- Re-plan triggers: a second real consumer appears; an adopter files a substantive issue; Mercury needs a breaking Nephtys change; v1.0 ships. Any of these → rewrite, don't patch.
