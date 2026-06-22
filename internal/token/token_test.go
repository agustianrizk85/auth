package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

func newTestSigner(t *testing.T) *Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	return NewSigner(priv)
}

func validClaims() Claims {
	now := time.Now()
	return Claims{
		Issuer:    "greenpark-auth",
		Subject:   "u1",
		Username:  "budi",
		Roles:     map[string]string{"finance": "admin"},
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(time.Hour).Unix(),
		JTI:       "j1",
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	s := newTestSigner(t)
	tok, err := s.Sign(validClaims())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Subject != "u1" || got.Roles["finance"] != "admin" {
		t.Fatalf("claims mismatch: %+v", got)
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	s1 := newTestSigner(t)
	s2 := newTestSigner(t)
	tok, _ := s1.Sign(validClaims())
	if _, err := Verify(tok, s2.Public()); err == nil {
		t.Fatal("expected signature error with wrong key")
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	s := newTestSigner(t)
	c := validClaims()
	c.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	tok, _ := s.Sign(c)
	if _, err := s.Verify(tok); err != ErrExpired {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

func TestVerifyRejectsTampered(t *testing.T) {
	s := newTestSigner(t)
	tok, _ := s.Sign(validClaims())
	// flip a character in the payload segment
	parts := strings.Split(tok, ".")
	parts[1] = parts[1][:len(parts[1])-1] + "A"
	if _, err := Verify(parts[0]+"."+parts[1]+"."+parts[2], s.Public()); err == nil {
		t.Fatal("expected error on tampered payload")
	}
}

func TestVerifyRejectsNoneAlg(t *testing.T) {
	s := newTestSigner(t)
	// craft a token with header alg=none and a valid-looking structure
	none := b64.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := b64.EncodeToString([]byte(`{"sub":"u1","exp":9999999999}`))
	forged := none + "." + payload + "."
	if _, err := Verify(forged, s.Public()); err == nil {
		t.Fatal("expected rejection of alg=none token")
	}
}

func TestJWKSContainsKey(t *testing.T) {
	s := newTestSigner(t)
	set := JWKS(s.Public(), s.KID())
	keys, ok := set["keys"].([]map[string]any)
	if !ok || len(keys) != 1 {
		t.Fatalf("bad JWKS shape: %+v", set)
	}
	if keys[0]["kid"] != s.KID() || keys[0]["crv"] != "Ed25519" {
		t.Fatalf("bad JWKS entry: %+v", keys[0])
	}
}
