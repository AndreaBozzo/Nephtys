# Contributing to Nephtys

Thanks for considering a contribution. This document is the short version of how
the project is built and what a change is expected to carry with it.

## Development setup

**Prerequisites:** Go 1.25+, Docker (for NATS), and `curl`. `golangci-lint` if
you want to run locally what CI runs.

```bash
git clone https://github.com/AndreaBozzo/Nephtys.git
cd Nephtys

make nats-up            # NATS with JetStream — all the inner loop needs
cp .env.example .env    # `make run` exports this; the binary reads only the environment
make run                # Nephtys on :3002, against NATS on :4222
```

Set `NEPHTYS_ADMIN_TOKEN` in `.env` before touching anything under
`/v1/streams`: with no token those routes answer `403`, by design.

`make docker-up` starts Prometheus and a provisioned Grafana as well, which is
what you want when working on metrics or the dashboard and unnecessary
otherwise.

In a second terminal, prove the whole path works:

```bash
make smoke   # registers a webhook stream, posts one event, reads it back
```

That is the fastest answer to "is my instance actually working": it exercises
HTTP in, the pipeline, and a published JetStream event without depending on any
external endpoint. It needs `make run` in the first terminal and
`NEPHTYS_ADMIN_TOKEN` set in `.env`.

## The loop

```bash
make all             # fmt + vet + test — run this before every commit
make test            # go test -race ./...
make check-examples  # every docs/examples/*.json must pass --config-check
make lint            # golangci-lint, as CI runs it
```

CI runs `gofmt -l`, `go vet`, `golangci-lint`, `make check-examples`, and the
race-enabled test suite, on every pull request. A PR that fails any of them will
not be reviewable, so run `make all` first.

### Windows

`.gitattributes` pins the working tree to LF everywhere, so `gofmt -l .` is
trustworthy. A checkout made before that file existed still has CRLF files; on a
clean tree, `git rm -r --cached . && git reset --hard` fixes it. If `gofmt`
suddenly reports every file as unformatted, that is what happened.

## What a change carries

- **Generic-first, and it is not negotiable.** No source-specific logic lives in
  Nephtys — no venue, vendor, or protocol-flavour helpers. Every feature has to
  make sense to a consumer who is not doing what you are doing. Source-specific
  handling belongs in the consumer.
- **The API surface evolves additively.** JSON fields on the config structs in
  `internal/domain` and on REST responses are a contract: add optional fields,
  never rename or remove one.
- **New config fields are validated and exemplified.** `--config-check` must
  reject a malformed value rather than silently defaulting it, and
  `docs/examples/` gets a runnable example that passes `make check-examples`.
- **Tests that can fail.** A regression test that passes against the unfixed
  code guards nothing — write the test, watch it fail, then fix.
- **`CHANGELOG.md`**, in Keep a Changelog format, for anything user-visible.
  Say what changed and why it changed; the changelog is where the reasoning
  lives.

`docs/LIFECYCLE.md` is the reference for how a stream is admitted, supervised,
restarted, and retired — read it before changing the manager, the supervisor, or
a connector's `Open`/`Run`/`Close`.

## Architecture in one paragraph

`cmd/nephtys` wires everything. `internal/connector` holds one file per source
kind implementing `StreamSource`: `Open` acquires local resources and never
touches the network, `Run` serves one session and returns, `Close` releases.
Connectors do not retry and hold no status — the manager's supervisor owns both.
`internal/pipeline` is the middleware chain (filter, transform, dedup, enrich,
threshold, batch) over opaque payloads. `internal/server` is the REST API and
the stream manager. `internal/broker` publishes to JetStream, `internal/store`
persists stream configs in a JetStream KV bucket, and `internal/telemetry` holds
the per-stream Prometheus metrics.

## Pull requests

1. Branch from `main`.
2. Make the change, with tests and a changelog entry.
3. `make all` and `make check-examples`.
4. Open the PR against `main`, describing what the change does and why the
   alternative was rejected. Reference the issue it closes.

Commit subjects follow the repository's existing style:
`type(scope): summary` — for example `fix(server): ...`, `feat(connector): ...`,
`docs: ...`.

## Where help is most useful

Look at the issues on the [project board](https://github.com/users/AndreaBozzo/projects/4)
first — the roadmap in [`ROADMAP.md`](ROADMAP.md) says which outcomes are
currently in play, and issues carry the acceptance criteria. Documentation,
reproducible examples, and tests are always welcome, and are the easiest place
to start.

## Questions

Open an issue. For anything security-related, see [`SECURITY.md`](SECURITY.md)
rather than filing publicly.

## Code of conduct

Be respectful and constructive. See [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md).
