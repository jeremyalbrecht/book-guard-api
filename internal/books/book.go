package books

import (
	"time"
)

type Status string

const (
	StatusToRead  Status = "to_read"
	StatusReading Status = "reading"
	StatusRead    Status = "read"
)

type Book struct {
	ID string `json:"id"`
	// UserID is the owning user's OIDC subject. It is set by the store from the
	// authenticated caller and never read from or written to client JSON, so one
	// user can never assign a book to another.
	UserID    string    `json:"-"`
	ISBN      string    `json:"isbn,omitempty"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	Status    Status    `json:"status"`
	Rating    int       `json:"rating,omitempty"`
	Opinion   string    `json:"opinion,omitempty"`
	Tags      []string  `json:"tags,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// The fields below are read-only enrichment data sourced from the shared,
	// per-ISBN edition (see internal/enrich). The store populates them on read
	// from a join; they are never written from client input. EnrichmentStatus is
	// "skipped" for a book with no ISBN.
	Description      string     `json:"description,omitempty"`
	Publisher        string     `json:"publisher,omitempty"`
	PublishedDate    string     `json:"published_date,omitempty"`
	PageCount        int        `json:"page_count,omitempty"`
	EnrichmentStatus string     `json:"enrichment_status,omitempty"`
	EnrichedAt       *time.Time `json:"enriched_at,omitempty"`
	HasCover         bool       `json:"-"`
	CoverURL         string     `json:"cover_url,omitempty"`
}

// applyCanonicalNames fills in a book's title/author from the shared edition when
// the book has none of its own. A book created from a bare ISBN (the scan flow)
// starts out nameless and gets its name here once the worker has enriched the
// edition; a book the user titled themselves is left alone, because their wording
// beats the upstream one. Both stores call this so they agree on the rule.
func applyCanonicalNames(b *Book, editionTitle, editionAuthor string) {
	if b.Title == "" {
		b.Title = editionTitle
	}
	if b.Author == "" {
		b.Author = editionAuthor
	}
}
