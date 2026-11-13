# Asana extractor

Extracts **users** and **projects** from the [Asana REST API](https://developers.asana.com/docs)
and stores **each record in its own JSON file**. Go standard library only.

## Run

```sh
cp .env.example .env && chmod 600 .env  # then put your Asana personal access token in .env
go run .                                # extract every workspace the token can see
go run . -workspace 1234567890          # only one workspace
go run . -debug                         # also log every page
```

## Configure

| Flag | Default | Meaning |
|---|---|---|
| `-base-url` | `https://app.asana.com/api/1.0` | API base URL |
| `-workspace` | (all) | Only extract this workspace gid |
| `-data-dir` | `data` | Output directory |
| `-rpm` | `150` | Client-side rate limit (Asana: 150/min free, 1500/min paid, per token) |
| `-max-retries` | `4` | Retries for 408 / 429 / 5xx / network errors |
| `-page-size` | `100` | Items per page (Asana max 100) |
| `-timeout` | `10s` | Per-request HTTP timeout |
| `-debug` | `false` | Debug logging |

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
main.go               flags + validation, slog, Ctrl+C/SIGTERM -> context cancel
internal/model        typed User / Project / Workspace + the opt_fields they need
internal/client       HTTP: auth, shared rate limiter, retries, 429, pagination, FetchAllAs[T]
internal/storage      atomic per-record JSON files, skip-unchanged, prune deleted
internal/extractor    workspaces -> users / projects -> storage
```

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
gofmt -l . && go vet ./... && go test -race ./...
```

`httptest` and a fake clock, so retry and rate-limit tests run instantly:
auth header, offset pagination, repeated offset, 429 with `Retry-After` (seconds and date),
a 429 pausing other requests, 5xx retry with backoff, give up after max retries, no retry on 401,
network errors, an HTML body, a cancelled context, limiter spacing, invalid items, per-record files,
pruning deleted records, and keeping old data when the API fails.

## Next steps / TODO

- **Task 2: periodic extraction**: `internal/poller` with two jobs on their own `time.Ticker`
  (e.g. `-users-interval`, `-projects-interval`; 30s and 5m), sharing the one `Client` and limiter.
- **Scale (thousands of users/projects)**:
  - Rate budget: 10k records = ~100 pages per full sync. At 150 rpm a 30s full resync does not fit;
    use the 5m interval for full syncs, or a paid tier.
  - Incremental sync: the Asana **Events API** (`/events?resource=<project>&sync=<token>`) or
    `modified_since` where it's supported, with a periodic full resync to catch deletions.
  - Stream page by page instead of holding every record in memory.
  - Fetch workspaces in parallel (bounded; Asana allows 50 concurrent GETs), still behind the shared limiter.
  - Webhooks instead of polling where possible.
- An expired pagination offset (400) → restart from page 1 once.
- A guard against mass deletion: refuse to prune more than X% of records in one run.
- `-log-format json`; metrics (requests, 429s, retries, durations).
- Storage behind an interface (S3 / a database); secrets from a secrets manager; a client per tenant token.
