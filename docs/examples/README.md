# Example stream configurations

Minimal, runnable examples that exercise different connector types. Each file is a JSON payload you can `POST` to `/v1/streams` to register the stream.

| File | Connector | Source | Why it's here |
|---|---|---|---|
| [`sensor_rest_poller.json`](sensor_rest_poller.json) | `rest_poller` | Open-Meteo public weather API | Canonical urban-sensor profile: low-rate JSON telemetry from a public open-data source. |
| [`sensor_sse.json`](sensor_sse.json) | `sse` | Wikimedia EventStreams | High-rate event stream that exercises the SSE connector against a real public endpoint. |
| [`crypto_websocket.json`](crypto_websocket.json) | `websocket` | Binance trade stream | Market-data profile, parity with the README example. |
| [`crypto_websocket_subscribe.json`](crypto_websocket_subscribe.json) | `websocket` | Coinbase Exchange ticker | Venue that requires a subscribe frame after connect: `on_connect_send` is sent verbatim after every handshake, including reconnects. |
| [`sensor_websocket_subscribe.json`](sensor_websocket_subscribe.json) | `websocket` | Illustrative IoT gateway | Sensor-side `on_connect_send` profile: an auth frame followed by a subscribe frame (list form, sent in order). |
| [`agent_telemetry_webhook.json`](agent_telemetry_webhook.json) | `webhook` | Your agents (inbound POST) | AI-agent telemetry profile: agents push observations/actions to a local webhook; Nephtys normalizes them onto the durable event spine for other agents (or dashboards) to consume. |

## Running an example

Assuming Nephtys is already running locally (`make run`):

```bash
# Pick an example and register it
curl -X POST http://localhost:3002/v1/streams \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $NEPHTYS_ADMIN_TOKEN" \
  -d @docs/examples/sensor_rest_poller.json

# Verify it's running
curl -H "Authorization: Bearer $NEPHTYS_ADMIN_TOKEN" \
  http://localhost:3002/v1/streams

# Tap the published events from NATS (separate terminal)
nats sub "nephtys.stream.>"
```

If `NEPHTYS_ADMIN_TOKEN` is unset, omit the `Authorization` header (auth is disabled).

## Notes on the public endpoints

- **Open-Meteo** is a free weather API and does not require an API key. The example polls a fixed coordinate (Bologna, Italy) every 60 seconds.
- **Wikimedia EventStreams** is a free public SSE endpoint emitting MediaWiki events. It can be high-volume — use the `dedup` or `batch` middleware in production. Note that `filter.match_types` matches the *connector-level* event type, not a field inside the payload: for SSE that is the `event:` frame name, which Wikimedia sets to `message` on every frame. Filtering this feed on the payload's own `type` values (`edit`, `new`, …) matches nothing and silently drops the entire stream.
- **Binance** is included as the trading-side reference. No API key is required for the public trade stream.
- **Coinbase Exchange** requires a subscribe message after connecting — the example uses `on_connect_send` for it. No API key is required for the public ticker channel.
- **IoT gateway** (`sensor_websocket_subscribe.json`) points at a placeholder host (`gateway.example.com`); swap in your gateway's URL, auth token, and channel names. It passes `--config-check` as-is but is not runnable without a real endpoint.

- **Agent telemetry** runs no external endpoint: Nephtys itself listens on `:3010` and agents `POST` JSON events to `/agent/events` with `Authorization: Bearer change-me` (rotate the token). Try it: `curl -X POST localhost:3010/agent/events -H "Authorization: Bearer change-me" -d '{"agent":"scout-1","observation":"queue_depth","value":42}'`.

These endpoints are illustrative. None are operated by the Nephtys project; consult the upstream provider's terms of use before relying on them in production.
