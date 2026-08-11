# CLAUDE.md — Ex-Libris

Self-hosted personal book-tracking API. Module `ex-libris-api` (in the `book-guard/`
directory). **Learning project**: the author is a platform engineer (Kubernetes,
OpenShift, Terraform, Istio) new to Go — keep code idiomatic, comment the *why* of
non-obvious decisions, and prefer clarity over cleverness.

## Commands
```bash
# Run locally (no auth, in-memory store — simplest):
AUTH_DISABLED=true go run ./cmd/api        # http://localhost:8080

# Run with Postgres:
docker run -d --name pg -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=exlibris \
  -p 5432:5432 pgvector/pgvector:pg16
STORE=postgres DATABASE_URL="postgres://postgres:pw@localhost:5432/exlibris?sslmode=disable" \
  AUTH_DISABLED=true go run ./cmd/api

go build ./... && go vet ./... && go test ./...   # gofmt -l . must be empty
```
- **OpenAPI** at `/openapi.json` (+`.yaml`); **docs UI** at `/docs`; `/healthz` open.
- API-client collections live in `bruno/` (Bruno) and `postman/`.

## Architecture
- **HTTP layer: Huma v2** (`github.com/danielgtaylor/huma/v2` + `humago` on a stdlib
  `http.ServeMux`). Book routes are Huma operations in `internal/books/handlers.go`
  (`Handler.Register(api, authMW)`); OpenAPI 3.1 is generated from the code. Request
  validation is via DTO struct tags (`BookWrite`, and pointer-field `BookPatch` for
  partial PATCH) — Huma returns 422 (validation) / 400 (malformed JSON) and
  `application/problem+json` error bodies.
- **Storage: `internal/books`.** `Store` = per-user CRUD; `Repository` adds edition/
  cover ops. Two impls: `MemoryStore` (default) and `PostgresStore` (pgx v5), chosen
  by `STORE`. **Multi-tenant:** every method takes `userID` (OIDC `sub`); cross-user
  access returns `ErrNotFound` (never leaks existence). IDs are **UUIDv7** (see
  `newID`). Migrations are multi-file `migrations/*.sql`, embedded and applied in
  order by `books.Migrate` on startup (idempotent).
- **Auth: `internal/auth`.** OIDC resource server — validates `Bearer` JWT access
  tokens against Authelia's JWKS (`OIDCVerifier`), requires membership in
  `OIDC_REQUIRED_GROUP` (else 403). Applied as a **per-operation Huma middleware**
  (`auth.NewHumaMiddleware`), so `/docs`, `/openapi.json`, `/healthz` stay public.
  `AUTH_DISABLED=true` → `StaticHumaMiddleware` with a fixed `dev` identity (dev only).
- **Enrichment: `internal/enrich`.** On create/ISBN-change (or `POST
  /books/{id}/refresh`) an ISBN is enqueued to an in-process `Worker` (goroutines +
  buffered channel, in-flight dedup) that fetches metadata + cover from **Open
  Library** and saves them to the **shared, per-ISBN** `editions`/`book_covers`
  (migration 0003) — so the external API is hit **once per ISBN**, and a second user
  adding the same ISBN sees it instantly. Startup + 15-min reconciler re-queues
  pending/failed. `GET /books/{id}/cover` serves the image (auth-scoped). The worker
  saves the cover *before* flipping status to `enriched` so that state is consistent.
  Package direction: `books → enrich` (enrich imports nothing from books/auth).

## Conventions
- Idiomatic Go: `if err != nil`, wrap with `%w`, `errors.Is/As`. `gofmt`- and
  `go vet`-clean. Comments explain *why*, not *what*.
- **TDD**: write a failing test first, confirm it fails for the right reason, then
  implement. Mockable seams everywhere (`auth.Verifier`, `enrich.Provider/Store`).
- **Testing**: Postgres via testcontainers-go (`pgvector/pgvector:pg16`, Ryuk
  disabled in `TestMain` — unreliable under colima); external HTTP mocked with
  `httptest` (mock OIDC provider in `auth_test.go`, mock Open Library in
  `enrich/openlibrary_test.go`). Tests **skip** when Docker is absent.
- **Env vars: document EVERY new one in `.env.example`** (root) as part of the same
  change — it's the single config reference. Do not add config that isn't there.

## Deployment note (Authelia, not app code)
For real OIDC, the Ex-Libris client in Authelia must: issue **JWT access tokens**
(RFC 9068 — opaque by default), include the **`groups`** claim, and use an audience
matching `OIDC_AUDIENCE`. Eventual target: Helm chart onto a home K8s cluster behind
Traefik + Authelia.

## In progress / open
- **ISBN-10 ↔ 13 normalization** — the user is implementing this themselves (a
  `normalizeISBN` in `internal/books/isbn.go`, applied in the create/update handlers,
  canonicalising to ISBN-13 so edition dedup works across formats). Offer review, not
  a rewrite, unless asked.
- **Resolved:** `editions.title`/`author` are now read. `POST /books` accepts an
  ISBN with no title/author (the scan flow posts just the barcode); reads fall
  back to the edition's canonical title/author when the book has none of its own
  (`applyCanonicalNames` in `book.go`, used by both stores). A book with no ISBN
  still requires title+author (`requireIdentifiable`). A book whose ISBN fails
  enrichment stays nameless until the user PATCHes a title, so clients must
  handle `enrichment_status: "failed"` with an empty title.
- Not built: Google Books provider/fallback; LLM keyword extraction + embeddings;
  pgvector recommendations; momox price check.

## Persistent memory
Session memory lives in
`~/.claude/projects/-Users-jalbrecht-Projects-book-guard/memory/` (see `MEMORY.md`):
user profile, project overview, the global-by-ISBN enrichment decision, and the
document-env-vars preference.
