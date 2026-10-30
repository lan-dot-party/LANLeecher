# API guide

LANLeecher has a JSON REST API under `/api/v1`. The OpenAPI 3.1 specification is the single source of
truth: it is served at `/api/openapi.json` (and `/api/openapi.yaml`), rendered at `/api/` and kept in
the repository at [`internal/api/openapi.yaml`](../internal/api/openapi.yaml). Tests call every
endpoint – including error cases – and validate status code, content type and body against the
specification, so code and documentation cannot drift apart.

## Authentication

Admins create tokens in the web UI under **Admin → API tokens**. A token is shown exactly once.

```
Authorization: Bearer llk_3Fq9…
```

- The prefix `llk_` makes tokens easy to spot in logs and secret scanners.
- Only a SHA-256 hash is stored. Tokens can expire, be limited to IP addresses or networks
  (`10.0.0.0/24 192.168.1.5`) and be revoked at any time.
- A logged-in browser session may call the API as well; `POST` requests then need the header
  `X-CSRF-Token` with the session's token.
- `/health` needs no authentication and returns `ok`.

### Scopes

| Scope | Grants |
| --- | --- |
| `read:rankings` | rankings, events, metrics – names only, no MACs or IPs |
| `read:clients` | client details: MAC, IP, hostname, histories, ports; MAC/IP/hostname fields in rankings |
| `write:events` | start and stop events |
| `read:board` | the board endpoints only – for bots and second screens |

Logged-in users have `read:*` as viewers and `write:events` from operator upwards.

## Conventions

- Bytes are integers, throughput is bit/s, timestamps are RFC 3339 in UTC.
- `bytes_down` is traffic *to* the client, `bytes_up` traffic *from* the client.
- MAC addresses are accepted in any notation (`AA-BB-CC-DD-EE-FF`, `aabb.ccdd.eeff`, …) and returned
  as `aa:bb:cc:dd:ee:ff`.
- With `read:clients`, devices carry `vendor`: the IEEE OUI vendor of the MAC, empty for randomized
  (locally administered) addresses.
- `?event=` takes an event slug, `active` (default) or – for rankings – `all`.
- Hidden ports and devices are left out unless `include_hidden=true`; then they carry `"hidden": true`.

## Endpoints

| Method & path | Scope | Purpose |
| --- | --- | --- |
| `GET /api/v1/rankings/{download\|upload\|total}` | read:rankings | Ranking; `event`, `limit` (1–500), `include_hidden`, `switch`, `from`, `to` |
| `GET /api/v1/events` | read:rankings | Events, newest first; `q` searches name and slug |
| `GET /api/v1/events/{slug}` | read:rankings | Event with totals |
| `POST /api/v1/events` | write:events | Start an event (stops the running one) |
| `POST /api/v1/events/{slug}/stop` | write:events | Stop the running event |
| `GET /api/v1/schedules` | read:rankings | Planned events (`all=true` includes finished ones) |
| `POST /api/v1/schedules` | write:events | Plan an event: `name`, `starts_at`, `ends_at` (exclusive), `notes` |
| `DELETE /api/v1/schedules/{id}` | write:events | Delete a planned event |
| `GET /api/v1/clients/search?q=` | read:clients | Search by partial MAC, IP or hostname |
| `GET /api/v1/clients/mac/{mac}` | read:clients | Totals per event (`events=N`), IP and hostname history, ports |
| `GET /api/v1/clients/mac/{mac}/timeseries` | read:clients | 5-minute buckets of one event |
| `GET /api/v1/clients/ip/{ip}` | read:clients | MACs that had this IP |
| `GET /api/v1/clients/hostname/{name}` | read:clients | MACs that had this hostname |
| `GET /api/v1/ports` | read:clients | Traffic of every port in an event, incl. uplinks and unassigned traffic |
| `GET /api/v1/board` | read:board | Beamer content; `limit` 10/20/50, `event` |
| `GET /api/v1/board/stream` | read:board | The same as Server-Sent Events after every poll |
| `GET /api/v1/metrics` | read:rankings | Prometheus metrics |

With `board.enabled: true` and `board.public: true` in the configuration, the board endpoints also
work without a token.

## Examples

Top 10 downloaders of the running event:

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  "https://lanleecher.example/api/v1/rankings/download?limit=10"
```

```json
{
  "event": {"name": "Basement LAN #12", "slug": "basement-lan-12", "started_at": "2026-11-13T16:00:00Z",
            "ended_at": null, "active": true, "notes": ""},
  "order": "download",
  "entries": [
    {"rank": 1, "type": "mac", "id": 17, "name": "DESKTOP-FRAG42", "bytes_down": 412000000000, "bytes_up": 18000000000},
    {"rank": 2, "type": "port_group", "id": 52, "name": "Table 7", "bytes_down": 98000000000, "bytes_up": 61000000000}
  ]
}
```

Upload ranking across all events, hidden entries included:

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  "https://lanleecher.example/api/v1/rankings/upload?event=all&limit=50&include_hidden=true"
```

History of a device over the last five LANs:

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  "https://lanleecher.example/api/v1/clients/mac/02-00-00-00-01-07?events=5"
```

Start an event from a script:

```bash
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name": "Basement LAN #13"}' https://lanleecher.example/api/v1/events
```

Plan an event from 15 to 18 April inclusive (end is exclusive, so midnight after the last day):

```bash
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name": "Basement LAN #13", "starts_at": "2027-04-15T00:00:00+02:00", "ends_at": "2027-04-19T00:00:00+02:00"}' \
  https://lanleecher.example/api/v1/schedules
```

Follow the board live:

```bash
curl -sN -H "Authorization: Bearer $TOKEN" https://lanleecher.example/api/v1/board/stream
```

```
event: board
data: {"event":{"name":"Basement LAN #12",…},"totals":{…},"top_download":[…],"top_upload":[…]}
```

A comment line `: ping` is sent every 25 seconds to keep proxies from closing the connection.
More examples (Discord, Grafana, Python, Go) are in [`examples/`](../examples/).

## Errors

Errors use RFC 9457 `application/problem+json`:

```json
{"type": "urn:lanleecher:problem:insufficient_scope", "title": "Forbidden", "status": 403,
 "detail": "this endpoint needs scope read:clients", "code": "insufficient_scope"}
```

The `code` is stable and meant for programs:

| Code | Status | Meaning |
| --- | --- | --- |
| `unauthorized` | 401 | no credentials sent |
| `invalid_token` | 401 | token unknown, revoked, expired or malformed |
| `insufficient_scope` | 403 | the token lacks the scope |
| `ip_not_allowed` | 403 | client IP not in the token's allowlist |
| `csrf` | 403 | session request without valid `X-CSRF-Token` |
| `rate_limited` | 429 | too many requests; see `Retry-After` |
| `invalid_parameter`, `invalid_body`, `invalid_mac`, `invalid_ip` | 400 | bad input |
| `no_active_event`, `event_not_found`, `mac_not_found`, `not_found` | 404 | nothing there |
| `event_not_active` | 409 | stopping an event that is not running |
| `internal` | 500 | see the server log |

## Rate limits

Each token (and each logged-in user) may send `api.rate_limit` requests per second with bursts of
`api.rate_burst` (defaults 10 and 30). Above that the API answers `429` with `Retry-After: 1`.

## Prometheus metrics

`GET /api/v1/metrics` (scope `read:rankings`, can be disabled with `api.metrics_enabled: false`):

| Metric | Labels | Meaning |
| --- | --- | --- |
| `lanleecher_throughput_bits_per_second` | `direction` | current throughput of all non-uplink ports |
| `lanleecher_event_bytes_total` | `event`, `direction` | traffic of the running event |
| `lanleecher_event_clients` | `event` | distinct MACs with traffic in the running event |
| `lanleecher_switch_up` | `switch` | 1 if the last poll of the switch succeeded |

Prometheus scrape configuration with a token:

```yaml
scrape_configs:
  - job_name: lanleecher
    metrics_path: /api/v1/metrics
    scheme: https
    authorization:
      credentials: llk_…
    static_configs:
      - targets: ["lanleecher.example"]
```

## Versioning

`/api/v1` stays compatible: fields and endpoints may be added, never removed or changed in meaning.
Breaking changes would come as `/api/v2`, announced in the [changelog](../CHANGELOG.md).
