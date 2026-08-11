# Ex-Libris

Self-hosted personal book-tracking API. Go, `net/http` (via [Huma v2](https://github.com/danielgtaylor/huma)), with an in-memory or Postgres store and background ISBN metadata/cover enrichment via Open Library.

> Learning project — a platform engineer's first Go codebase. Code favors idiomatic Go and clarity over cleverness.

## Features

- CRUD for books, scoped per authenticated user (OIDC `sub`)
- Add a book from just an ISBN barcode scan — title/author fill in automatically
- Background enrichment worker fetches metadata + cover art from Open Library, shared globally per ISBN (fetched once, reused across users)
- OIDC resource-server auth (validates bearer JWTs against an Authelia-style JWKS), with a group-membership check
- OpenAPI 3.1 generated from code, served at `/openapi.json` and a docs UI at `/docs`

## Quick start

```bash
# In-memory store, no auth — fastest way to try it:
AUTH_DISABLED=true go run ./cmd/api        # http://localhost:8080
```

With Postgres:

```bash
docker run -d --name pg -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=exlibris \
  -p 5432:5432 pgvector/pgvector:pg16

STORE=postgres DATABASE_URL="postgres://postgres:pw@localhost:5432/exlibris?sslmode=disable" \
  AUTH_DISABLED=true go run ./cmd/api
```

Migrations run automatically on startup.

## Configuration

All environment variables are documented in [`.env.example`](.env.example) — copy it to `.env` and adjust. Highlights:

| Variable | Purpose |
|---|---|
| `ADDR` | HTTP listen address (default `:8080`) |
| `STORE` | `memory` (default) or `postgres` |
| `DATABASE_URL` | Postgres connection string (required if `STORE=postgres`) |
| `AUTH_DISABLED` | Skip OIDC, use a static `dev` identity — local dev only |
| `OIDC_ISSUER`, `OIDC_AUDIENCE`, `OIDC_REQUIRED_GROUP` | OIDC resource-server config |
| `ENRICH_DISABLED` | Disable the background metadata/cover enrichment worker |
| `OPENLIBRARY_BASE_URL`, `OPENLIBRARY_COVERS_URL` | Override Open Library hosts |

## Development

```bash
go build ./... && go vet ./... && go test ./...
gofmt -l .   # must be empty
```

- API client collection for manual testing: [`bruno/`](bruno) ([Bruno](https://www.usebruno.com/)).
- Postgres tests use [testcontainers-go](https://golang.testcontainers.org/) and are skipped automatically when Docker isn't available.

## Architecture

- **HTTP**: Huma v2 on a stdlib `http.ServeMux`; book routes in `internal/books/handlers.go`.
- **Storage**: `internal/books`, with `MemoryStore` and `PostgresStore` (pgx v5) implementations behind a common interface. Every method is scoped by user ID; IDs are UUIDv7.
- **Auth**: `internal/auth` — OIDC JWT validation against a JWKS, applied per-operation so `/docs`, `/openapi.json`, and `/healthz` stay public.
- **Enrichment**: `internal/enrich` — an in-process worker fetches ISBN metadata/covers from Open Library and stores them globally per ISBN, so each ISBN is only fetched once regardless of how many users own a copy.

See [`CLAUDE.md`](CLAUDE.md) for the full architecture and conventions reference.

## Deployment

Not yet packaged; eventual target is a Helm chart onto a home Kubernetes cluster behind Traefik + Authelia. For real OIDC, the Ex-Libris client in Authelia must issue JWT access tokens (RFC 9068), include a `groups` claim, and use an audience matching `OIDC_AUDIENCE`.

## Status

Learning/personal project, under active development. Not built yet: Google Books fallback provider, LLM keyword extraction + embeddings, pgvector-based recommendations, price checking.
