package enrich

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// lastSearchQuery records the query string of the most recent /search.json hit so
// tests can assert on what went over the wire.
var lastSearchQuery url.Values

// mockOpenLibrary serves the Open Library endpoints the provider uses, so the
// provider can be tested without touching the network.
func mockOpenLibrary(t *testing.T) *OpenLibraryProvider {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/search.json", func(w http.ResponseWriter, r *http.Request) {
		lastSearchQuery = r.URL.Query()
		// Three works: one with several ISBNs across editions, one with only an
		// ISBN-10, and one with none at all.
		fmt.Fprint(w, `{"numFound":3,"docs":[
			{"title":"Fantastic Mr Fox","author_name":["Roald Dahl"],
			 "first_publish_year":1970,"publisher":["Puffin","Knopf"],
			 "isbn":["0140328726","978-0-14-032872-1","9780241558324"]},
			{"title":"Only Ten","author_name":["A. Nother","Co Author"],
			 "first_publish_year":1999,"isbn":["080442957X"]},
			{"title":"No ISBN At All","author_name":["Ghost"]}
		]}`)
	})
	mux.HandleFunc("/api/books", func(w http.ResponseWriter, r *http.Request) {
		// Respond only for the known ISBN; anything else gets an empty object,
		// which is how Open Library signals "unknown".
		if r.URL.Query().Get("bibkeys") == "ISBN:9780140328721" {
			fmt.Fprint(w, `{"ISBN:9780140328721":{
				"title":"Fantastic Mr Fox",
				"authors":[{"name":"Roald Dahl"}],
				"publishers":[{"name":"Puffin"}],
				"publish_date":"1988",
				"number_of_pages":96,
				"excerpts":[{"text":"Boggis, Bunce and Bean."}]
			}}`)
			return
		}
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("/b/isbn/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/b/isbn/9780140328721-L.jpg" {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("ETag", `"cover-v1"`)
			w.Write([]byte("\xff\xd8\xff\xe0JPEGDATA")) // fake but non-empty image bytes
			return
		}
		http.NotFound(w, r) // default=false behaviour: 404 when no cover
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return NewOpenLibraryProvider(srv.URL, srv.URL)
}

func TestOpenLibraryLookup(t *testing.T) {
	p := mockOpenLibrary(t)

	m, err := p.Lookup(context.Background(), "9780140328721")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if m.Title != "Fantastic Mr Fox" || m.Author != "Roald Dahl" || m.Publisher != "Puffin" {
		t.Errorf("unexpected metadata: %+v", m)
	}
	if m.PublishedDate != "1988" || m.PageCount != 96 {
		t.Errorf("unexpected date/pages: %+v", m)
	}
	if m.Description == "" {
		t.Errorf("expected a best-effort description from the excerpt")
	}
}

func TestOpenLibraryLookupUnknownISBN(t *testing.T) {
	p := mockOpenLibrary(t)

	_, err := p.Lookup(context.Background(), "0000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestOpenLibraryFetchCover(t *testing.T) {
	p := mockOpenLibrary(t)

	c, err := p.FetchCover(context.Background(), "9780140328721")
	if err != nil {
		t.Fatalf("FetchCover: %v", err)
	}
	if c.ContentType != "image/jpeg" || len(c.Bytes) == 0 {
		t.Errorf("unexpected cover: type=%q len=%d", c.ContentType, len(c.Bytes))
	}
	if c.ETag != `"cover-v1"` {
		t.Errorf("expected ETag to be carried through, got %q", c.ETag)
	}
}

func TestOpenLibraryFetchCoverMissing(t *testing.T) {
	p := mockOpenLibrary(t)

	_, err := p.FetchCover(context.Background(), "0000000000000")
	if !errors.Is(err, ErrNoCover) {
		t.Fatalf("expected ErrNoCover, got %v", err)
	}
}

func TestOpenLibrarySearchMapsResults(t *testing.T) {
	p := mockOpenLibrary(t)

	got, err := p.Search(context.Background(), "fantastic mr fox", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// The ISBN-less work is dropped: a suggestion that cannot be added is worse
	// than no suggestion.
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 (the ISBN-less work dropped): %+v", len(got), got)
	}

	first := got[0]
	// Of the three candidates, the 13-digit one wins — that is what a scanner
	// would have produced for the same book.
	if first.ISBN != "9780140328721" {
		t.Errorf("first ISBN = %q, want the 13-digit 9780140328721", first.ISBN)
	}
	if first.Title != "Fantastic Mr Fox" {
		t.Errorf("first title = %q", first.Title)
	}
	if first.Author != "Roald Dahl" {
		t.Errorf("first author = %q", first.Author)
	}
	if first.Year != 1970 {
		t.Errorf("first year = %d, want 1970", first.Year)
	}
	if first.Publisher != "Puffin" {
		t.Errorf("first publisher = %q, want the first listed", first.Publisher)
	}

	// With no 13-digit candidate the ISBN-10 is used as-is.
	if got[1].ISBN != "080442957X" {
		t.Errorf("second ISBN = %q, want the ISBN-10 fallback", got[1].ISBN)
	}
	if got[1].Author != "A. Nother, Co Author" {
		t.Errorf("second author = %q, want the joined list", got[1].Author)
	}
}

func TestOpenLibrarySearchSendsQueryFieldsAndLimit(t *testing.T) {
	p := mockOpenLibrary(t)

	if _, err := p.Search(context.Background(), "la taupe", 5); err != nil {
		t.Fatalf("Search: %v", err)
	}

	if got := lastSearchQuery.Get("q"); got != "la taupe" {
		t.Errorf("q = %q", got)
	}
	// Restricting `fields` drops everything not named, so omitting isbn would
	// make every result unusable.
	if fields := lastSearchQuery.Get("fields"); !strings.Contains(fields, "isbn") {
		t.Errorf("fields = %q, must include isbn", fields)
	}
	if got := lastSearchQuery.Get("limit"); got != "5" {
		t.Errorf("limit = %q, want 5", got)
	}
}

func TestOpenLibrarySearchClampsLimit(t *testing.T) {
	p := mockOpenLibrary(t)

	for _, tt := range []struct{ in, want string }{
		{"0", "10"},
		{"-3", "10"},
		{"999", "25"},
	} {
		var limit int
		fmt.Sscanf(tt.in, "%d", &limit)
		if _, err := p.Search(context.Background(), "x", limit); err != nil {
			t.Fatalf("Search(limit=%d): %v", limit, err)
		}
		if got := lastSearchQuery.Get("limit"); got != tt.want {
			t.Errorf("limit %d went out as %q, want %q", limit, got, tt.want)
		}
	}
}

func TestOpenLibrarySearchEmptyQuery(t *testing.T) {
	p := mockOpenLibrary(t)

	got, err := p.Search(context.Background(), "   ", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d results for a blank query, want none", len(got))
	}
}

func TestOpenLibrarySearchUpstreamFailure(t *testing.T) {
	// 400 rather than 5xx: get() retries 5xx three times with backoff, which
	// would make this test slow for no extra coverage.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	p := NewOpenLibraryProvider(srv.URL, srv.URL)

	if _, err := p.Search(context.Background(), "anything", 10); err == nil {
		t.Fatal("expected an error from a failing upstream")
	}
}
