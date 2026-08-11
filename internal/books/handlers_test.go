package books_test

import (
	"bytes"
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
	"ex-libris-api/internal/books"
	"ex-libris-api/internal/enrich"
)

// tokenAsSubjectVerifier is a test Verifier that treats the raw bearer token as
// the user's subject. It lets one test server act as many users just by varying
// the Authorization header, while still exercising the real auth middleware.
type tokenAsSubjectVerifier struct{}

func (tokenAsSubjectVerifier) Verify(_ context.Context, rawToken string) (auth.Identity, error) {
	return auth.Identity{Subject: rawToken, Username: rawToken}, nil
}

// fakeEnqueuer records the ISBNs handlers enqueue, so tests can assert enrichment
// was triggered without running the real worker.
type fakeEnqueuer struct {
	mu    sync.Mutex
	isbns []string
}

func (f *fakeEnqueuer) Enqueue(isbn string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isbns = append(f.isbns, isbn)
}

func (f *fakeEnqueuer) enqueued() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.isbns...)
}

// testAPI bundles a running server with the store and enqueuer behind it, so tests
// that need to seed data or inspect enqueue calls can reach them.
type testAPI struct {
	srv   *httptest.Server
	store *books.MemoryStore
	enq   *fakeEnqueuer
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	store := books.NewMemoryStore()
	enq := &fakeEnqueuer{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := books.NewHandler(store, enq, logger)
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("test", "1.0.0"))
	// Register with the real Huma auth middleware so handlers receive an Identity.
	h.Register(api, auth.NewHumaMiddleware(api, tokenAsSubjectVerifier{}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &testAPI{srv: srv, store: store, enq: enq}
}

// newTestServer is the minimal server used by the CRUD tests that don't need to
// reach the store or enqueuer.
func newTestServer() *httptest.Server {
	return newTestAPIServer()
}

func newTestAPIServer() *httptest.Server {
	store := books.NewMemoryStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := books.NewHandler(store, &fakeEnqueuer{}, logger)
	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("test", "1.0.0"))
	h.Register(api, auth.NewHumaMiddleware(api, tokenAsSubjectVerifier{}))
	return httptest.NewServer(mux)
}

// request issues an authenticated request as user, returning the response.
func request(t *testing.T, srv *httptest.Server, method, path, user, body string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+user)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func TestCreateAndGetBook(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()

	body := `{"title":"La Taupe","author":"John le Carré","isbn":"9782070368224"}`
	resp := request(t, srv, http.MethodPost, "/books", "alice", body)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var created books.Book
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected an ID to be assigned")
	}
	if created.Status != books.StatusToRead {
		t.Fatalf("expected default status %q, got %q", books.StatusToRead, created.Status)
	}

	getResp := request(t, srv, http.MethodGet, "/books/"+created.ID, "alice", "")
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", getResp.StatusCode)
	}
}

func TestCreateBookValidation(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"title and author but no isbn", `{"title":"Kim","author":"Kipling"}`, http.StatusUnprocessableEntity},
		{"empty isbn", `{"title":"Kim","author":"Kipling","isbn":""}`, http.StatusUnprocessableEntity},
		{"isbn that is not one", `{"isbn":"abc"}`, http.StatusUnprocessableEntity},
		{"malformed json", `{not-json`, http.StatusBadRequest},
		{"isbn alone", `{"isbn":"9782070368224"}`, http.StatusCreated},
		{"isbn with hyphens", `{"isbn":"978-2-07-036822-4"}`, http.StatusCreated},
		{"isbn with title and author", `{"title":"Kim","author":"Kipling","isbn":"0140328726"}`, http.StatusCreated},
	}

	srv := newTestServer()
	defer srv.Close()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := request(t, srv, http.MethodPost, "/books", "alice", tt.body)
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("expected %d, got %d", tt.wantStatus, resp.StatusCode)
			}
		})
	}
}

func TestGetUnknownBookReturns404(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()

	resp := request(t, srv, http.MethodGet, "/books/does-not-exist", "alice", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestPatchIsPartial(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()

	resp := request(t, srv, http.MethodPost, "/books", "alice", `{"title":"Red Sparrow","author":"Jason Matthews","isbn":"9781476706122"}`)
	var created books.Book
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	patchResp := request(t, srv, http.MethodPatch, "/books/"+created.ID, "alice", `{"status":"read","rating":4}`)
	defer patchResp.Body.Close()

	var updated books.Book
	if err := json.NewDecoder(patchResp.Body).Decode(&updated); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if updated.Title != "Red Sparrow" {
		t.Fatalf("expected title to survive the partial patch, got %q", updated.Title)
	}
	if updated.Rating != 4 {
		t.Fatalf("expected rating 4, got %d", updated.Rating)
	}
}

// TestBooksAreScopedPerUser verifies that one user cannot see, fetch, or delete
// another user's book through the HTTP layer.
func TestBooksAreScopedPerUser(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()

	resp := request(t, srv, http.MethodPost, "/books", "alice", `{"title":"Kim","author":"Kipling","isbn":"9780140183528"}`)
	var aliceBook books.Book
	json.NewDecoder(resp.Body).Decode(&aliceBook)
	resp.Body.Close()

	// Bob lists: must be empty.
	listResp := request(t, srv, http.MethodGet, "/books", "bob", "")
	var bobBooks []books.Book
	json.NewDecoder(listResp.Body).Decode(&bobBooks)
	listResp.Body.Close()
	if len(bobBooks) != 0 {
		t.Fatalf("bob should see no books, got %d", len(bobBooks))
	}

	// Bob tries to fetch Alice's book by id: must be 404, not 200.
	getResp := request(t, srv, http.MethodGet, "/books/"+aliceBook.ID, "bob", "")
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob fetching alice's book: expected 404, got %d", getResp.StatusCode)
	}

	// Bob tries to delete it: also 404, and Alice's book survives.
	delResp := request(t, srv, http.MethodDelete, "/books/"+aliceBook.ID, "bob", "")
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob deleting alice's book: expected 404, got %d", delResp.StatusCode)
	}
	aliceGet := request(t, srv, http.MethodGet, "/books/"+aliceBook.ID, "alice", "")
	aliceGet.Body.Close()
	if aliceGet.StatusCode != http.StatusOK {
		t.Fatalf("alice's book should still exist, got %d", aliceGet.StatusCode)
	}
}

func TestCreateEnqueuesEnrichment(t *testing.T) {
	ta := newTestAPI(t)

	// Every book now has an ISBN, so every create enqueues exactly one.
	resp := request(t, ta.srv, http.MethodPost, "/books", "alice",
		`{"title":"La Taupe","author":"le Carré","isbn":"9782070368224"}`)
	var withISBN books.Book
	json.NewDecoder(resp.Body).Decode(&withISBN)
	resp.Body.Close()
	if withISBN.EnrichmentStatus != "pending" {
		t.Errorf("status = %q, want pending", withISBN.EnrichmentStatus)
	}

	got := ta.enq.enqueued()
	if len(got) != 1 || got[0] != "9782070368224" {
		t.Fatalf("expected exactly the ISBN enqueued, got %v", got)
	}
}

func TestCoverEndpointServesBytesAndScopes(t *testing.T) {
	ta := newTestAPI(t)
	const isbn = "9782070368224"

	resp := request(t, ta.srv, http.MethodPost, "/books", "alice",
		`{"title":"La Taupe","author":"le Carré","isbn":"`+isbn+`"}`)
	var book books.Book
	json.NewDecoder(resp.Body).Decode(&book)
	resp.Body.Close()

	// Seed a cover directly on the shared edition (as the worker would).
	ctx := context.Background()
	ta.store.SaveEnrichment(ctx, isbn, &enrich.Metadata{Title: "T"}, enrich.StatusEnriched)
	if err := ta.store.SaveCover(ctx, isbn, &enrich.Cover{ContentType: "image/jpeg", Bytes: []byte("JPEGBYTES")}); err != nil {
		t.Fatalf("seed cover: %v", err)
	}

	// The owner gets the raw image with the right content type.
	coverResp := request(t, ta.srv, http.MethodGet, "/books/"+book.ID+"/cover", "alice", "")
	body, _ := io.ReadAll(coverResp.Body)
	coverResp.Body.Close()
	if coverResp.StatusCode != http.StatusOK {
		t.Fatalf("cover status = %d, want 200", coverResp.StatusCode)
	}
	if ct := coverResp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("content-type = %q, want image/jpeg", ct)
	}
	if string(body) != "JPEGBYTES" {
		t.Errorf("cover body = %q, want JPEGBYTES", body)
	}

	// A book's GET now advertises the cover URL.
	getResp := request(t, ta.srv, http.MethodGet, "/books/"+book.ID, "alice", "")
	var enriched books.Book
	json.NewDecoder(getResp.Body).Decode(&enriched)
	getResp.Body.Close()
	if enriched.CoverURL != "/books/"+book.ID+"/cover" {
		t.Errorf("cover_url = %q", enriched.CoverURL)
	}

	// Bob owns no book with this ISBN, so the cover route is 404 for him.
	bobResp := request(t, ta.srv, http.MethodGet, "/books/"+book.ID+"/cover", "bob", "")
	bobResp.Body.Close()
	if bobResp.StatusCode != http.StatusNotFound {
		t.Errorf("bob cover status = %d, want 404", bobResp.StatusCode)
	}
}

func TestRefreshQueuesOrRejects(t *testing.T) {
	ta := newTestAPI(t)

	resp := request(t, ta.srv, http.MethodPost, "/books", "alice",
		`{"title":"La Taupe","author":"le Carré","isbn":"9782070368224"}`)
	var book books.Book
	json.NewDecoder(resp.Body).Decode(&book)
	resp.Body.Close()

	refreshResp := request(t, ta.srv, http.MethodPost, "/books/"+book.ID+"/refresh", "alice", "")
	refreshResp.Body.Close()
	if refreshResp.StatusCode != http.StatusAccepted {
		t.Fatalf("refresh status = %d, want 202", refreshResp.StatusCode)
	}

	// A book with no ISBN cannot be refreshed. The API no longer lets one be
	// created, so this legacy shape is seeded straight into the store — the
	// handler guard still has to hold for rows that predate the rule.
	noISBN := &books.Book{Title: "Journal", Author: "Me"}
	if err := ta.store.Create(context.Background(), "alice", noISBN); err != nil {
		t.Fatalf("seed no-ISBN book: %v", err)
	}
	noISBNRefresh := request(t, ta.srv, http.MethodPost, "/books/"+noISBN.ID+"/refresh", "alice", "")
	noISBNRefresh.Body.Close()
	if noISBNRefresh.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("refresh of no-ISBN book = %d, want 422", noISBNRefresh.StatusCode)
	}
}

// TestCreateFromISBNAlone covers the scan flow: the client posts only the
// barcode it read, and the server's enrichment supplies title and author.
func TestCreateFromISBNAlone(t *testing.T) {
	api := newTestAPI(t)

	resp := request(t, api.srv, http.MethodPost, "/books", "alice", `{"isbn":"9782070368224"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for an ISBN-only create, got %d", resp.StatusCode)
	}

	var created books.Book
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected an ID to be assigned")
	}
	if created.EnrichmentStatus != string(enrich.StatusPending) {
		t.Errorf("enrichment_status = %q, want pending", created.EnrichmentStatus)
	}
	if got := api.enq.enqueued(); len(got) != 1 || got[0] != "9782070368224" {
		t.Errorf("enqueued = %v, want [9782070368224]", got)
	}

	// Once the worker has run, the book reads back with the edition's title.
	err := api.store.SaveEnrichment(context.Background(), "9782070368224",
		&enrich.Metadata{Title: "La Taupe", Author: "John le Carré"}, enrich.StatusEnriched)
	if err != nil {
		t.Fatalf("save enrichment: %v", err)
	}

	getResp := request(t, api.srv, http.MethodGet, "/books/"+created.ID, "alice", "")
	defer getResp.Body.Close()
	var fetched books.Book
	if err := json.NewDecoder(getResp.Body).Decode(&fetched); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if fetched.Title != "La Taupe" || fetched.Author != "John le Carré" {
		t.Errorf("title/author = %q/%q, want the enriched values", fetched.Title, fetched.Author)
	}
}

// TestCreateRequiresAnISBN is the whole point of the rule: a book with no ISBN
// is a ghost. Nothing can enrich it, nothing can name it canonically, and it can
// never have a cover — so it must not be possible to create one, no matter how
// much else the client sends.
func TestCreateRequiresAnISBN(t *testing.T) {
	srv := newTestServer()
	defer srv.Close()

	for _, body := range []string{`{}`, `{"title":"La Taupe"}`, `{"author":"le Carré"}`,
		`{"title":"La Taupe","author":"le Carré"}`, `{"title":"La Taupe","author":"le Carré","status":"read"}`} {
		resp := request(t, srv, http.MethodPost, "/books", "alice", body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("POST %s = %d, want 422", body, resp.StatusCode)
		}
	}
}

// TestCreateStoresCanonicalISBN proves the stored ISBN is separator-free. It
// matters beyond tidiness: books.isbn is the key of the shared edition and its
// cover, so "978-2-07-036822-4" and "9782070368224" arriving from two clients
// must not become two editions and two Open Library fetches.
func TestCreateStoresCanonicalISBN(t *testing.T) {
	ta := newTestAPI(t)

	resp := request(t, ta.srv, http.MethodPost, "/books", "alice", `{"isbn":"978-2-07-036822-4"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var created books.Book
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.ISBN != "9782070368224" {
		t.Errorf("created isbn = %q, want the stripped form", created.ISBN)
	}

	getResp := request(t, ta.srv, http.MethodGet, "/books/"+created.ID, "alice", "")
	defer getResp.Body.Close()
	var fetched books.Book
	if err := json.NewDecoder(getResp.Body).Decode(&fetched); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if fetched.ISBN != "9782070368224" {
		t.Errorf("stored isbn = %q, want the stripped form", fetched.ISBN)
	}

	// The edition key follows the same canonical form.
	if got := ta.enq.enqueued(); len(got) != 1 || got[0] != "9782070368224" {
		t.Errorf("enqueued = %v, want [9782070368224]", got)
	}
}

// TestPatchCannotClearOrCorruptISBN closes the other door into a ghost book:
// creating one is rejected, so updating a good book into one must be too.
func TestPatchCannotClearOrCorruptISBN(t *testing.T) {
	ta := newTestAPI(t)

	resp := request(t, ta.srv, http.MethodPost, "/books", "alice", `{"isbn":"9782070368224"}`)
	var created books.Book
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	for _, tt := range []struct {
		name string
		body string
	}{
		{"cleared", `{"isbn":""}`},
		{"not an isbn", `{"isbn":"nope"}`},
		{"truncated", `{"isbn":"978207036822"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			patch := request(t, ta.srv, http.MethodPatch, "/books/"+created.ID, "alice", tt.body)
			defer patch.Body.Close()
			if patch.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("PATCH %s = %d, want 422", tt.body, patch.StatusCode)
			}
		})
	}

	// A well-formed replacement is accepted, and canonicalised on the way in.
	patch := request(t, ta.srv, http.MethodPatch, "/books/"+created.ID, "alice",
		`{"isbn":"978-0-14-032872-1"}`)
	defer patch.Body.Close()
	if patch.StatusCode != http.StatusOK {
		t.Fatalf("PATCH with a valid isbn = %d, want 200", patch.StatusCode)
	}
	var updated books.Book
	if err := json.NewDecoder(patch.Body).Decode(&updated); err != nil {
		t.Fatalf("decode patch response: %v", err)
	}
	if updated.ISBN != "9780140328721" {
		t.Errorf("patched isbn = %q, want the stripped form", updated.ISBN)
	}
}
