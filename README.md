# 🌞 Daywatch

A self-hosted, Nightwatch-compatible monitoring panel written in Go, backed by PostgreSQL.

Daywatch speaks the [Laravel Nightwatch](https://github.com/laravel/nightwatch) agent wire
protocol, so the official `laravel/nightwatch` package sends its telemetry straight to your
own server — no Nightwatch subscription, no code changes in your app beyond two `.env` lines.

It captures and visualizes every record type the Nightwatch package emits:

| Type | Panel section |
|---|---|
| `request` | Requests (with per-route stats, P95, 5xx counts) |
| `query` | Queries (most frequent / slowest) |
| `exception` | Exceptions (grouped, with stack traces) |
| `log` | Logs |
| `command` | Artisan commands |
| `queued-job` / `job-attempt` | Queue activity |
| `scheduled-task` | Scheduler runs |
| `cache-event` | Cache hits/misses |
| `outgoing-request` | Outgoing HTTP |
| `mail` / `notification` | Mail & notifications |
| `user` | Seen users |

Every record keeps its full raw payload (JSONB) and is linked by `trace_id`, so you can open
a request and see all queries, cache events, logs, and exceptions that happened inside it.

## How it works

```
Laravel app (laravel/nightwatch package)
        │  TCP  {len}:v1:{tokenHash}:{json records}   ← same protocol as the official agent
        ▼
Daywatch :2407  ──►  PostgreSQL  ──►  Web panel :8080
```

The `laravel/nightwatch` package normally talks to a local `nightwatch:agent` process, which
relays to Laravel's cloud. Daywatch implements that agent's listener protocol (payload `v1`,
`2:OK` acknowledgments, `PING` frames, xxh128 token-hash validation), so the package connects
to it directly. **You do not run `php artisan nightwatch:agent` at all.**

## Quick start (Docker)

```bash
cp .env.example .env          # set NIGHTWATCH_TOKEN, DAYWATCH_USERNAME, DAYWATCH_PASSWORD
docker compose up -d --build
```

- Web panel: http://localhost:8080
- Ingest: TCP port 2407

## Configure your Laravel app

Install the official package in your Laravel project (Laravel 10+, PHP 8.2+):

```bash
composer require laravel/nightwatch
```

Then point it at Daywatch in your app's `.env`:

```dotenv
NIGHTWATCH_TOKEN=the-same-secret-you-set-for-daywatch
NIGHTWATCH_INGEST_URI=daywatch-host:2407
```

- Same Docker network: `NIGHTWATCH_INGEST_URI=daywatch:2407`
- Laravel in Docker, Daywatch on host: `NIGHTWATCH_INGEST_URI=host.docker.internal:2407`
- Same machine, no Docker: `NIGHTWATCH_INGEST_URI=127.0.0.1:2407`

Hit a few routes in your app and open the Daywatch panel. That's it.

To also capture application **logs**, add the `nightwatch` channel (auto-registered by the
package) to your log stack:

```dotenv
LOG_CHANNEL=stack
LOG_STACK=single,nightwatch
```

> Tokens are never sent in plain text: the package transmits the first 7 hex chars of
> `xxh128(NIGHTWATCH_TOKEN)`, and Daywatch validates against the same hash. If
> `NIGHTWATCH_TOKEN` is unset on the Daywatch side, any token is accepted (fine for local
> dev; don't do it in production).

## Exception triage

The **Exceptions** page groups identical exceptions (by the package's `_group` hash) with
occurrence counts, first/last seen, and **Open / Resolved / Ignored** tabs:

- The detail view renders the full **stack trace** — application frames are highlighted
  and show the captured source snippet with the failing line marked; vendor frames are
  collapsed.
- **Resolve** an exception when you've fixed it: if it ever happens again it automatically
  reopens. **Ignore** silences a group permanently (new occurrences are still stored, the
  group just stays out of the open tab).
- Charts also plot **P95/P99** duration lines (dashed) next to the average, so latency
  tails are visible at a glance.

## Alerting

The **Alerts** page lets you create threshold rules evaluated every 30 seconds against
incoming records, e.g. *"≥5 error requests in 5 minutes"*:

- **Condition**: record type (or any), severity class (errors / warnings / any), threshold
  count, and sliding window.
- **Channel**: a webhook URL with a format — `json` (generic), `slack`, `discord`, or
  `telegram` (needs a chat ID; point the URL at `https://api.telegram.org/bot<TOKEN>/sendMessage`).
- **Cooldown** silences a rule after it fires so a sustained incident doesn't spam you.
- Every firing is recorded in the history table with its delivery status; a **test** button
  sends a `[TEST]` notification immediately to verify the wiring.

Set `DW_BASE_URL` (e.g. `https://daywatch.example.com`) to include a panel link in
notifications.

## Panel authentication

Set `DAYWATCH_USERNAME` and `DAYWATCH_PASSWORD` to put the panel behind a login. Sessions
are JWTs (HS256) stored in an HttpOnly cookie, valid for 7 days. The signing secret is
derived deterministically from the credentials so sessions survive restarts; set
`DW_JWT_SECRET` to control it explicitly (rotating it logs everyone out). Leaving both
credentials empty runs the panel without a login (a warning is logged). The TCP ingest
port is unaffected — it authenticates via the Nightwatch token hash as always.

## Configuration

All settings are environment variables (see `.env.example` for the compose-level ones):

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | — (required) | Postgres connection string |
| `NIGHTWATCH_TOKEN` | empty | Shared secret; empty accepts any token |
| `DAYWATCH_USERNAME` / `DAYWATCH_PASSWORD` | empty | Panel login; both empty disables auth |
| `DW_JWT_SECRET` | derived | Explicit JWT signing secret |
| `DW_BASE_URL` | empty | Public panel URL included in alert notifications |
| `DW_INGEST_ADDR` | `:2407` | TCP ingest bind address |
| `DW_HTTP_ADDR` | `:8080` | Web panel bind address |
| `DW_RETENTION_DAYS` | `14` | Prune records older than N days (0 = keep forever) |
| `DW_INGEST_PORT` / `DW_HTTP_PORT` | `2407` / `8080` | Host ports published by docker compose |

## Development

```bash
go test ./...                                  # unit tests (protocol framing, token hash)
DATABASE_URL=postgres://... go run ./cmd/daywatch
```

The repository layout:

```
cmd/daywatch/        entrypoint
internal/config/     env config + xxh128 token hash (matches PHP's hash('xxh128', ...))
internal/ingest/     TCP server implementing the Nightwatch agent protocol
internal/store/      Postgres schema, batch inserts (COPY), aggregate queries, pruning
internal/web/        embedded HTML panel (no external assets)
```

## Compatibility notes

- Payload version `v1`, as produced by `laravel/nightwatch` v1.x.
- Unknown/extra record fields are preserved verbatim in the `data` JSONB column, so panel
  features degrade gracefully if the package adds fields.
- Records with unknown `t` types are still stored and visible via trace/record views.
