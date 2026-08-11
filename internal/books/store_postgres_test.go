package books_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"ex-libris-api/internal/books"
)

// pgUser is the owner used by the single-user Postgres tests; the scoping test
// introduces a second user of its own.
const pgUser = "user-1"

// testPool is a pool against a single throwaway Postgres container shared by all
// tests in this package. It is nil if Docker is unavailable, in which case the
// Postgres tests skip rather than fail.
var testPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()

	// Disable the Ryuk resource reaper: it is unreliable under some local Docker
	// backends (e.g. colima), and TestMain terminates the container itself below,
	// so the reaper adds nothing here.
	os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")

	// pgvector/pgvector ships the vector extension the migration enables.
	pgC, err := postgres.Run(ctx, "pgvector/pgvector:pg16",
		postgres.WithDatabase("exlibris"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		// No usable Docker daemon: leave testPool nil so the Postgres tests skip.
		// Log the reason so a genuine startup failure is not silently invisible.
		fmt.Fprintf(os.Stderr, "postgres container unavailable, skipping Postgres store tests: %v\n", err)
		os.Exit(m.Run())
	}

	connString, err := pgC.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		panic(err)
	}
	testPool, err = pgxpool.New(ctx, connString)
	if err != nil {
		panic(err)
	}
	if err := books.Migrate(ctx, testPool); err != nil {
		panic(err)
	}

	code := m.Run()

	testPool.Close()
	// Best-effort container teardown; the test process is exiting regardless.
	_ = testcontainers.TerminateContainer(pgC)
	os.Exit(code)
}

// newPostgresStore returns a store against a freshly-truncated books table, so
// each test starts from a known-empty state on the shared container.
func newPostgresStore(t *testing.T) *books.PostgresStore {
	t.Helper()
	if testPool == nil {
		t.Skip("Docker not available; skipping Postgres store tests")
	}
	if _, err := testPool.Exec(context.Background(), "TRUNCATE books, editions, book_covers CASCADE"); err != nil {
		t.Fatalf("truncate tables: %v", err)
	}
	return books.NewPostgresStore(testPool)
}

func TestPostgresCreateAssignsIDDefaultsAndTimestamps(t *testing.T) {
	store := newPostgresStore(t)
	ctx := context.Background()

	b := &books.Book{Title: "La Taupe", Author: "John le Carré", ISBN: "9782070368224"}
	before := time.Now().UTC()
	if err := store.Create(ctx, pgUser, b); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if b.ID == "" {
		t.Error("expected Create to assign an ID")
	}
	if b.UserID != pgUser {
		t.Errorf("expected UserID %q, got %q", pgUser, b.UserID)
	}
	if b.Status != books.StatusToRead {
		t.Errorf("expected default status %q, got %q", books.StatusToRead, b.Status)
	}
	if b.CreatedAt.Before(before) || b.UpdatedAt.Before(before) {
		t.Errorf("expected timestamps set at or after %v, got created=%v updated=%v", before, b.CreatedAt, b.UpdatedAt)
	}

	// The row must actually be persisted; query directly so this test exercises
	// only Create (not Get).
	var title, author, isbn, status, userID string
	err := testPool.QueryRow(ctx,
		"SELECT title, author, isbn, status, user_id FROM books WHERE id=$1", b.ID,
	).Scan(&title, &author, &isbn, &status, &userID)
	if err != nil {
		t.Fatalf("query persisted row: %v", err)
	}
	if title != "La Taupe" || author != "John le Carré" || isbn != "9782070368224" ||
		status != string(books.StatusToRead) || userID != pgUser {
		t.Errorf("persisted row mismatch: title=%q author=%q isbn=%q status=%q user_id=%q", title, author, isbn, status, userID)
	}
}

func TestPostgresGetRoundTripsAllFields(t *testing.T) {
	store := newPostgresStore(t)
	ctx := context.Background()

	b := &books.Book{
		ISBN:    "9781476706122",
		Title:   "Red Sparrow",
		Author:  "Jason Matthews",
		Status:  books.StatusRead,
		Rating:  4,
		Opinion: "tense",
		Tags:    []string{"spy", "thriller"},
	}
	if err := store.Create(ctx, pgUser, b); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Get(ctx, pgUser, b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != b.ID || got.Title != b.Title || got.Author != b.Author ||
		got.Status != b.Status || got.Rating != b.Rating || got.Opinion != b.Opinion {
		t.Errorf("scalar fields mismatch: %+v", got)
	}
	if got.UserID != pgUser {
		t.Errorf("UserID = %q, want %q", got.UserID, pgUser)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "spy" || got.Tags[1] != "thriller" {
		t.Errorf("tags did not round-trip: %v", got.Tags)
	}
	if !got.CreatedAt.Equal(b.CreatedAt) || !got.UpdatedAt.Equal(b.UpdatedAt) {
		t.Errorf("timestamps did not round-trip: created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}
}

func TestPostgresGetUnknownReturnsErrNotFound(t *testing.T) {
	store := newPostgresStore(t)

	_, err := store.Get(context.Background(), pgUser, "does-not-exist")
	if !errors.Is(err, books.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestPostgresListReturnsEmptySliceWhenNoBooks(t *testing.T) {
	store := newPostgresStore(t)

	got, err := store.List(context.Background(), pgUser)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty list, got %d books", len(got))
	}
}

func TestPostgresListOrdersByCreatedAt(t *testing.T) {
	store := newPostgresStore(t)
	ctx := context.Background()

	titles := []string{"first", "second", "third"}
	for i, title := range titles {
		// Distinct synthetic ISBNs: books.isbn is constrained, and reusing one
		// would silently share an edition between the rows.
		isbn := fmt.Sprintf("978%010d", i)
		if err := store.Create(ctx, pgUser, &books.Book{ISBN: isbn, Title: title, Author: "a"}); err != nil {
			t.Fatalf("Create %q: %v", title, err)
		}
		// Guarantee strictly increasing created_at so the ordering assertion is
		// deterministic rather than dependent on sub-microsecond insert timing.
		time.Sleep(time.Millisecond)
	}

	got, err := store.List(ctx, pgUser)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != len(titles) {
		t.Fatalf("expected %d books, got %d", len(titles), len(got))
	}
	for i, want := range titles {
		if got[i].Title != want {
			t.Errorf("position %d: expected %q, got %q", i, want, got[i].Title)
		}
	}
}

func TestPostgresUpdatePersistsAndPreservesCreatedAt(t *testing.T) {
	store := newPostgresStore(t)
	ctx := context.Background()

	b := &books.Book{ISBN: "9781476706122", Title: "Red Sparrow", Author: "Jason Matthews"}
	if err := store.Create(ctx, pgUser, b); err != nil {
		t.Fatalf("Create: %v", err)
	}
	createdAt := b.CreatedAt

	// A caller could pass a bogus CreatedAt; like MemoryStore, Update must keep
	// the original and refresh UpdatedAt regardless.
	b.Status = books.StatusRead
	b.Rating = 5
	b.Tags = []string{"spy"}
	b.CreatedAt = time.Now().Add(-100 * time.Hour)
	time.Sleep(time.Millisecond)

	if err := store.Update(ctx, pgUser, b); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !b.CreatedAt.Equal(createdAt) {
		t.Errorf("expected CreatedAt preserved as %v, got %v", createdAt, b.CreatedAt)
	}
	if !b.UpdatedAt.After(createdAt) {
		t.Errorf("expected UpdatedAt refreshed after %v, got %v", createdAt, b.UpdatedAt)
	}

	got, err := store.Get(ctx, pgUser, b.ID)
	if err != nil {
		t.Fatalf("Get after Update: %v", err)
	}
	if got.Status != books.StatusRead || got.Rating != 5 || len(got.Tags) != 1 || got.Tags[0] != "spy" {
		t.Errorf("update not persisted: %+v", got)
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Errorf("persisted CreatedAt changed: got %v want %v", got.CreatedAt, createdAt)
	}
}

func TestPostgresUpdateUnknownReturnsErrNotFound(t *testing.T) {
	store := newPostgresStore(t)

	err := store.Update(context.Background(), pgUser, &books.Book{ID: "does-not-exist", Title: "x", Author: "y"})
	if !errors.Is(err, books.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestPostgresDeleteRemovesBook(t *testing.T) {
	store := newPostgresStore(t)
	ctx := context.Background()

	b := &books.Book{ISBN: "9780140183528", Title: "Kim", Author: "Kipling"}
	if err := store.Create(ctx, pgUser, b); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.Delete(ctx, pgUser, b.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, pgUser, b.ID); !errors.Is(err, books.ErrNotFound) {
		t.Fatalf("expected book gone after Delete, Get returned %v", err)
	}
}

func TestPostgresDeleteUnknownReturnsErrNotFound(t *testing.T) {
	store := newPostgresStore(t)

	err := store.Delete(context.Background(), pgUser, "does-not-exist")
	if !errors.Is(err, books.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestPostgresScopesByUser is the core multi-tenant guarantee: one user's book is
// invisible and immutable to another, and List only ever returns the caller's.
func TestPostgresScopesByUser(t *testing.T) {
	store := newPostgresStore(t)
	ctx := context.Background()

	const alice, bob = "alice", "bob"
	a := &books.Book{ISBN: "9780140183528", Title: "Kim", Author: "Kipling"}
	if err := store.Create(ctx, alice, a); err != nil {
		t.Fatalf("Create as alice: %v", err)
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

	// Alice's book is untouched by all of bob's attempts.
	if got, err := store.Get(ctx, alice, a.ID); err != nil || got.Title != "Kim" {
		t.Errorf("alice's book should be intact: got=%+v err=%v", got, err)
	}
}

// TestDatabaseRejectsISBNlessBook proves migration 0004 actually ran: the API
// guards the door, but the point of the constraint is that nothing else — a
// script, a psql session, a future handler bug — can put a ghost book in the
// table either.
func TestDatabaseRejectsISBNlessBook(t *testing.T) {
	newPostgresStore(t) // skips when Docker is absent, and truncates the tables

	ctx := context.Background()
	insert := `INSERT INTO books (id, user_id, isbn, title, author, status, rating, created_at, updated_at)
	           VALUES ($1, 'alice', $2, 'Ghost', 'Nobody', 'to_read', 0, now(), now())`

	for _, tt := range []struct {
		name string
		isbn string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"not an isbn", "nope"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := testPool.Exec(ctx, insert, "ghost-"+tt.name, tt.isbn); err == nil {
				t.Fatalf("insert with isbn %q succeeded; the constraint is missing", tt.isbn)
			}
		})
	}

	// A hyphenated ISBN written before canonicalisation existed stays legal.
	if _, err := testPool.Exec(ctx, insert, "legacy", "978-2-07-036822-4"); err != nil {
		t.Errorf("legacy hyphenated isbn was rejected: %v", err)
	}
}

// TestMigrationRemovesExistingGhostBooks exercises the destructive half of
// migration 0004 against data that already exists. The other tests truncate
// first, so without this the DELETE-then-constrain ordering would never be
// proven — and getting it backwards would abort every startup on a database
// that has ghosts in it.
func TestMigrationRemovesExistingGhostBooks(t *testing.T) {
	newPostgresStore(t) // skips without Docker, truncates the tables
	ctx := context.Background()

	// Recreate the pre-migration world: no constraint, and a ghost row in it.
	if _, err := testPool.Exec(ctx, `ALTER TABLE books DROP CONSTRAINT books_isbn_shape`); err != nil {
		t.Fatalf("drop constraint: %v", err)
	}
	if _, err := testPool.Exec(ctx, `ALTER TABLE books ALTER COLUMN isbn DROP NOT NULL`); err != nil {
		t.Fatalf("drop not null: %v", err)
	}
	insert := `INSERT INTO books (id, user_id, isbn, title, author, status, rating, created_at, updated_at)
	           VALUES ($1, 'alice', $2, $3, 'Nobody', 'to_read', 0, now(), now())`
	for _, row := range []struct{ id, isbn, title string }{
		{"ghost-empty", "", "Ghost"},
		{"keeper", "9782070368224", "La Taupe"},
		{"keeper-hyphenated", "978-0-14-032872-1", "Legacy"},
	} {
		if _, err := testPool.Exec(ctx, insert, row.id, row.isbn, row.title); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO editions (isbn) VALUES ('')`); err != nil {
		t.Fatalf("seed empty edition: %v", err)
	}

	if err := books.Migrate(ctx, testPool); err != nil {
		t.Fatalf("re-running migrations over existing data: %v", err)
	}

	var remaining []string
	rows, err := testPool.Query(ctx, `SELECT id FROM books ORDER BY id`)
	if err != nil {
		t.Fatalf("query books: %v", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		remaining = append(remaining, id)
	}
	rows.Close()

	// The ghost is gone; the real book and the legacy hyphenated one survive.
	want := []string{"keeper", "keeper-hyphenated"}
	if len(remaining) != len(want) || remaining[0] != want[0] || remaining[1] != want[1] {
		t.Errorf("books after migration = %v, want %v", remaining, want)
	}

	// And the constraint is back, so migrations stay idempotent across restarts.
	if _, err := testPool.Exec(ctx, insert, "ghost-again", "", "Ghost"); err == nil {
		t.Error("constraint was not re-applied: a ghost book was accepted")
	}
	if err := books.Migrate(ctx, testPool); err != nil {
		t.Errorf("second re-run of migrations: %v", err)
	}
}
