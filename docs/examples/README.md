# Example stream configurations

Minimal, runnable examples that exercise different connector types. Each file is a JSON payload you can `POST` to `/v1/streams` to register the stream.

| File | Connector | Source | Why it's here |
|---|---|---|---|
| [`sensor_rest_poller.json`](sensor_rest_poller.json) | `rest_poller` | Open-Meteo public weather API | Canonical urban-sensor profile: low-rate JSON telemetry from a public open-data source. |
| [`sensor_sse.json`](sensor_sse.json) | `sse` | Wikimedia EventStreams | High-rate event stream that exercises the SSE connector against a real public endpoint. |
| [`crypto_websocket.json`](crypto_websocket.json) | `websocket` | Binance trade stream | Market-data profile, parity with the README example. |

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
- **Wikimedia EventStreams** is a free public SSE endpoint emitting MediaWiki events. It can be high-volume — use the `dedup` or `filter` middleware in production.
- **Binance** is included as the trading-side reference. No API key is required for the public trade stream.

These endpoints are illustrative. None are operated by the Nephtys project; consult the upstream provider's terms of use before relying on them in production.
