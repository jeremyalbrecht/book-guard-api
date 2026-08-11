package enrich

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
)

// fakeStore is an in-memory enrich.Store for exercising the worker logic.
type fakeStore struct {
	mu      sync.Mutex
	status  map[string]Status
	meta    map[string]*Metadata
	covers  map[string]*Cover
	pending []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{status: map[string]Status{}, meta: map[string]*Metadata{}, covers: map[string]*Cover{}}
}

func (s *fakeStore) PendingEnrichment(_ context.Context, _ int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.pending...), nil
}

func (s *fakeStore) EditionStatus(_ context.Context, isbn string) (Status, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.status[isbn]
	return st, ok, nil
}

func (s *fakeStore) SaveEnrichment(_ context.Context, isbn string, m *Metadata, status Status) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[isbn] = status
	if m != nil {
		s.meta[isbn] = m
	}
	return nil
}

func (s *fakeStore) SaveCover(_ context.Context, isbn string, c *Cover) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.covers[isbn] = c
	return nil
}

// stubProvider returns canned metadata/covers keyed by ISBN.
type stubProvider struct {
	meta      map[string]*Metadata
	cover     map[string]*Cover
	lookupErr map[string]error
	calls     int
}

func (p *stubProvider) Lookup(_ context.Context, isbn string) (*Metadata, error) {
	p.calls++
	if err := p.lookupErr[isbn]; err != nil {
		return nil, err
	}
	if m := p.meta[isbn]; m != nil {
		return m, nil
	}
	return nil, ErrNotFound
}

func (p *stubProvider) FetchCover(_ context.Context, isbn string) (*Cover, error) {
	if c := p.cover[isbn]; c != nil {
		return c, nil
	}
	return nil, ErrNoCover
}

func testWorker(store Store, provider Provider) *Worker {
	return NewWorker(store, provider, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{})
}

func TestProcessEnrichesMetadataAndCover(t *testing.T) {
	store := newFakeStore()
	store.status["isbn1"] = StatusPending
	prov := &stubProvider{
		meta:  map[string]*Metadata{"isbn1": {Title: "T", Author: "A"}},
		cover: map[string]*Cover{"isbn1": {ContentType: "image/jpeg", Bytes: []byte("img")}},
	}
	w := testWorker(store, prov)

	w.process(context.Background(), "isbn1")

	if store.status["isbn1"] != StatusEnriched {
		t.Errorf("status = %q, want enriched", store.status["isbn1"])
	}
	if store.meta["isbn1"].Title != "T" {
		t.Errorf("metadata not saved: %+v", store.meta["isbn1"])
	}
	if store.covers["isbn1"] == nil {
		t.Errorf("cover not saved")
	}
}

func TestProcessSkipsAlreadyEnriched(t *testing.T) {
	store := newFakeStore()
	store.status["isbn1"] = StatusEnriched
	prov := &stubProvider{} // any Lookup would return ErrNotFound and fail the edition
	w := testWorker(store, prov)

	w.process(context.Background(), "isbn1")

	if prov.calls != 0 {
		t.Errorf("expected provider not to be called for an already-enriched edition")
	}
	if store.status["isbn1"] != StatusEnriched {
		t.Errorf("status changed unexpectedly to %q", store.status["isbn1"])
	}
}

func TestProcessMarksFailedWhenUpstreamMissing(t *testing.T) {
	store := newFakeStore()
	store.status["isbn1"] = StatusPending
	w := testWorker(store, &stubProvider{}) // no meta => ErrNotFound

	w.process(context.Background(), "isbn1")

	if store.status["isbn1"] != StatusFailed {
		t.Errorf("status = %q, want failed", store.status["isbn1"])
	}
}

func TestProcessSavesMetadataEvenWithoutCover(t *testing.T) {
	store := newFakeStore()
	store.status["isbn1"] = StatusPending
	prov := &stubProvider{meta: map[string]*Metadata{"isbn1": {Title: "T"}}} // no cover
	w := testWorker(store, prov)

	w.process(context.Background(), "isbn1")

	if store.status["isbn1"] != StatusEnriched {
		t.Errorf("status = %q, want enriched", store.status["isbn1"])
	}
	if store.covers["isbn1"] != nil {
		t.Errorf("did not expect a cover to be saved")
	}
}

func TestEnqueueDeduplicatesInFlight(t *testing.T) {
	w := testWorker(newFakeStore(), &stubProvider{})

	w.Enqueue("isbn1")
	w.Enqueue("isbn1") // duplicate while first is still queued

	if got := len(w.queue); got != 1 {
		t.Fatalf("expected 1 queued item, got %d", got)
	}
}

func TestReconcileEnqueuesPending(t *testing.T) {
	store := newFakeStore()
	store.pending = []string{"a", "b"}
	w := testWorker(store, &stubProvider{})

	w.reconcile(context.Background())

	got := map[string]bool{}
	for len(w.queue) > 0 {
		got[<-w.queue] = true
	}
	if !got["a"] || !got["b"] {
		t.Errorf("expected pending isbns enqueued, got %v", got)
	}
}

// Guard: worker satisfies the Enqueuer interface handlers depend on.
var _ Enqueuer = (*Worker)(nil)

// Guard: a Lookup error other than ErrNotFound still fails the edition.
func TestProcessMarksFailedOnLookupError(t *testing.T) {
	store := newFakeStore()
	store.status["isbn1"] = StatusPending
	prov := &stubProvider{lookupErr: map[string]error{"isbn1": errors.New("boom")}}
	w := testWorker(store, prov)

	w.process(context.Background(), "isbn1")

	if store.status["isbn1"] != StatusFailed {
		t.Errorf("status = %q, want failed", store.status["isbn1"])
	}
}
