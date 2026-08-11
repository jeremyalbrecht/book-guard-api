package books_test

import (
	"context"
	"testing"

	"ex-libris-api/internal/books"
	"ex-libris-api/internal/enrich"
)

// canonicalStore is the slice of store behaviour the canonical-title suite needs.
// Both MemoryStore and PostgresStore satisfy it, so the same assertions run
// against each.
type canonicalStore interface {
	Create(ctx context.Context, userID string, b *books.Book) error
	Get(ctx context.Context, userID, id string) (*books.Book, error)
	List(ctx context.Context, userID string) ([]*books.Book, error)
	EnsureEdition(ctx context.Context, isbn string) error
	SaveEnrichment(ctx context.Context, isbn string, m *enrich.Metadata, s enrich.Status) error
}

func TestMemoryCanonicalTitleFallback(t *testing.T) {
	runCanonicalTitleSuite(t, books.NewMemoryStore())
}

func TestPostgresCanonicalTitleFallback(t *testing.T) {
	runCanonicalTitleSuite(t, newPostgresStore(t))
}

// runCanonicalTitleSuite covers the point of surfacing editions.title/author: a
// book created from a bare ISBN carries no title of its own, so reads fall back
// to the shared edition's canonical values. A book that *does* have its own
// title keeps it — the user's wording always wins over the upstream one.
func runCanonicalTitleSuite(t *testing.T, store canonicalStore) {
	ctx := context.Background()
	const isbn = "9782070368224"

	metadata := &enrich.Metadata{Title: "La Taupe", Author: "John le Carré"}

	// A book added by ISBN alone, before the worker has enriched anything.
	bare := &books.Book{ISBN: isbn}
	if err := store.Create(ctx, "alice", bare); err != nil {
		t.Fatalf("create bare book: %v", err)
	}
	if err := store.EnsureEdition(ctx, isbn); err != nil {
		t.Fatalf("ensure edition: %v", err)
	}

	got, err := store.Get(ctx, "alice", bare.ID)
	if err != nil {
		t.Fatalf("get before enrichment: %v", err)
	}
	if got.Title != "" || got.Author != "" {
		t.Errorf("before enrichment: title/author = %q/%q, want empty", got.Title, got.Author)
	}

	if err := store.SaveEnrichment(ctx, isbn, metadata, enrich.StatusEnriched); err != nil {
		t.Fatalf("save enrichment: %v", err)
	}

	got, err = store.Get(ctx, "alice", bare.ID)
	if err != nil {
		t.Fatalf("get after enrichment: %v", err)
	}
	if got.Title != metadata.Title {
		t.Errorf("title = %q, want %q from the edition", got.Title, metadata.Title)
	}
	if got.Author != metadata.Author {
		t.Errorf("author = %q, want %q from the edition", got.Author, metadata.Author)
	}

	// The same fallback must apply on List, not just Get.
	list, err := store.List(ctx, "alice")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list returned %d books, want 1", len(list))
	}
	if list[0].Title != metadata.Title {
		t.Errorf("list title = %q, want %q", list[0].Title, metadata.Title)
	}

	// A book the user titled themselves is never overwritten by the edition.
	own := &books.Book{ISBN: isbn, Title: "La Taupe (édition de poche)", Author: "le Carré"}
	if err := store.Create(ctx, "bob", own); err != nil {
		t.Fatalf("create titled book: %v", err)
	}
	got, err = store.Get(ctx, "bob", own.ID)
	if err != nil {
		t.Fatalf("get titled book: %v", err)
	}
	if got.Title != own.Title || got.Author != own.Author {
		t.Errorf("user's own title/author = %q/%q, want %q/%q",
			got.Title, got.Author, own.Title, own.Author)
	}
}
