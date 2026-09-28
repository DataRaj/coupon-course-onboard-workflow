-- Course Marketplace MVP schema. All money is stored in integer minor units (paise).

CREATE TABLE IF NOT EXISTS courses (
    id                   UUID PRIMARY KEY,
    provider             TEXT        NOT NULL,
    provider_product_id  TEXT        NOT NULL,
    provider_plan_id     TEXT        NOT NULL,

    title                TEXT        NOT NULL,
    plan_name            TEXT        NOT NULL DEFAULT '',
    description          TEXT        NOT NULL DEFAULT '',

    class                INTEGER,
    board                TEXT,
    subject              TEXT,
    language             TEXT,
    target_exam          TEXT,
    thumbnail_url        TEXT,
    duration             TEXT,

    mrp_amount           BIGINT,
    selling_price_amount BIGINT      NOT NULL,
    currency             TEXT        NOT NULL DEFAULT 'INR',

    checkout_url         TEXT        NOT NULL DEFAULT '',
    provider_active      BOOLEAN     NOT NULL DEFAULT TRUE,

    provider_created_at  TIMESTAMPTZ,
    provider_updated_at  TIMESTAMPTZ,
    last_synced_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT courses_provider_identity UNIQUE (provider, provider_product_id, provider_plan_id)
);

CREATE INDEX IF NOT EXISTS courses_active_idx ON courses (provider_active, subject, class);
CREATE INDEX IF NOT EXISTS courses_search_idx ON courses USING gin (to_tsvector('simple', title || ' ' || plan_name));

CREATE TABLE IF NOT EXISTS course_price_snapshots (
    id                   UUID PRIMARY KEY,
    course_id            UUID        NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    mrp_amount           BIGINT,
    selling_price_amount BIGINT      NOT NULL,
    currency             TEXT        NOT NULL,
    source               TEXT        NOT NULL
        CHECK (source IN ('SYNC', 'CLAIM_REVALIDATION', 'CHECKOUT_REVALIDATION', 'RECONCILIATION')),
    captured_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS course_price_snapshots_course_idx
    ON course_price_snapshots (course_id, captured_at DESC);

CREATE TABLE IF NOT EXISTS offers (
    id            UUID PRIMARY KEY,
    course_id     UUID        NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    discount_type TEXT        NOT NULL DEFAULT 'PERCENT' CHECK (discount_type = 'PERCENT'),
    discount_bps  INTEGER     NOT NULL CHECK (discount_bps > 0 AND discount_bps <= 10000),
    coin_cost     BIGINT      NOT NULL CHECK (coin_cost >= 0),
    starts_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    ends_at       TIMESTAMPTZ,
    status        TEXT        NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE', 'INACTIVE')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One live offer per course is enough for this MVP.
CREATE UNIQUE INDEX IF NOT EXISTS offers_one_active_per_course
    ON offers (course_id) WHERE status = 'ACTIVE';

CREATE TABLE IF NOT EXISTS wallet_accounts (
    user_id         UUID PRIMARY KEY,
    available_coins BIGINT      NOT NULL DEFAULT 0 CHECK (available_coins >= 0),
    reserved_coins  BIGINT      NOT NULL DEFAULT 0 CHECK (reserved_coins >= 0),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS wallet_ledger (
    id            UUID PRIMARY KEY,
    user_id       UUID        NOT NULL,
    redemption_id UUID,
    type          TEXT        NOT NULL
        CHECK (type IN ('CREDIT', 'RESERVE', 'RELEASE', 'CONSUME', 'RESTORE')),
    amount        BIGINT      NOT NULL CHECK (amount > 0),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS wallet_ledger_user_idx ON wallet_ledger (user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS redemptions (
    id                        UUID PRIMARY KEY,
    user_id                   UUID        NOT NULL,
    course_id                 UUID        NOT NULL REFERENCES courses (id),
    offer_id                  UUID        NOT NULL REFERENCES offers (id),

    provider                  TEXT        NOT NULL,
    provider_product_id       TEXT        NOT NULL,
    provider_plan_id          TEXT        NOT NULL,

    price_snapshot_id         UUID REFERENCES course_price_snapshots (id),

    -- Frozen quote: historical rows must not move when today's provider price changes.
    mrp_amount                BIGINT,
    provider_price_amount     BIGINT      NOT NULL,
    discount_bps              INTEGER     NOT NULL,
    coin_cost                 BIGINT      NOT NULL,
    expected_discount_amount  BIGINT      NOT NULL,
    expected_checkout_amount  BIGINT      NOT NULL,
    currency                  TEXT        NOT NULL,

    status                    TEXT        NOT NULL CHECK (status IN (
        'COINS_RESERVED', 'COUPON_CREATING', 'COUPON_ISSUED', 'CHECKOUT_OPENED',
        'PURCHASE_PENDING', 'PURCHASE_CONFIRMED', 'PAYMENT_FAILED', 'EXPIRED',
        'PROVIDER_ERROR', 'REFUND_PENDING', 'REFUNDED')),

    coupon_id                 UUID,
    provider_customer_id      TEXT,
    provider_invoice_id       TEXT,
    provider_transaction_id   TEXT,

    quote_verified_at         TIMESTAMPTZ NOT NULL,
    expires_at                TIMESTAMPTZ NOT NULL,

    checkout_opened_at        TIMESTAMPTZ,
    purchase_confirmed_at     TIMESTAMPTZ,
    refunded_at               TIMESTAMPTZ,

    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS redemptions_user_idx ON redemptions (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS redemptions_expiry_idx ON redemptions (status, expires_at);

-- Duplicate provider payments can never be attributed to two redemptions.
CREATE UNIQUE INDEX IF NOT EXISTS redemptions_provider_transaction_uniq
    ON redemptions (provider, provider_transaction_id)
    WHERE provider_transaction_id IS NOT NULL;

-- At most one in-flight redemption per user+course+offer.
CREATE UNIQUE INDEX IF NOT EXISTS redemptions_one_active
    ON redemptions (user_id, course_id, offer_id)
    WHERE status IN ('COINS_RESERVED', 'COUPON_CREATING', 'COUPON_ISSUED',
                     'CHECKOUT_OPENED', 'PURCHASE_PENDING');

CREATE TABLE IF NOT EXISTS coupons (
    id                  UUID PRIMARY KEY,
    redemption_id       UUID        NOT NULL REFERENCES redemptions (id) ON DELETE CASCADE,
    provider            TEXT        NOT NULL,
    provider_coupon_id  TEXT,
    coupon_code         TEXT        NOT NULL,
    discount_bps        INTEGER     NOT NULL,
    status              TEXT        NOT NULL
        CHECK (status IN ('CREATING', 'ACTIVE', 'USED', 'DISABLED', 'EXPIRED', 'FAILED')),
    provider_product_id TEXT        NOT NULL,
    provider_plan_id    TEXT        NOT NULL,
    -- Set when remote deactivation still owes a retry.
    deactivate_after    TIMESTAMPTZ,
    expires_at          TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS coupons_code_uniq ON coupons (coupon_code);
CREATE UNIQUE INDEX IF NOT EXISTS coupons_provider_id_uniq
    ON coupons (provider, provider_coupon_id) WHERE provider_coupon_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS coupons_redemption_uniq ON coupons (redemption_id);

ALTER TABLE redemptions
    DROP CONSTRAINT IF EXISTS redemptions_coupon_fk;
ALTER TABLE redemptions
    ADD CONSTRAINT redemptions_coupon_fk FOREIGN KEY (coupon_id) REFERENCES coupons (id);

CREATE TABLE IF NOT EXISTS provider_transactions (
    id                      UUID PRIMARY KEY,
    provider                TEXT        NOT NULL,
    provider_transaction_id TEXT        NOT NULL,
    redemption_id           UUID REFERENCES redemptions (id),
    kind                    TEXT        NOT NULL CHECK (kind IN ('PAYMENT', 'REFUND')),
    invoice_id              TEXT,
    customer_id             TEXT,
    amount                  BIGINT      NOT NULL,
    currency                TEXT        NOT NULL,
    status                  TEXT        NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT provider_transactions_uniq UNIQUE (provider, provider_transaction_id, kind)
);

CREATE TABLE IF NOT EXISTS provider_webhook_events (
    id                UUID PRIMARY KEY,
    provider          TEXT        NOT NULL,
    provider_event_id TEXT,
    event_type        TEXT        NOT NULL,
    payload_hash      TEXT        NOT NULL,
    payload           JSONB       NOT NULL,
    headers           JSONB       NOT NULL DEFAULT '{}'::jsonb,
    status            TEXT        NOT NULL DEFAULT 'RECEIVED'
        CHECK (status IN ('RECEIVED', 'PROCESSING', 'PROCESSED', 'IGNORED', 'FAILED')),
    attempt_count     INTEGER     NOT NULL DEFAULT 0,
    last_error        TEXT,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at      TIMESTAMPTZ,
    CONSTRAINT provider_webhook_events_dedup UNIQUE (provider, payload_hash)
);

CREATE INDEX IF NOT EXISTS provider_webhook_events_pending_idx
    ON provider_webhook_events (status, received_at);

CREATE TABLE IF NOT EXISTS provider_sync_runs (
    id            UUID PRIMARY KEY,
    provider      TEXT        NOT NULL,
    status        TEXT        NOT NULL CHECK (status IN ('RUNNING', 'SUCCEEDED', 'FAILED')),
    items_seen    INTEGER     NOT NULL DEFAULT 0,
    items_changed INTEGER     NOT NULL DEFAULT 0,
    error         TEXT,
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS provider_sync_runs_recent_idx
    ON provider_sync_runs (provider, started_at DESC);

CREATE TABLE IF NOT EXISTS idempotency_keys (
    user_id      UUID  NOT NULL,
    operation    TEXT  NOT NULL,
    key          TEXT  NOT NULL,
    request_hash TEXT  NOT NULL,
    resource_id  UUID,
    response_json JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, operation, key)
);
