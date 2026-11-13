# Asana extractor

[![CI](https://github.com/Bl1tzz18/reco-tech-assignment/actions/workflows/ci.yml/badge.svg)](https://github.com/Bl1tzz18/reco-tech-assignment/actions/workflows/ci.yml)

Extracts **users** and **projects** from the [Asana REST API](https://developers.asana.com/docs)
and stores **each record in its own JSON file**, once or periodically (every 5 minutes or every
30 seconds). Go standard library only.

## Quick start

```sh
cp .env.example .env && chmod 600 .env   # put your Asana personal access token in .env
make once                                 # extract once -> data/<workspace>/{users,projects}/<gid>.json
make run                                  # extract every 5 minutes until Ctrl+C
make check                                # gofmt + vet + tests (race detector)
```

Requires Go 1.25+. `make` lists every target.

## Run

```sh
cp .env.example .env && chmod 600 .env  # then put your Asana personal access token in .env
go run . -once                          # extract once and exit
go run .                                # option 1: every 5 minutes (default), until Ctrl+C
go run . -interval 30s                  # option 2: every 30 seconds
go run . -users-interval 30s -projects-interval 5m   # per-resource intervals
go run . -workspace 1234567890          # only one workspace
go run . -log-level debug               # also log every request, page and limiter wait
go run . -log-format json -metrics-addr 127.0.0.1:9090   # production-style: JSON logs + health/metrics
```

## Configure

| Flag | Default | Meaning |
|---|---|---|
| `-base-url` | `https://app.asana.com/api/1.0` | API base URL |
| `-interval` | `5m` | Extraction interval for all resources (min `5s`) |
| `-users-interval` | `-interval` | Override for users |
| `-projects-interval` | `-interval` | Override for projects |
| `-run-timeout` | `10m` | Max duration of one sync run; past it the run is cancelled and counted as failed |
| `-once` | `false` | Extract once and exit |
| `-workspace` | (all) | Only extract this workspace gid |
| `-data-dir` | `data` | Output directory |
| `-rpm` | `150` | Client-side rate limit (Asana: 150/min free, 1500/min paid, per token) |
| `-max-retries` | `4` | Retries for 408 / 429 / 5xx / network errors |
| `-page-size` | `100` | Items per page (Asana max 100) |
| `-timeout` | `10s` | Per-request HTTP timeout |
| `-log-format` | `text` | `text` or `json` |
| `-log-level` | `info` | `debug`, `info`, `warn` or `error` |
| `-metrics-addr` | (off) | Serve `/healthz`, `/metrics`, `/debug/vars`, `/debug/pprof/` (e.g. `127.0.0.1:9090`) |

| `-env-file` | `.env` | Optional `KEY=VALUE` file, loaded at startup |

Env: `API_TOKEN` (required). Read from the real environment first, then from `.env`
(git-ignored, `chmod 600`). In production, inject it from a secrets manager as an env var; no file is needed.

## Output

```
data/
  <workspace_gid>/
    users/
      _meta.json            {"resource","workspace","fetched_at","count"}
      <user_gid>.json       {"gid","name","email"}
    projects/
      _meta.json
      <project_gid>.json    {"gid","name","archived","created_at","modified_at","owner","team","workspace","permalink_url"}
```

Validate a run:

```sh
jq . data/*/users/_meta.json
ls data/*/users/*.json | grep -v _meta | wc -l    # must equal "count"
jq . data/*/projects/<gid>.json                   # spot-check against Asana
```

## Design

```
main.go               flags + validation, slog, Ctrl+C/SIGTERM -> context cancel, jobs, -once
budget.go             estimated req/min of all jobs vs. -rpm (warns above 70%)
internal/model        typed User / Project / Workspace + the opt_fields they need
internal/client       HTTP: auth, shared rate limiter, retries, 429, pagination, FetchAllAs[T]
internal/storage      atomic per-record JSON files, skip-unchanged, prune deleted
internal/extractor    workspaces -> users / projects -> storage
internal/poller       Job{Name, Interval, Sync}: run now, then every interval
internal/obs          logging (context attrs, redaction), metrics, health/metrics/pprof endpoints
```

- **Periodic extraction**: two jobs (users, projects), each in its own goroutine with its own
  `time.Ticker`. A job runs immediately, then on every tick. Ticks missed during a slow run are dropped,
  so runs never pile up. A failed run is logged (WARN, then ERROR after 3 in a row, then "recovered")
  and retried on the next tick; a panic in one job is recovered and doesn't stop the other.
  Ctrl+C / SIGTERM cancels the context: in-flight requests abort and the process waits for both jobs.
- **One client for all jobs**, so both share one rate limiter (the limit is per token).

- **Auth**: `Authorization: Bearer $API_TOKEN`. The token is never logged or put in URLs.
- **Rate limits**: one token-bucket limiter per `Client`, shared by every request, because Asana
  limits per token. Requests are evenly spaced (burst 1), the safest choice against a per-minute window.
- **429**: honour `Retry-After` (seconds or HTTP-date, capped at 5m) and **pause the shared limiter**,
  so every request backs off, not just the one that got the 429.
- **Retries**: exponential backoff (0.5s, 1s, 2s… cap 30s) with jitter for 408, 5xx and network
  errors. Other 4xx (400/401/403/404) are not retried: retrying won't fix a bad token or a bad request.
- **Pagination**: Asana's `limit` + `next_page.offset`; guards against a repeated offset and endless paging.
- **Typed decode + validation**: only the fields we need. Records without a `gid` are skipped and counted;
  if *all* records are invalid the sync fails (likely a schema change).
- **Storage**: temp file in the same dir + `rename`, so readers never see half a file.
  Unchanged records aren't rewritten (cheap at thousands of records). Files of deleted records are
  pruned, **only after the whole fetch and every write succeeded**: a failed or partial fetch never
  overwrites or deletes good data. `_meta.json` is written last, so `fetched_at` matches complete data.
- **Path safety**: file names come from API data, so every path segment must match `[A-Za-z0-9_-]`.
- **Errors**: wrapped with context (`sync projects of workspace X: fetch /projects page 3: ...`);
  one failing workspace or resource doesn't stop the others.

## Test

```sh
make check   # gofmt + go vet + go test -race ./...   (CI also runs golangci-lint and govulncheck)
make cover   # go test -coverpkg=./... ... -> 87%
```

Use `-coverpkg=./...`: without it Go only counts a package's own tests and under-reports code that
other packages' tests exercise. No test calls the real API: everything runs against `httptest` fakes,
a fake clock (client) or `testing/synctest` (poller), so the suite is fast and deterministic.

| Area | What is tested |
|---|---|
| End to end (`run`) | `-once` extracts every page of users and projects into files and metrics; a failing projects job still stores users; token from env or `.env`, error without it; periodic mode runs both jobs immediately and shuts down cleanly on cancel; observability server serves `/healthz`, `/metrics`, `/debug/vars`, fails on a port in use, stops cleanly |
| Client | auth header, offset pagination, repeated offset, 429 + `Retry-After` (seconds and date), a 429 pausing other requests, 5xx retry with backoff, give up after max retries, no retry on 401, network errors, HTML body, cancelled context, limiter spacing, invalid items, metrics, token never logged |
| Extractor | one file per record, pruning deleted records, old data kept when the API fails, several workspaces with one failing (the others still complete), pagination through the extractor, `-workspace` skips discovery |
| Model | decoding **real (anonymised) Asana responses** in `internal/model/testdata/`, null owner/team, unknown fields, required `gid` |
| Storage | pretty JSON, no `.tmp` left behind, unchanged files skipped, pruning keeps metadata, unsafe names rejected |
| Poller | exact run times on a fake clock, no pile-up, hung run cancelled by its timeout, panics and errors survived, shutdown is not a failure, no goroutine leaks, job metrics |
| Observability | context attributes on every log, secret redaction, readable JSON durations, counters, `ok`/`failing`/`stale`/`stuck` health, `/healthz` 200/503 |
| Config | flag defaults, interval and logging flags, every invalid flag reported at once, rate-budget warning, `.env` parsing |

Not covered (low risk): `main()` itself (only `os.Exit` around `run`), disk-full/permission errors in
storage, jitter randomness.

Demo of both intervals against the real API (users every 5s, projects every 15s, Ctrl+C after ~20s):

```sh
go run . -users-interval 5s -projects-interval 15s
```

## Observability

**Logs** (`log/slog`, text or JSON). Every line of a periodic run carries `job` and `run`, set once on
the context by the poller and added automatically by the handler, so you can filter one run end to end
(poller → extractor → HTTP client):

```json
{"level":"INFO","msg":"sync ok","resource":"users","workspace":"1200000000000001","items":5,"written":0,"unchanged":5,"removed":0,"took":"739ms","job":"users","run":2}
```

| Level | What |
|---|---|
| DEBUG | every HTTP request (method, path, status, bytes, duration; never headers), every page, limiter waits, rate budget |
| INFO | startup config + version (git revision), `sync ok` per workspace/resource, `run ok`, shutdown, metrics summary |
| WARN | retries, 429s, a failed run (1-2 in a row), a run slower than its interval, rate budget exceeded, skipped invalid records |
| ERROR | a job failing 3+ times in a row (alert on this), fatal startup errors |

Secrets: the token is never logged. As a safety net, attributes named `token`, `authorization`,
`password`, `secret` are redacted by the handler. A test checks that the token never appears in debug logs.

**Metrics** (`-metrics-addr`, stdlib `expvar`, JSON):

| Endpoint | Content |
|---|---|
| `GET /healthz` | `200 {"status":"ok","jobs":{"users":"ok","projects":"failing"}}`; **503** when a job is `stuck` (a run is in progress past `-run-timeout`) or `stale` (no success for 3 intervals). Use it as the liveness probe |
| `GET /metrics` | `http_requests_by_status`, `http_retries_total`, `http_rate_limited_total`, `http_request_ms_total`, `rate_limiter_wait_ms_total`, `goroutines`, per job (`runs`, `failures`, `consecutive_failures`, `running_since`, `last_success`, `last_duration_ms`, `last_error`), per workspace/resource (`items`, `written`, `unchanged`, `removed`, `fetched_at`), `uptime_s` |
| `GET /debug/vars` | the same + Go runtime `memstats` |
| `GET /debug/pprof/` | CPU / heap / goroutine profiles |

Bind it to localhost or a private network: pprof exposes internals.
On exit the headline counters are also logged (`metrics summary`), so even `-once` runs leave a record.

Useful signals: `rate_limiter_wait_ms_total` growing fast = the intervals are too aggressive for `-rpm`;
any `429` = the limiter is set above the real limit (or another client shares the token);
`consecutive_failures >= 3` or `/healthz` 503 = page someone.

## Keeping the goroutines healthy

There are three long-lived goroutines: one per job (users, projects) and the observability server.

| Risk | Guard |
|---|---|
| A job panics | `RunOnce` recovers it into an error; the job and the other job keep running |
| A run hangs | every run gets `context.WithTimeout(-run-timeout)`; every wait (HTTP 10s timeout, limiter, retry backoff, `Retry-After` capped at 5m) is bounded and ctx-aware |
| A goroutine is stuck anyway | `/healthz` reports `stuck` (run in progress past the timeout) → 503 |
| A job silently stops or keeps failing | `/healthz` reports `stale` after 3 intervals without success → 503; WARN → ERROR after 3 consecutive failures |
| Slow runs pile up | `time.Ticker` drops missed ticks: runs are back to back, never concurrent |
| Data races | shared state is behind a mutex (limiter, metrics) or atomic (`expvar`); the jobs write to different dirs; `go test -race` |
| Goroutine leaks | `goroutines` gauge in `/metrics`; poller tests run in `testing/synctest` bubbles, which fail if a goroutine is left behind |
| Shutdown | Ctrl+C/SIGTERM cancels the context, and a pending tick can never start a new run after that; `wg.Wait()` waits for both jobs |
| Truly unexpected crash | let the process die and be restarted by its supervisor (systemd `Restart=always`, Kubernetes `livenessProbe: httpGet /healthz`); self-healing inside the process is less reliable than a restart |

The timing tests use `testing/synctest` (fake clock): runs start at exactly 0s, 30s, 60s, 90s; 45s runs on a
30s interval start at 0s, 45s, 90s (no pile-up); a hung run is cancelled at its timeout and the next run
still starts on time; shutdown during a run is not counted as a failure.

## Scale: thousands of projects and employees

Requests per full sync = `ceil(records / 100)` pages per resource and workspace (+1 for `/workspaces`).

| Records (users + projects) | Requests / sync | Every 5m | Every 30s | Fits 150 rpm (free) at 70%? |
|---|---|---|---|---|
| 5 + 3 (this account) | 4 | 0.8/min | 8/min | yes |
| 5k + 5k | ~102 | ~20/min | ~204/min | 5m yes, 30s **no** (needs paid: 1500 rpm) |
| 50k + 50k | ~1002 | ~200/min | ~2000/min | 5m only on paid, 30s **no** |

What the code already does about it:
- **The shared limiter never exceeds `-rpm`**, whatever the intervals: syncs just take longer.
  `budget.go` warns when the configured intervals need more than 70% of `-rpm`, and the poller warns
  when a run takes longer than its interval.
- 100 items per page (the maximum) is the fewest requests possible.
- Unchanged records aren't rewritten, so disk I/O scales with *changes*, not with total records.
- Per-resource intervals, e.g. fast-changing data more often than slow-changing data.

What I'd add next, in order:
1. **Incremental sync**: the Asana **Events API** (`GET /events?resource=<gid>&sync=<token>`) returns only
   what changed since the last sync token. That makes a 30s interval cheap at any size. Keep a full resync
   (e.g. hourly or daily) to catch anything missed (sync tokens expire, which returns a 412).
2. **Stream page by page** instead of holding all records in memory (needed at ~millions of records).
3. **Bounded parallelism across workspaces/teams** (Asana allows 50 concurrent GETs), still behind the
   shared limiter; useful on paid plans where the rpm budget is much larger than one sequential stream uses.
4. **Webhooks** for near real-time changes, with polling as the fallback.
5. **Multi-tenant**: one client and limiter per customer token; jobs distributed over workers (queue),
   storage in S3/a database instead of local files.

## Next steps / TODO

- An expired pagination offset (400) → restart from page 1 once.
- A guard against mass deletion: refuse to prune more than X% of records in one run.
- Adaptive interval: back off (x2, capped) after 429s or failures; reset after a success.
- Observability: Prometheus or OpenTelemetry exporter instead of expvar (only `internal/obs` changes);
  latency histograms instead of totals; trace IDs propagated with OpenTelemetry instead of `job`/`run`.
- Storage behind an interface (S3 / a database); secrets from a secrets manager; a client per tenant token.
