// Package repository is the persistence layer for the master auth service,
// backed by PostgreSQL. It owns users, their per-department roles, the
// department catalogue, and refresh tokens.
package repository

import (
	"context"
	"errors"

	"greenpark/auth/internal/domain"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("data tidak ditemukan")

// ErrUsernameTaken is returned when creating/renaming to an existing username.
var ErrUsernameTaken = errors.New("username sudah dipakai")

// Repository is the storage contract used by the service layer.
type Repository interface {
	// Users.
	CreateUser(ctx context.Context, u domain.User) error
	UpdateUser(ctx context.Context, u domain.User) error
	DeleteUser(ctx context.Context, id string) error
	UserByID(ctx context.Context, id string) (domain.User, error)
	UserByUsername(ctx context.Context, username string) (domain.User, error)
	ListUsers(ctx context.Context) ([]domain.User, error)
	CountUsers(ctx context.Context) (int, error)

	// Per-department role grants.
	SetMembership(ctx context.Context, userID, dept string, role domain.Role) error
	RemoveMembership(ctx context.Context, userID, dept string) error

	// Department catalogue.
	UpsertDepartment(ctx context.Context, d domain.Department) error
	ListDepartments(ctx context.Context) ([]domain.Department, error)
	DeleteDepartment(ctx context.Context, code string) error

	// Role catalogue (master data managed by the super admin).
	ListRoles(ctx context.Context) ([]domain.RoleDef, error)
	UpsertRole(ctx context.Context, r domain.RoleDef) error
	DeleteRole(ctx context.Context, value string) error

	// Refresh tokens.
	StoreRefresh(ctx context.Context, t domain.RefreshToken) error
	RefreshByHash(ctx context.Context, hash string) (domain.RefreshToken, error)
	RevokeRefresh(ctx context.Context, id string) error
	RevokeAllForUser(ctx context.Context, userID string) error
	DeleteExpiredRefresh(ctx context.Context) error

	Close() error
}
