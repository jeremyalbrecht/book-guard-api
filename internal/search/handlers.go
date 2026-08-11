// Package search exposes free-text book search over the enrichment provider, so
// a user can add a book they are holding but whose barcode they cannot scan.
//
// It is separate from internal/books because a search result is not a book: it
// is a candidate the user has not chosen yet, and nothing is stored until they
// do. Only the ISBN survives the choice — everything else about the book comes
// from enriching that ISBN, exactly as it does for a scan.
package search

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"ex-libris-api/internal/auth"
	"ex-libris-api/internal/enrich"
)

const (
	// A user-facing request must fail fast. The provider retries three times
	// against a 10s client timeout, so without a budget here an unreachable
	// Open Library would leave the caller waiting some twenty seconds.
	defaultTimeout = 5 * time.Second

	// Long enough to absorb a user backspacing over a title and retyping it,
	// short enough that a corrected catalogue entry shows up the same session.
	cacheTTL     = 60 * time.Second
	cacheMaxSize = 256
)

type Handler struct {
	// searcher may be nil, which means search is not configured; the endpoint
	// then reports 503 rather than pretending nothing matched.
	searcher enrich.Searcher
	logger   *slog.Logger
	timeout  time.Duration
	cache    *resultCache
}

func NewHandler(searcher enrich.Searcher, logger *slog.Logger) *Handler {
	return &Handler{
		searcher: searcher,
		logger:   logger,
		timeout:  defaultTimeout,
		cache:    newResultCache(),
	}
}

// SearchHit is one candidate edition. Deliberately flat and small: it is rendered
// as a row in a suggestion list while the user is still typing.
type SearchHit struct {
	ISBN      string `json:"isbn" example:"9782070368224"`
	Title     string `json:"title" example:"La Taupe"`
	Author    string `json:"author,omitempty" example:"John le Carré"`
	Year      int    `json:"year,omitempty" example:"1974"`
	Publisher string `json:"publisher,omitempty" example:"Gallimard"`
}

type searchInput struct {
	Q string `query:"q" required:"true" minLength:"3" maxLength:"200" doc:"Free-text title and/or author" example:"la taupe le carré"`
	// Defaults are applied by Huma from the tag, so a client can omit it.
	Limit int `query:"limit" minimum:"1" maximum:"25" default:"10" doc:"Maximum number of suggestions"`
}

// The results are wrapped in an object rather than returned as a bare array so
// fields like a truncation flag can be added later without breaking clients.
type searchOutput struct {
	Body struct {
		Results []SearchHit `json:"results"`
	}
}

func (h *Handler) Register(api huma.API, authMW func(huma.Context, func(huma.Context))) {
	huma.Register(api, huma.Operation{
		OperationID: "search-editions",
		Method:      http.MethodGet,
		Path:        "/search",
		Summary:     "Search editions by title or author",
		Description: "Finds candidate editions for a book whose barcode cannot be scanned. " +
			"Results carry an ISBN, which is all POST /books needs; the title and author shown " +
			"here come from the upstream work record and may differ from the edition that ends " +
			"up being enriched.",
		Tags:        []string{"search"},
		Security:    []map[string][]string{{"bearer": {}}},
		Middlewares: huma.Middlewares{authMW},
		Errors: []int{
			http.StatusUnprocessableEntity,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout,
		},
	}, h.search)
}

func (h *Handler) search(ctx context.Context, in *searchInput) (*searchOutput, error) {
	// Defence in depth: the middleware guarantees an identity, and searching is
	// a privilege of being signed in even though nothing is stored.
	if id, ok := auth.FromContext(ctx); !ok || id.Subject == "" {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	if h.searcher == nil {
		return nil, huma.Error503ServiceUnavailable("search is not available")
	}

	query := strings.TrimSpace(in.Q)
	if hits, ok := h.cache.get(query, in.Limit); ok {
		return respond(hits), nil
	}

	// Bound the upstream call rather than the whole request, so a slow provider
	// produces a 504 instead of holding the connection open.
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	found, err := h.searcher.Search(ctx, query, in.Limit)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, huma.Error504GatewayTimeout("search timed out")
		}
		h.logger.Error("search", "query", query, "error", err)
		return nil, huma.Error502BadGateway("search provider unavailable")
	}

	hits := make([]SearchHit, 0, len(found))
	for _, r := range found {
		hits = append(hits, SearchHit{
			ISBN:      r.ISBN,
			Title:     r.Title,
			Author:    r.Author,
			Year:      r.Year,
			Publisher: r.Publisher,
		})
	}
	h.cache.put(query, in.Limit, hits)
	return respond(hits), nil
}

// respond wraps hits in the output envelope, guaranteeing a non-nil slice so the
// JSON is `[]` and never `null`.
func respond(hits []SearchHit) *searchOutput {
	if hits == nil {
		hits = []SearchHit{}
	}
	out := &searchOutput{}
	out.Body.Results = hits
	return out
}

// resultCache is a small TTL cache in front of the provider. Search is issued per
// keystroke and nothing else in this service rate-limits anything, so without it
// a user retyping a title hammers Open Library — whose 429s the provider's retry
// would amplify rather than relieve.
type resultCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	hits      []SearchHit
	expiresAt time.Time
}

func newResultCache() *resultCache {
	return &resultCache{entries: make(map[string]cacheEntry)}
}

// cacheKey folds case and spacing, because a user editing a query passes through
// the same text repeatedly in slightly different shapes.
func cacheKey(query string, limit int) string {
	return strings.ToLower(strings.Join(strings.Fields(query), " ")) + "\x00" + strconv.Itoa(limit)
}

func (c *resultCache) get(query string, limit int) ([]SearchHit, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[cacheKey(query, limit)]
	if !ok || time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return entry.hits, true
}

func (c *resultCache) put(query string, limit int, hits []SearchHit) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Bounded so a long session cannot grow it without limit. Dropping expired
	// entries first usually makes room; if it does not, evict the nearest to
	// expiry, which is the least recently stored.
	if len(c.entries) >= cacheMaxSize {
		now := time.Now()
		var oldestKey string
		var oldestAt time.Time
		for k, e := range c.entries {
			if now.After(e.expiresAt) {
				delete(c.entries, k)
				continue
			}
			if oldestAt.IsZero() || e.expiresAt.Before(oldestAt) {
				oldestKey, oldestAt = k, e.expiresAt
			}
		}
		if len(c.entries) >= cacheMaxSize && oldestKey != "" {
			delete(c.entries, oldestKey)
		}
	}

	c.entries[cacheKey(query, limit)] = cacheEntry{hits: hits, expiresAt: time.Now().Add(cacheTTL)}
}
