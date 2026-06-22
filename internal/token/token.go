// Package token issues and verifies compact JSON Web Tokens signed with
// Ed25519 (JWS "EdDSA"). It is deliberately single-algorithm: the verifier
// accepts only "EdDSA", which removes the classic JWT algorithm-confusion and
// "alg: none" attack surface. Departments verify tokens with the public key
// alone, so the signing secret never leaves the auth service.
package token

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var b64 = base64.RawURLEncoding

// Claims is the JWT payload carried in every access token.
type Claims struct {
	Issuer    string            `json:"iss"`
	Subject   string            `json:"sub"` // user ID
	Username  string            `json:"username"`
	Email     string            `json:"email,omitempty"`
	Name      string            `json:"name"`
	Super     bool              `json:"super,omitempty"`
	Roles     map[string]string `json:"roles,omitempty"` // department code -> role
	IssuedAt  int64             `json:"iat"`
	ExpiresAt int64             `json:"exp"`
	JTI       string            `json:"jti"`
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// Errors returned by Verify.
var (
	ErrMalformed = errors.New("token tidak valid")
	ErrSignature = errors.New("tanda tangan token tidak cocok")
	ErrExpired   = errors.New("token kedaluwarsa")
)

// Signer signs claims with an Ed25519 private key under a stable key ID (kid).
type Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	kid  string
}

// NewSigner derives the public key and key ID from the private key.
func NewSigner(priv ed25519.PrivateKey) *Signer {
	pub := priv.Public().(ed25519.PublicKey)
	return &Signer{priv: priv, pub: pub, kid: KeyID(pub)}
}

// KeyID returns a short, stable identifier derived from the public key, used as
// the JWT "kid" and the JWKS key id so verifiers can select the right key.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return b64.EncodeToString(sum[:9])
}

// KID returns this signer's key ID.
func (s *Signer) KID() string { return s.kid }

// Public returns the verification (public) key.
func (s *Signer) Public() ed25519.PublicKey { return s.pub }

// Sign serialises and signs the claims, returning a compact JWS string.
func (s *Signer) Sign(c Claims) (string, error) {
	hb, err := json.Marshal(header{Alg: "EdDSA", Typ: "JWT", Kid: s.kid})
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signing := b64.EncodeToString(hb) + "." + b64.EncodeToString(cb)
	sig := ed25519.Sign(s.priv, []byte(signing))
	return signing + "." + b64.EncodeToString(sig), nil
}

// Verify validates the signature with this signer's public key and returns the
// claims, enforcing expiry. Convenience wrapper around the package Verify.
func (s *Signer) Verify(tok string) (Claims, error) { return Verify(tok, s.pub) }

// Verify checks the compact JWS against pub and returns the claims. It rejects
// any algorithm other than EdDSA and enforces the exp claim.
func Verify(tok string, pub ed25519.PublicKey) (Claims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return Claims{}, ErrMalformed
	}
	hb, err := b64.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var h header
	if err := json.Unmarshal(hb, &h); err != nil {
		return Claims{}, ErrMalformed
	}
	if h.Alg != "EdDSA" {
		return Claims{}, fmt.Errorf("%w: alg %q tidak didukung", ErrSignature, h.Alg)
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return Claims{}, ErrSignature
	}
	cb, err := b64.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(cb, &c); err != nil {
		return Claims{}, ErrMalformed
	}
	if c.ExpiresAt > 0 && time.Now().Unix() > c.ExpiresAt {
		return Claims{}, ErrExpired
	}
	return c, nil
}

// JWKS returns the JSON Web Key Set advertising the public key, for the
// /.well-known/jwks.json endpoint. Ed25519 keys use the OKP key type.
func JWKS(pub ed25519.PublicKey, kid string) map[string]any {
	return map[string]any{
		"keys": []map[string]any{{
			"kty": "OKP",
			"crv": "Ed25519",
			"use": "sig",
			"alg": "EdDSA",
			"kid": kid,
			"x":   b64.EncodeToString(pub),
		}},
	}
}
