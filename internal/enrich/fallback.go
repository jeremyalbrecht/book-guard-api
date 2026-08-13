package enrich

import (
	"context"
	"log/slog"
)

// FallbackProvider tries a primary Provider first and falls back to a secondary
// one when the primary has nothing for an ISBN. It exists because no single
// source has full coverage: Open Library, our primary, has gaps for small-press
// and non-English editions that Google Books often fills.
type FallbackProvider struct {
	primary, secondary Provider
	logger             *slog.Logger
}

// NewFallbackProvider builds a FallbackProvider. primary is always tried first;
// secondary is only consulted when primary fails.
func NewFallbackProvider(primary, secondary Provider, logger *slog.Logger) *FallbackProvider {
	return &FallbackProvider{primary: primary, secondary: secondary, logger: logger}
}

func (p *FallbackProvider) Lookup(ctx context.Context, isbn string) (*Metadata, error) {
	m, err := p.primary.Lookup(ctx, isbn)
	if err == nil {
		return m, nil
	}

	m, err2 := p.secondary.Lookup(ctx, isbn)
	if err2 == nil {
		p.logger.Info("enrich: fallback provider found what the primary missed", "isbn", isbn)
		return m, nil
	}
	return nil, err2
}

func (p *FallbackProvider) FetchCover(ctx context.Context, isbn string) (*Cover, error) {
	c, err := p.primary.FetchCover(ctx, isbn)
	if err == nil {
		return c, nil
	}

	c, err2 := p.secondary.FetchCover(ctx, isbn)
	if err2 == nil {
		p.logger.Info("enrich: fallback provider found a cover the primary missed", "isbn", isbn)
		return c, nil
	}
	return nil, err2
}
