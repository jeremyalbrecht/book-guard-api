package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	defaultOpenLibraryBase = "https://openlibrary.org"
	defaultCoversBase      = "https://covers.openlibrary.org"
	defaultCoverMaxBytes   = 5 << 20 // 5 MiB
	defaultSearchLimit     = 10
	maxSearchLimit         = 25
	// Open Library asks clients to send a descriptive User-Agent.
	userAgent = "ex-libris/1.0 (self-hosted book tracker)"
)

// OpenLibraryProvider fetches metadata and covers from Open Library. Base URLs are
// injectable so tests can point it at an httptest server instead of the internet.
type OpenLibraryProvider struct {
	client        *http.Client
	baseURL       string
	coversURL     string
	coverMaxBytes int64
}

// NewOpenLibraryProvider builds a provider. Empty URLs fall back to the public
// Open Library hosts.
func NewOpenLibraryProvider(baseURL, coversURL string) *OpenLibraryProvider {
	if baseURL == "" {
		baseURL = defaultOpenLibraryBase
	}
	if coversURL == "" {
		coversURL = defaultCoversBase
	}
	return &OpenLibraryProvider{
		client:        &http.Client{Timeout: 10 * time.Second},
		baseURL:       strings.TrimRight(baseURL, "/"),
		coversURL:     strings.TrimRight(coversURL, "/"),
		coverMaxBytes: defaultCoverMaxBytes,
	}
}

// olData mirrors the fields we use from Open Library's `jscmd=data` response.
type olData struct {
	Title   string `json:"title"`
	Authors []struct {
		Name string `json:"name"`
	} `json:"authors"`
	Publishers []struct {
		Name string `json:"name"`
	} `json:"publishers"`
	PublishDate string `json:"publish_date"`
	Pages       int    `json:"number_of_pages"`
	Excerpts    []struct {
		Text string `json:"text"`
	} `json:"excerpts"`
}

func (p *OpenLibraryProvider) Lookup(ctx context.Context, isbn string) (*Metadata, error) {
	u := fmt.Sprintf("%s/api/books?bibkeys=ISBN:%s&format=json&jscmd=data",
		p.baseURL, url.QueryEscape(isbn))
	resp, err := p.get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openlibrary lookup: status %d", resp.StatusCode)
	}

	// The payload is keyed by "ISBN:<isbn>"; an unknown ISBN yields an empty object.
	var payload map[string]olData
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode openlibrary response: %w", err)
	}
	data, ok := payload["ISBN:"+isbn]
	if !ok {
		return nil, ErrNotFound
	}

	m := &Metadata{
		Title:         data.Title,
		PublishedDate: data.PublishDate,
		PageCount:     data.Pages,
	}
	authors := make([]string, 0, len(data.Authors))
	for _, a := range data.Authors {
		authors = append(authors, a.Name)
	}
	m.Author = strings.Join(authors, ", ")
	if len(data.Publishers) > 0 {
		m.Publisher = data.Publishers[0].Name
	}
	// Description is best-effort: jscmd=data usually omits it, but an excerpt is a
	// reasonable stand-in when present.
	if len(data.Excerpts) > 0 {
		m.Description = data.Excerpts[0].Text
	}
	return m, nil
}

// olSearch mirrors the fields we request from Open Library's search endpoint.
type olSearch struct {
	Docs []struct {
		Title            string   `json:"title"`
		AuthorName       []string `json:"author_name"`
		FirstPublishYear int      `json:"first_publish_year"`
		Publisher        []string `json:"publisher"`
		ISBN             []string `json:"isbn"`
	} `json:"docs"`
}

// isbn13 matches a canonical ISBN-13, which always begins 978 or 979.
var isbn13 = regexp.MustCompile(`^97[89][0-9]{10}$`)

// Search finds candidate editions by free text, for a user picking a book they
// are holding but whose barcode they cannot scan.
//
// Caveat worth knowing before trusting a result: /search.json returns *works*,
// and a work's `isbn` list aggregates every edition of it — every translation,
// printing and binding — in no meaningful order. The ISBN returned here is
// therefore a plausible edition of the right book, not necessarily the copy in
// the user's hand, so the cover and page count that enrichment later fetches may
// belong to a different printing. Scanning the barcode remains the accurate
// path; this is the fallback.
func (p *OpenLibraryProvider) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	// `fields` must name isbn: restricting the field list drops everything not
	// asked for, and a result with no ISBN cannot be turned into a book.
	u := fmt.Sprintf("%s/search.json?q=%s&fields=title,author_name,first_publish_year,publisher,isbn&limit=%d",
		p.baseURL, url.QueryEscape(query), limit)
	resp, err := p.get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openlibrary search: status %d", resp.StatusCode)
	}

	var payload olSearch
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode openlibrary search: %w", err)
	}

	results := make([]SearchResult, 0, len(payload.Docs))
	for _, doc := range payload.Docs {
		isbn := pickISBN(doc.ISBN)
		if isbn == "" {
			// Nothing to create a book from, so showing it would only frustrate.
			continue
		}
		r := SearchResult{
			ISBN:  isbn,
			Title: doc.Title,
			// Same joining convention as Lookup, so a book looks the same
			// whichever path added it.
			Author: strings.Join(doc.AuthorName, ", "),
			Year:   doc.FirstPublishYear,
		}
		if len(doc.Publisher) > 0 {
			r.Publisher = doc.Publisher[0]
		}
		results = append(results, r)
	}
	return results, nil
}

// pickISBN chooses one ISBN from a work's aggregated list. An ISBN-13 is
// preferred because that is what a barcode scan produces, so the same book added
// either way lands on the same edition row.
func pickISBN(candidates []string) string {
	var fallback string
	for _, raw := range candidates {
		isbn := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(raw))
		if isbn13.MatchString(isbn) {
			return isbn
		}
		if fallback == "" && len(isbn) == 10 {
			fallback = isbn
		}
	}
	return fallback
}

func (p *OpenLibraryProvider) FetchCover(ctx context.Context, isbn string) (*Cover, error) {
	// default=false makes Open Library return 404 (not a blank placeholder) when it
	// has no cover, so we can distinguish "no cover" from a real image.
	u := fmt.Sprintf("%s/b/isbn/%s-L.jpg?default=false", p.coversURL, url.PathEscape(isbn))
	resp, err := p.get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoCover
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openlibrary cover: status %d", resp.StatusCode)
	}

	// Read one byte past the cap so we can detect (and reject) oversized images.
	body, err := io.ReadAll(io.LimitReader(resp.Body, p.coverMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read cover: %w", err)
	}
	if int64(len(body)) > p.coverMaxBytes {
		return nil, fmt.Errorf("cover exceeds %d bytes", p.coverMaxBytes)
	}
	if len(body) == 0 {
		return nil, ErrNoCover
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/jpeg"
	}
	return &Cover{
		ContentType: ct,
		Bytes:       body,
		ETag:        resp.Header.Get("ETag"),
		SourceURL:   u,
	}, nil
}

// get issues a GET with the required User-Agent, retrying transient failures.
func (p *OpenLibraryProvider) get(ctx context.Context, rawURL string) (*http.Response, error) {
	return httpGetWithRetry(ctx, p.client, rawURL, userAgent)
}
