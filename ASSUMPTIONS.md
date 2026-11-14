# Assumptions

Every place the brief (or the starter kit) was unclear, incomplete or contradicted itself, what
was decided, and what it would cost to decide the other way. Entries marked **contradiction** are
places where two statements in the brief cannot both hold. References like "§3.2" are to the brief.

Each decision here is implemented as written; [ARCHITECTURE.md](ARCHITECTURE.md) explains how, and
the tests named in the code pin the behaviour.

| Area | Entries |
| --- | --- |
| Rules | A1–A10 |
| Idempotency | A11–A12 |
| Lifecycle and phases | A13–A16 |
| Retries and operations | A17–A18 |
| Validation and API | A19–A22 |
| Health, durability, build | A23–A25 |
| Scope | A26 |

---

## A1 — R1 "always rejected" vs R4 "bypasses moderation entirely" (contradiction)

**Ambiguity:** R1 says a listing with a banned term "is **always rejected**". R4 says a trusted seller
"**bypasses moderation entirely** — their listings are published immediately". A trusted seller
posting "replica" satisfies both rules and they demand opposite results. The seed has trusted sellers
(`s-1001`, trust 92) so this is not hypothetical.
**Decision:** R1 wins, for every seller. Banned content is rejected with reasons `[R1]` only.
**Rationale:** "always" is the only absolute in the rule set, and banned-content moderation is the
one check that protects the marketplace regardless of who posts. A trust score is a statement about
a seller's *history*, not about this listing's content. Reading R4 as "bypass the *review queue*"
rather than "bypass every rule" removes the conflict and keeps R4 meaningful.
**Cost of being wrong:** If R4 was meant to override R1, trusted sellers' banned listings would be
published; the change is the order of two checks in `Engine.Decide` and the tests that pin it.

## A2 — R4 vs R5: a trusted seller who is not verified (contradiction)

**Ambiguity:** R4 publishes sellers with `trust_score >= 80` "entirely" bypassing moderation; R5 sends
`verified: false` sellers to `needs_human`. Seed seller `s-1005` ("unverified, high trust", trust 88)
satisfies both.
**Decision:** R4 wins: `s-1005` is published with reasons `[R4]`. R5 is not evaluated.
**Rationale:** R4 is the only rule that says "entirely", and the brief gives R5 no precedence over it.
The seed's note on `s-1005` marks it as a deliberate test of exactly this overlap, and "bypass
moderation entirely" is the most direct reading. The R4 reason says which checks were not applied
(`detail.skipped: ["R2", "R5"]`), so a moderator reading the listing later can tell they never ran.
**Cost of being wrong:** If verification should outrank trust, unverified high-trust sellers would
need review. That means R5 is evaluated before R4 (a reorder in `Decide`, plus tests).

## A3 — What R4 skips, and the order rules are applied in

**Ambiguity:** "Bypasses moderation entirely" does not say which rules a trusted seller still faces.
The brief also never says whether several rules can fire at once or which wins.
**Decision:** Rules are applied in a fixed order and evaluation **stops at the first that applies**:
R1 (reject) → R3 (reject) → R4 (publish) → R2/R5 (escalate) → otherwise publish. So R4 skips exactly
R2 and R5, the two "send to a human" rules, and never R1 or R3. Reasons list only what decided the
outcome (plus both R2 and R5 when both fire; see A5).
**Rationale:** R4 describes skipping *review*, and R2/R5 are the review triggers. R1 is absolute (A1).
R3 protects the *buyer-facing* catalogue from the same item twice, and a trusted seller can still
double-post; exempting trusted sellers would let the most prolific sellers flood it. Stopping at the
first rule keeps decisions explainable: the reason list is the reason, not a log of every check.
**Cost of being wrong:** If trusted sellers should also skip R3 (or R2/R5 should be evaluated and
recorded even when R4 applies), that is a change to `Decide` and to the matrix test; the stored
decisions for already-decided listings would not change.

## A4 — R4 "published immediately" vs the asynchronous `202 pending` contract

**Ambiguity (contradiction):** R4 says trusted sellers' listings "are published immediately", while
the contract requires `POST /v1/listings` to return `202` with status `pending` and not evaluate inline.
**Decision:** The contract wins. A trusted seller's listing is also created `pending` and published by
the worker, normally within a fraction of a second (the worker polls every 250 ms). "Immediately" is
read as "without waiting for a human".
**Rationale:** The wire contract is normative and the conformance suite depends on it; "immediately"
is qualitative. Evaluating R4 in the request would also mean the HTTP handler runs a rule, which the
brief forbids.
**Cost of being wrong:** If `POST` should return `published` for trusted sellers, it contradicts the
OpenAPI contract and would need the handler to run R1 and R3 (which need the duplicate lookup under
the seller lock) inside the request.

## A5 — R2 and R5 are one group

**Ambiguity:** Both lead to `needs_human`; the brief does not say whether both are reported when both fire.
**Decision:** Both are always evaluated together (when R4 does not apply) and the decision carries one
reason per rule that fired, in the order R2, R5.
**Rationale:** "Decisions are explainable": a moderator should see *all* the reasons a human is needed,
and neither rule is more important than the other.
**Cost of being wrong:** Reporting only the first would drop a reason; trivial to change, no stored data affected.

## A6 — R3: what "the same as this one" means

**Ambiguity:** The brief does not define "same": exact copy, same title, fuzzy match? Does price count?
**Decision:** Same seller, same `category_id`, and the same **title and description after
normalisation**: Unicode NFC, lower-cased, every run of Unicode white space (spaces, tabs, newlines,
NBSP, …) collapsed to one space, trimmed. **Price is not compared.** Diacritics are *not* stripped
and nothing is fuzzy-matched. The normalised text is only used for comparison; the stored and
returned text is exactly what was sent. The comparison is a SHA-256 "fingerprint" stored in a column.
**Rationale:** Changing only the price does not make a different item for sale, and a seller reposting
the same ad with a different price is the case the rule exists for. Case and whitespace differences are
accidental. Stripping diacritics or fuzzy matching would start to merge genuinely different listings
("hàng" and "hang"), and false positives here silently reject real sellers.
**Cost of being wrong:** A stricter definition (include price) lets price-bumped reposts through; a
looser one (fuzzy) rejects legitimate listings. Changing the normalisation means bumping the fingerprint
version prefix (`dup/v1`) and recomputing stored fingerprints.

## A7 — R3: which listings count as "active"

**Ambiguity:** "Another **active** listing" is not defined against the status enum.
**Decision:** Active means `auto_approved`, `needs_human`, `published`. A `pending` listing is **not**
active, and neither are `auto_rejected`, `rejected` or `expired`. (`auto_approved` is never stored,
A14, but is listed so the definition matches the enum.) Consequence accepted: if listing B is rejected
as a duplicate of A (which was waiting in `needs_human`) and a moderator then rejects A, B is *not*
re-examined and the seller has no live listing; they must resubmit.
**Rationale:** `pending` must be excluded: a listing that has not been decided cannot make another
one a duplicate, or two identical listings submitted together could each wait on the other and both be
rejected. `needs_human` must be included, otherwise a moderator could approve two identical
listings. Rejected and expired listings are not live.
**Cost of being wrong:** If `needs_human` should not count, two identical listings can both reach a
human and both be approved unless the human step also checks R3. If a rejected duplicate should be
revived when its original is rejected, that needs a re-evaluation job.

## A8 — R3 under concurrency: the first listing *to commit* wins, not the first submitted

**Ambiguity:** When two identical listings are in flight, "the same seller already has another
active listing" depends on timing. The brief does not say which survives.
**Decision:** Whichever automated result commits first is kept; the other is rejected as its
duplicate (`duplicate_of` points at the winner). This may be the listing that was submitted second.
Implemented with a transaction-scoped advisory lock per seller around the duplicate lookup and the
status update, plus a partial unique index as a backstop.
**Rationale:** Submission order is not commit order, and "the earlier one wins" cannot be enforced
without making `POST` take a lock. An earlier-created-wins rule that only looks at created time is
unsafe: the earlier listing's transaction may still be uncommitted when the later one decides, so
both could pass. Committed order is the only order the database can agree on.
**Cost of being wrong:** If submission order must decide, `POST` has to take the per-seller lock
and assign a per-seller sequence, adding contention to every submission.

## A9 — R1 matching rules

**Ambiguity:** "Case-insensitive, word-boundary match": boundaries for non-ASCII text, multi-word
terms, and what the message must contain.
**Decision:** Each seed term is matched as a literal whole phrase, case-insensitively. A boundary is
required on a side only when the term has a letter, mark, digit or `_` at that edge, and the
neighbouring character must not be one of those (Unicode-aware; Go's `\b` is ASCII-only). So `fake`
matches in `fake-phone` and `(fake)` but not `fakes`, `unfake`, `fake2` or `fakeđ`. A multi-word term
matches only with the exact spacing in the seed ("hang  gia" with two spaces does not match). No
diacritic stripping: "hàng giả" does not match "hang gia". Title and description are both checked,
and every match is reported in one `R1` reason; the message quotes each term **as spelled in the
seed** (`title contains banned term "hang gia"`), which is what the contract requires.
**Rationale:** The literal reading of "word boundary" and of the seed, with no cleverness that could
over-reject. Diacritic handling was not asked for, and the seed only has unaccented terms.
**Cost of being wrong:** A listing can evade R1 with extra spaces inside a phrase, with accented
spelling, or with a decomposed (NFD) form of an accented term, since R1 does not normalise Unicode. If those matter, normalise both sides
(NFC, whitespace) before matching; that is a contained change in `internal/rules/banned.go`.

## A10 — R2 boundaries, free listings, overflow

**Ambiguity:** "More than 3× or less than one-third": are exactly 3× and exactly ⅓ included? What about
price 0? `price_vnd` has no maximum.
**Decision:** Both bounds are strict, so exactly 3× and exactly ⅓ do not fire. Price 0 is valid and
always fires R2 (it is below a third of any positive median). The comparison is exact for every int64
without multiplying (`price/3` against the median with the remainder, and `price` against
`ceil(median/3)`), so a huge price cannot overflow. The seed is validated so medians are in `(0, MaxInt64/3]`.
Tests compare against `math/big` over 200,000 random and edge cases.
**Rationale:** The wording says "more than" and "less than". Nothing in the brief says free items are
forbidden, and the rule flags them for a human, which is the safe outcome.
**Cost of being wrong:** If the bounds were meant to be inclusive, the boundary listings (for
example 96,000,000 for motorbikes) would go to a human instead of publishing.

## A11 — Idempotency key: where it is, its scope, and what "no key" means

**Ambiguity:** §2 says "the same key" but neither the brief nor `openapi.yaml` names the key, where it
travels, its scope, or what happens without one. The `409` text ("a submission key … reused with a
different request body") implies a key separate from the body.
**Decision:** The key is the `Idempotency-Key` request header, scoped **per seller** (two sellers using
the same key are two independent submissions). Empty, over 255 bytes, or repeated headers are `400`.
**No header means no idempotency: every request creates a listing.** Keys have no expiry and live
as long as the listing.
**Rationale:** `Idempotency-Key` is the de-facto standard header. Per-seller scope stops one seller's key from
colliding with another's and leaks nothing between them. Treating a missing key as "no protection"
(rather than deriving one from the body) means a legitimate repost is never silently swallowed.
**Cost of being wrong:** If the suite sends the key another way, requests would not be deduplicated.
If keys should be global, two sellers using the same key would collide. A client that retries without
a key creates duplicates, which R3 may then reject (A8): the README says to send a key.

## A12 — Idempotency semantics: comparison, replay, errors, ordering

**Ambiguity:** What is "the same request"? What does a replay return? Do failed requests consume the key?
**Decision:**
- The comparison is on the five business fields **as decoded** from JSON: field order, whitespace and
  escaping do not matter, **any change to a value does** (case, price, inner whitespace, and NFC vs NFD
  spelling of the same text). Unknown fields are ignored and not compared.
- Same key + same body → `200` with the original id and its **current** status (which may already be
  `published`). Same key + different body → `409`. A new listing → `202` `pending`, sent only after commit.
- Requests rejected as invalid (`400`/`422`) never reach storage, so they **do not claim the key**;
  validation errors are reported before key conflicts.
- `price_vnd` `-0` and `0` decode to the same value and so hash the same.
**Rationale:** The brief demands exactly one listing per key under aggressive concurrent retries, which
the database uniqueness constraint enforces in this design (A24, ARCHITECTURE.md). Hashing decoded values means a
retry that re-serialises the body is a replay, not a conflict. Not consuming the key on invalid input
lets a client fix the request and reuse its key.
**Cost of being wrong:** If normalised comparison (case-insensitive, NFC) was wanted, harmless retries
that differ only in case would return `409` instead of `200`. If an invalid request should consume
the key, a corrected resubmission would conflict.

## A13 — The lifecycle diagram versus the API's status enum

**Ambiguity (contradiction):** The diagram shows `draft`, `auto_approved` and `expired`; the API enum has
no `draft`, and nothing in the brief says what creates a draft or moves a listing to `expired`.
**Decision:** `draft` is not modelled (nothing can create it). `expired` stays in the enum but **is
never set** and there is no expiry job.
**Rationale:** Expiry needs a retention period the brief never states. The brief calls it a lifecycle change
("Publication after approval and expiry are lifecycle changes, not additional moderation phases") and none of
the required guarantees depends on it. With about a day of effort it was cut rather than invented.
**Cost of being wrong:** Listings stay published forever, so a published listing blocks identical
listings from that seller indefinitely (A7). Adding expiry is a periodic job plus a retention setting.

## A14 — Approval and publication are one step; `auto_approved` is never stored

**Ambiguity:** The diagram has `auto_approved → published`, but no trigger for the second step.
**Decision:** An approval commits as `published` in the same transaction; a listing never rests in
`auto_approved`. The status remains in the enum (so `?status=auto_approved` is valid and always empty).
**Rationale:** The brief says publication "after approval" is a lifecycle change, not a moderation
phase, and does not require a separate transaction. One step means no recovery path for a listing
stuck between the two, and a test only has to wait for `published`.
**Cost of being wrong:** If the suite expects to observe `auto_approved`, it never will. A separate
publisher would need its own retry/recovery path.

## A15 — A human decision replaces the automated one; no history

**Ambiguity:** The brief says a human resolution is a separate phase and that "no additional phase field or
history endpoint is required", but also that decisions must be explainable.
**Decision:** The moderator's decision **replaces** `decision` and `decided_at` (outcome `approved`/`rejected`,
one reason `HUMAN` carrying `moderator_id` and `note`). The automated escalation's reasons are no longer
visible. No history table.
**Rationale:** Matches the OpenAPI text ("A human resolution may replace the automated escalation
outcome"). A history table was cut for time (A26).
**Cost of being wrong:** A moderator cannot see why the listing was escalated once resolved, and there is
no audit trail. This is the third thing to add for production (ARCHITECTURE.md).

## A16 — Human decision endpoint details

**Ambiguity:** Validation, ordering of checks, repeated identical decisions, who may call it.
**Decision:**
- The body is validated first (`400`, same strictness as the submit endpoint, unknown fields ignored), so a bad body
  to an unknown id is `400`, not `404`. Then `404` (no such listing) or `409` (not `needs_human`).
- Repeating the same decision is also `409`, because the OpenAPI text says "subsequent requests return 409".
- `moderator_id` must be present and a string; **the schema does not forbid an empty one, so empty is accepted** and
  recorded as given. `note` is optional.
- **There is no authentication**: any caller can decide, and `moderator_id` is self-asserted.
- A moderator can only resolve `needs_human`. They cannot override `auto_rejected`, `published` or `pending`.
**Rationale:** Follows the contract as written; auth is not mentioned in the brief.
**Cost of being wrong:** An open decision endpoint is unacceptable outside an exercise (first item for
production). If the order should be 404-before-400, one swap.

## A17 — How incomplete evaluation is retried, and what happens when it cannot finish

**Ambiguity:** §2 requires documenting "how incomplete evaluation is retried or brought to an
operator/moderator's attention" and forbids leaving accepted work unprocessed indefinitely.
**Decision:** Pending listings are claimed with a 30 s lease; a crashed or failed attempt is retried
after the lease or an exponential backoff (1 s doubling, capped at 30 s). After **5** normal attempts
the next claim takes the retries-exhausted path: banned content is still rejected (`R1`), a duplicate
is still rejected (`R3`), and anything else goes to a moderator as `needs_human` with reason
`SYS_EVAL_FAILED` carrying the stored last error and the attempt count. **R1 is never skipped:** if R1
itself cannot run, nothing is decided and the listing stays `pending` and keeps being retried; there is no
path from "R1 failing" to an approval or an escalation. Trusted sellers get no shortcut on this path.
**Rationale:** A system failure must not become an approval, or a banned listing's route to a human
approval button. Escalating to the existing human flow reuses the one resolution path instead of adding a second
"stuck listing" mechanism. An active duplicate is rejected rather than escalated so the queue cannot hold
a second copy.
**Cost of being wrong:** A genuinely slow dependency could send many listings to moderators together. Retry
count, lease and backoff are configuration. A listing whose R1 cannot run stays `pending` indefinitely,
which is the deliberate trade against approving unscreened content.

## A18 — "Brought to an operator's attention": logs only, no metrics

**Ambiguity:** The brief does not say how an operator should learn that work is stuck.
**Decision:** An attempt that fails after `MAX_ATTEMPTS` logs at error level ("needs operator attention", with
the listing id). There is **no metric, counter, alert or admin endpoint**, and `/readyz` does not go unhealthy
because listings are failing.
**Rationale:** Cut for time. Readiness is about whether *this instance* can serve; a bad batch of listings
should not remove every instance from rotation.
**Cost of being wrong:** An operator has to read logs to notice stuck listings. Second item for production.

## A19 — Request validation beyond the OpenAPI schema

**Ambiguity:** The schema leaves many cases open.
**Decision:**
- Strict JSON: duplicate keys at any depth (including inside unknown fields), data after the object, malformed JSON, and nesting over 32 are `400`.
  Unknown fields are accepted and ignored. `Content-Type` is not checked.
- `price_vnd` must be digits only and fit int64. `-0` is accepted as `0`. Fractions (`1.0`), exponents (`1e3`),
  other negatives, strings and booleans are `400`.
- `title` is checked for length only (1–200 characters, counted as characters, not bytes) and kept exactly as sent;
  a title of only spaces is allowed. `description` may be empty, at most 5,000 characters.
- U+0000 in a stored string field is `400`. This is a storage limit (PostgreSQL text and jsonb cannot hold it), not a content rule. Ignored fields are not subject to this storage check.
- A body over 1 MiB is `400`, using the same invalid-request response as other body errors. The brief permits additional status codes; `413` was also an available design choice.
**Rationale:** Duplicate keys are a classic way for two parsers to disagree about a payload; the schema
says `integer`, so floats are rejected; the title rule follows the schema (`minLength: 1`) and does not invent policy.
**Cost of being wrong:** Stricter than some clients expect (for example `price_vnd: 1000.0` is rejected). A
looser parser would accept more but make the idempotency hash ambiguous.

## A20 — `400` vs `422`, and unknown ids

**Ambiguity:** A request can be both malformed and reference an unknown seller; `422` is "seller or category
does not exist".
**Decision:** Format and schema problems (`400`) are reported before existence checks (`422`); the seller is
checked before the category. An empty `seller_id` is a string, so it is a `422`, not a `400`.
**Rationale:** The client must fix a malformed request before references are meaningful.
**Cost of being wrong:** A suite that expects `422` for a body that is also malformed would see `400`.

## A21 — Listing the listings

**Ambiguity:** Behaviour for bad query parameters, ordering ties, pagination.
**Decision:** A parameter that is present must be valid or the request is a `400`: `limit` an integer 1–100 (default 20),
`status` one of the seven values, `seller_id` non-empty, none repeated. An unknown `seller_id` returns an empty
list, not an error. Order is `created_at DESC, id DESC`. There is no pagination beyond `limit`.
**Rationale:** Failing loudly on a typo is safer than silently returning everything. Pagination is not in the contract.
**Cost of being wrong:** A client that sends `limit=0` or an unknown status gets `400` instead of a default. Only the
newest 100 are reachable.

## A22 — Seed data

**Ambiguity:** "Must be loaded at startup — it's a representative sample, not the complete set."
**Decision:** The seed is read from `SEED_DIR` at startup (not compiled in), so a different seed can be dropped in, validated, and
held in memory. The process **exits** if it is invalid: duplicate or empty ids, a category median not in
`(0, MaxInt64/3]`, a blank keyword. Unknown fields (such as `note`) are ignored. It is never written to the database.
**Rationale:** Failing at startup is safer than evaluating listings against data the rules cannot trust; a blank keyword would
match every listing.
**Cost of being wrong:** A larger seed with something outside these checks would stop the service from starting.

## A23 — What `/livez` and `/readyz` report

**Ambiguity:** The brief asks only that this be recorded.
**Decision:** `/livez` is `200 {"status":"ok"}` while the process can answer; it checks nothing else. `/readyz` is `200` when
a database ping succeeds and the worker pool is healthy (running, every worker has made progress within
`2 × lease + poll interval`), otherwise `503`; the body lists each check as `ok` or `failed`, and error text goes to the log, not the response.
Seed and migrations are not probed: the process does not open the port until both are done.
**Rationale:** Liveness that depended on the database would make an orchestrator restart healthy processes during a database outage.
**Cost of being wrong:** A hung worker is only noticed after about a minute, and a failing rule (R1) does not affect readiness.

## A24 — Storage, durability and topology scope

**Ambiguity:** §3.2 makes infrastructure my choice and durability across a process restart optional, but concurrent
correctness required.
**Decision:** One PostgreSQL, one or more service copies. All state is in the database, and it **is durable**: data survives
worker, service and `make run` restarts (it lives in a named volume that `make run` keeps; `make test` uses memory and discards it).
Concurrent correctness is enforced by the database, not by process memory (unique indexes, compare-and-set, advisory locks, `SKIP LOCKED`).
**Rationale:** The guarantees asked for are exactly what PostgreSQL provides; durability came for free.
**Cost of being wrong:** Several service processes against one database have not been run (the concurrency tests use many goroutines in
one process), and read replicas or a transaction-mode pooler are untested.

## A25 — The `make` contract

**Ambiguity:** "Stop any resources it starts when it is terminated" and "honor PORT" leave the details open; does stopping erase data?
**Decision:** `PORT` sets the published host port (default 8080); the container always listens on 8080. Each port gets its own
Compose project, so instances do not share anything. `make run` runs in the foreground and removes containers and the network on
SIGINT/SIGTERM but **keeps the data volume**. `make test` builds a test image, runs `go test -race` against a database in
memory, and always removes its containers, network, volumes and image, exiting non-zero on failure. PostgreSQL is not published.
**Rationale:** Keeping the volume matches "durability is optional but nice" and makes a restart behave like production; removing
everything else follows "without affecting others".
**Cost of being wrong:** A grader that expects a clean database on every `make run` would find old listings from a previous run
on the same `PORT`. Remove the volume (`docker compose -p lm-<PORT> down -v`) or use another port.

## A26 — "Production-quality" in about one day: what was cut

**Ambiguity (contradiction):** §2 asks for production-quality design while the introduction asks for roughly a day of effort and says
that knowing what to cut is part of what is judged.
**Decision:** The correctness-critical parts are complete and tested against a real database: idempotency, one result per phase, stale-attempt
protection, duplicate handling, retries and the human path. **Cut:** metrics and alerting, expiry, authentication, decision history, pagination,
idempotency-key expiry, rate limiting, request tracing, and the split into separate API and worker deployments. Each is listed in
[ARCHITECTURE.md](ARCHITECTURE.md#not-implemented) with its consequence.
**Rationale:** Those items add operability and policy around a core that has to be right first; none of them can silently produce a wrong
result, with the exception of the unauthenticated decision endpoint (A16).
**Cost of being wrong:** See the individual entries; the order I would add them is in ARCHITECTURE.md.
