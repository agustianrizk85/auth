package authmw

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// signToken builds a minimal EdDSA JWT the way the auth service does, so the
// verifier can be tested without importing the auth internal packages.
func signToken(t *testing.T, priv ed25519.PrivateKey, c Claims) string {
	t.Helper()
	enc := base64.RawURLEncoding
	hb, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": "test"})
	cb, _ := json.Marshal(c)
	signing := enc.EncodeToString(hb) + "." + enc.EncodeToString(cb)
	sig := ed25519.Sign(priv, []byte(signing))
	return signing + "." + enc.EncodeToString(sig)
}

func newPinnedVerifier(t *testing.T) (*Verifier, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	v, err := New(Options{PublicKey: pub, Department: "finance", Issuer: "greenpark-auth"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return v, priv
}

func okClaims() Claims {
	return Claims{
		Issuer:    "greenpark-auth",
		Subject:   "u1",
		Username:  "budi",
		Roles:     map[string]string{"finance": "viewer"},
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
}

func TestRequireAuthAllowsDeptMember(t *testing.T) {
	v, priv := newPinnedVerifier(t)
	tok := signToken(t, priv, okClaims())

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	called := false
	v.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if From(r.Context()).Username != "budi" {
			t.Fatal("claims not in context")
		}
	})(rec, req)
	if !called || rec.Code != http.StatusOK {
		t.Fatalf("expected handler call + 200, got called=%v code=%d", called, rec.Code)
	}
}

func TestRequireAdminRejectsViewer(t *testing.T) {
	v, priv := newPinnedVerifier(t)
	tok := signToken(t, priv, okClaims()) // finance:viewer

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	v.RequireAdmin(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not run for viewer")
	})(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
}

func TestRequireAuthRejectsOtherDept(t *testing.T) {
	v, priv := newPinnedVerifier(t)
	c := okClaims()
	c.Roles = map[string]string{"sales": "admin"} // not finance
	tok := signToken(t, priv, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	v.RequireAuth(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler should not run for non-member")
	})(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
}

func TestSuperUserAccessesAnyDept(t *testing.T) {
	v, priv := newPinnedVerifier(t)
	c := okClaims()
	c.Super = true
	c.Roles = nil
	tok := signToken(t, priv, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/x", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	called := false
	v.RequireAdmin(func(http.ResponseWriter, *http.Request) { called = true })(rec, req)
	if !called {
		t.Fatal("super user should pass RequireAdmin")
	}
}

func TestRejectsWrongIssuerAndExpiry(t *testing.T) {
	v, priv := newPinnedVerifier(t)

	bad := okClaims()
	bad.Issuer = "someone-else"
	if _, err := v.Verify(signToken(t, priv, bad)); err == nil {
		t.Fatal("expected issuer rejection")
	}

	expired := okClaims()
	expired.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	if _, err := v.Verify(signToken(t, priv, expired)); err == nil {
		t.Fatal("expected expiry rejection")
	}
}

func TestJWKSFetch(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	enc := base64.RawURLEncoding
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "OKP", "crv": "Ed25519", "use": "sig", "alg": "EdDSA",
				"kid": "test", "x": enc.EncodeToString(pub),
			}},
		})
	}))
	defer srv.Close()

	v, err := New(Options{JWKSURL: srv.URL, Department: "finance", Issuer: "greenpark-auth"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := v.Verify(signToken(t, priv, okClaims())); err != nil {
		t.Fatalf("verify via JWKS: %v", err)
	}
}
