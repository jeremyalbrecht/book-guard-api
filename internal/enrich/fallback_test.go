package enrich

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

func testFallback(primary, secondary Provider) *FallbackProvider {
	return NewFallbackProvider(primary, secondary, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestFallbackLookupUsesPrimaryWhenFound(t *testing.T) {
	primary := &stubProvider{meta: map[string]*Metadata{"isbn1": {Title: "Primary"}}}
	secondary := &stubProvider{meta: map[string]*Metadata{"isbn1": {Title: "Secondary"}}}
	f := testFallback(primary, secondary)

	m, err := f.Lookup(context.Background(), "isbn1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if m.Title != "Primary" {
		t.Errorf("title = %q, want Primary", m.Title)
	}
	if secondary.calls != 0 {
		t.Errorf("expected secondary not to be called, got %d calls", secondary.calls)
	}
}

func TestFallbackLookupFallsBackOnNotFound(t *testing.T) {
	primary := &stubProvider{} // no meta => ErrNotFound
	secondary := &stubProvider{meta: map[string]*Metadata{"isbn1": {Title: "Secondary"}}}
	f := testFallback(primary, secondary)

	m, err := f.Lookup(context.Background(), "isbn1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if m.Title != "Secondary" {
		t.Errorf("title = %q, want Secondary", m.Title)
	}
	if secondary.calls != 1 {
		t.Errorf("expected secondary to be called once, got %d", secondary.calls)
	}
}

func TestFallbackLookupBothMiss(t *testing.T) {
	primary := &stubProvider{}
	secondary := &stubProvider{}
	f := testFallback(primary, secondary)

	_, err := f.Lookup(context.Background(), "isbn1")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestFallbackLookupFallsBackOnTransientError(t *testing.T) {
	primary := &stubProvider{lookupErr: map[string]error{"isbn1": errors.New("boom")}}
	secondary := &stubProvider{meta: map[string]*Metadata{"isbn1": {Title: "Secondary"}}}
	f := testFallback(primary, secondary)

	m, err := f.Lookup(context.Background(), "isbn1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if m.Title != "Secondary" {
		t.Errorf("title = %q, want Secondary", m.Title)
	}
}

func TestFallbackFetchCoverUsesPrimaryWhenFound(t *testing.T) {
	primary := &stubProvider{cover: map[string]*Cover{"isbn1": {ContentType: "image/jpeg"}}}
	secondary := &stubProvider{cover: map[string]*Cover{"isbn1": {ContentType: "image/png"}}}
	f := testFallback(primary, secondary)

	c, err := f.FetchCover(context.Background(), "isbn1")
	if err != nil {
		t.Fatalf("FetchCover: %v", err)
	}
	if c.ContentType != "image/jpeg" {
		t.Errorf("content type = %q, want image/jpeg", c.ContentType)
	}
}

func TestFallbackFetchCoverFallsBackOnNoCover(t *testing.T) {
	primary := &stubProvider{} // no cover => ErrNoCover
	secondary := &stubProvider{cover: map[string]*Cover{"isbn1": {ContentType: "image/png"}}}
	f := testFallback(primary, secondary)

	c, err := f.FetchCover(context.Background(), "isbn1")
	if err != nil {
		t.Fatalf("FetchCover: %v", err)
	}
	if c.ContentType != "image/png" {
		t.Errorf("content type = %q, want image/png", c.ContentType)
	}
}

func TestFallbackFetchCoverBothMiss(t *testing.T) {
	primary := &stubProvider{}
	secondary := &stubProvider{}
	f := testFallback(primary, secondary)

	_, err := f.FetchCover(context.Background(), "isbn1")
	if !errors.Is(err, ErrNoCover) {
		t.Fatalf("expected ErrNoCover, got %v", err)
	}
}

// Guard: FallbackProvider satisfies the Provider interface the worker depends on.
var _ Provider = (*FallbackProvider)(nil)
