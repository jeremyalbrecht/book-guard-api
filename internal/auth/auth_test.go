package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"

	"ex-libris-api/internal/auth"
)

const (
	testAudience = "ex-libris"
	testGroup    = "book-users"
	testKeyID    = "test-key"
)

// mockOIDC is a minimal OIDC provider: it serves a discovery document and a JWKS
// so go-oidc can validate tokens signed by signKey, without a real Authelia.
type mockOIDC struct {
	server *httptest.Server
	signer jose.Signer
}

func newMockOIDC(t *testing.T) *mockOIDC {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: testKeyID}},
		(&jose.SignerOptions{}).WithType("at+jwt"),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}

	m := &mockOIDC{signer: signer}
	mux := http.NewServeMux()
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)

	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       key.Public(),
		KeyID:     testKeyID,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}}}
	mux.HandleFunc("/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, jwks)
	})
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                m.server.URL,
			"jwks_uri":                              m.server.URL + "/jwks.json",
			"authorization_endpoint":                m.server.URL + "/authorize",
			"token_endpoint":                        m.server.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	return m
}

// claims is a superset of what the verifier reads, so individual tests can bend
// any single field (issuer, audience, expiry, groups) to exercise a failure.
type claims struct {
	Issuer   string   `json:"iss"`
	Subject  string   `json:"sub"`
	Audience []string `json:"aud"`
	Expiry   int64    `json:"exp"`
	IssuedAt int64    `json:"iat"`
	Username string   `json:"preferred_username"`
	Groups   []string `json:"groups"`
}

// validClaims is a well-formed token for the given subject and groups.
func (m *mockOIDC) validClaims(sub string, groups ...string) claims {
	now := time.Now()
	return claims{
		Issuer:   m.server.URL,
		Subject:  sub,
		Audience: []string{testAudience},
		Expiry:   now.Add(time.Hour).Unix(),
		IssuedAt: now.Unix(),
		Username: sub,
		Groups:   groups,
	}
}

func (m *mockOIDC) sign(t *testing.T, c claims) string {
	t.Helper()
	raw, err := josejwt.Signed(m.signer).Claims(c).Serialize()
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return raw
}

func (m *mockOIDC) newVerifier(t *testing.T, requiredGroup string) auth.Verifier {
	t.Helper()
	v, err := auth.NewOIDCVerifier(context.Background(), m.server.URL, testAudience, requiredGroup)
	if err != nil {
		t.Fatalf("NewOIDCVerifier: %v", err)
	}
	return v
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestVerifyValidTokenInGroupReturnsIdentity(t *testing.T) {
	m := newMockOIDC(t)
	v := m.newVerifier(t, testGroup)

	token := m.sign(t, m.validClaims("alice", testGroup, "other"))
	id, err := v.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.Subject != "alice" {
		t.Errorf("Subject = %q, want alice", id.Subject)
	}
	if id.Username != "alice" {
		t.Errorf("Username = %q, want alice", id.Username)
	}
	if len(id.Groups) != 2 || id.Groups[0] != testGroup {
		t.Errorf("Groups = %v, want [%s other]", id.Groups, testGroup)
	}
}

func TestVerifyValidTokenMissingGroupIsForbidden(t *testing.T) {
	m := newMockOIDC(t)
	v := m.newVerifier(t, testGroup)

	token := m.sign(t, m.validClaims("bob", "some-other-group"))
	_, err := v.Verify(context.Background(), token)
	if !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	m := newMockOIDC(t)
	v := m.newVerifier(t, testGroup)

	expired := m.validClaims("alice", testGroup)
	expired.Expiry = time.Now().Add(-time.Hour).Unix()

	wrongAud := m.validClaims("alice", testGroup)
	wrongAud.Audience = []string{"someone-else"}

	tests := map[string]string{
		"expired":     m.sign(t, expired),
		"wrong aud":   m.sign(t, wrongAud),
		"garbage":     "not-a-jwt",
		"empty token": "",
	}
	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), token)
			if !errors.Is(err, auth.ErrUnauthenticated) {
				t.Fatalf("expected ErrUnauthenticated, got %v", err)
			}
		})
	}
}

// meOutput is the body for the probe operation used to test the middleware: it
// echoes back the subject the middleware injected into the context.
type meOutput struct {
	Body struct {
		Subject string `json:"subject"`
	}
}

// TestHumaMiddleware drives NewHumaMiddleware in front of a real Huma operation,
// asserting the HTTP status per case and that a valid token's identity reaches
// the handler via FromContext. Per-user book scoping is covered end-to-end in the
// books package's Huma tests.
func TestHumaMiddleware(t *testing.T) {
	m := newMockOIDC(t)
	v := m.newVerifier(t, testGroup)

	mux := http.NewServeMux()
	api := humago.New(mux, huma.DefaultConfig("test", "1.0.0"))
	huma.Register(api, huma.Operation{
		OperationID: "me",
		Method:      http.MethodGet,
		Path:        "/me",
		Middlewares: huma.Middlewares{auth.NewHumaMiddleware(api, v)},
	}, func(ctx context.Context, _ *struct{}) (*meOutput, error) {
		id, _ := auth.FromContext(ctx)
		out := &meOutput{}
		out.Body.Subject = id.Subject
		return out, nil
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	tests := []struct {
		name       string
		authHeader string
		wantStatus int
		wantSub    string
	}{
		{"no header", "", http.StatusUnauthorized, ""},
		{"malformed header", "Basic abc", http.StatusUnauthorized, ""},
		{"missing group", "Bearer " + m.sign(t, m.validClaims("bob", "nope")), http.StatusForbidden, ""},
		{"valid", "Bearer " + m.sign(t, m.validClaims("alice", testGroup)), http.StatusOK, "alice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/me", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantSub != "" {
				var body struct {
					Subject string `json:"subject"`
				}
				json.NewDecoder(resp.Body).Decode(&body)
				if body.Subject != tt.wantSub {
					t.Errorf("handler saw subject %q, want %q", body.Subject, tt.wantSub)
				}
			}
		})
	}
}
