// Package domain holds the core identity types shared across the master auth
// service: users, their per-department roles, departments, and refresh tokens.
package domain

import "time"

// Role enumerates a user's access level within a single department.
type Role string

const (
	RoleAdmin  Role = "admin"  // full master-data access for that department
	RoleViewer Role = "viewer" // read-only dashboard access for that department
)

// Valid reports whether r is a usable role. Any non-empty value is accepted so
// departments with their own role vocabularies (e.g. legalpermit's
// ceo/dirops/kadep, perencanaan's ceo/kadep/pic/tim) can be represented. The
// well-known RoleAdmin/RoleViewer are the defaults for the dashboard backends.
func (r Role) Valid() bool { return r != "" }

// User is a person who can authenticate. A user may hold roles in several
// departments at once (the Roles map). Super users implicitly have admin
// access to every department and the auth admin API.
//
// Password material is never serialised to clients (json:"-"); it only lives in
// the persisted store.
type User struct {
	ID           string          `json:"id"`
	Username     string          `json:"username"`
	Email        string          `json:"email"`
	Name         string          `json:"name"`
	Super        bool            `json:"super"`
	Active       bool            `json:"active"`
	Roles        map[string]Role `json:"roles"` // department code -> role
	PasswordHash string          `json:"-"`
	CreatedAt    time.Time       `json:"createdAt"`
	UpdatedAt    time.Time       `json:"updatedAt"`
}

// HasDept reports whether the user may access the given department at all.
func (u User) HasDept(code string) bool {
	if u.Super {
		return true
	}
	_, ok := u.Roles[code]
	return ok
}

// IsAdminOf reports whether the user may perform writes in the given department.
func (u User) IsAdminOf(code string) bool {
	if u.Super {
		return true
	}
	return u.Roles[code] == RoleAdmin
}

// Department is a business unit the auth service governs.
type Department struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// RefreshToken is a long-lived, revocable credential used to mint new access
// tokens. Only the SHA-256 hash of the secret is stored — never the secret.
type RefreshToken struct {
	ID        string
	UserID    string
	Hash      string
	ExpiresAt time.Time
	Revoked   bool
	CreatedAt time.Time
}
