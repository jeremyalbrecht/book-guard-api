// Package auth authenticates requests against an OIDC provider (Authelia) by
// validating JWT bearer access tokens, and authorizes them by requiring
// membership in a configured group. It exposes Huma middlewares that attach the
// resulting Identity to the request context.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/danielgtaylor/huma/v2"
)

// ErrUnauthenticated means the caller presented no credential or one that failed
// validation (missing/expired/bad-signature/wrong-audience token). It maps to 401.
var ErrUnauthenticated = errors.New("unauthenticated")

// ErrForbidden means the caller authenticated but lacks the required group. It
// maps to 403.
var ErrForbidden = errors.New("forbidden")

// Identity is the authenticated caller. Subject is the stable OIDC `sub` and is
// used as the per-user ownership key; Username/Groups are for logging and the
// group check respectively.
type Identity struct {
	Subject  string
	Username string
	Groups   []string
}

// Verifier turns a raw bearer token into an Identity, or returns
// ErrUnauthenticated / ErrForbidden. It is an interface so handlers and tests do
// not need a live identity provider.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (Identity, error)
}

// OIDCVerifier validates RFC 9068 JWT access tokens against an OIDC provider's
// JWKS and enforces the required-group policy.
type OIDCVerifier struct {
	verifier      *oidc.IDTokenVerifier
	requiredGroup string
}

// NewOIDCVerifier performs OIDC discovery against issuer (so the provider must be
// reachable) and returns a Verifier that accepts tokens whose audience contains
// audience and, when requiredGroup is non-empty, whose groups claim contains it.
func NewOIDCVerifier(ctx context.Context, issuer, audience, requiredGroup string) (*OIDCVerifier, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %s: %w", issuer, err)
	}
	return &OIDCVerifier{
		verifier:      provider.Verifier(&oidc.Config{ClientID: audience}),
		requiredGroup: requiredGroup,
	}, nil
}

func (v *OIDCVerifier) Verify(ctx context.Context, rawToken string) (Identity, error) {
	// Signature, issuer, audience and expiry are all checked here against the
	// provider's published keys.
	token, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}

	var claims struct {
		Username string   `json:"preferred_username"`
		Groups   []string `json:"groups"`
	}
	if err := token.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("%w: decode claims: %v", ErrUnauthenticated, err)
	}

	id := Identity{Subject: token.Subject, Username: claims.Username, Groups: claims.Groups}

	// Authentication succeeded; now the authorization policy. Return the identity
	// alongside the error so callers can log who was rejected.
	if v.requiredGroup != "" && !contains(claims.Groups, v.requiredGroup) {
		return id, fmt.Errorf("%w: not in group %q", ErrForbidden, v.requiredGroup)
	}
	return id, nil
}

func contains(groups []string, want string) bool {
	for _, g := range groups {
		if g == want {
			return true
		}
	}
	return false
}

type identityKey struct{}

// FromContext returns the Identity attached by the auth middleware, if any.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// ContextWithIdentity attaches id to ctx so FromContext can retrieve it. The auth
// middlewares use it; it is exported so other adapters can inject identity too.
func ContextWithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// NewHumaMiddleware validates the Authorization: Bearer token on each operation
// it guards and, on success, attaches the Identity to the context for the
// handler to read via FromContext. It short-circuits with 401 (unauthenticated)
// or 403 (authenticated but not in the required group). It is applied per
// operation, so unauthenticated routes (docs, the OpenAPI spec, health) stay open.
func NewHumaMiddleware(api huma.API, v Verifier) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		raw, ok := parseBearer(ctx.Header("Authorization"))
		if !ok {
			_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "unauthenticated")
			return
		}
		id, err := v.Verify(ctx.Context(), raw)
		if err != nil {
			status := http.StatusUnauthorized
			if errors.Is(err, ErrForbidden) {
				status = http.StatusForbidden
			}
			_ = huma.WriteErr(api, ctx, status, http.StatusText(status))
			return
		}
		next(huma.WithContext(ctx, ContextWithIdentity(ctx.Context(), id)))
	}
}

// StaticHumaMiddleware injects a fixed identity without requiring a token. It
// exists only for local development (the AUTH_DISABLED switch) and must never
// front untrusted callers.
func StaticHumaMiddleware(id Identity) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		next(huma.WithContext(ctx, ContextWithIdentity(ctx.Context(), id)))
	}
}

// parseBearer extracts the token from an "Authorization: Bearer <token>" value.
func parseBearer(h string) (string, bool) {
	const prefix = "Bearer "
	// Scheme names are case-insensitive per RFC 7235.
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	return token, token != ""
}
