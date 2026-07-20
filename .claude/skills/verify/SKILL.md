---
name: verify
description: Build, run, and drive Daywatch end-to-end against a throwaway Postgres to verify changes at the real surfaces (TCP ingest + web panel).
---

# Verifying Daywatch changes

Surfaces: TCP ingest on `DW_INGEST_ADDR` (Nightwatch agent wire protocol) and the
HTML panel on `DW_HTTP_ADDR`. Drive both; unit tests only cover protocol framing.

## Throwaway environment (never touch the user's running stack)

The user usually has a live Daywatch (compose stack + a `daywatch` process in tmux,
ports 2407/8807/8080). Always use fresh ports and a disposable database:

```bash
docker run -d --name dw-verify-pg -e POSTGRES_USER=dw -e POSTGRES_PASSWORD=dw \
  -e POSTGRES_DB=dw -p 127.0.0.1:15544:5432 postgres:16-alpine
DATABASE_URL=postgres://dw:dw@127.0.0.1:15544/dw \
  DW_INGEST_ADDR=127.0.0.1:12407 DW_HTTP_ADDR=127.0.0.1:18080 \
  go run ./cmd/daywatch   # run in background; logs to stderr
# cleanup: kill the /tmp/go-build*/exe/daywatch PID, then: docker rm -f dw-verify-pg
```

Startup logs to expect: "database ready", "ingest listening", "web panel
listening", "rollup backfill complete". With no apps registered any token is
accepted (open ingest) — convenient for tests.

## Feeding records (real wire protocol)

Frame: `{len}:v1:{tokenHash}:{json-array}` where len covers `v1:{hash}:{payload}`.
Server acks `2:OK` and closes. Any 7-char hash works in open-ingest mode.

```python
import socket, json, time
def send(records, host="127.0.0.1", port=12407):
    body = f"v1:abcdef0:{json.dumps(records)}"
    s = socket.create_connection((host, port)); s.sendall(f"{len(body)}:{body}".encode())
    ack = s.recv(16); s.close(); return ack   # expect b"2:OK"

send([{"t":"request","timestamp":time.time(),"trace_id":"tr1","_group":"g1",
       "duration":12000,"status_code":"200","method":"GET","url":"/x","route_path":"/x",
       "execution_stage":"production"}])
```

Useful fields: `t` (record type), `_group` (grouping hash), `execution_stage`
(env/stage), `user` (user id), durations are **microseconds**. Exceptions:
`class`, `message`, `handled`, `file`, `line`, `trace` (JSON string).

## Driving the panel

Plain curl + grep on the HTML works well (no JS needed for data assertions):
- `/` dashboard, `/section/requests`, `/exceptions`, `/users`, `/trace/{id}`
- Scope params: `?app=`, `?stage=`, `?range=1h|24h|7d|30d`
- `range=7d`/`30d` exercises the **rollups** query path (hourly pre-aggregates);
  shorter ranges aggregate raw records live. Rollup backfill runs at startup,
  ticker every 5 min — restart the app to force a re-rollup after ingesting.

## Gotchas

- `pkill -f "go run ./cmd/daywatch"` kills your own shell (pattern matches the
  zsh -c wrapper) → exit 144. Get PIDs with `pgrep -f "exe/daywatch"` in one
  command, `kill <pid>` in the next.
- Trace IDs render truncated (8 chars) in tables — grep for route paths or
  messages instead.
- Migrations run at startup with a 60s retry loop; to test a migration, pre-create
  the old-format table in psql before first launch.
