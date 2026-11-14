# Listing Moderation Service — agent notes

Take-home exercise: async moderation pipeline. Go, one process (API + worker),
Postgres. The contract is `openapi.yaml` (do not change what it defines; adding
is fine), `make run` (:8080, honours `PORT`) and `make test`. `seed/` is
read as-is at startup. BRIEF.md is not in the repo.

## Commands
- `make test` — tests in containers with `-race`, real Postgres, self-cleaning.
- `make run` — compose up in the foreground; SIGTERM removes containers, keeps the data volume.
- Fast loop without Docker: `go test ./...` (DB tests skip without `DATABASE_URL`).
- This Windows machine has `mingw32-make`, not `make`. Keep files LF.

## Decisions already made — do not re-litigate, record changes in ASSUMPTIONS.md
**Rule order** (stop at the first that applies):
1. R1 banned keyword → `auto_rejected`, reasons `[R1]` only. Applies to every seller.
2. R3 duplicate → `auto_rejected`, reasons `[R3]` + `duplicate_of`. Every seller.
3. R4 `trust_score >= 80` → `published`, reasons `[R4]`. R2 and R5 are **not run**.
4. R2 and R5 run together; any fired → `needs_human`, one reason per fired rule.
5. Nothing fired → `published`, reason `AUTO_PASS`.
Approve and publish happen in one transaction; `auto_approved` is never stored.

**R1:** whole seed phrase, case-insensitive, boundary = not letter/mark/number/`_`
(no `\b`: RE2's is ASCII-only). No `\s+`, no diacritic stripping. Message must
contain the seed term (title/description in `detail` alone is not enough).
**R2:** strict. Exactly 3x or exactly 1/3 does not fire. Never multiply
unchecked: `price_vnd` has no maximum, so use overflow-free comparisons.
**R3:** same seller + category + title + description; **price excluded**. Title
and description: NFC, lowercase, collapse all Unicode whitespace, trim. No diacritic
stripping, no fuzzy matching. "Active" = `auto_approved`, `needs_human`, `published`
(never `pending`). First valid commit wins, not first submitted. Lookup runs inside
`pg_advisory_xact_lock(seller)` at READ COMMITTED, with the CAS in the same transaction.
**Idempotency:** header `Idempotency-Key` only, scoped to the seller. No key → always a
new listing. Hash covers the 5 business fields as decoded, verbatim (no R3 normalisation;
NFC vs NFD differ → 409). Unknown fields are ignored and not hashed. Validate (400/422)
before looking at the key; invalid requests do not store a key. Flow: `INSERT … ON
CONFLICT DO NOTHING`, then a separate `SELECT`; 202 only after commit, 200 replay, 409 mismatch.
**Parser:** reject duplicate JSON keys at any depth (unknown fields can nest), `UseNumber`,
`price_vnd` must be digits only within int64 (`-0` is accepted as 0; fractions, exponents and
other negatives are 400), reject trailing data after the object. Title is checked for length
only (1..200 characters), never trimmed or judged. U+0000 in any string is a 400: a Postgres
storage limit, not a content rule. Oversized bodies (>1 MiB) are 400.
**Worker:** claim with `FOR UPDATE SKIP LOCKED` + `lease_token`; every lease update is
conditional on that token. Result commit is a CAS on `status='pending'`. MAX_ATTEMPTS 5,
lease 30s, exponential backoff from 1s. After the limit: R1 found → still `auto_rejected`;
R1 clean → R3 check, then `needs_human` with `SYS_EVAL_FAILED`; **R1 itself failing → stay
pending, keep retrying, alert. Never open an approve path without having run R1.**
**Human decision:** `UPDATE … WHERE status='needs_human'`; 200 / 404 / 409. Body is validated
first (400). `moderator_id` must be present and a string; an empty string is allowed (the schema
does not forbid it). Reason `HUMAN`; no history kept. Expiry is not implemented.

## Conventions
- Rules are pure functions (`internal/rules`); DB access only in `internal/store`.
- Tests hit real Postgres (`internal/testdb` gives each test its own schema). Build races
  deterministically: hold a transaction, wait with `pg_blocking_pids`, release. No sleeps.
- Errors from the DB never go into responses; log them.

## Things the model got wrong earlier — check these
- Overflow when multiplying price by 3.
- Believing "only look at earlier-created listings" makes R3 safe (it does not; commit order differs).
- Precomputing R2/R5 for sellers that skip them.
- A retry fallback that could reach human approval without running R1.
- Assuming the JSON body is flat; stale workers updating a newer lease.

## Process
Every ambiguity or deviation → `ASSUMPTIONS.md` (A1… format from the brief).
Do not commit unless asked.
