package books_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"ex-libris-api/internal/books"
)

// TestCreateAssignsUUIDv7 pins the ID format: a valid, version-7 UUID, so IDs are
// globally unique across datacenters without coordination.
func TestCreateAssignsUUIDv7(t *testing.T) {
	store := books.NewMemoryStore()
	b := &books.Book{Title: "Kim", Author: "Kipling"}
	if err := store.Create(context.Background(), "alice", b); err != nil {
		t.Fatalf("Create: %v", err)
	}
	id, err := uuid.Parse(b.ID)
	if err != nil {
		t.Fatalf("ID %q is not a valid UUID: %v", b.ID, err)
	}
	if id.Version() != 7 {
		t.Errorf("expected UUID version 7, got v%d (%s)", id.Version(), b.ID)
	}
}

// TestMemoryScopesByUser mirrors the Postgres scoping test for the in-memory
// store, so both Store implementations are held to the same isolation contract.
func TestMemoryScopesByUser(t *testing.T) {
	store := books.NewMemoryStore()
	ctx := context.Background()

	const alice, bob = "alice", "bob"
	a := &books.Book{Title: "Kim", Author: "Kipling"}
	if err := store.Create(ctx, alice, a); err != nil {
		t.Fatalf("Create as alice: %v", err)
	}
	if a.UserID != alice {
		t.Fatalf("expected UserID alice, got %q", a.UserID)
	}

	if _, err := store.Get(ctx, bob, a.ID); !errors.Is(err, books.ErrNotFound) {
		t.Errorf("bob Get of alice's book: expected ErrNotFound, got %v", err)
	}
	if err := store.Update(ctx, bob, &books.Book{ID: a.ID, Title: "hijack", Author: "x"}); !errors.Is(err, books.ErrNotFound) {
		t.Errorf("bob Update of alice's book: expected ErrNotFound, got %v", err)
	}
	if err := store.Delete(ctx, bob, a.ID); !errors.Is(err, books.ErrNotFound) {
		t.Errorf("bob Delete of alice's book: expected ErrNotFound, got %v", err)
	}

	bobList, err := store.List(ctx, bob)
	if err != nil {
		t.Fatalf("List as bob: %v", err)
	}
	if len(bobList) != 0 {
		t.Errorf("bob should see no books, got %d", len(bobList))
	}

	aliceList, err := store.List(ctx, alice)
	if err != nil {
		t.Fatalf("List as alice: %v", err)
	}
	if len(aliceList) != 1 || aliceList[0].Title != "Kim" {
		t.Errorf("alice should see exactly her book, got %+v", aliceList)
	}
}

// TestLegacyISBNlessBookReportsSkipped covers rows that predate the mandatory
// ISBN. The API rejects them and migration 0004 removed them from Postgres, but
// the store must still describe one sensibly if it meets one.
func TestLegacyISBNlessBookReportsSkipped(t *testing.T) {
	store := books.NewMemoryStore()
	ctx := context.Background()

	legacy := &books.Book{Title: "Journal", Author: "Me"}
	if err := store.Create(ctx, "alice", legacy); err != nil {
		t.Fatalf("create legacy book: %v", err)
	}

	got, err := store.Get(ctx, "alice", legacy.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.EnrichmentStatus != "skipped" {
		t.Errorf("enrichment status = %q, want skipped", got.EnrichmentStatus)
	}
}
