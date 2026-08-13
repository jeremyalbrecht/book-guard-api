package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultGoogleBooksBase = "https://www.googleapis.com"

// GoogleBooksProvider fetches metadata and covers from the Google Books API. It
// exists as a fallback for editions Open Library has no record of (small-press
// and non-English books in particular) — see FallbackProvider. Base URL is
// injectable so tests can point it at an httptest server instead of the internet.
type GoogleBooksProvider struct {
	client        *http.Client
	baseURL       string
	apiKey        string
	coverMaxBytes int64
}

// NewGoogleBooksProvider builds a provider. An empty baseURL falls back to the
// public Google Books API host. apiKey is optional: Google Books answers
// unauthenticated requests, but its anonymous quota is very low in practice, so a
// key is recommended for anything beyond occasional use (see .env.example).
func NewGoogleBooksProvider(baseURL, apiKey string) *GoogleBooksProvider {
	if baseURL == "" {
		baseURL = defaultGoogleBooksBase
	}
	return &GoogleBooksProvider{
		client:        &http.Client{Timeout: 10 * time.Second},
		baseURL:       strings.TrimRight(baseURL, "/"),
		apiKey:        apiKey,
		coverMaxBytes: defaultCoverMaxBytes,
	}
}

// gbVolumeInfo mirrors the fields we use from a Google Books volumes item.
type gbVolumeInfo struct {
	Title         string   `json:"title"`
	Authors       []string `json:"authors"`
	Publisher     string   `json:"publisher"`
	PublishedDate string   `json:"publishedDate"`
	Description   string   `json:"description"`
	PageCount     int      `json:"pageCount"`
	ImageLinks    struct {
		Thumbnail string `json:"thumbnail"`
	} `json:"imageLinks"`
}

type gbVolumesResponse struct {
	TotalItems int `json:"totalItems"`
	Items      []struct {
		VolumeInfo gbVolumeInfo `json:"volumeInfo"`
	} `json:"items"`
}

func (p *GoogleBooksProvider) Lookup(ctx context.Context, isbn string) (*Metadata, error) {
	info, err := p.lookupVolume(ctx, isbn)
	if err != nil {
		return nil, err
	}

	m := &Metadata{
		Title:         info.Title,
		Author:        strings.Join(info.Authors, ", "),
		Description:   info.Description,
		Publisher:     info.Publisher,
		PublishedDate: info.PublishedDate,
		PageCount:     info.PageCount,
	}
	return m, nil
}

func (p *GoogleBooksProvider) FetchCover(ctx context.Context, isbn string) (*Cover, error) {
	// Unlike Open Library, Google Books has no deterministic cover-by-ISBN URL, so
	// the thumbnail link must be read from the same volume lookup as Lookup.
	info, err := p.lookupVolume(ctx, isbn)
	if err != nil {
		return nil, err
	}
	if info.ImageLinks.Thumbnail == "" {
		return nil, ErrNoCover
	}
	coverURL := info.ImageLinks.Thumbnail

	resp, err := httpGetWithRetry(ctx, p.client, coverURL, userAgent)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("googlebooks cover: status %d", resp.StatusCode)
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
		SourceURL:   coverURL,
	}, nil
}

// lookupVolume queries the volumes endpoint for isbn and returns the first
// match's volumeInfo. An ISBN with no results yields ErrNotFound.
func (p *GoogleBooksProvider) lookupVolume(ctx context.Context, isbn string) (*gbVolumeInfo, error) {
	u := fmt.Sprintf("%s/books/v1/volumes?q=isbn:%s", p.baseURL, url.QueryEscape(isbn))
	if p.apiKey != "" {
		u += "&key=" + url.QueryEscape(p.apiKey)
	}

	resp, err := httpGetWithRetry(ctx, p.client, u, userAgent)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("googlebooks lookup: status %d", resp.StatusCode)
	}

	var payload gbVolumesResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode googlebooks response: %w", err)
	}
	if payload.TotalItems == 0 || len(payload.Items) == 0 {
		return nil, ErrNotFound
	}
	return &payload.Items[0].VolumeInfo, nil
}
