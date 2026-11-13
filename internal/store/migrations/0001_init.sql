-- listings is both the system of record and the work queue: a row with
-- status = 'pending' is a job waiting for the worker.
CREATE TABLE listings (
    id               text        PRIMARY KEY,
    seller_id        text        NOT NULL,
    category_id      text        NOT NULL,
    title            text        NOT NULL,
    description      text        NOT NULL,
    price_vnd        bigint      NOT NULL CHECK (price_vnd >= 0),
    created_at       timestamptz NOT NULL DEFAULT now(),

    -- Moderation state. The result and its status are stored together:
    -- a listing is 'pending' exactly when it has no decision yet.
    status           text        NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'auto_approved', 'auto_rejected',
                                       'needs_human', 'published', 'rejected', 'expired')),
    decision         jsonb,
    decided_at       timestamptz,
    CONSTRAINT decision_matches_status CHECK ((status = 'pending') = (decision IS NULL)),
    CONSTRAINT decided_at_matches_status CHECK ((status = 'pending') = (decided_at IS NULL)),

    -- Idempotent submission. request_hash covers the five business fields as
    -- decoded; a NULL key means "not idempotent", and NULLs never collide.
    idempotency_key  text,
    request_hash     bytea       NOT NULL,
    CONSTRAINT listings_seller_key_unique UNIQUE (seller_id, idempotency_key),

    -- R3 duplicate detection: hash of category + normalised title/description.
    dup_fingerprint  bytea       NOT NULL,

    -- Worker bookkeeping. lease_token identifies the current claim so a stale
    -- worker cannot touch a lease that now belongs to someone else.
    attempts         integer     NOT NULL DEFAULT 0,
    lease_token      uuid,
    lease_expires_at timestamptz,
    last_error       text
);

-- Safety net for R3: at most one active listing per seller and fingerprint.
CREATE UNIQUE INDEX listings_active_fingerprint
    ON listings (seller_id, dup_fingerprint)
    WHERE status IN ('auto_approved', 'needs_human', 'published');

-- Worker claim queue.
CREATE INDEX listings_pending_queue ON listings (created_at) WHERE status = 'pending';

-- GET /v1/listings: newest first, optionally filtered by seller or status.
CREATE INDEX listings_by_created ON listings (created_at DESC, id DESC);
CREATE INDEX listings_by_seller ON listings (seller_id, created_at DESC, id DESC);
CREATE INDEX listings_by_status ON listings (status, created_at DESC, id DESC);
