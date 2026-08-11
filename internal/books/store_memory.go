package books

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"ex-libris-api/internal/enrich"
)

var _ enrich.Store = (*MemoryStore)(nil)

// memEdition is the in-memory twin of the shared `editions` row: per-ISBN metadata
// that all users' books referencing that ISBN share.
type memEdition struct {
	Title, Author, Description, Publisher, PublishedDate string
	PageCount                                            int
	Status                                               enrich.Status
	EnrichedAt                                           *time.Time
	HasCover                                             bool
}

type MemoryStore struct {
	mu       sync.RWMutex
	books    map[string]*Book
	editions map[string]*memEdition   // keyed by ISBN (global)
	covers   map[string]*enrich.Cover // keyed by ISBN (global)
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		books:    make(map[string]*Book),
		editions: make(map[string]*memEdition),
		covers:   make(map[string]*enrich.Cover),
	}
}

// newID returns a UUIDv7 string. v7 is random-based, so any datacenter can mint
// one with no coordination, but it embeds a timestamp and is therefore
// time-ordered — which keeps primary-key index inserts local instead of
// scattering them the way fully-random v4 would. uuid.Must panics only if the
// system RNG fails, which is unrecoverable and should crash loudly.
func newID() string {
	return uuid.Must(uuid.NewV7()).String()
}

func (s *MemoryStore) Create(ctx context.Context, userID string, b *Book) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	b.ID = newID()
	b.UserID = userID
	if b.Status == "" {
		b.Status = StatusToRead
	}
	now := time.Now().UTC()
	b.CreatedAt = now
	b.UpdatedAt = now

	s.books[b.ID] = b
	return nil
}

// ownedBook returns the book with id only if it belongs to userID. A book owned
// by someone else is reported as ErrNotFound so its existence never leaks.
func (s *MemoryStore) ownedBook(userID, id string) (*Book, bool) {
	b, ok := s.books[id]
	if !ok || b.UserID != userID {
		return nil, false
	}
	return b, true
}

func (s *MemoryStore) Get(ctx context.Context, userID, id string) (*Book, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	b, ok := s.ownedBook(userID, id)
	if !ok {
		return nil, ErrNotFound
	}
	cp := *b
	s.applyEdition(&cp)
	return &cp, nil
}

// applyEdition merges the shared per-ISBN edition into a book copy. The caller
// must hold at least the read lock. It mirrors the Postgres LEFT JOIN, including
// the "skipped" status for a book with no ISBN.
func (s *MemoryStore) applyEdition(b *Book) {
	if e, ok := s.editions[b.ISBN]; ok && b.ISBN != "" {
		applyCanonicalNames(b, e.Title, e.Author)
		b.Description = e.Description
		b.Publisher = e.Publisher
		b.PublishedDate = e.PublishedDate
		b.PageCount = e.PageCount
		b.EnrichmentStatus = string(e.Status)
		b.EnrichedAt = e.EnrichedAt
		b.HasCover = e.HasCover
		return
	}
	b.Description, b.Publisher, b.PublishedDate = "", "", ""
	b.PageCount = 0
	b.EnrichedAt = nil
	b.HasCover = false
	b.EnrichmentStatus = enrichmentStatusFor(b.ISBN, nil)
}

func (s *MemoryStore) List(ctx context.Context, userID string) ([]*Book, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*Book, 0)
	for _, b := range s.books {
		if b.UserID != userID {
			continue
		}
		cp := *b
		s.applyEdition(&cp)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (s *MemoryStore) Update(ctx context.Context, userID string, b *Book) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.ownedBook(userID, b.ID)
	if !ok {
		return ErrNotFound
	}
	b.UserID = userID
	b.CreatedAt = existing.CreatedAt
	b.UpdatedAt = time.Now().UTC()
	s.books[b.ID] = b
	return nil
}

func (s *MemoryStore) Delete(ctx context.Context, userID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.ownedBook(userID, id); !ok {
		return ErrNotFound
	}
	delete(s.books, id)
	return nil
}

// --- Shared per-ISBN editions & covers (global metadata, not user-scoped) ---

func (s *MemoryStore) EnsureEdition(ctx context.Context, isbn string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.editions[isbn]; !ok {
		s.editions[isbn] = &memEdition{Status: enrich.StatusPending}
	}
	return nil
}

func (s *MemoryStore) MarkEditionPending(ctx context.Context, isbn string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.editions[isbn]
	if !ok {
		e = &memEdition{}
		s.editions[isbn] = e
	}
	e.Status = enrich.StatusPending
	return nil
}

func (s *MemoryStore) PendingEnrichment(ctx context.Context, limit int) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0)
	for isbn, e := range s.editions {
		if e.Status == enrich.StatusPending || e.Status == enrich.StatusFailed {
			out = append(out, isbn)
		}
	}
	sort.Strings(out) // stable order (map iteration is random)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) EditionStatus(ctx context.Context, isbn string) (enrich.Status, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.editions[isbn]
	if !ok {
		return "", false, nil
	}
	return e.Status, true, nil
}

func (s *MemoryStore) SaveEnrichment(ctx context.Context, isbn string, m *enrich.Metadata, status enrich.Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.editions[isbn]
	if !ok {
		e = &memEdition{}
		s.editions[isbn] = e
	}
	e.Status = status
	if m != nil {
		e.Title, e.Author = m.Title, m.Author
		e.Description, e.Publisher, e.PublishedDate = m.Description, m.Publisher, m.PublishedDate
		e.PageCount = m.PageCount
		now := time.Now().UTC()
		e.EnrichedAt = &now
	}
	return nil
}

func (s *MemoryStore) SaveCover(ctx context.Context, isbn string, c *enrich.Cover) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *c
	s.covers[isbn] = &cp
	e, ok := s.editions[isbn]
	if !ok {
		e = &memEdition{Status: enrich.StatusPending}
		s.editions[isbn] = e
	}
	e.HasCover = true
	return nil
}

func (s *MemoryStore) CoverForBook(ctx context.Context, userID, bookID string) (*enrich.Cover, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.ownedBook(userID, bookID)
	if !ok {
		return nil, ErrNotFound
	}
	c, ok := s.covers[b.ISBN]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *c
	return &cp, nil
}
