# Architecture

How the system is laid out and why. The code is the reference for behaviour; this
document explains the choices behind it. Decisions about unclear parts of the brief are in
[ASSUMPTIONS.md](ASSUMPTIONS.md).

## Overview

```
                 ┌──────────────────────── one Go process ────────────────────────┐
  seller ──POST──►  HTTP API                          worker pool (N goroutines)   │
  client ◄─202───   validate · store as pending       claim ─► R1 ─► commit        │
         ──GET───►  read                              backoff on failure           │
  moderator ─────►  decision (compare-and-set)        retries-exhausted path       │
                 │            │                                │                  │
                 └────────────┼────────────────────────────────┼──────────────────┘
                              ▼                                ▼
                       ┌─────────────────── PostgreSQL ───────────────────┐
                       │  listings   (system of record AND work queue)     │
                       └───────────────────────────────────────────────────┘
        seed/*.json: read once at startup into memory, immutable, never written to the database
```

- **One table is both the record and the queue.** A row with `status = 'pending'` is a job.
  Creating the listing and enqueueing it are the same insert, so there is no window in which
  one exists without the other.
- **Correctness lives in the database**, not in process memory: unique constraints, a
  compare-and-set on `status`, an advisory lock per seller, row locks with `SKIP LOCKED`.
  The API and the workers could be separate processes, or several copies, without changing
  any guarantee.
- **The rules are pure functions** (`internal/rules`): no database, no clock, no I/O. The one
  database-dependent rule, R3 (duplicate), is injected as a callback, so the worker can run it
  inside its lock while the decision order stays in one place.

## Technology choices

| Choice | Why | What it costs |
| --- | --- | --- |
| **Go**, `net/http` (1.22 patterns), `pgx/v5` | Small dependency set (pgx and `x/text`), one static binary, easy to run API and workers in one process. | – |
| **PostgreSQL** as store and queue | The guarantees asked for (one listing per idempotency key, one result per phase, concurrent duplicates) are exactly what unique indexes, `INSERT … ON CONFLICT`, row locks and transactional advisory locks give. A broker would add a second system whose state must be kept consistent with the first. | Polling instead of push; throughput is bounded by one database. Fine for this load, and see "first changes" below. |
| **Polling workers**, 250 ms | Simple, and a pending row is the only source of truth, so a lost wake-up cannot lose work. | Up to ~250 ms of added latency; idle workers issue a cheap indexed query. |
| **No message broker, no Redis** | Nothing in R1–R5 needs an external effect (the brief says so). | – |
| **docker compose** for `make run` / `make test` | The brief assumes only `make`, Docker and `curl` on the host. | A cold start includes image pulls and a Go build. |

## Data model

One migration, [`0001_init.sql`](internal/store/migrations/0001_init.sql), embedded in the binary and
applied at startup inside one transaction under an advisory lock, so several instances starting
together queue up and the schema is never half-migrated.

`listings`:

| Group | Columns |
| --- | --- |
| Content | `id` (`l-` + 24 hex), `seller_id`, `category_id`, `title`, `description`, `price_vnd bigint`, `created_at` |
| Result | `status`, `decision jsonb`, `decided_at` |
| Idempotency | `idempotency_key` (nullable), `request_hash` |
| Duplicate rule | `dup_fingerprint` |
| Worker | `attempts`, `lease_token uuid`, `lease_expires_at`, `last_error` |

Constraints that carry the design:

- `status` is checked against the API's seven values.
- **`(status = 'pending') = (decision IS NULL)`** and the same for `decided_at`: a result and its status
  are stored together or not at all. The database refuses a "published" row without a decision.
- **`UNIQUE (seller_id, idempotency_key)`**. A NULL key never conflicts, so requests without a
  key always create a listing.
- **Partial unique index on `(seller_id, dup_fingerprint)` for `auto_approved`, `needs_human`, `published`**:
  at most one active listing per seller and fingerprint. This is the safety net behind the seller lock.
- Indexes for the claim query (`created_at WHERE status = 'pending'`) and for the list endpoint.

Sellers, categories and banned keywords are **not** in the database. They are read from
`seed/` at startup, validated (unique ids; median price in `(0, MaxInt64/3]`;
no blank keyword) and held in memory. The process exits if they are invalid. The R2
comparison itself avoids overflow independently of that seed-validation limit.

## Lifecycle as implemented

```
POST ─► pending ─┬─► auto_rejected                     (R1, R3)
                 ├─► published                         (R4, or nothing fired)
                 └─► needs_human ─┬─► published        (moderator approves)
                                  └─► rejected         (moderator rejects)
```

- `auto_approved` is part of the API's enum but **never stored**: approval and publication
  happen in the same transaction, so a listing goes straight from `pending` to `published`.
- `draft` is in the brief's diagram but not in the API's status enum and nothing creates a draft; it is not modelled.
- `expired` is in the enum; nothing sets it (see [Not implemented](#not-implemented)).
- Each phase has at most one committed result: the automated phase (`pending` → one of three
  outcomes) and the human phase (`needs_human` → one of two). A human result replaces the
  escalation as the listing's current `decision`; no history is kept.

## Submitting a listing (`POST /v1/listings`)

The handler does these in order, and the order is part of the contract:

1. Read the `Idempotency-Key` header: absent means no key; present but empty, over 255 bytes, or
   repeated is a `400`.
2. Read the body (at most 1 MiB) and parse it strictly, a `400` on any problem
   (`internal/listing/parse.go`): malformed JSON, **duplicate keys at any depth** (including inside
   unknown fields, which may nest), data after the object, nesting deeper than 32, wrong types,
   a missing field, lengths, U+0000 (a PostgreSQL storage limit), and `price_vnd` that is not a
   plain integer in int64 (`-0` is read as 0; fractions, exponents, other negatives and strings are rejected).
   Unknown top-level fields are ignored.
3. `seller_id` / `category_id` must exist in the seed, otherwise `422`.
4. Only now does it touch the database. A request that failed steps 1–3 never reaches storage, so
   it cannot claim an idempotency key.

The storage step is one short `READ COMMITTED` transaction (`Store.CreateListing`):

```sql
SET LOCAL lock_timeout = 3000;
INSERT INTO listings (...) VALUES (...)
ON CONFLICT (seller_id, idempotency_key) DO NOTHING
RETURNING id;
-- row returned     -> we created it: COMMIT, answer 202
-- no row returned  -> the key exists and is committed:
SELECT id, status, request_hash FROM listings WHERE seller_id = $1 AND idempotency_key = $2;
-- same request_hash -> 200 with the current status;  different -> 409
```

Why this is correct under concurrent retries: if another transaction is inserting the same key,
our `INSERT` waits on the unique index for it to finish. If it commits, `DO NOTHING` applies and we
replay its result. If it rolls back (a crash, a failed commit), our insert proceeds and we create
the listing. For one key exactly one transaction ever inserts. **`202` is only sent after `COMMIT`
returned**, so a client never holds an id that does not exist. If the key's holder stays busy for
more than `lock_timeout` (3 s), the request gets `503` + `Retry-After`.

`request_hash` is a SHA-256 over the five business fields **as decoded** (length-prefixed, so
`("ab","c")` and `("a","bc")` differ). Field order, whitespace and escaping in the JSON do not matter; any change to a
value does, including case, whitespace inside a string, price, and NFC vs NFD spelling of the
same text. Unknown fields are not hashed.

## The worker

`internal/worker` runs `WORKERS` goroutines. Each loops: claim one listing, process it, repeat;
sleep `POLL_INTERVAL` when there is nothing to claim.

**Claim** (`Store.ClaimPending`), one statement:

```sql
UPDATE listings
SET attempts = attempts + 1,
    lease_token = gen_random_uuid(),
    lease_expires_at = now() + make_interval(secs => $lease)
WHERE id IN (SELECT id FROM listings
             WHERE status = 'pending' AND (lease_expires_at IS NULL OR lease_expires_at < now())
             ORDER BY created_at, id LIMIT $n
             FOR UPDATE SKIP LOCKED)
RETURNING ...
```

- `SKIP LOCKED` means concurrent claimers never wait for, or return, the same row.
- Every claim, including a re-claim after an expired lease, bumps `attempts` and issues a **new
  `lease_token`**. The token identifies this attempt.
- The lease clock is the database's `now()`, so instances with different clocks agree.
- The claim commits immediately. If the worker dies afterwards, the lease runs out (30 s) and
  another worker claims the listing again. A crashed attempt still counts in `attempts`.

**Processing** (`worker.handle`), in this order:

1. **R1 (banned keywords) runs first, outside any transaction.** If R1 cannot run (an error or a panic), nothing
   is committed: the failure is recorded and the listing is retried. There is no code path from
   here to an approval or an escalation that has not first run R1 successfully.
2. `Store.CommitAutomated` runs the rest of the decision inside the transaction below, passing
   R1's result in.
3. Any failure records the error and a backoff, guarded by the lease token (next sections).

**The commit transaction** (`Store.CommitAutomated`, `READ COMMITTED`):

```
BEGIN
  SET LOCAL lock_timeout = 3000
  1. SELECT pg_advisory_xact_lock(1, hashtext(seller_id))         -- one commit per seller at a time
  2. decision := Decide(R1 result, findDuplicate)                  -- R3 lookup runs HERE, inside the lock
       findDuplicate: another listing, same seller + dup_fingerprint,
                      status IN ('auto_approved','needs_human','published')
  3. UPDATE listings SET status, decision, decided_at, lease_token = NULL, lease_expires_at = NULL
       WHERE id = $id AND status = 'pending'                      -- compare-and-set
       0 rows -> ErrNotPending (another attempt or a human got there first), rollback
  4. COMMIT                                                        -- releases the advisory lock
```

Why this gives the guarantees:

- **One automated result per listing.** Step 3 only updates a row that is still `pending`. Two
  attempts racing (a slow worker whose lease expired plus the worker that took over) cannot both win,
  and a late attempt cannot overwrite the committed result **or a later human resolution**, because
  once the status has moved on the compare-and-set matches nothing. The lease token is deliberately *not* part
  of this check: a valid result that commits first wins, whichever attempt produced it.
- **R3 is race-free: the first listing to commit wins.** Because step 1 serialises all commits for a
  seller from the lookup through `COMMIT`, whatever another commit wrote is already visible to the
  lookup in step 2. Other automated commits for that seller wait for the lock; human rejection
  may still remove an existing listing from the active set. A listing that is still
  `pending`, or whose transaction has not committed, is not "active" and is never seen. So of two
  identical eligible listings, the one whose commit succeeds first becomes active and the other is rejected as
  its duplicate, **whichever was submitted first**. (Ordering by submission time would be wrong:
  submission order is not commit order.) The advisory lock is transaction-scoped, so a crash or rollback
  releases it; `READ COMMITTED` is required, because under `REPEATABLE READ` the snapshot would be taken before waiting for the lock.
- **Backstop.** If some code path ever made an active duplicate without the lock, the partial unique
  index fails the `UPDATE` with `23505`; `CommitAutomated` then redoes the whole transaction (up to 3 times),
  and the redo finds the duplicate and rejects it.
- Nothing slow runs while the lock is held: R1 ran before, and inside the lock there is one lookup
  and one update.
- Different sellers normally use different lock keys. A collision in the 32-bit seller hash
  can serialize unrelated sellers, adding contention without weakening correctness.

**Recording a failure** (`Store.RecordFailure`):

```sql
UPDATE listings SET last_error = $msg, lease_expires_at = now() + $backoff
WHERE id = $id AND lease_token = $token AND status = 'pending'
```

Only the holder of the current lease token can change the lease, the error or the backoff. A worker that
was too slow, whose listing was re-claimed, matches nothing (`ErrLeaseLost`) and leaves the newer
attempt's state alone. So the token protects *lease bookkeeping*; the `status` compare-and-set protects *results*.

## Retries and failure handling

| What goes wrong | What happens |
| --- | --- |
| Worker crashes or is killed mid-attempt | Its transaction rolls back and releases the seller lock. The lease expires after `LEASE_DURATION` and another worker claims the listing again. |
| Commit fails, database unreachable, `lock_timeout` | The error and a backoff are recorded under the lease token. The listing is not claimable until the backoff has passed. |
| Backoff | `BACKOFF_BASE` (1 s) doubling per attempt, capped at `BACKOFF_MAX` (30 s). It works by pushing `lease_expires_at` into the future. |
| A slow worker finishes after being replaced | Its valid result may still win if the listing is pending. Once another result is committed, its compare-and-set loses (`ErrNotPending`). A failure report with the old token loses (`ErrLeaseLost`). |
| The record-failure write itself fails | Logged. The lease simply expires and the listing is claimed again. |
| Process receives SIGTERM | HTTP stops accepting and the pool stops claiming, sharing `SHUTDOWN_TIMEOUT` (15 s). In-flight listings finish; if the deadline passes, in-flight database calls are cancelled, their transactions roll back, and the leases expire into a later claim. Nothing is lost, only delayed. |

**Retries exhausted.** After `MAX_ATTEMPTS` (5) normal attempts, a listing whose R1 screening
succeeds can be resolved through a narrower decision. The next claim (attempt 6) uses `Engine.DecideAfterRetries`:

1. R1 found a banned term → `auto_rejected` `[R1]`. **R1 is never skipped**, so a banned listing
   cannot reach a human approval button because the system was struggling.
2. Otherwise R3 finds an active duplicate → `auto_rejected` `[R3]`. (Escalating would queue a
   second active listing for the same item.)
3. Otherwise → `needs_human` `[SYS_EVAL_FAILED]`, with the stored `last_error` and the number of
   failed attempts in the message and `detail`. A moderator resolves it through the normal decision
   endpoint. R4, R2 and R5 are not consulted, so even a trusted seller gets no shortcut.

All of this still goes through `CommitAutomated`, so the seller lock and the status compare-and-set apply.

**If R1 itself cannot run**, none of the above happens: nothing is committed, the listing stays
`pending`, and it keeps being retried, with a backoff that grows to `BACKOFF_MAX`. No path opens an approval without
R1. When R1 works again the listing is past its retries, so it takes the exhausted path above
(banned content → rejected; clean → a moderator).

**Operator visibility is logs only.** An attempt past `MAX_ATTEMPTS` that fails logs at error level
("needs operator attention", with the listing id). There is no metric or alert yet: see
[Not implemented](#not-implemented). `/readyz` deliberately does not go `503` because a few
listings are failing.

## Human decision

`POST /v1/listings/{id}/decision` validates the body (`400`), then runs one statement:

```sql
UPDATE listings SET status = $published_or_rejected, decision = $decision, decided_at = now()
WHERE id = $id AND status = 'needs_human'
RETURNING ...
```

One row → `200`. Zero rows → a second query tells "no such listing" (`404`) from "not awaiting
a human" (`409`). When moderators race, the later `UPDATE`s wait for the row lock, re-read the
row, find it is no longer `needs_human`, and update nothing, so there is exactly one winner. The new decision replaces the escalation.
No seller lock is needed: `needs_human` and `published` are both "active" for R3, so approving
moves between active states, and rejecting only leaves the active set; neither can create a second
active duplicate.

## Health

| Probe | Reports | Deliberately does not |
| --- | --- | --- |
| `GET /livez` | `200 {"status":"ok"}` whenever the process can serve a request. | Check any dependency. If it did, a database outage would make an orchestrator restart every instance, which cannot fix the database. |
| `GET /readyz` | `200 {"status":"ok","checks":{"database":"ok","worker":"ok"}}`, or `503 {"status":"unavailable","checks":{…"failed"}}`. Checks: a database ping, and the worker pool (running, and every worker has gone round its loop within `2 × LEASE_DURATION + POLL_INTERVAL`, so a worker wedged on a call that never returns fails readiness). The whole probe is bounded to 2 s. | Leak error details (they go to the log); report seed or migrations; fail because individual listings are failing. |

Seed and migrations are not probed because the startup order guarantees them: the process loads
and validates the seed, connects to the database, migrates, and only then starts the workers
and opens the port. A listening process therefore always has both; there is nothing left to report.
Before the port opens there is no answer at all (connection refused), not a `503`.

## Topology, durability and restarts

**Supported topology:** one PostgreSQL (a single primary) and one or more copies of the service. Every
guarantee is enforced by the database. Copies must use the same seed data and compatible
rule/fingerprint versions; no instance keeps moderation state another one needs, so copies
can be added without coordination: they claim with `SKIP LOCKED`, serialise per seller with advisory
locks, and agree on lease expiry through the database clock. API-only or worker-only processes would also work
(there is a single binary and no flag for it yet). *This was designed for, but not tested with
several processes*: the concurrency tests use many goroutines and connections in one process. Everything reads and writes the primary; read replicas are not used. A connection pooler was not tried: pgx's default
prepared-statement cache would need to be changed behind a transaction-mode pooler.

| Event | What survives |
| --- | --- |
| **One worker goroutine dies or a request is cancelled mid-attempt** | Everything committed. The open transaction rolls back; the listing is claimed again once its lease expires (≤ 30 s). |
| **Whole service process restarts** (crash, `kill`, deploy) | Everything committed: listings, results, idempotency keys, attempts, last error. In-flight attempts roll back and are retried after their leases expire. Clients retrying with the same `Idempotency-Key` get the original listing. Pending listings are picked up by the new process. |
| **`make run` stopped and started again** | Same, because the PostgreSQL data lives in the named volume `lm-<PORT>_pgdata`, which `make run` keeps. Only `docker compose … down -v` or deleting the volume erases it. |
| **PostgreSQL restarts or crashes** | PostgreSQL's normal durability (default `fsync`) applies: committed transactions survive. The service retries the connection at startup (`DB_WAIT`); while it runs, failed attempts are retried with backoff and `/readyz` reports `503`. |
| **`make test`** | Nothing: the test database is in memory and is removed. |

## How the concurrency tests work

Races are built, not hoped for. Tests hold a transaction open at the exact point of interest
(a hook between `INSERT` and `COMMIT`, or between the status update and `COMMIT`), start the
competing request, and wait until PostgreSQL itself reports it is blocked behind the holder
(`pg_blocking_pids`). Only then do they release the holder and check the outcome. There are no sleeps standing in for ordering.
Covered: concurrent identical and conflicting idempotency keys, a first transaction that rolls back
while others wait, `lock_timeout`, concurrent claimers, expired leases, stale workers (token and
status), two duplicates racing to commit in either order, the unique-index backstop, two moderators
racing, and the retries-exhausted path with R1 failing, through store-level and HTTP tests against
a real database. Mutation checks
(deliberately breaking the lock, the compare-and-set, the token check, the R1 step and so on) were run by hand to confirm
the tests fail without the guarantee; they are not part of `make test`.

## Security notes

- No authentication or authorisation anywhere; see below.
- Error responses never include database errors; they are logged.
- The compose database uses fixed development credentials (`lm`/`lm`) and is not published to the host. Not for production.
- The container runs as a non-root user. Request bodies are limited to 1 MiB; HTTP server timeouts are set.

## Not implemented

| Gap | Consequence |
| --- | --- |
| **Metrics** | No metrics endpoint, no counter for listings stuck past `MAX_ATTEMPTS`, no queue-depth or age gauge. Operators have logs only. |
| **Expiry** | `expired` is never set. Published listings stay published, so a published listing blocks identical listings from the same seller indefinitely. |
| **Authentication / authorisation** | Anyone who can reach the service can approve or reject listings, and `moderator_id` is self-asserted. |
| **History** | Only the current decision is stored. A human decision erases the automated escalation's reasons; there is no audit trail of who decided what and when. |
| **Pagination** | The list endpoint returns the newest `limit` (≤ 100) items; there is no cursor. |
| **Idempotency-key expiry** | Keys live as long as their listing. |
| **Rate limiting**, tracing, structured request ids | – |
| **Re-evaluating a listing rejected as a duplicate** | If the first listing is later rejected by a moderator, the second (already `auto_rejected`) is not re-examined. |
| **JSON error bodies for unknown routes and methods** | Go's default plain-text `404`/`405`. |

## What I would change first for production

1. **Authenticate callers and take the moderator's identity from the credential, not from the request body.**
   Today anybody can resolve a listing and claim to be anyone. This is the largest hole in the trust model, and it
   is a small change (middleware plus using the verified identity as `moderator_id`).
2. **Metrics and alerting** for the places this design deliberately parks failures: listings with
   `attempts > MAX_ATTEMPTS` and still `pending` (R1 failing), oldest-pending age, queue depth, attempts per decision,
   lock-wait time. The design relies on someone noticing; today only logs say so.
3. **An append-only decision history table**, so a human result no longer erases the automated reasons, and so there
   is an audit trail of who decided what. Needed before real moderators use it.
4. **Expiry**: a job that moves `published` listings to `expired` after a retention period (and frees their slot for R3).
5. **Split API and workers** into separately scalable processes (the code supports it; a flag and a deployment each are missing), and
   add `LISTEN/NOTIFY` so idle workers wake on new work instead of polling.
6. A TTL and cleanup for idempotency keys, cursor pagination, and a managed PostgreSQL with backups and non-default credentials.
