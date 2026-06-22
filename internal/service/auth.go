// Package service holds the master auth business logic: authenticating users,
// issuing short-lived access tokens (JWT) plus long-lived revocable refresh
// tokens, and administering users and their per-department roles.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"greenpark/auth/internal/domain"
	"greenpark/auth/internal/passwd"
	"greenpark/auth/internal/repository"
	"greenpark/auth/internal/token"
)

// Errors surfaced to the transport layer.
var (
	ErrInvalidCredentials = errors.New("username atau password salah")
	ErrAccountDisabled    = errors.New("akun dinonaktifkan")
	ErrInvalidRefresh     = errors.New("refresh token tidak valid atau kedaluwarsa")
)

// IDGen produces unique identifiers for new users and tokens.
type IDGen func() string

// Auth is the authentication service.
type Auth struct {
	repo       repository.Repository
	signer     *token.Signer
	issuer     string
	accessTTL  time.Duration
	refreshTTL time.Duration
	newID      IDGen
}

// Tokens is the credential pair returned by Login/Refresh.
type Tokens struct {
	AccessToken  string      `json:"accessToken"`
	RefreshToken string      `json:"refreshToken"`
	TokenType    string      `json:"tokenType"`
	ExpiresIn    int         `json:"expiresIn"` // access-token lifetime, seconds
	User         domain.User `json:"user"`
}

// NewAuth wires the auth service.
func NewAuth(repo repository.Repository, signer *token.Signer, issuer string, accessTTL, refreshTTL time.Duration, newID IDGen) *Auth {
	if newID == nil {
		newID = randomID
	}
	return &Auth{
		repo:       repo,
		signer:     signer,
		issuer:     issuer,
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
		newID:      newID,
	}
}

// Login verifies credentials and returns a fresh access+refresh token pair.
func (a *Auth) Login(ctx context.Context, username, password string) (Tokens, error) {
	u, err := a.repo.UserByUsername(ctx, username)
	if err != nil || !passwd.Verify(password, u.PasswordHash) {
		// Run a dummy hash on the not-found path to blunt user enumeration via
		// timing. The cost difference between bcrypt and a no-op is otherwise
		// observable.
		if err != nil {
			_ = passwd.Verify(password, dummyHash)
		}
		return Tokens{}, ErrInvalidCredentials
	}
	if !u.Active {
		return Tokens{}, ErrAccountDisabled
	}
	return a.issue(ctx, u)
}

// Refresh rotates a refresh token: the presented token is revoked and a brand
// new access+refresh pair is issued (refresh-token rotation). A reused or
// revoked token is rejected.
func (a *Auth) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	rec, err := a.repo.RefreshByHash(ctx, hashToken(refreshToken))
	if err != nil || rec.Revoked || time.Now().After(rec.ExpiresAt) {
		return Tokens{}, ErrInvalidRefresh
	}
	u, err := a.repo.UserByID(ctx, rec.UserID)
	if err != nil {
		return Tokens{}, ErrInvalidRefresh
	}
	if !u.Active {
		_ = a.repo.RevokeAllForUser(ctx, u.ID)
		return Tokens{}, ErrAccountDisabled
	}
	// Rotate: invalidate the presented token before minting a replacement.
	if err := a.repo.RevokeRefresh(ctx, rec.ID); err != nil {
		return Tokens{}, err
	}
	return a.issue(ctx, u)
}

// Logout revokes the presented refresh token (no error if unknown).
func (a *Auth) Logout(ctx context.Context, refreshToken string) error {
	rec, err := a.repo.RefreshByHash(ctx, hashToken(refreshToken))
	if err != nil {
		return nil
	}
	return a.repo.RevokeRefresh(ctx, rec.ID)
}

// LogoutAll revokes every refresh token for the user (e.g. "sign out everywhere").
func (a *Auth) LogoutAll(ctx context.Context, userID string) error {
	return a.repo.RevokeAllForUser(ctx, userID)
}

// Verify validates an access token (JWT) and returns its claims. Used by the
// auth service's own protected endpoints; departments verify independently with
// the published public key.
func (a *Auth) Verify(accessToken string) (token.Claims, error) {
	return a.signer.Verify(accessToken)
}

// UserByID loads the current persisted user (used by /me to return fresh data).
func (a *Auth) UserByID(ctx context.Context, id string) (domain.User, error) {
	return a.repo.UserByID(ctx, id)
}

// issue builds and persists a new token pair for u.
func (a *Auth) issue(ctx context.Context, u domain.User) (Tokens, error) {
	now := time.Now()
	claims := token.Claims{
		Issuer:    a.issuer,
		Subject:   u.ID,
		Username:  u.Username,
		Email:     u.Email,
		Name:      u.Name,
		Super:     u.Super,
		Roles:     rolesToStrings(u.Roles),
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(a.accessTTL).Unix(),
		JTI:       a.newID(),
	}
	access, err := a.signer.Sign(claims)
	if err != nil {
		return Tokens{}, err
	}

	refresh, err := randomSecret()
	if err != nil {
		return Tokens{}, err
	}
	rec := domain.RefreshToken{
		ID:        a.newID(),
		UserID:    u.ID,
		Hash:      hashToken(refresh),
		ExpiresAt: now.Add(a.refreshTTL),
	}
	if err := a.repo.StoreRefresh(ctx, rec); err != nil {
		return Tokens{}, err
	}

	u.PasswordHash = ""
	return Tokens{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
		ExpiresIn:    int(a.accessTTL.Seconds()),
		User:         u,
	}, nil
}

func rolesToStrings(roles map[string]domain.Role) map[string]string {
	if len(roles) == 0 {
		return nil
	}
	out := make(map[string]string, len(roles))
	for dept, role := range roles {
		out[dept] = string(role)
	}
	return out
}

// hashToken is the at-rest representation of a refresh secret (SHA-256 hex).
// Refresh tokens are high-entropy random values, so a fast hash is appropriate
// here (unlike passwords, which need bcrypt).
func hashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// rand.Read essentially never fails; fall back to a fixed marker so the
		// caller still gets a non-empty (if non-unique) id rather than panicking.
		return "id-fallback"
	}
	return hex.EncodeToString(b)
}

// dummyHash is a valid bcrypt hash of a throwaway password, used to equalise
// timing on the user-not-found path. Generated once at init.
var dummyHash = func() string {
	h, _ := passwd.Hash("timing-equaliser")
	return h
}()
