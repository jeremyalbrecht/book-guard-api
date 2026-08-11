-- Enrichment metadata is intrinsic to an ISBN, so it lives in a global `editions`
-- entity shared across all users rather than being copied onto each user's book.
-- This means Open Library is queried once per ISBN ever: when a second user adds
-- the same ISBN, the description and cover are already present with no refetch.
-- The per-user `books` table is left unchanged.
CREATE TABLE IF NOT EXISTS editions (
    isbn              TEXT PRIMARY KEY,
    title             TEXT,
    author            TEXT,
    description       TEXT,
    publisher         TEXT,
    published_date    TEXT,           -- Open Library dates are free-form (e.g. "1998", "May 2005")
    page_count        INTEGER NOT NULL DEFAULT 0,
    enrichment_status TEXT NOT NULL DEFAULT 'pending', -- pending | enriched | failed
    enriched_at       TIMESTAMPTZ,
    has_cover         BOOLEAN NOT NULL DEFAULT false
);

-- Partial index keeps the reconciler's "what still needs work" sweep cheap.
CREATE INDEX IF NOT EXISTS editions_todo_idx
    ON editions (enrichment_status) WHERE enrichment_status IN ('pending', 'failed');

-- Cover bytes are also intrinsic to the ISBN, so they are keyed by ISBN and shared.
CREATE TABLE IF NOT EXISTS book_covers (
    isbn         TEXT PRIMARY KEY REFERENCES editions(isbn) ON DELETE CASCADE,
    content_type TEXT NOT NULL,
    bytes        BYTEA NOT NULL,
    etag         TEXT,
    source_url   TEXT,
    fetched_at   TIMESTAMPTZ NOT NULL
);
