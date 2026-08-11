package search_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"ex-libris-api/internal/auth"
	"ex-libris-api/internal/enrich"
	"ex-libris-api/internal/search"
)

// tokenAsSubjectVerifier treats the raw bearer token as the user's subject, so a
// test can act as any user by varying the header while the real auth middleware
// still runs. (A six-line copy of the one in books_test: exporting it would mean
// a test-only package for a single type.)
type tokenAsSubjectVerifier struct{}

func (tokenAsSubjectVerifier) Verify(_ context.Context, rawToken string) (auth.Identity, error) {
	return auth.Identity{Subject: rawToken, Username: rawToken}, nil
}

// fakeSearcher stands in for Open Library, and counts calls so the cache can be
// observed from the outside.
type fakeSearcher struct {
	mu      sync.Mutex
	results []enrich.SearchResult
	err     error
	calls   int
}

func (f *fakeSearcher) Search(_ context.Context, _ string, _ int) ([]enrich.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.results, f.err
}

func (f *fakeSearcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newTestServer(t *testing.T, searcher enrich.Searcher) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("test", "1.0.0"))
	search.NewHandler(searcher, logger).Register(api, auth.NewHumaMiddleware(api, tokenAsSubjectVerifier{}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, path, user string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if user != "" {
		req.Header.Set("Authorization", "Bearer "+user)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

type searchBody struct {
	Results []struct {
		ISBN      string `json:"isbn"`
		Title     string `json:"title"`
		Author    string `json:"author"`
		Year      int    `json:"year"`
		Publisher string `json:"publisher"`
	} `json:"results"`
}

func TestSearchReturnsResults(t *testing.T) {
	fake := &fakeSearcher{results: []enrich.SearchResult{
		{ISBN: "9780140328721", Title: "Fantastic Mr Fox", Author: "Roald Dahl", Publisher: "Puffin", Year: 1970},
		{ISBN: "080442957X", Title: "Only Ten", Author: "A. Nother"},
	}}
	srv := newTestServer(t, fake)

	resp := get(t, srv, "/search?q=fantastic", "alice")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body searchBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(body.Results))
	}
	if body.Results[0].ISBN != "9780140328721" || body.Results[0].Title != "Fantastic Mr Fox" {
		t.Errorf("first result = %+v", body.Results[0])
	}
	if body.Results[0].Year != 1970 || body.Results[0].Publisher != "Puffin" {
		t.Errorf("first result lost its year or publisher: %+v", body.Results[0])
	}
}

// TestSearchWithNoHitsIsNotAnError: an empty shelf of suggestions is a normal
// answer, and a 404 would make the UI show an error state for it.
func TestSearchWithNoHitsIsNotAnError(t *testing.T) {
	srv := newTestServer(t, &fakeSearcher{results: nil})

	resp := get(t, srv, "/search?q=zzzzzz", "alice")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	raw, _ := io.ReadAll(resp.Body)
	// An explicit [] rather than null, so clients can iterate without a nil check.
	if !json.Valid(raw) {
		t.Fatalf("invalid JSON: %s", raw)
	}
	var body searchBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Results == nil {
		t.Errorf("results was null, want an empty array: %s", raw)
	}
}

func TestSearchValidatesQuery(t *testing.T) {
	srv := newTestServer(t, &fakeSearcher{})

	for _, tt := range []struct{ name, path string }{
		{"missing q", "/search"},
		{"q too short", "/search?q=ab"},
		{"limit below range", "/search?q=taupe&limit=0"},
		{"limit above range", "/search?q=taupe&limit=99"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp := get(t, srv, tt.path, "alice")
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Errorf("GET %s = %d, want 422", tt.path, resp.StatusCode)
			}
		})
	}
}

func TestSearchRequiresAuthentication(t *testing.T) {
	srv := newTestServer(t, &fakeSearcher{})

	resp := get(t, srv, "/search?q=taupe", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestSearchUpstreamFailureIsBadGateway(t *testing.T) {
	srv := newTestServer(t, &fakeSearcher{err: context.Canceled})

	resp := get(t, srv, "/search?q=taupe", "alice")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestSearchTimeoutIsGatewayTimeout(t *testing.T) {
	srv := newTestServer(t, &fakeSearcher{err: context.DeadlineExceeded})

	resp := get(t, srv, "/search?q=taupe", "alice")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
}

// TestSearchWithoutASearcherIsUnavailable covers the ENRICH_DISABLED-style wiring
// where no provider exists: say so plainly rather than pretending there are no
// results.
func TestSearchWithoutASearcherIsUnavailable(t *testing.T) {
	srv := newTestServer(t, nil)

	resp := get(t, srv, "/search?q=taupe", "alice")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// TestSearchServesRepeatQueryFromCache matters because search runs per keystroke
// against a public API that asks for good citizenship, and nothing else in this
// codebase rate-limits anything.
func TestSearchServesRepeatQueryFromCache(t *testing.T) {
	fake := &fakeSearcher{results: []enrich.SearchResult{{ISBN: "9780140328721", Title: "Fantastic Mr Fox"}}}
	srv := newTestServer(t, fake)

	for i := 0; i < 3; i++ {
		resp := get(t, srv, "/search?q=fantastic+mr+fox", "alice")
		resp.Body.Close()
	}
	// Case and surrounding whitespace must not defeat the cache: a user
	// backspacing over a title reproduces the same query constantly.
	resp := get(t, srv, "/search?q=Fantastic+Mr+Fox+", "alice")
	resp.Body.Close()

	if got := fake.callCount(); got != 1 {
		t.Errorf("upstream called %d times, want 1", got)
	}

	// A different query still goes upstream.
	resp = get(t, srv, "/search?q=la+taupe", "alice")
	resp.Body.Close()
	if got := fake.callCount(); got != 2 {
		t.Errorf("upstream called %d times after a new query, want 2", got)
	}
}
