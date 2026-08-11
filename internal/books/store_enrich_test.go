package books_test

import (
	"context"
	"errors"
	"testing"

	"ex-libris-api/internal/books"
	"ex-libris-api/internal/enrich"
)

// enrichableStore is the slice of store behaviour the enrichment suite needs; both
// MemoryStore and PostgresStore satisfy it, so the same assertions run against both.
type enrichableStore interface {
	Create(ctx context.Context, userID string, b *books.Book) error
	Get(ctx context.Context, userID, id string) (*books.Book, error)
	EnsureEdition(ctx context.Context, isbn string) error
	MarkEditionPending(ctx context.Context, isbn string) error
	PendingEnrichment(ctx context.Context, limit int) ([]string, error)
	SaveEnrichment(ctx context.Context, isbn string, m *enrich.Metadata, s enrich.Status) error
	SaveCover(ctx context.Context, isbn string, c *enrich.Cover) error
	CoverForBook(ctx context.Context, userID, bookID string) (*enrich.Cover, error)
}

func TestMemoryEnrichmentSharedByISBN(t *testing.T) {
	runSharedEditionSuite(t, books.NewMemoryStore())
}

func TestPostgresEnrichmentSharedByISBN(t *testing.T) {
	runSharedEditionSuite(t, newPostgresStore(t))
}

// runSharedEditionSuite is the core guarantee of this feature: enrichment lives in
// a per-ISBN edition shared across users, so a second user adding the same ISBN
// sees the metadata and cover immediately with no refetch.
func runSharedEditionSuite(t *testing.T, store enrichableStore) {
	ctx := context.Background()
	const isbn = "9780140328721"

	alice := &books.Book{Title: "La Taupe", Author: "le Carré", ISBN: isbn}
	if err := store.Create(ctx, "alice", alice); err != nil {
		t.Fatalf("create alice book: %v", err)
	}
	if err := store.EnsureEdition(ctx, isbn); err != nil {
		t.Fatalf("ensure edition: %v", err)
	}

	// Before enrichment: pending (has ISBN, edition not yet enriched).
	if got, _ := store.Get(ctx, "alice", alice.ID); got.EnrichmentStatus != "pending" {
		t.Errorf("pre-enrichment status = %q, want pending", got.EnrichmentStatus)
	}

	// Enrich the shared edition once.
	meta := &enrich.Metadata{
		Title: "Tinker Tailor Soldier Spy", Author: "John le Carré",
		Description: "A mole hunt.", Publisher: "Hodder", PublishedDate: "1974", PageCount: 400,
	}
	if err := store.SaveEnrichment(ctx, isbn, meta, enrich.StatusEnriched); err != nil {
		t.Fatalf("save enrichment: %v", err)
	}
	if err := store.SaveCover(ctx, isbn, &enrich.Cover{ContentType: "image/jpeg", Bytes: []byte("JPEG"), ETag: `"v1"`}); err != nil {
		t.Fatalf("save cover: %v", err)
	}

	// Alice's book now surfaces the shared metadata + cover flag.
	gotA, err := store.Get(ctx, "alice", alice.ID)
	if err != nil {
		t.Fatalf("get alice book: %v", err)
	}
	if gotA.EnrichmentStatus != "enriched" || gotA.Description != "A mole hunt." ||
		gotA.Publisher != "Hodder" || gotA.PublishedDate != "1974" || gotA.PageCount != 400 || !gotA.HasCover {
		t.Errorf("alice book not enriched as expected: %+v", gotA)
	}
	if gotA.EnrichedAt == nil {
		t.Errorf("expected EnrichedAt to be set")
	}

	// The payoff: a SECOND user adds the same ISBN and sees enriched data at once,
	// while keeping their own title/author. No new enrichment call is involved.
	bob := &books.Book{Title: "My own title", Author: "My own author", ISBN: isbn}
	if err := store.Create(ctx, "bob", bob); err != nil {
		t.Fatalf("create bob book: %v", err)
	}
	gotB, err := store.Get(ctx, "bob", bob.ID)
	if err != nil {
		t.Fatalf("get bob book: %v", err)
	}
	if gotB.EnrichmentStatus != "enriched" || gotB.Description != "A mole hunt." || !gotB.HasCover {
		t.Errorf("shared edition not visible to second user: %+v", gotB)
	}
	if gotB.Title != "My own title" || gotB.Author != "My own author" {
		t.Errorf("second user's own title/author was overwritten: %+v", gotB)
	}

	// Cover is fetched by book id, with ownership enforced.
	cov, err := store.CoverForBook(ctx, "bob", bob.ID)
	if err != nil || string(cov.Bytes) != "JPEG" || cov.ContentType != "image/jpeg" {
		t.Errorf("bob CoverForBook = %+v, err=%v", cov, err)
	}
	if _, err := store.CoverForBook(ctx, "carol", bob.ID); !errors.Is(err, books.ErrNotFound) {
		t.Errorf("carol (owns no such book) CoverForBook: expected ErrNotFound, got %v", err)
	}

	// Enriched editions are not pending; MarkEditionPending re-queues them.
	if p, _ := store.PendingEnrichment(ctx, 10); len(p) != 0 {
		t.Errorf("expected no pending editions, got %v", p)
	}
	if err := store.MarkEditionPending(ctx, isbn); err != nil {
		t.Fatalf("mark pending: %v", err)
	}
	if p, _ := store.PendingEnrichment(ctx, 10); len(p) != 1 || p[0] != isbn {
		t.Errorf("expected [%s] pending, got %v", isbn, p)
	}
}
