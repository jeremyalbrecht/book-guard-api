-- Scope books to an owning user (OIDC subject). DEFAULT '' exists only so the
-- column can be added to a table that may already hold pre-auth dev rows;
-- application inserts always set user_id explicitly, so the default is never
-- relied on for new data. The index backs the per-user filtering every query does.
ALTER TABLE books ADD COLUMN IF NOT EXISTS user_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS books_user_id_idx ON books (user_id);
