package enrich

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// httpGetWithRetry issues a GET with the given User-Agent, retrying a few times
// on transient failures (network errors, 429, 5xx) with a small quadratic
// backoff. Shared by every Provider so each one gets the same resilience against
// upstream hiccups without duplicating the retry loop.
func httpGetWithRetry(ctx context.Context, client *http.Client, rawURL, userAgent string) (*http.Response, error) {
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", userAgent)

		resp, err := client.Do(req)
		switch {
		case err != nil:
			lastErr = err
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			resp.Body.Close()
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		default:
			return resp, nil
		}

		if attempt < maxAttempts {
			select {
			case <-time.After(time.Duration(attempt*attempt) * 200 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, fmt.Errorf("request failed after %d attempts: %w", maxAttempts, lastErr)
}
