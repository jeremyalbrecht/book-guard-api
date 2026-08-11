package books

import (
	"context"
	"errors"

	"ex-libris-api/internal/enrich"
)

var ErrNotFound = errors.New("book not found")

// Store persists books scoped to a user. Every method takes the owning user's ID
// (the OIDC subject) so no caller can reach another user's books: reads and
// writes that target a book owned by someone else behave as if it does not exist
// (ErrNotFound), rather than leaking its existence.
type Store interface {
	Create(ctx context.Context, userID string, b *Book) error
	Get(ctx context.Context, userID, id string) (*Book, error)
	List(ctx context.Context, userID string) ([]*Book, error)
	Update(ctx context.Context, userID string, b *Book) error
	Delete(ctx context.Context, userID, id string) error
}

// Repository is the full persistence surface the server wires up: the per-user
// Store, the shared edition/cover operations the HTTP handlers call, and the
// enrich.Store the background worker uses. Both MemoryStore and PostgresStore
// satisfy it, so one value serves the handlers and the worker.
type Repository interface {
	Store
	enrich.Store

	// EnsureEdition creates a pending edition for an ISBN if none exists (no-op
	// otherwise), so newly added ISBNs get picked up for enrichment.
	EnsureEdition(ctx context.Context, isbn string) error
	// MarkEditionPending forces an edition back to pending (manual refresh).
	MarkEditionPending(ctx context.Context, isbn string) error
	// CoverForBook returns the cover for a book the caller owns, else ErrNotFound.
	CoverForBook(ctx context.Context, userID, bookID string) (*enrich.Cover, error)
}
