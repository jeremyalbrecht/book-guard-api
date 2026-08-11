-- A book with no ISBN is a ghost: nothing can enrich it, nothing can give it a
-- canonical title or a cover, and no client can ever repair it. The API has been
-- able to create one since the beginning; this closes the door at the storage
-- layer too, so no script, migration or psql session can reintroduce them.

-- DESTRUCTIVE. These rows carry no recoverable identity — that is precisely what
-- makes them ghosts, and why deleting is the only available repair. Take a
-- pg_dump before the first run against real data.
-- The handlers always wrote '' rather than NULL, but accept both.
DELETE FROM books WHERE isbn IS NULL OR btrim(isbn) = '';

-- An edition keyed by the empty string can only ever be junk. Cascades to
-- book_covers via its foreign key.
DELETE FROM editions WHERE btrim(isbn) = '';

ALTER TABLE books ALTER COLUMN isbn SET NOT NULL;

-- The check runs on the *stripped* form so rows written before the handler began
-- canonicalising (which could contain the hyphens printed on the book) stay
-- legal. New writes arrive already stripped from internal/books/isbn.go.
--
-- Those legacy hyphenated rows are deliberately left as they are: rewriting an
-- ISBN means rewriting the editions key too, and book_covers references
-- editions(isbn) with no ON UPDATE CASCADE, so it would need a three-table
-- transaction with collision handling. They behave exactly as they do today.
--
-- Postgres has no ADD CONSTRAINT IF NOT EXISTS, and migrations run on every
-- startup, so the catalogue probe is what keeps this idempotent.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'books_isbn_shape' AND conrelid = 'books'::regclass
    ) THEN
        ALTER TABLE books ADD CONSTRAINT books_isbn_shape
            CHECK (regexp_replace(isbn, '[- ]', '', 'g') ~ '^([0-9]{9}[0-9Xx]|[0-9]{13})$');
    END IF;
END $$;
