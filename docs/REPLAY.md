# Replay and recovery

Every event Nephtys accepts is already durable. Publishing goes to JetStream, and
JetStream keeps what it stored until a retention limit removes it — so replaying
an outage, backfilling a new consumer, or re-reading yesterday afternoon needs
nothing from the Nephtys API. It is entirely a consumer-side decision: pick where
to start, and read.

That is the whole design. Nephtys deliberately exposes no replay endpoint,
because one would only be a worse wrapper around what the broker already does.
This document is the recipe for doing it directly.

- [What is retained](#what-is-retained)
- [Where a replay can start](#where-a-replay-can-start)
- [Two counters and two clocks](#two-counters-and-two-clocks)
- [Recipe: look before you replay](#recipe-look-before-you-replay)
- [Recipe: read a replay by eye](#recipe-read-a-replay-by-eye)
- [Recipe: recover after a consumer outage](#recipe-recover-after-a-consumer-outage)
- [Recipe: backfill from a point in time](#recipe-backfill-from-a-point-in-time)
- [The example consumer](#the-example-consumer)
- [Pull, not push](#pull-not-push)
  - [A trap when writing your own consumer](#a-trap-when-writing-your-own-consumer)
- [What replay does not do](#what-replay-does-not-do)

The `nats` CLI is used throughout: `go install github.com/nats-io/natscli/nats@latest`,
or see [natscli releases](https://github.com/nats-io/natscli/releases). Set
`NATS_URL` (or pass `-s`) so the examples below reach your broker.

## What is retained

Nephtys creates one JetStream stream at startup and publishes every accepted
event to it:

| | |
|---|---|
| Stream name | `NEPHTYS` |
| Subjects | `nephtys.stream.>` — one subject per stream's configured `topic` |
| Storage | File |
| Retention | Limits, discard old |
| Maximum age | 72h |
| Maximum bytes | unlimited |

```console
$ nats stream info NEPHTYS
...
Limits:

             Maximum Messages: unlimited
          Maximum Per Subject: unlimited
                Maximum Bytes: unlimited
                  Maximum Age: 3d0h0m0s

State:

                     Messages: 6
                        Bytes: 1.3 KiB
               First Sequence: 1 @ 2026-09-08 18:24:47
                Last Sequence: 6 @ 2026-09-08 18:24:49
```

**The maximum age is the ceiling on every replay in this document.** `First
Sequence` is the oldest event that still exists; nothing before it can be
recovered, from any consumer, by any means.

**Nephtys re-applies this configuration on every start.** It calls
`UpdateStream` unconditionally, so a `nats stream edit NEPHTYS --max-age=168h`
holds until the next Nephtys restart and is then silently reverted to 72h. There
is no environment variable for it yet: a deployment that needs a longer window
has to re-apply the edit after each start, or change
`broker.DefaultConfig()` and rebuild. Retention is otherwise a broker-side
concern and the CLI is the right place to inspect it.

## Where a replay can start

A JetStream consumer's *deliver policy* is the only thing that decides where a
replay begins. Both tools below set it:

| Start at | `nats sub` | `consumer.go` |
|---|---|---|
| The oldest retained event | `--all` | `-from all` (default) |
| A stream sequence | `--start-sequence 4210` | `-from 4210` |
| A relative time | `--since 2h` | `-from 2h` |
| An absolute instant | *not available* | `-from 2026-09-08T06:00:00Z` |
| The newest event, then onward | `--last` | `-from last` |
| The newest event of each subject | `--last-per-subject` | `-from last-per-subject` |
| Only what is published from now on | `--new` | `-from new` |

The absolute-instant row is why the [example consumer](#the-example-consumer)
exists: the CLI's `--deliver` and `--since` take a duration ago, and "replay from
06:00 UTC, when the incident started" is not naturally a duration.

A start position applies **only when the consumer is created**. On a durable
consumer every later run resumes from its acknowledgement floor and the deliver
policy is inert — which is the point of a durable, and a surprise exactly once.

## Two counters and two clocks

A replayed message carries two of each, and they are not interchangeable.

**JetStream's** stream sequence and store timestamp are assigned by the broker on
publish. They are dense, strictly increasing, unique within the stream, and they
are what every deliver policy positions on. `nats stream get NEPHTYS 3` and
`-from 4210` mean these.

**The envelope's** `seq` and `timestamp` come from the source. `timestamp` is
when Nephtys built the event; `seq` is whatever sequencing the connector could
infer — for WebSocket, a top-level `e`/`E`/`seq`/`u`/`lastUpdateId`/`t` key; for
everything else, usually nothing at all, in which case the field is omitted and
reads back as `0`. They can repeat, arrive out of order, or be absent.

Position replays on JetStream's coordinates. Use the envelope's for application
logic — ordering trades, detecting a gap in a sensor's own counter — and never
the reverse.

## Recipe: look before you replay

Find the window, the subjects, and one event, before committing a consumer to
anything:

```bash
nats stream info NEPHTYS                 # retention window and sequence range
nats stream subjects NEPHTYS             # which streams actually published
nats stream get NEPHTYS 3                # one event, by stream sequence
```

```console
$ nats stream subjects NEPHTYS
╭───────────────────────────────────────────────────────╮
│              1 Subjects in stream NEPHTYS             │
├─────────────────────────────┬───────┬─────────┬───────┤
│ Subject                     │ Count │ Subject │ Count │
├─────────────────────────────┼───────┼─────────┼───────┤
│ nephtys.stream.sensors.demo │ 6     │         │       │
╰─────────────────────────────┴───────┴─────────┴───────╯

$ nats stream get NEPHTYS 3
Item: NEPHTYS#3 received 2026-09-08 16:24:48.3438322 +0000 UTC (22.99s) on Subject nephtys.stream.sensors.demo

Headers:
  Content-Type: application/json

{"source":"replay_demo","type":"webhook_recv","timestamp":1788884688343,"payload":{"sensor_id":"s-3","temperature_c":21.5}}
```

## Recipe: read a replay by eye

`nats sub` against the stream creates a throwaway consumer that acknowledges
nothing, so it cannot disturb a durable consumer's position or anyone else's
progress. It is the right tool for looking, and the wrong one for loading.

```bash
# Everything still retained on one subject tree, then exit at the end.
nats sub "nephtys.stream.sensors.>" --stream NEPHTYS --all --terminate-at-end

# From a stream sequence.
nats sub "nephtys.stream.>" --stream NEPHTYS --start-sequence 4 --terminate-at-end

# From two hours ago.
nats sub "nephtys.stream.>" --stream NEPHTYS --since 2h --terminate-at-end
```

`--terminate-at-end` is what makes these commands finish: without it the
subscription catches up and then follows live, which is useful for watching a
stream and useless in a script.

## Recipe: recover after a consumer outage

A consumer that was down does not need Nephtys to have done anything. Its
position is server-side state, and it resumes from there.

Create the durable consumer once:

```bash
nats consumer add NEPHTYS warehouse-loader \
  --pull --deliver=all --filter "nephtys.stream.sensors.>" \
  --ack explicit --replay instant --max-deliver=-1 --defaults
```

- `--pull` — the consumer asks for work rather than being pushed it; see
  [Pull, not push](#pull-not-push).
- `--deliver=all` — start from the oldest retained event. This applies only at
  creation. `--deliver` also accepts `new`, `last`, `subject`, a duration such as
  `1h`, and a bare stream sequence.
- `--ack explicit` — every event is acknowledged individually, and the
  acknowledgement floor is the recovery point.
- `--replay instant` — deliver as fast as the consumer takes them. `original`
  re-paces the replay to the events' original spacing, which is occasionally what
  you want for a simulation and never what you want for a backfill.
- `--max-deliver=-1` — an event that is never acknowledged is retried forever
  rather than being dropped after a few tries.

Then read from it, as many times as you like:

```console
$ nats consumer next NEPHTYS warehouse-loader --count 2 --ack
[18:25:19] subj: nephtys.stream.sensors.demo / tries: 1 / cons seq: 1 / str seq: 1 / pending: 5
{"source":"replay_demo","type":"webhook_recv","timestamp":1788884687283,"payload":{"sensor_id":"s-1","temperature_c":19.5}}
[18:25:19] subj: nephtys.stream.sensors.demo / tries: 1 / cons seq: 2 / str seq: 2 / pending: 4
{"source":"replay_demo","type":"webhook_recv","timestamp":1788884687843,"payload":{"sensor_id":"s-2","temperature_c":20.5}}

$ nats consumer report NEPHTYS
╭──────────────────┬──────┬────────────┬──────────┬─────────────┬─────────────┬─────────────┬───────────╮
│ Consumer         │ Mode │ Ack Policy │ Ack Wait │ Ack Pending │ Redelivered │ Unprocessed │ Ack Floor │
├──────────────────┼──────┼────────────┼──────────┼─────────────┼─────────────┼─────────────┼───────────┤
│ warehouse-loader │ Pull │ Explicit   │ 30.00s   │ 0           │ 0           │ 4 / 66%     │ 2         │
╰──────────────────┴──────┴────────────┴──────────┴─────────────┴─────────────┴─────────────┴───────────╯
```

`Ack Floor: 2` and `Unprocessed: 4` are the recovery state, and they survive the
consumer process, the Nephtys process, and the broker. Running the same command
again delivers events 3 and 4 — the outage is over as soon as the consumer comes
back, with nothing to reconfigure.

`nats consumer report NEPHTYS` is also the monitoring view: a growing
`Unprocessed` is a consumer falling behind, and a non-zero `Redelivered` is one
failing to acknowledge.

## Recipe: backfill from a point in time

A new consumer that needs history — a warehouse loader, a freshly deployed model,
a dashboard someone wants populated — starts at a time rather than a sequence,
because that is the coordinate an incident or a business day is described in.

Relative, with the CLI:

```bash
nats sub "nephtys.stream.sensors.>" --stream NEPHTYS --since 6h --terminate-at-end
```

Absolute, with the example consumer:

```bash
go run docs/replay/consumer.go \
  -subject "nephtys.stream.sensors.>" \
  -from 2026-09-08T06:00:00Z
```

Start times are inclusive: an event stored at exactly the given instant is
delivered.

## The example consumer

[`docs/replay/consumer.go`](replay/consumer.go) is a complete replay consumer in
one file, with no dependency beyond the NATS client. It is meant to be read and
copied, not imported — the event envelope is redeclared in it so that it compiles
outside this repository.

```bash
go run docs/replay/consumer.go [flags]
```

| Flag | Default | Meaning |
|---|---|---|
| `-server` | `$NATS_URL`, else `nats://127.0.0.1:4222` | Broker URL |
| `-stream` | `NEPHTYS` | JetStream stream to bind to |
| `-subject` | `nephtys.stream.>` | Subject filter within the stream |
| `-from` | `all` | `all`, `new`, `last`, `last-per-subject`, a stream sequence, a duration ago (`2h`), or an RFC 3339 instant |
| `-durable` | unset | Durable consumer name. Resumes from its acknowledgement floor; `-from` then applies only to the first run |
| `-count` | `0` | Stop after this many events; `0` means no limit |
| `-idle` | `5s` | Exit once no event arrives for this long — how a backfill knows it has reached the end. `0` follows the stream indefinitely, and is what `-durable` wants for a long-running loader |

Each event prints as one tab-separated line: stream sequence, store timestamp,
subject, then the envelope.

```console
$ go run docs/replay/consumer.go -from 4
replaying nephtys.stream.> from stream sequence 4
4	2026-09-08T16:24:48.845973Z	nephtys.stream.sensors.demo	source=replay_demo	type=webhook_recv	seq=0	{"sensor_id":"s-4","temperature_c":22.5}
5	2026-09-08T16:24:49.3599032Z	nephtys.stream.sensors.demo	source=replay_demo	type=webhook_recv	seq=0	{"sensor_id":"s-5","temperature_c":23.5}
6	2026-09-08T16:24:49.8873186Z	nephtys.stream.sensors.demo	source=replay_demo	type=webhook_recv	seq=0	{"sensor_id":"s-6","temperature_c":24.5}
no event for 5s — stopping
3 event(s) replayed
```

The outage-recovery story, end to end — the second run resumes immediately, with
no waiting and no redelivery:

```console
$ go run docs/replay/consumer.go -durable demo-loader -count 2
1	...	{"sensor_id":"s-1","temperature_c":19.5}
2	...	{"sensor_id":"s-2","temperature_c":20.5}
2 event(s) replayed

$ go run docs/replay/consumer.go -durable demo-loader
3	...	{"sensor_id":"s-3","temperature_c":21.5}
4	...	{"sensor_id":"s-4","temperature_c":22.5}
5	...	{"sensor_id":"s-5","temperature_c":23.5}
6	...	{"sensor_id":"s-6","temperature_c":24.5}
no event for 5s — stopping
4 event(s) replayed
```

Binary events print their content type and byte count instead of a payload,
since they carry no JSON envelope — see [What replay does not
do](#what-replay-does-not-do).

## Pull, not push

Both the CLI recipe and the example consumer use **pull** consumers, and the
reason is worth stating because the push version looks like it works.

A push consumer streams ahead of acknowledgements: the server delivers up to
`MaxAckPending` events as fast as it can. Stop the consumer after two of them and
the rest are already delivered and unacknowledged — so a restarted consumer
receives *nothing* until `AckWait` expires and the server gives up on them. That
is 30 seconds by default, during which the recovery you just performed looks like
data loss. It is not; it is a redelivery timer. But it is not a behaviour to build
a recovery procedure on.

A pull consumer only ever holds what it asked for. Stop it and at most one batch
is outstanding; the next run resumes on the following event immediately. For
replay and backfill — where the whole point is a deterministic restart — that is
the only shape that behaves.

### A trap when writing your own consumer

The NATS client deletes the JetStream consumer behind any subscription it
created, when that subscription is drained or unsubscribed — **including a
durable one**. A consumer that tidies up after itself with the obvious

```go
defer func() { _ = sub.Drain() }()
```

therefore destroys the acknowledgement floor it exists to keep, and every run
replays from the beginning. Worse, it can look like it works: the delete is
asynchronous, so a process that exits promptly often wins the race and the
durable survives by luck.

Close the connection instead and leave the consumer alone. Ephemeral consumers
are reaped by the server on their own once they go idle. Flush before closing,
though: acknowledgements are published without waiting for a reply, so the last
batch of them is still buffered in the client when the loop ends.
`docs/replay/consumer.go` does both.

## What replay does not do

- **It cannot recover a dropped event.** Pipelines run *before* publishing, so
  what is in JetStream is what the pipeline accepted. An event a `filter`,
  `dedup` or `threshold` middleware dropped was never published and does not
  exist to replay — `nephtys_events_dropped_by_pipeline_total` counts them, which
  is all that remains of them.
- **It does not re-run pipelines.** Replaying feeds the stored events to your
  consumer as they were published. Changing a stream's pipeline changes what is
  published from then on and nothing that came before.
- **Batched events replay as batches.** A stream with the `batch` middleware
  publishes one message per batch, with `type` suffixed `_batch` and `payload` as
  a JSON array of the batched payloads. A replay returns those envelopes, not the
  individual events — a consumer that reads both live and replayed data already
  handles this, since it is the same shape either way.
- **Binary events have no envelope.** Events whose `content_type` is not
  `application/json` are published as raw bytes carrying `Content-Type` and, when
  the source supplied sequencing, `X-Nephtys-Seq` headers. Read the headers; do
  not try to JSON-decode the body.
- **Removing a stream from Nephtys does not remove its events.** `DELETE
  /v1/streams/{id}` stops ingest and deletes the stored config. Everything it
  published stays in JetStream until retention expires it, and stays replayable
  from the subject it used.
- **It stops at the retention window.** See [What is
  retained](#what-is-retained): 72h by default, re-applied at every Nephtys
  start.

---

Related: [`LIFECYCLE.md`](LIFECYCLE.md) for what happens to a *stream* across a
restart, and the [Persistence](../README.md#persistence) section of the README
for how stream configurations survive one.
