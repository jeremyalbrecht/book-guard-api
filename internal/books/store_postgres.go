package books

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"ex-libris-api/internal/enrich"
	"ex-libris-api/migrations"
)

// Compile-time checks that PostgresStore satisfies the user-facing Store and the
// enrichment Store the worker depends on.
var (
	_ Store        = (*PostgresStore)(nil)
	_ enrich.Store = (*PostgresStore)(nil)
)

// PostgresStore is a Store backed by Postgres via a pgx connection pool.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a Store backed by the given pool. Migrate must have
// been run against the same database first.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// Migrate applies every embedded migration in order. Each migration is
// idempotent, so it is safe to call on every startup.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	all, err := migrations.All()
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	for _, m := range all {
		if _, err := pool.Exec(ctx, m.SQL); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.Name, err)
		}
	}
	return nil
}

func (s *PostgresStore) Create(ctx context.Context, userID string, b *Book) error {
	// Mirror MemoryStore: the application assigns the ID, defaults the status,
	// and stamps both timestamps, so behaviour is identical whichever store the
	// handlers run against. Ownership comes from the authenticated caller.
	b.ID = newID()
	b.UserID = userID
	if b.Status == "" {
		b.Status = StatusToRead
	}
	// Truncate to microseconds: Postgres timestamps only store that much
	// precision, so keeping the extra nanoseconds here would make the in-memory
	// value never equal what a later Get scans back.
	now := time.Now().UTC().Truncate(time.Microsecond)
	b.CreatedAt = now
	b.UpdatedAt = now

	_, err := s.pool.Exec(ctx, `
		INSERT INTO books (id, user_id, isbn, title, author, status, rating, opinion, tags, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		b.ID, b.UserID, b.ISBN, b.Title, b.Author, b.Status, b.Rating, b.Opinion, b.Tags, b.CreatedAt, b.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert book: %w", err)
	}
	return nil
}

// bookSelect is the shared SELECT list + FROM for Get and List. It LEFT JOINs the
// shared edition so enrichment data (which is per-ISBN, not per-book) is merged
// into each returned book; the join yields NULLs for a book with no ISBN or whose
// edition has not been created yet.
const bookSelect = `
	b.id, b.user_id, b.isbn, b.title, b.author, b.status, b.rating, b.opinion, b.tags, b.created_at, b.updated_at,
	e.title, e.author, e.description, e.publisher, e.published_date, e.page_count,
	e.enrichment_status, e.enriched_at, e.has_cover
	FROM books b LEFT JOIN editions e ON e.isbn = b.isbn`

// scanBook reads one joined row into a Book. Edition columns are nullable (LEFT
// JOIN), so they scan into pointers and are dereferenced with the zero value when
// absent. The tags column (text[]) scans straight into []string via pgx.
func scanBook(row pgx.Row) (*Book, error) {
	var b Book
	var (
		editionTitle, editionAuthor                             *string
		description, publisher, publishedDate, enrichmentStatus *string
		pageCount                                               *int
		enrichedAt                                              *time.Time
		hasCover                                                *bool
	)
	err := row.Scan(&b.ID, &b.UserID, &b.ISBN, &b.Title, &b.Author, &b.Status,
		&b.Rating, &b.Opinion, &b.Tags, &b.CreatedAt, &b.UpdatedAt,
		&editionTitle, &editionAuthor,
		&description, &publisher, &publishedDate, &pageCount, &enrichmentStatus, &enrichedAt, &hasCover)
	if err != nil {
		return nil, err
	}
	applyCanonicalNames(&b, strDeref(editionTitle), strDeref(editionAuthor))
	b.Description = strDeref(description)
	b.Publisher = strDeref(publisher)
	b.PublishedDate = strDeref(publishedDate)
	if pageCount != nil {
		b.PageCount = *pageCount
	}
	b.EnrichedAt = enrichedAt
	if hasCover != nil {
		b.HasCover = *hasCover
	}
	b.EnrichmentStatus = enrichmentStatusFor(b.ISBN, enrichmentStatus)
	return &b, nil
}

func (s *PostgresStore) Get(ctx context.Context, userID, id string) (*Book, error) {
	// Scoping by user_id means another user's book is indistinguishable from a
	// missing one.
	row := s.pool.QueryRow(ctx, "SELECT "+bookSelect+" WHERE b.id=$1 AND b.user_id=$2", id, userID)
	b, err := scanBook(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get book %s: %w", id, err)
	}
	return b, nil
}

func (s *PostgresStore) List(ctx context.Context, userID string) ([]*Book, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+bookSelect+" WHERE b.user_id=$1 ORDER BY b.created_at", userID)
	if err != nil {
		return nil, fmt.Errorf("list books: %w", err)
	}
	defer rows.Close()

	// Return a non-nil empty slice for no rows, matching MemoryStore (and so the
	// list handler serialises [] rather than null).
	out := make([]*Book, 0)
	for rows.Next() {
		b, err := scanBook(rows)
		if err != nil {
			return nil, fmt.Errorf("scan book: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate books: %w", err)
	}
	return out, nil
}

func (s *PostgresStore) Update(ctx context.Context, userID string, b *Book) error {
	// Mirror MemoryStore: created_at is immutable and updated_at is refreshed
	// server-side, whatever the caller passed. created_at is never written; the
	// RETURNING clause reads back the stored value so b reflects reality, and a
	// row owned by another user (or absent) yields no result rather than an update.
	b.UserID = userID
	now := time.Now().UTC().Truncate(time.Microsecond) // see Create: match Postgres timestamp precision
	b.UpdatedAt = now

	row := s.pool.QueryRow(ctx, `
		UPDATE books
		SET isbn=$2, title=$3, author=$4, status=$5, rating=$6, opinion=$7, tags=$8, updated_at=$9
		WHERE id=$1 AND user_id=$10
		RETURNING created_at`,
		b.ID, b.ISBN, b.Title, b.Author, b.Status, b.Rating, b.Opinion, b.Tags, b.UpdatedAt, userID)
	if err := row.Scan(&b.CreatedAt); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("update book %s: %w", b.ID, err)
	}
	return nil
}

func (s *PostgresStore) Delete(ctx context.Context, userID, id string) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM books WHERE id=$1 AND user_id=$2", id, userID)
	if err != nil {
		return fmt.Errorf("delete book %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- Shared per-ISBN editions & covers (global metadata, not user-scoped) ---

// EnsureEdition creates a pending edition row for an ISBN if none exists yet, so
// the reconciler and worker have something to act on. It is a no-op when the
// edition already exists (any status), so it never resets prior enrichment.
func (s *PostgresStore) EnsureEdition(ctx context.Context, isbn string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO editions (isbn, enrichment_status) VALUES ($1, 'pending') ON CONFLICT (isbn) DO NOTHING`, isbn)
	if err != nil {
		return fmt.Errorf("ensure edition %s: %w", isbn, err)
	}
	return nil
}

// MarkEditionPending forces an edition back to pending (used by the manual refresh
// endpoint) so the worker re-fetches even if it was already enriched or failed.
func (s *PostgresStore) MarkEditionPending(ctx context.Context, isbn string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO editions (isbn, enrichment_status) VALUES ($1, 'pending')
		 ON CONFLICT (isbn) DO UPDATE SET enrichment_status='pending'`, isbn)
	if err != nil {
		return fmt.Errorf("mark edition pending %s: %w", isbn, err)
	}
	return nil
}

func (s *PostgresStore) PendingEnrichment(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT isbn FROM editions WHERE enrichment_status IN ('pending','failed') ORDER BY isbn LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending editions: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0)
	for rows.Next() {
		var isbn string
		if err := rows.Scan(&isbn); err != nil {
			return nil, fmt.Errorf("scan pending isbn: %w", err)
		}
		out = append(out, isbn)
	}
	return out, rows.Err()
}

func (s *PostgresStore) EditionStatus(ctx context.Context, isbn string) (enrich.Status, bool, error) {
	var status string
	err := s.pool.QueryRow(ctx, `SELECT enrichment_status FROM editions WHERE isbn=$1`, isbn).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("edition status %s: %w", isbn, err)
	}
	return enrich.Status(status), true, nil
}

func (s *PostgresStore) SaveEnrichment(ctx context.Context, isbn string, m *enrich.Metadata, status enrich.Status) error {
	// A nil m (e.g. a failed lookup) updates only the status; the metadata columns
	// keep whatever they had.
	if m == nil {
		_, err := s.pool.Exec(ctx,
			`INSERT INTO editions (isbn, enrichment_status) VALUES ($1, $2)
			 ON CONFLICT (isbn) DO UPDATE SET enrichment_status=EXCLUDED.enrichment_status`, isbn, string(status))
		if err != nil {
			return fmt.Errorf("save enrichment status %s: %w", isbn, err)
		}
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO editions (isbn, title, author, description, publisher, published_date, page_count, enrichment_status, enriched_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
		ON CONFLICT (isbn) DO UPDATE SET
			title=EXCLUDED.title, author=EXCLUDED.author, description=EXCLUDED.description,
			publisher=EXCLUDED.publisher, published_date=EXCLUDED.published_date, page_count=EXCLUDED.page_count,
			enrichment_status=EXCLUDED.enrichment_status, enriched_at=EXCLUDED.enriched_at`,
		isbn, m.Title, m.Author, m.Description, m.Publisher, m.PublishedDate, m.PageCount, string(status))
	if err != nil {
		return fmt.Errorf("save enrichment %s: %w", isbn, err)
	}
	return nil
}

func (s *PostgresStore) SaveCover(ctx context.Context, isbn string, c *enrich.Cover) error {
	// Upsert the bytes and flip the edition's has_cover flag atomically.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin cover tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		INSERT INTO book_covers (isbn, content_type, bytes, etag, source_url, fetched_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (isbn) DO UPDATE SET
			content_type=EXCLUDED.content_type, bytes=EXCLUDED.bytes, etag=EXCLUDED.etag,
			source_url=EXCLUDED.source_url, fetched_at=EXCLUDED.fetched_at`,
		isbn, c.ContentType, c.Bytes, c.ETag, c.SourceURL); err != nil {
		return fmt.Errorf("save cover %s: %w", isbn, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE editions SET has_cover=true WHERE isbn=$1`, isbn); err != nil {
		return fmt.Errorf("mark has_cover %s: %w", isbn, err)
	}
	return tx.Commit(ctx)
}

// CoverForBook returns the cover bytes for a book the caller owns. Ownership is
// enforced by joining through books on user_id, so one user cannot read a cover
// via another user's book id; a missing/foreign book yields ErrNotFound.
func (s *PostgresStore) CoverForBook(ctx context.Context, userID, bookID string) (*enrich.Cover, error) {
	var (
		c    enrich.Cover
		etag *string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT c.content_type, c.bytes, c.etag
		FROM book_covers c
		JOIN books b ON b.isbn = c.isbn
		WHERE b.id=$1 AND b.user_id=$2`, bookID, userID).Scan(&c.ContentType, &c.Bytes, &etag)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("cover for book %s: %w", bookID, err)
	}
	c.ETag = strDeref(etag)
	return &c, nil
}

func strDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// enrichmentStatusFor reports the status to surface in a Book response: the shared
// edition's status when present, "skipped" when the book has no ISBN, otherwise
// "pending" (the edition row has not been created yet).
func enrichmentStatusFor(isbn string, editionStatus *string) string {
	if editionStatus != nil {
		return *editionStatus
	}
	if isbn == "" {
		return "skipped"
	}
	return string(enrich.StatusPending)
}
