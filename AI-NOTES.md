# AI Notes

I read through the brief and starter files first. I used Claude Opus 5.5 for requirements and design, then Sonnet 5.5 for small coding tasks. I delegated implementation and tests, and worked through the behavior and trade-offs in chat. I chose Go with net/http and pgx to keep dependencies small and run the API and worker in one binary.

Claude set up Git and CLAUDE.md, built the Docker/Make scaffold, seed loading, migrations and health checks, then implemented rules, listing hashes, submission/read APIs and idempotency. Tests run against real Postgres with the race detector. These stages are now committed.

I questioned the initial price check because multiplying by three could overflow int64. Claude replaced it with division/remainder comparisons and tests against math/big. I also challenged creation-time ordering for duplicates: an earlier listing might not have committed yet. The store now checks duplicates under a seller lock, with tests for concurrent commits and stale attempts.

We corrected proposals that precomputed skipped rules or bypassed R1 after retries. We kept duplicate matching separate from idempotency: changing the price can still be a duplicate, but reusing a key with a changed price returns 409. I dropped separate approval and publication transactions during design to avoid another recovery path.

During API review, I pointed out that whitespace-only titles and -0 satisfy the schema. I asked Claude to remove those extra restrictions. It changed the parser and added tests showing that title whitespace is preserved and -0 hashes like 0. These fixes are in the API commit.

An HTTP test also caught items:null instead of []; Claude fixed it. Claude corrected two wrong price-test expectations and reran an invalid mutation check after fixing Git Bash path conversion.

Claude has also added claim/reclaim, token-checked failure updates, the polling worker pool, retries and moderator decisions, with end-to-end tests. I asked it to remove the empty moderator_id restriction too: the schema has no minLength. That fix and its tests are committed. Final checks from a fresh clone passed: containerized tests, HTTP flows, persistence after restart, and SIGTERM sent directly to GNU make. Startup took 45 seconds with partially cached images; an empty-cache cold start remains unverified.
