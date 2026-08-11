package books

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"ex-libris-api/internal/auth"
	"ex-libris-api/internal/enrich"
)

type Handler struct {
	store    Repository
	enricher enrich.Enqueuer
	logger   *slog.Logger
}

func NewHandler(store Repository, enricher enrich.Enqueuer, logger *slog.Logger) *Handler {
	return &Handler{store: store, enricher: enricher, logger: logger}
}

// BookWrite is the request body for creating a book. Its validation tags drive
// both the OpenAPI schema and Huma's request validation, so the rules live in one
// place. Title/Author are optional because a scan posts nothing but an ISBN and
// enrichment supplies the names; the ISBN itself is required, because a book
// without one can never be enriched, named or given a cover. The shape check
// lives in the handler (see isbnFromInput) so the message is a human one and so
// PATCH can reuse the same rule.
type BookWrite struct {
	ISBN    string   `json:"isbn" required:"true" minLength:"10" maxLength:"17" doc:"ISBN-10 or ISBN-13; hyphens and spaces are accepted and stripped" example:"9782070368224"`
	Title   string   `json:"title,omitempty" doc:"Book title; optional when an ISBN is given, as enrichment fills it in" example:"La Taupe" maxLength:"500"`
	Author  string   `json:"author,omitempty" doc:"Author name; optional when an ISBN is given, as enrichment fills it in" example:"John le Carré" maxLength:"500"`
	Status  Status   `json:"status,omitempty" doc:"Reading status; defaults to to_read" enum:"to_read,reading,read" example:"reading"`
	Rating  int      `json:"rating,omitempty" doc:"Rating from 0 to 5" minimum:"0" maximum:"5" example:"4"`
	Opinion string   `json:"opinion,omitempty" doc:"Free-form notes about the book"`
	Tags    []string `json:"tags,omitempty" doc:"Freeform tags"`
}

func (w BookWrite) toBook() *Book {
	return &Book{
		ISBN:    w.ISBN,
		Title:   w.Title,
		Author:  w.Author,
		Status:  w.Status,
		Rating:  w.Rating,
		Opinion: w.Opinion,
		Tags:    w.Tags,
	}
}

// BookPatch is the request body for a partial update. Every field is a pointer so
// a nil field means "not provided" (leave unchanged), which is what makes PATCH
// genuinely partial rather than a full replace.
type BookPatch struct {
	ISBN    *string   `json:"isbn,omitempty" minLength:"10" maxLength:"17" doc:"ISBN-10 or ISBN-13; hyphens and spaces are accepted and stripped"`
	Title   *string   `json:"title,omitempty" doc:"Book title" minLength:"1" maxLength:"500"`
	Author  *string   `json:"author,omitempty" doc:"Author name" minLength:"1" maxLength:"500"`
	Status  *Status   `json:"status,omitempty" doc:"Reading status" enum:"to_read,reading,read"`
	Rating  *int      `json:"rating,omitempty" doc:"Rating from 0 to 5" minimum:"0" maximum:"5"`
	Opinion *string   `json:"opinion,omitempty" doc:"Free-form notes about the book"`
	Tags    *[]string `json:"tags,omitempty" doc:"Freeform tags"`
}

func (p BookPatch) applyTo(b *Book) {
	if p.ISBN != nil {
		b.ISBN = *p.ISBN
	}
	if p.Title != nil {
		b.Title = *p.Title
	}
	if p.Author != nil {
		b.Author = *p.Author
	}
	if p.Status != nil {
		b.Status = *p.Status
	}
	if p.Rating != nil {
		b.Rating = *p.Rating
	}
	if p.Opinion != nil {
		b.Opinion = *p.Opinion
	}
	if p.Tags != nil {
		b.Tags = *p.Tags
	}
}

// The remaining structs are Huma's input/output shapes: fields tagged `path`
// become path parameters, and a field named Body carries the JSON body.
type createBookInput struct {
	Body BookWrite
}

type updateBookInput struct {
	ID   string `path:"id" doc:"Book ID (UUIDv7)" example:"019fcbfc-59f4-7e34-9432-2490207c0439"`
	Body BookPatch
}

type bookIDInput struct {
	ID string `path:"id" doc:"Book ID (UUIDv7)" example:"019fcbfc-59f4-7e34-9432-2490207c0439"`
}

type bookOutput struct {
	Body *Book
}

type listBooksOutput struct {
	Body []*Book
}

// coverOutput streams the raw cover image. The header-tagged fields set the
// response headers; Body carries the bytes.
type coverOutput struct {
	ContentType  string `header:"Content-Type"`
	CacheControl string `header:"Cache-Control"`
	ETag         string `header:"ETag"`
	Body         []byte
}

type refreshOutput struct {
	Body struct {
		Status string `json:"status" example:"queued"`
		ISBN   string `json:"isbn" example:"9782070368224"`
	}
}

// Register wires the book operations onto the Huma API. authMW guards every book
// operation; because it is attached per-operation, the docs and OpenAPI routes
// stay public. The bearer security requirement makes the docs UI show an
// "Authorize" button and send the token.
func (h *Handler) Register(api huma.API, authMW func(huma.Context, func(huma.Context))) {
	secure := huma.Middlewares{authMW}
	bearer := []map[string][]string{{"bearer": {}}}

	huma.Register(api, huma.Operation{
		OperationID:   "create-book",
		Method:        http.MethodPost,
		Path:          "/books",
		Summary:       "Create a book",
		Tags:          []string{"books"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusUnprocessableEntity},
		Security:      bearer,
		Middlewares:   secure,
	}, h.create)

	huma.Register(api, huma.Operation{
		OperationID: "list-books",
		Method:      http.MethodGet,
		Path:        "/books",
		Summary:     "List your books",
		Tags:        []string{"books"},
		Security:    bearer,
		Middlewares: secure,
	}, h.list)

	huma.Register(api, huma.Operation{
		OperationID: "get-book",
		Method:      http.MethodGet,
		Path:        "/books/{id}",
		Summary:     "Get a book by ID",
		Tags:        []string{"books"},
		Errors:      []int{http.StatusNotFound},
		Security:    bearer,
		Middlewares: secure,
	}, h.get)

	huma.Register(api, huma.Operation{
		OperationID: "update-book",
		Method:      http.MethodPatch,
		Path:        "/books/{id}",
		Summary:     "Partially update a book",
		Tags:        []string{"books"},
		Errors:      []int{http.StatusNotFound, http.StatusUnprocessableEntity},
		Security:    bearer,
		Middlewares: secure,
	}, h.update)

	huma.Register(api, huma.Operation{
		OperationID:   "delete-book",
		Method:        http.MethodDelete,
		Path:          "/books/{id}",
		Summary:       "Delete a book",
		Tags:          []string{"books"},
		DefaultStatus: http.StatusNoContent,
		Errors:        []int{http.StatusNotFound},
		Security:      bearer,
		Middlewares:   secure,
	}, h.delete)

	huma.Register(api, huma.Operation{
		OperationID: "get-book-cover",
		Method:      http.MethodGet,
		Path:        "/books/{id}/cover",
		Summary:     "Get a book's cover image",
		Description: "Returns the cover image bytes for a book you own. The route is authenticated, so a UI must fetch it with the bearer token (e.g. into an object URL) rather than a plain <img src>.",
		Tags:        []string{"books"},
		Errors:      []int{http.StatusNotFound},
		Security:    bearer,
		Middlewares: secure,
	}, h.cover)

	huma.Register(api, huma.Operation{
		OperationID:   "refresh-book",
		Method:        http.MethodPost,
		Path:          "/books/{id}/refresh",
		Summary:       "Re-fetch metadata and cover for a book's ISBN",
		Description:   "Queues the book's ISBN for (re)enrichment from the external source. Enrichment is asynchronous; poll GET /books/{id} for the result.",
		Tags:          []string{"books"},
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusNotFound, http.StatusUnprocessableEntity},
		Security:      bearer,
		Middlewares:   secure,
	}, h.refresh)
}

// requireUser pulls the authenticated subject from the context. The auth
// middleware guarantees it is present; this guard is defence in depth.
func requireUser(ctx context.Context) (string, error) {
	id, ok := auth.FromContext(ctx)
	if !ok || id.Subject == "" {
		return "", huma.Error401Unauthorized("unauthenticated")
	}
	return id.Subject, nil
}

// isbnFromInput validates client-supplied ISBN input and returns the canonical
// form to store. A book with no valid ISBN is a "ghost": nothing can enrich it,
// name it or give it a cover, so it is rejected at the door rather than left for
// someone to find later.
func isbnFromInput(raw string) (string, error) {
	isbn, err := canonicalISBN(raw)
	if err != nil {
		return "", huma.Error422UnprocessableEntity(err.Error())
	}
	return isbn, nil
}

func (h *Handler) create(ctx context.Context, in *createBookInput) (*bookOutput, error) {
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	isbn, err := isbnFromInput(in.Body.ISBN)
	if err != nil {
		return nil, err
	}

	b := in.Body.toBook()
	// Store the canonical form, not what was typed: books.isbn keys the shared
	// edition and its cover.
	b.ISBN = isbn
	if err := h.store.Create(ctx, userID, b); err != nil {
		h.logger.Error("create book", "error", err)
		return nil, huma.Error500InternalServerError("could not save book")
	}
	// Kick off enrichment for the shared edition off the request path. If another
	// user already added this ISBN it is already enriched, so the worker no-ops.
	h.enqueueEnrichment(ctx, b.ISBN)

	// Re-read so the response carries the shared edition: an ISBN another user
	// already added is enriched, so the book comes back with its title, author
	// and cover immediately instead of looking empty until the next fetch.
	created, err := h.store.Get(ctx, userID, b.ID)
	if err != nil {
		h.logger.Error("get book after create", "error", err)
		return nil, huma.Error500InternalServerError("could not fetch book")
	}
	setCoverURL(created)
	return &bookOutput{Body: created}, nil
}

// enqueueEnrichment ensures a shared edition exists for the ISBN and queues it for
// the worker. The empty-ISBN guard is defence for rows created before the ISBN
// became mandatory; new books cannot reach here without one.
func (h *Handler) enqueueEnrichment(ctx context.Context, isbn string) {
	if isbn == "" {
		return
	}
	if err := h.store.EnsureEdition(ctx, isbn); err != nil {
		h.logger.Error("ensure edition", "isbn", isbn, "error", err)
		return
	}
	h.enricher.Enqueue(isbn)
}

// setCoverURL fills the app-relative cover URL when the book has a stored cover, so
// clients don't construct it themselves.
func setCoverURL(b *Book) {
	if b.HasCover {
		b.CoverURL = "/books/" + b.ID + "/cover"
	}
}

func (h *Handler) list(ctx context.Context, _ *struct{}) (*listBooksOutput, error) {
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	list, err := h.store.List(ctx, userID)
	if err != nil {
		h.logger.Error("list books", "error", err)
		return nil, huma.Error500InternalServerError("could not list books")
	}
	for _, b := range list {
		setCoverURL(b)
	}
	return &listBooksOutput{Body: list}, nil
}

func (h *Handler) get(ctx context.Context, in *bookIDInput) (*bookOutput, error) {
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	b, err := h.store.Get(ctx, userID, in.ID)
	if errors.Is(err, ErrNotFound) {
		return nil, huma.Error404NotFound("book not found")
	}
	if err != nil {
		h.logger.Error("get book", "error", err)
		return nil, huma.Error500InternalServerError("could not fetch book")
	}
	setCoverURL(b)
	return &bookOutput{Body: b}, nil
}

func (h *Handler) update(ctx context.Context, in *updateBookInput) (*bookOutput, error) {
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	existing, err := h.store.Get(ctx, userID, in.ID)
	if errors.Is(err, ErrNotFound) {
		return nil, huma.Error404NotFound("book not found")
	}
	if err != nil {
		h.logger.Error("get book for update", "error", err)
		return nil, huma.Error500InternalServerError("could not fetch book")
	}

	// PATCH must not be able to turn a good book into a ghost, so an ISBN that is
	// present is held to the same rule as on create — and canonicalised, so the
	// edition key stays consistent however the client spelled it.
	if in.Body.ISBN != nil {
		canonical, err := isbnFromInput(*in.Body.ISBN)
		if err != nil {
			return nil, err
		}
		in.Body.ISBN = &canonical
	}

	oldISBN := existing.ISBN
	in.Body.applyTo(existing)
	if err := h.store.Update(ctx, userID, existing); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, huma.Error404NotFound("book not found")
		}
		h.logger.Error("update book", "error", err)
		return nil, huma.Error500InternalServerError("could not update book")
	}
	// If the ISBN changed, enrich the newly referenced edition.
	if existing.ISBN != oldISBN {
		h.enqueueEnrichment(ctx, existing.ISBN)
	}
	// Re-read so the response reflects the (possibly different) edition's metadata.
	b, err := h.store.Get(ctx, userID, in.ID)
	if err != nil {
		h.logger.Error("get book after update", "error", err)
		return nil, huma.Error500InternalServerError("could not fetch book")
	}
	setCoverURL(b)
	return &bookOutput{Body: b}, nil
}

func (h *Handler) delete(ctx context.Context, in *bookIDInput) (*struct{}, error) {
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.store.Delete(ctx, userID, in.ID); errors.Is(err, ErrNotFound) {
		return nil, huma.Error404NotFound("book not found")
	} else if err != nil {
		h.logger.Error("delete book", "error", err)
		return nil, huma.Error500InternalServerError("could not delete book")
	}
	return &struct{}{}, nil
}

func (h *Handler) cover(ctx context.Context, in *bookIDInput) (*coverOutput, error) {
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	c, err := h.store.CoverForBook(ctx, userID, in.ID)
	if errors.Is(err, ErrNotFound) {
		return nil, huma.Error404NotFound("cover not found")
	}
	if err != nil {
		h.logger.Error("get cover", "error", err)
		return nil, huma.Error500InternalServerError("could not fetch cover")
	}
	return &coverOutput{
		ContentType:  c.ContentType,
		CacheControl: "private, max-age=86400",
		ETag:         c.ETag,
		Body:         c.Bytes,
	}, nil
}

func (h *Handler) refresh(ctx context.Context, in *bookIDInput) (*refreshOutput, error) {
	userID, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	b, err := h.store.Get(ctx, userID, in.ID)
	if errors.Is(err, ErrNotFound) {
		return nil, huma.Error404NotFound("book not found")
	}
	if err != nil {
		h.logger.Error("get book for refresh", "error", err)
		return nil, huma.Error500InternalServerError("could not fetch book")
	}
	if b.ISBN == "" {
		return nil, huma.Error422UnprocessableEntity("book has no ISBN to enrich")
	}

	// Force the shared edition back to pending so the worker re-fetches even if it
	// was already enriched, then queue it.
	if err := h.store.MarkEditionPending(ctx, b.ISBN); err != nil {
		h.logger.Error("mark edition pending", "isbn", b.ISBN, "error", err)
		return nil, huma.Error500InternalServerError("could not queue refresh")
	}
	h.enricher.Enqueue(b.ISBN)

	out := &refreshOutput{}
	out.Body.Status = "queued"
	out.Body.ISBN = b.ISBN
	return out, nil
}
