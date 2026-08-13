package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jackc/pgx/v5/pgxpool"

	"ex-libris-api/internal/auth"
	"ex-libris-api/internal/books"
	"ex-libris-api/internal/enrich"
	"ex-libris-api/internal/search"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// Select the storage backend at startup. Default is in-memory so the server
	// runs with no dependencies; STORE=postgres switches to the pgx-backed store
	// (which needs DATABASE_URL). closeStore releases backend resources (the
	// connection pool) on shutdown.
	store, closeStore := newStore(logger)
	defer closeStore()

	// Enrichment runs in-process: the enqueuer feeds the worker, which fetches
	// metadata/covers off the request path. worker is nil when ENRICH_DISABLED.
	// Open Library is the primary source; it has gaps for small-press and
	// non-English editions, so Google Books backstops it for Lookup/FetchCover.
	// Search stays on Open Library alone (olProvider directly) — the fallback only
	// applies to per-ISBN enrichment, not free-text search.
	olProvider := enrich.NewOpenLibraryProvider(
		os.Getenv("OPENLIBRARY_BASE_URL"), os.Getenv("OPENLIBRARY_COVERS_URL"))
	gbProvider := enrich.NewGoogleBooksProvider(
		os.Getenv("GOOGLEBOOKS_BASE_URL"), os.Getenv("GOOGLEBOOKS_API_KEY"))
	provider := enrich.NewFallbackProvider(olProvider, gbProvider, logger)
	enricher, worker := newEnricher(store, provider, logger)

	handler := books.NewHandler(store, enricher, logger)

	// Huma owns the mux: it registers the book operations plus the generated
	// OpenAPI spec (/openapi.json, /openapi.yaml) and docs UI (/docs). The auth
	// middleware is attached per book operation inside Register, so those doc
	// routes and /healthz stay public.
	mux := http.NewServeMux()
	api := newAPI(mux)
	authMiddleware := newAuthMiddleware(api, logger)
	handler.Register(api, authMiddleware)
	search.NewHandler(olProvider, logger).Register(api, authMiddleware)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	addr := ":8080"
	if v := os.Getenv("ADDR"); v != "" {
		addr = v
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start the enrichment worker + reconciler; both stop when ctx is cancelled.
	if worker != nil {
		go worker.Run(ctx)
	}

	go func() {
		logger.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}

// newStore builds the storage backend chosen by the STORE env var and returns a
// cleanup function to call on shutdown. Configuration failures are fatal: the
// server has nothing to serve without a store, so it exits rather than starting
// in a broken state.
// noopEnqueuer discards enqueue calls; used when ENRICH_DISABLED is set.
type noopEnqueuer struct{}

func (noopEnqueuer) Enqueue(string) {}

// newEnricher builds the enrichment enqueuer and worker over the given provider.
// When ENRICH_DISABLED is set it returns a no-op enqueuer and a nil worker, so
// creates still succeed but nothing is fetched in the background (useful for
// offline dev and tests). It does not affect GET /search, which is a synchronous
// request the user is waiting on rather than background work.
func newEnricher(store enrich.Store, provider enrich.Provider, logger *slog.Logger) (enrich.Enqueuer, *enrich.Worker) {
	if os.Getenv("ENRICH_DISABLED") == "true" {
		logger.Warn("ENRICH_DISABLED=true: background ISBN enrichment is off (search still works)")
		return noopEnqueuer{}, nil
	}
	worker := enrich.NewWorker(store, provider, logger, enrich.Options{})
	logger.Info("ISBN enrichment enabled (Open Library)")
	return worker, worker
}

func newStore(logger *slog.Logger) (books.Repository, func()) {
	if os.Getenv("STORE") != "postgres" {
		logger.Info("using in-memory store")
		return books.NewMemoryStore(), func() {}
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		logger.Error("STORE=postgres requires DATABASE_URL")
		os.Exit(1)
	}

	// Use a bounded context for startup so a dead database fails fast instead of
	// blocking indefinitely before the server is even listening.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		logger.Error("connect to postgres", "error", err)
		os.Exit(1)
	}
	if err := books.Migrate(ctx, pool); err != nil {
		logger.Error("run migrations", "error", err)
		os.Exit(1)
	}

	logger.Info("using postgres store")
	return books.NewPostgresStore(pool), pool.Close
}

// newAPI builds the Huma API (OpenAPI 3.1 metadata, bearer security scheme,
// generated spec + docs UI) on the given mux.
func newAPI(mux *http.ServeMux) huma.API {
	cfg := huma.DefaultConfig("Ex-Libris API", "1.0.0")
	cfg.Info.Description = "Self-hosted personal book tracker. Book routes require a bearer access token issued by Authelia (OIDC)."
	// Declaring the scheme makes the docs UI show an "Authorize" button and marks
	// the book operations as secured in the generated spec. Enforcement itself is
	// done by the auth middleware, not by Huma.
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearer": {
			Type:         "http",
			Scheme:       "bearer",
			BearerFormat: "JWT",
			Description:  "OIDC access token issued by Authelia",
		},
	}
	return humago.New(mux, cfg)
}

// newAuthMiddleware builds the Huma auth middleware from the environment. Real
// deployments set OIDC_ISSUER (Authelia) and validate JWT access tokens; local
// development can set AUTH_DISABLED=true to bypass auth entirely with a static
// 'dev' identity (no token needed). A misconfigured verifier is fatal — the
// server must never start silently open.
func newAuthMiddleware(api huma.API, logger *slog.Logger) func(huma.Context, func(huma.Context)) {
	requiredGroup := os.Getenv("OIDC_REQUIRED_GROUP")

	if os.Getenv("AUTH_DISABLED") == "true" {
		logger.Warn("AUTH_DISABLED=true: authentication is bypassed with a static 'dev' identity — never use this outside local development")
		return auth.StaticHumaMiddleware(auth.Identity{
			Subject:  "dev",
			Username: "dev",
			Groups:   []string{requiredGroup},
		})
	}

	issuer := os.Getenv("OIDC_ISSUER")
	if issuer == "" {
		logger.Error("OIDC_ISSUER is required (or set AUTH_DISABLED=true for local development)")
		os.Exit(1)
	}
	audience := os.Getenv("OIDC_AUDIENCE")

	// Discovery happens here with a bounded context so an unreachable IdP fails
	// fast rather than hanging startup.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	verifier, err := auth.NewOIDCVerifier(ctx, issuer, audience, requiredGroup)
	if err != nil {
		logger.Error("configure OIDC verifier", "error", err)
		os.Exit(1)
	}
	logger.Info("OIDC authentication enabled",
		"issuer", issuer, "audience", audience, "required_group", requiredGroup)
	return auth.NewHumaMiddleware(api, verifier)
}
