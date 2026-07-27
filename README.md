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

Every record keeps its full raw payload (JSONB) and is linked by `trace_id`: the trace page
renders an APM-style **waterfall** — every query, cache event, outgoing request, and log
positioned on the request's timeline, so gaps and slow spans are visible at a glance. A
**Users** page aggregates per-user activity (requests, errors, last seen) with a per-user
activity feed that can be narrowed to one record type, and colour-codes and syntax-highlights
each entry by what it is — SQL, an HTTP verb and path, or an exception class and message.

Records carry only a numeric user id, so wherever one is shown — the requests list, a record's
detail page, the activity feed — Daywatch resolves it against that user's most recent `user`
record and displays the name alongside the id (`مریم کریمی #42`), linked to their page. The id
always stays visible, since that is what the raw payload contains.

Long-range charts (7d/30d) are served from **hourly rollups** maintained in the background,
so they stay fast regardless of traffic volume and survive raw-record pruning
(`DW_ROLLUP_DAYS`, default 90).

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
cp .env.example .env          # set DAYWATCH_USERNAME, DAYWATCH_PASSWORD
docker compose up -d --build
```

- Web panel: http://localhost:8080
- Ingest: TCP port 2407

## Configure your Laravel app

Install the official package in your Laravel project (Laravel 10+, PHP 8.2+):

```bash
composer require laravel/nightwatch
```

Then create an app on Daywatch's **Apps** page (**Create app + token**) and point your
Laravel `.env` at Daywatch with the generated token:

```dotenv
NIGHTWATCH_TOKEN=token-from-the-apps-page
NIGHTWATCH_INGEST_URI=daywatch-host:2407
```

(Alternatively, setting `NIGHTWATCH_TOKEN` on the Daywatch side before first boot seeds an
app named `default` with that token — see [Multiple apps](#multiple-apps).)

- Same Docker network: `NIGHTWATCH_INGEST_URI=daywatch:2407`
- Laravel in Docker, Daywatch on the same host: `NIGHTWATCH_INGEST_URI=host.docker.internal:2407`
- Daywatch on another machine: `NIGHTWATCH_INGEST_URI=<hostname-or-lan-ip>:2407`
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
> neither `NIGHTWATCH_TOKEN` nor `DW_APPS` is set on the Daywatch side, any token is
> accepted (fine for local dev; don't do it in production).

## Multiple apps

One Daywatch instance can monitor several Laravel apps, managed entirely from the
panel's **Apps** page:

1. Enter a name (e.g. `shop`) and click **Create app + token** — Daywatch generates a
   random ingest token and stores the app in Postgres.
2. Copy the token into that Laravel app's `.env` as `NIGHTWATCH_TOKEN`.
3. Traffic appears immediately — token hashes are resolved against the database on every
   frame, so **no restart is needed** for new apps, token rotations, or deletions.

The Apps page shows per-app ingest totals and last-seen times, and offers **rotate**
(generate a new token; the old one stops working at once) and **delete** (the token is
revoked; already-stored records are kept under "All apps"). Once at least one app is
registered, unknown tokens are rejected.

The **Ingested** column is served from the hourly rollups plus the last two hours of raw
records, so the page stays fast on large databases. It therefore counts everything an app
has ever sent within the rollup window (`DW_ROLLUP_DAYS`), including records already pruned
from storage — not the number of rows currently held.

An **app switcher** in the top bar (All apps | shop | blog | …) scopes every dashboard,
chart, section, and exception view to the selected app, and alert rules can target one
app or all of them.

## Environments

A **stage switcher** appears next to the app switcher once records from more than one
execution stage (the package's `NIGHTWATCH_STAGE` / `execution_stage`, e.g. `production`
vs `staging`) have been seen. It scopes every view the same way the app switcher does,
and combines with it — e.g. app `shop`, stage `production`. Hourly chart rollups are
kept per stage, so long-range charts stay correct under the filter. Records ingested
before this feature (or without a stage) only appear under "All stages".

Env variables still work as a **first-boot seed**: `NIGHTWATCH_TOKEN` registers an app
named `default`, and `DW_APPS=shop:token-a,blog:token-b` registers several. They are
inserted only if the name is free — after that, the panel is the source of truth.

## Search

Every section page has a search box. It matches each record's **summary** — the most
descriptive field of its payload (`METHOD url` for requests, the SQL for queries, the
message for exceptions and logs, the job or command name), extracted on ingest into an
indexed column. Daywatch creates a `pg_trgm` GIN index over it at startup; if the
extension is unavailable the search still works, just without index support.

Tick **deep** next to the box to search the entire raw payload instead — any field of any
record, including ones the summary doesn't cover. Nothing can index that (every matching
row's JSONB is cast to text), so keep the time range tight when you use it.

Records ingested before this feature get their summary from a batched backfill that runs
at startup and reports `summary backfill complete` when done; ingest keeps running
throughout.

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
- SQL queries, PHP stack-trace snippets, and JSON payloads are **syntax highlighted**
  with a built-in highlighter (no external assets, works offline).

## Alerting

The **Alerts** page lets you create threshold rules evaluated every 30 seconds against
incoming records, e.g. *"≥5 error requests in 5 minutes"*:

- **Condition**: either a *count threshold* — record type (or any), severity class
  (errors / warnings / any), threshold count, sliding window — or *new exception appears*,
  which fires when an exception group is seen for the very first time.
- **Channel**: a webhook URL with a format — `json` (generic), `slack`, `discord`,
  `telegram` (needs a chat ID; point the URL at `https://api.telegram.org/bot<TOKEN>/sendMessage`),
  or `ntfy` (see below).
- **Cooldown** silences a rule after it fires so a sustained incident doesn't spam you.
  It runs from the last *delivered* notification: if the channel is unreachable the alert
  reached nobody, so the rule is retried on the next evaluation instead of going quiet.
  The first failure is retried on the next evaluation, and further consecutive failures
  back off (30s, 1m, 2m, … up to the cooldown), so an endpoint that is down for good is
  not hammered every 30 seconds.
- Every firing is recorded in the history table with its delivery status; a **test** button
  sends a `[TEST]` notification immediately to verify the wiring.
- **Edit** reopens a rule in the same form. Its firing history and paused/active state are
  kept, so an edited rule stays within its current cooldown. Stored channel passwords are
  never rendered back into the page: leave the password field blank to keep the saved one,
  or tick **clear credentials** to remove it.

Set `DW_BASE_URL` (e.g. `https://daywatch.example.com`) to include a panel link in
notifications.

### ntfy

Point the URL at the topic on your own server (or ntfy.sh), e.g.
`https://ntfy.example.com/daywatch`. The message is published as the request body, with
the rule name and app as the notification title, `high` priority for error rules, and
`DW_BASE_URL` as the click action.

Protected servers are supported through the **Username / Password** fields on the rule:

- both set → HTTP basic auth (`auth-user`/`auth-pass` on the topic's ACL);
- password only → sent as `Authorization: Bearer <token>`, for ntfy access tokens (`tk_…`).

Credentials are stored in Postgres in plaintext, so prefer a per-topic user or a scoped
access token over an admin account. The fields also apply to the generic `json` format,
for self-hosted webhooks behind basic auth.

## Panel authentication

Set `DAYWATCH_USERNAME` and `DAYWATCH_PASSWORD` to put the panel behind a login. Sessions
are JWTs (HS256) stored in an HttpOnly cookie, valid for 7 days. The signing secret is
derived deterministically from the credentials so sessions survive restarts; set
`DW_JWT_SECRET` to control it explicitly (rotating it logs everyone out). Leaving both
credentials empty runs the panel without a login (a warning is logged). The TCP ingest
port is unaffected — it authenticates via the Nightwatch token hash as always.

## Running in production

Put the panel behind a TLS-terminating reverse proxy and forward the protocol header —
the session cookie gets its `Secure` flag automatically when the request arrives over
HTTPS (`X-Forwarded-Proto: https`):

```caddy
daywatch.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

```nginx
server {
    listen 443 ssl;
    server_name daywatch.example.com;
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header X-Forwarded-Proto https;
        # live updates use SSE:
        proxy_buffering off;
        proxy_read_timeout 1h;
    }
}
```

The TCP ingest port (2407) is app-to-Daywatch traffic authenticated by token hash; expose
it to your app servers only (private network / firewall), not the public internet.

`GET /healthz` (no auth) returns `200 ok` only when Postgres answers within 2 seconds, and
`503` otherwise — the container `HEALTHCHECK` uses it, so a Daywatch that is listening but
cannot reach its database is reported unhealthy instead of quietly serving errors.

Daywatch acknowledges a frame before storing its records, matching the official agent, so
on `SIGTERM` it stops accepting connections and **drains in-flight batches** before
exiting rather than dropping records the app was told had landed. Give it room to finish:
the compose file sets `stop_grace_period: 40s`, and any other supervisor (systemd's
`TimeoutStopSec`, Kubernetes' `terminationGracePeriodSeconds`) should allow the same.

Login attempts are rate-limited per IP (5 failures / 15 minutes → temporary lockout) on
top of a constant-time credential check. Note that app ingest tokens are stored in
Postgres in plain text (the panel shows them for copy/paste), so protect the database
accordingly.

## Configuration

All settings are environment variables (see `.env.example` for the compose-level ones):

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | — (required) | Postgres connection string |
| `NIGHTWATCH_TOKEN` | empty | Seeds app `default` on first boot (apps are DB-managed via the panel) |
| `DW_APPS` | empty | Seeds multiple apps on first boot: `name:token,name2:token2` |
| `DAYWATCH_USERNAME` / `DAYWATCH_PASSWORD` | empty | Panel login; both empty disables auth |
| `DW_JWT_SECRET` | derived | Explicit JWT signing secret |
| `DW_BASE_URL` | empty | Public panel URL included in alert notifications |
| `DW_INGEST_ADDR` | `:2407` | TCP ingest bind address |
| `DW_HTTP_ADDR` | `:8080` | Web panel bind address |
| `DW_RETENTION_DAYS` | `14` | Prune records older than N days (0 = keep forever) |
| `DW_ROLLUP_DAYS` | `90` | Keep hourly chart rollups for N days (0 = forever) |
| `DW_INGEST_PORT` / `DW_HTTP_PORT` | `2407` / `8080` | Host ports published by docker compose |

## Development

```bash
go test -race ./...                            # protocol framing, alerts, search, panel helpers
gofmt -l . && go vet ./...
DATABASE_URL=postgres://... go run ./cmd/daywatch
```

CI runs exactly those three (`.github/workflows/go-checks.yml`), and both the GHCR image
build and the release job depend on them, so a failing test blocks the artifact.

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
