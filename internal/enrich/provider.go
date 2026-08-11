// Package enrich fetches book metadata and cover images from an external source
// (Open Library) and persists them into the shared, per-ISBN edition entity. It is
// self-contained: it knows nothing about users, HTTP handlers, or the books
// package, and depends only on the small Store and Enqueuer interfaces defined
// here, so it is straightforward to test with fakes.
package enrich

import (
	"context"
	"errors"
)

// Status is the enrichment state of an edition (a per-ISBN metadata record).
type Status string

const (
	StatusPending  Status = "pending"
	StatusEnriched Status = "enriched"
	StatusFailed   Status = "failed"
)

// ErrNotFound means the provider has no metadata for the ISBN.
var ErrNotFound = errors.New("edition not found upstream")

// ErrNoCover means the provider has no cover image for the ISBN.
var ErrNoCover = errors.New("no cover available")

// Metadata is the subset of edition data we fetch and store.
type Metadata struct {
	Title         string
	Author        string
	Description   string
	Publisher     string
	PublishedDate string
	PageCount     int
}

// Cover is a downloaded cover image.
type Cover struct {
	ContentType string
	Bytes       []byte
	ETag        string
	SourceURL   string
}

// SearchResult is one candidate edition from a free-text search. It is
// deliberately thinner than Metadata: it is a picking list shown to a user, not
// data we store. Only the ISBN survives being chosen — everything the book ends
// up with comes from enriching that ISBN.
type SearchResult struct {
	ISBN      string
	Title     string
	Author    string
	Publisher string
	Year      int
}

// Searcher finds editions by free text. It is separate from Provider so the
// worker's test fakes need not grow a method they would never call, and so a
// search-only backend could exist later without a Lookup or FetchCover.
type Searcher interface {
	Search(ctx context.Context, query string, limit int) ([]SearchResult, error)
}

// Provider fetches edition metadata and covers from an external source.
type Provider interface {
	Lookup(ctx context.Context, isbn string) (*Metadata, error)
	FetchCover(ctx context.Context, isbn string) (*Cover, error)
}

// Store persists enrichment results. Everything is keyed by ISBN: editions and
// covers are global metadata shared across users, so there is no per-user scoping.
type Store interface {
	// PendingEnrichment returns up to limit ISBNs whose editions still need work
	// (status pending or failed). It backs the reconciler sweep.
	PendingEnrichment(ctx context.Context, limit int) ([]string, error)
	// EditionStatus reports an edition's current status; ok is false when no
	// edition row exists yet.
	EditionStatus(ctx context.Context, isbn string) (status Status, ok bool, err error)
	// SaveEnrichment records the outcome. A nil m updates only the status (used to
	// mark an edition failed without any metadata to store).
	SaveEnrichment(ctx context.Context, isbn string, m *Metadata, status Status) error
	SaveCover(ctx context.Context, isbn string, c *Cover) error
}

// Enqueuer schedules an ISBN for (re)enrichment. It is what the HTTP handlers
// depend on, so creating a book never blocks on the external source.
type Enqueuer interface {
	Enqueue(isbn string)
}
