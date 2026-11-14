# Listing Moderation Service

A small service that decides whether a classified listing goes live. A seller submits a
listing and gets an immediate `202`; a pool of workers in the same process moderates it
in the background (publish, reject, or send to a human); the client polls for the
outcome. A moderator resolves the listings that need a human.

Go, one process (HTTP API + workers), PostgreSQL. The API contract is
[`openapi.yaml`](openapi.yaml), unchanged from the starter kit. Implementation extensions
are listed under [API](#api).

| Document | What is in it |
| --- | --- |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | Layout, transactions, concurrency, retries, health, topology, durability, what to change first for production |
| [`ASSUMPTIONS.md`](ASSUMPTIONS.md) | Every place the brief was unclear or contradictory, and what was decided |
| [`CLAUDE.md`](CLAUDE.md) | **Agent configuration** (Claude Code): the decisions, conventions and past mistakes the AI tooling is held to |
| [`AI-NOTES.md`](AI-NOTES.md) | How AI was used while building this |

## Requirements

`make`, Docker (with the Compose plugin) and `curl`. Nothing else: Go runs inside containers.

## Run it

```bash
make run                 # API on http://localhost:8080
PORT=8081 make run       # same service on another port, side by side with the first
```

`make run` builds the image, starts PostgreSQL and the service with `docker compose`, and stays
in the foreground. Ctrl-C or SIGTERM stops and removes the containers and the network.

- `PORT` is the published host port (default `8080`). Inside the container the service
  always listens on `8080`. Each port gets its own Compose project (`lm-<PORT>`), so two
  instances never share containers, networks or data.
- **Data survives stopping `make run`.** PostgreSQL's data lives in a named volume
  (`lm-<PORT>_pgdata`) that is kept. To start from empty: `docker compose -p lm-8080 down -v`.
- PostgreSQL is not published to the host.
- The service is ready when `curl localhost:8080/readyz` returns `200`.

## Test it

```bash
make test
```

Runs `go test -race ./...` in a container against a throwaway PostgreSQL (data in memory).
No service has to be running. It exits non-zero on any failure and always removes what it
created (containers, network, volumes, the test image).

- Prerequisites: Docker running, and network access the first time to pull `golang:1.26`
  and `postgres:17-alpine`.
- The tests are about 360 cases including subtests. Concurrency tests run against the real database
  and build each race deterministically (see
  [ARCHITECTURE.md](ARCHITECTURE.md#how-the-concurrency-tests-work)); nothing is mocked at the
  database boundary.
- Without Docker, `go test ./...` (Go 1.26) runs everything except the tests that need a
  database, which are skipped. Set `DATABASE_URL` to point them at your own PostgreSQL.

## Configuration

Environment variables, all optional except `DATABASE_URL`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | – (required) | PostgreSQL URL. `docker-compose.yml` sets it. |
| `HTTP_ADDR` | `:8080` | Listen address inside the process. |
| `SEED_DIR` | `seed` (`/app/seed` in the image) | Folder with `sellers.json`, `categories.json`, `banned_keywords.json`. Validated at startup; the process exits if they are invalid. |
| `DB_WAIT` | `60s` | How long startup waits for PostgreSQL. |
| `SHUTDOWN_TIMEOUT` | `15s` | Deadline for graceful shutdown (HTTP and workers share it). |
| `WORKERS` | `4` | Concurrent workers; each handles one listing at a time. |
| `POLL_INTERVAL` | `250ms` | How long an idle worker waits before looking for work again. |
| `LEASE_DURATION` | `30s` | How long a claim is held before another worker may take the listing. |
| `MAX_ATTEMPTS` | `5` | Normal attempts per listing. The next claim takes the retries-exhausted path. |
| `BACKOFF_BASE` | `1s` | Rest after the first failed attempt; doubles each time. |
| `BACKOFF_MAX` | `30s` | Cap for the backoff. |

`docker-compose.yml` only sets `DATABASE_URL`, so the worker settings are at their defaults
unless you add them under `app.environment`.

## API

Contract: [`openapi.yaml`](openapi.yaml). Errors use its `{"error": {"code", "message"}}` shape.

| Request | Success | Errors |
| --- | --- | --- |
| `POST /v1/listings` | `202` `{id, status: "pending"}`; `200` same body for a replay of an earlier submission | `400` invalid request, `409` key reused with a different body, `422` unknown seller or category, `503` same key still in flight (retry) |
| `GET /v1/listings/{id}` | `200` listing with `status`, `decision` (null while pending) and `decided_at` | `404` |
| `GET /v1/listings?seller_id=&status=&limit=` | `200` `{items}`, newest first, `limit` 1–100 (default 20) | `400` for an invalid parameter |
| `POST /v1/listings/{id}/decision` | `200` the updated listing | `400` invalid body, `404`, `409` not awaiting a human (never escalated, or already resolved) |
| `GET /livez`, `GET /readyz` | `200` | `readyz`: `503` |

Additions beyond the contract: the `Idempotency-Key` request header on `POST /v1/listings`,
a `Location` header on `202`, `Retry-After` on `503`, and the error codes `invalid_request`,
`unknown_seller`, `unknown_category`, `idempotency_key_reused`, `busy`, `not_found`,
`not_awaiting_review`, `internal`.

**Send an `Idempotency-Key`.** A key is scoped to the seller. Repeating a request with the
same key and the same body returns the original listing (`200`); the same key with a different
body is a `409`. A request *without* a key always creates a new listing, so a client that
retries without one may create extra listings (which the duplicate rule may then reject).

### Rules, in the order they are applied

Evaluation stops at the first rule that applies:

1. **R1** a banned keyword in the title or description → `auto_rejected`. Every seller.
2. **R3** the same seller already has an active listing with the same category, title and
   description (price ignored) → `auto_rejected`. Every seller.
3. **R4** `trust_score >= 80` → `published`; R2 and R5 are not evaluated.
4. **R2** price more than 3× or less than ⅓ of the category median, and/or **R5** unverified
   seller → `needs_human`, with one reason per rule that fired.
5. Nothing fired → `published` (reason `AUTO_PASS`).

Why it is ordered this way, and the contradictions in the brief it resolves, are in
[`ASSUMPTIONS.md`](ASSUMPTIONS.md) (A1–A4).

### Try it

POSIX shell examples. Replace the ids with the ones you get back.

```bash
# Submit. 202 with {"id": "...", "status": "pending"}.
curl -i -X POST localhost:8080/v1/listings \
  -H 'Idempotency-Key: demo-1' -H 'Content-Type: application/json' \
  -d '{"seller_id":"s-1002","title":"iPhone 13","description":"Like new","price_vnd":6000000,"category_id":"c-phones"}'

# The same request again: 200, same id. A different body with the same key: 409.
curl -i -X POST localhost:8080/v1/listings \
  -H 'Idempotency-Key: demo-1' -H 'Content-Type: application/json' \
  -d '{"seller_id":"s-1002","title":"iPhone 13","description":"Like new","price_vnd":6000000,"category_id":"c-phones"}'

# Poll for the outcome (usually decided within a fraction of a second).
curl -s localhost:8080/v1/listings/l-0123456789abcdef01234567
# {"id":"l-...","seller_id":"s-1002",...,"status":"published","decided_at":"...",
#  "decision":{"outcome":"approved","reasons":[{"rule_id":"AUTO_PASS","message":"no moderation rule fired"}]}}

# A banned term is rejected, and the reason quotes the term.
curl -s -X POST localhost:8080/v1/listings -d \
  '{"seller_id":"s-1002","title":"Replica watch","description":"","price_vnd":900000,"category_id":"c-fashion"}'

# An unverified seller (s-1004) goes to a human.
curl -s -X POST localhost:8080/v1/listings -d \
  '{"seller_id":"s-1004","title":"Laptop","description":"","price_vnd":18000000,"category_id":"c-laptops"}'

# List what is waiting for a moderator.
curl -s 'localhost:8080/v1/listings?status=needs_human'

# A moderator decides: 200 for the first request, 409 for any later one.
curl -s -X POST localhost:8080/v1/listings/l-0123456789abcdef01234567/decision \
  -d '{"decision":"approve","moderator_id":"m-1","note":"photos check out"}'
```

The seed sellers cover the edge cases: `s-1006` has trust 80 (trusted) and `s-1007` has 79;
`s-1004` and `s-1008` are unverified; `s-1005` is trusted but unverified.

## Repository layout

```
cmd/server/            main: wiring, startup order, graceful shutdown
internal/config/       environment configuration
internal/seed/         load and validate seed/*.json
internal/listing/      domain types, JSON parsing and validation, request hash, duplicate fingerprint
internal/rules/        R1-R5 as pure functions, the decision order, retries-exhausted decision
internal/store/        PostgreSQL: migrations, create/read, worker claim/commit/failure, human decision
internal/worker/       worker pool: polling, backoff, retries-exhausted path, heartbeat
internal/httpapi/      handlers, probes, middleware
internal/testdb/       an isolated schema per database test
seed/                  reference data, loaded as shipped (do not edit)
```

## Status and known limitations

What is verified, and what is not:

- A fresh clone passed `make test` with real PostgreSQL and the race
  detector, then `PORT=8081 make run` and HTTP smoke checks. Verification used GNU Make 4.4.1
  inside an Alpine Linux runner container connected to Docker Desktop on Windows. This checks
  the Linux make/Compose path, not a separate native Linux host. Windows `mingw32-make` was
  also tested earlier; macOS has not been tested.
- HTTP smoke checks covered automatic publication, trusted-seller R1 rejection, R3 duplicates,
  idempotent replay and conflict, human approval with an empty moderator id, subsequent `409`,
  unknown-id `404`, list arrays, and health probes.
- `/readyz` returned `200` after **45 s** from that clone, including the runtime image build,
  and **6 s** on restart. The previously measured Windows starts were 34 s with a cached
  PostgreSQL image and 23 s with warm caches. **None is an empty-image-cache cold-machine
  timing.** The strict 180-second cold-cache requirement remains unverified.
- SIGTERM was sent directly to the GNU `make run` process, not just its recipe shell.
  It exited `143`; containers and network were removed and the data volume was kept.
  Starting it again preserved a published listing. Both shutdowns passed the cleanup checks.
- Several service processes against one database have not been run; the concurrency tests use
  many connections and goroutines in one process.
- No load or throughput measurements.

Deliberately not implemented (details and consequences in [ARCHITECTURE.md](ARCHITECTURE.md#not-implemented)):

- **Metrics.** Failures are only logged. There is no metrics endpoint and no counter of listings
  stuck in `pending` after too many attempts.
- **Expiry.** `expired` is in the status enum but nothing ever sets it.
- **Authentication and authorisation.** Every endpoint is open, including the moderator
  decision; `moderator_id` is whatever the caller sends.
- **History.** A listing keeps only its current decision. A human decision replaces the
  automated one; there is no audit trail.
- Pagination beyond `limit` on the list endpoint, TTL for idempotency keys, rate limiting.
