package enrich

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// lastGoogleBooksQuery records the query string of the most recent /volumes hit
// so tests can assert on what went over the wire (e.g. the API key).
var lastGoogleBooksQuery url.Values

// mockGoogleBooks serves the Google Books endpoint the provider uses, so the
// provider can be tested without touching the network.
func mockGoogleBooks(t *testing.T, apiKey string) *GoogleBooksProvider {
	t.Helper()
	// srvURL is filled in once the server is listening: the volumes handler needs
	// to point the thumbnail link back at the same server's cover route.
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/books/v1/volumes", func(w http.ResponseWriter, r *http.Request) {
		lastGoogleBooksQuery = r.URL.Query()
		if r.URL.Query().Get("q") == "isbn:9782070368224" {
			fmt.Fprintf(w, `{"totalItems":1,"items":[{"volumeInfo":{
				"title":"Le Petit Prince",
				"authors":["Antoine de Saint-Exupéry"],
				"publisher":"Gallimard",
				"publishedDate":"1946",
				"description":"A classic novella.",
				"pageCount":96,
				"imageLinks":{"thumbnail":"%s/books/content?id=abc&img=1"}
			}}]}`, srvURL)
			return
		}
		fmt.Fprint(w, `{"totalItems":0,"items":[]}`)
	})
	mux.HandleFunc("/books/content", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("\xff\xd8\xff\xe0JPEGDATA"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	srvURL = srv.URL
	return NewGoogleBooksProvider(srv.URL, apiKey)
}

func TestGoogleBooksLookup(t *testing.T) {
	p := mockGoogleBooks(t, "")

	m, err := p.Lookup(context.Background(), "9782070368224")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if m.Title != "Le Petit Prince" || m.Author != "Antoine de Saint-Exupéry" {
		t.Errorf("unexpected metadata: %+v", m)
	}
	if m.Publisher != "Gallimard" || m.PublishedDate != "1946" || m.PageCount != 96 {
		t.Errorf("unexpected publisher/date/pages: %+v", m)
	}
	if m.Description != "A classic novella." {
		t.Errorf("unexpected description: %+v", m)
	}
}

func TestGoogleBooksLookupUnknownISBN(t *testing.T) {
	p := mockGoogleBooks(t, "")

	_, err := p.Lookup(context.Background(), "0000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGoogleBooksLookupSendsAPIKey(t *testing.T) {
	p := mockGoogleBooks(t, "test-key")

	if _, err := p.Lookup(context.Background(), "9782070368224"); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got := lastGoogleBooksQuery.Get("key"); got != "test-key" {
		t.Errorf("key = %q, want test-key", got)
	}
}

func TestGoogleBooksLookupOmitsKeyWhenUnset(t *testing.T) {
	p := mockGoogleBooks(t, "")

	if _, err := p.Lookup(context.Background(), "9782070368224"); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if lastGoogleBooksQuery.Has("key") {
		t.Errorf("expected no key param, got %q", lastGoogleBooksQuery.Get("key"))
	}
}

func TestGoogleBooksFetchCover(t *testing.T) {
	p := mockGoogleBooks(t, "")

	c, err := p.FetchCover(context.Background(), "9782070368224")
	if err != nil {
		t.Fatalf("FetchCover: %v", err)
	}
	if c.ContentType != "image/jpeg" || len(c.Bytes) == 0 {
		t.Errorf("unexpected cover: type=%q len=%d", c.ContentType, len(c.Bytes))
	}
}

// FetchCover on an ISBN with no volume at all surfaces ErrNotFound (from the
// underlying lookup), not ErrNoCover — the worker only calls FetchCover after a
// successful Lookup, so in practice this case can't be reached, but the
// distinction (unknown book vs. known book with no image) matters for anyone
// calling the provider directly.
func TestGoogleBooksFetchCoverUnknownISBN(t *testing.T) {
	p := mockGoogleBooks(t, "")

	_, err := p.FetchCover(context.Background(), "0000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGoogleBooksFetchCoverNoImageLinks(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/books/v1/volumes", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"totalItems":1,"items":[{"volumeInfo":{"title":"No Cover Book"}}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p := NewGoogleBooksProvider(srv.URL, "")

	_, err := p.FetchCover(context.Background(), "9782070368224")
	if !errors.Is(err, ErrNoCover) {
		t.Fatalf("expected ErrNoCover, got %v", err)
	}
}

func TestGoogleBooksFetchCoverRejectsOversized(t *testing.T) {
	// srvURL is filled in once the server is listening: the volumes handler needs
	// to point the thumbnail link back at the same server's cover route.
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/books/v1/volumes", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"totalItems":1,"items":[{"volumeInfo":{
			"title":"Big Cover Book",
			"imageLinks":{"thumbnail":"%s/books/content"}
		}}]}`, srvURL)
	})
	mux.HandleFunc("/books/content", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(make([]byte, defaultCoverMaxBytes+1))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	srvURL = srv.URL

	p := NewGoogleBooksProvider(srv.URL, "")
	_, err := p.FetchCover(context.Background(), "9782070368224")
	if err == nil {
		t.Fatal("expected an error for an oversized cover")
	}
}

func TestGoogleBooksLookupUpstreamFailure(t *testing.T) {
	// 400 rather than 5xx: the retry helper retries 5xx three times with backoff,
	// which would make this test slow for no extra coverage.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	p := NewGoogleBooksProvider(srv.URL, "")

	if _, err := p.Lookup(context.Background(), "9782070368224"); err == nil {
		t.Fatal("expected an error from a failing upstream")
	}
}
