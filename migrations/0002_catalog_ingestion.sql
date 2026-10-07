-- Catalog acquisition is independent of commerce-provider operations.
ALTER TABLE courses ADD COLUMN catalog_details JSONB;
ALTER TABLE courses ADD COLUMN commercial_observed_at TIMESTAMPTZ;
ALTER TABLE courses ADD COLUMN commercial_status TEXT
    CHECK (commercial_status IN ('CURRENT', 'STALE'));
ALTER TABLE course_price_snapshots DROP CONSTRAINT course_price_snapshots_source_check;
ALTER TABLE course_price_snapshots ADD CONSTRAINT course_price_snapshots_source_check
    CHECK (source IN ('SYNC','CLAIM_REVALIDATION','CHECKOUT_REVALIDATION','RECONCILIATION','CATALOG_INGESTION'));

CREATE TABLE catalog_sources (
    id UUID PRIMARY KEY,
    provider TEXT NOT NULL,
    slug TEXT NOT NULL,
    external_id TEXT,
    canonical_url TEXT NOT NULL,
    course_id UUID UNIQUE REFERENCES courses(id),
    last_attempted_at TIMESTAMPTZ NOT NULL,
    last_successful_at TIMESTAMPTZ,
    extractor_version TEXT NOT NULL,
    result TEXT NOT NULL CHECK (result IN ('RUNNING','SUCCEEDED','INCOMPLETE','FAILED')),
    normalized_hash TEXT,
    UNIQUE(provider, slug),
    UNIQUE(provider, canonical_url),
    UNIQUE(provider, external_id)
);
CREATE TABLE catalog_ingestion_runs (
    id UUID PRIMARY KEY,
    source_id UUID NOT NULL REFERENCES catalog_sources(id),
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    acquired_at TIMESTAMPTZ,
    extractor_version TEXT NOT NULL,
    result TEXT NOT NULL CHECK (result IN ('RUNNING','SUCCEEDED','INCOMPLETE','FAILED')),
    normalized_hash TEXT,
    provenance JSONB NOT NULL DEFAULT '{}',
    warnings JSONB NOT NULL DEFAULT '[]',
    error_code TEXT,
    error TEXT
);
CREATE INDEX catalog_ingestion_runs_recent ON catalog_ingestion_runs(source_id, started_at DESC);
