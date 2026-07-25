# Benchmarks

## Edge comparison against Node-RED on a Raspberry Pi 5 (definitive)

Run on 2026-07-25 under the formal protocol in the companion repo
[`AndreaBozzo/uic2026-nephtys`](https://github.com/AndreaBozzo/uic2026-nephtys)
(`RASPBERRY_PI_BENCHMARK.md`, orchestrator `demo/comparison/run-pi-comparison.ps1`),
and reported in the accepted IEEE UIC 2026 short paper. Nephtys `c146ee7` against
Node-RED 5.0.1, both native on a Pi 5 (4 GB) with NATS, three interleaved trials each,
12,000 deterministic events per slot after a discarded 1,200-event warm-up.

| Metric | Nephtys | Node-RED | Ratio |
|---|---:|---:|---:|
| Tool RSS | 19.51 ± 0.07 MB | 128.47 ± 0.44 MB | **6.59×** |
| Tool + NATS RSS | 38.85 ± 0.10 MB | 147.07 ± 0.48 MB | 3.79× |
| CPU (100 % = 1 core) | 0.32 ± 0.00 % | 0.72 ± 0.01 % | 2.23× |
| Latency p95 | 2009 ± 1 ms | 2013 ± 1 ms | — |
| Wall power | 3.610 ± 0.005 W | 3.584 ± 0.014 W | 0.99× |

All six slots were valid on the first attempt, the SoC never throttled
(`throttled=0x0` across 1,316 samples, 45–51 °C), and **every slot produced the same
retained-event sequence hash and the same 155 output batches / 7,733 retained events /
67.30 % byte and 98.71 % message reduction as the x86-64 run** — so the pipelines are
equivalent across architectures, not merely similar.

**On energy, no claim is made in either direction.** Nephtys measured 0.70 % *higher*
wall power than Node-RED — the opposite sign to the memory result, and below one
quantisation step of the meter's power reading. The board draws ~3.0 W idle, so at
40 events/s the platform floor dominates and a 6.59× resident-memory advantage does
**not** become measurable energy savings. Read the footprint advantage as headroom for
co-located workloads on a small board, not as reduced power draw.

Raw data, per-second samples, per-slot logs, the recorded protocol deviations, and the
validity-gate record are in
[`demo/comparison/results/pi-20260725T075732Z/`](https://github.com/AndreaBozzo/uic2026-nephtys/tree/main/demo/comparison/results/pi-20260725T075732Z)
(see its `notes.md`).

## Power characterization (preliminary, exploratory)

> **Scope — read first.** This is an **exploratory, single-system** power-vs-throughput
> characterization of Nephtys on a Raspberry Pi 5, superseded as headline evidence by
> the controlled comparison above. It remains useful for the one thing the controlled
> run does not provide: how Nephtys' own power scales *with load*, since that run
> measures a single 40 events/s operating point.
>
> These numbers were collected over **Wi-Fi with a single trial per point**, so they
> carry more variance and are not directly comparable to the wired, repeated protocol.
> Treat them as an order-of-magnitude characterization, not as headline claims.

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
