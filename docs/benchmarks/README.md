# Benchmarks

## Power characterization (preliminary, exploratory)

> **Scope — read first.** This is an **exploratory, single-system** power-vs-throughput
> characterization of Nephtys on a Raspberry Pi 5. It is **not** the project's
> definitive energy evaluation. The definitive result is a **controlled Nephtys-vs-Node-RED
> wall-power comparison** run under a formal protocol (wired Ethernet, interleaved
> multi-slot runs, mean ± sample SD, validity gates) — see
> `RASPBERRY_PI_BENCHMARK.md` and `demo/comparison/` in the companion repo
> [`AndreaBozzo/uic2026-nephtys`](https://github.com/AndreaBozzo/uic2026-nephtys).
>
> These numbers were collected over **Wi-Fi with a single trial per point**, so they
> carry more variance and are not directly comparable to a wired, repeated protocol.
> Treat them as an order-of-magnitude characterization of *how Nephtys' own power
> scales with load*, useful for capacity intuition — not as headline claims.

### What it measures

The energy cost of running the full stack (OS + NATS JetStream + Nephtys) on edge
hardware, at the wall, across four idle baselines and three load points.

- **Instrument:** wall-plug smart meter (Shelly Plug S MTR Gen3), measured **at the
  socket** so PSU conversion losses are included.
- **Sampled from a separate host, never from the Pi** — polling the meter from the
  device under test would add CPU and Wi-Fi traffic to the very thing being measured.
- **Dual method per window:** average power from the integrated energy counter
  (`aenergy` delta — primary) and from instantaneous power samples (dispersion
  control). The two agreed within 0.5–4.6 % across all runs.
- **Load:** an exact-rate SSE generator (`power/loadgen.go`) on the sampling host.
  Nephtys' pipeline is empty, so one event = one NATS publish.

### Results (2026-07-23, 20-minute windows)

Raspberry Pi 5 (4 GB), USB SSD, active cooler, Wi-Fi. `vcgencmd get_throttled` = `0x0`
(no throttling/undervoltage). Power via the energy method (primary).

| Level | Load | Power | Δ vs idle | Marginal energy | Latency (ingest→publish) |
|-------|------|-------|-----------|-----------------|--------------------------|
| L0 · OS only        | idle    | 3.096 W | —        | —          | —       |
| L1 · + NATS         | idle    | 3.099 W | +0.003 W | —          | —       |
| L2 · + Nephtys      | idle    | 3.096 W | 0.000 W  | —          | —       |
| L3 · low            | 10 ev/s | 3.714 W | +0.62 W  | 64 mJ/ev   | 0.41 ms |
| L3 · mid            | 100 ev/s| 3.717 W | +0.62 W  | 6.3 mJ/ev  | 0.24 ms |
| L3 · high           | 1000 ev/s| 4.338 W| +1.24 W  | 1.3 mJ/ev  | 0.21 ms |

All load runs: ingested bytes = published bytes (zero loss); 168.7 MB / 1.19 M events
at 1000 ev/s, processed in real time with no backpressure.

**Observations (not claims):** the idle software stack shows no power increase above
the bare OS within meter resolution; the load curve is strongly sublinear (a fixed
~0.6 W activation cost dominates, then marginal energy per event falls ~50× as
throughput rises); latency does not degrade with load.

> Following the companion protocol's guidance, absolute wall power is the primary
> figure; the idle-baseline deltas are shown as secondary context and should not be
> read as a subtracted energy-superiority claim within meter resolution.

### Reproduce

```bash
# on the sampling host (NOT the Pi):
export SHELLY_IP=<meter-ip>            # Shelly Plug S MTR Gen3, HTTP RPC, auth disabled
go run power/loadgen.go -addr :8099    # exact-rate SSE + REST generator

# one idle window (Nephtys already running on the Pi):
SHELLY_IP=$SHELLY_IP ./power/measure-power.sh L2_idle 1200 2

# one load point end-to-end (creates SSE stream on the Pi, measures, cleans up):
PI=andrea@pi5.local PC_IP=<host-lan-ip> \
  ./power/run-load-level.sh L3_high_1000evs 1000 1200
```

### Known limitations

- **Wi-Fi, not wired Ethernet** — adds power draw and variance; the formal protocol
  uses Ethernet.
- **Single trial per point** — no across-trial mean ± SD; only within-window
  dispersion is reported.
- **Meter quantization** — the Shelly `aenergy` counter accumulates in ~1-minute
  blocks and is unusable on short windows (< a few minutes); it is reliable on the
  20-minute windows used here, cross-checked against instantaneous samples.
- Single-system characterization, **not** a tool-to-tool comparison.
