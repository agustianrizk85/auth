package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"greenpark/auth/internal/domain"
	"greenpark/auth/internal/passwd"
	"greenpark/auth/internal/repository"
)

// Validation errors for user administration.
var (
	ErrUsernameRequired = errors.New("username wajib diisi")
	ErrPasswordRequired = errors.New("password wajib diisi")
	ErrPasswordTooShort = errors.New("password minimal 6 karakter")
	ErrInvalidRole      = errors.New("role tidak valid (admin atau viewer)")
	ErrUnknownDept      = errors.New("departemen tidak dikenal")
)

// Users is the admin service for managing accounts and their department roles.
type Users struct {
	repo  repository.Repository
	auth  *Auth
	newID IDGen
	depts map[string]struct{} // known department codes, for validation
}

// NewUsers wires the user-admin service. knownDepts gates which department
// codes may be granted as roles.
func NewUsers(repo repository.Repository, auth *Auth, knownDepts []domain.Department, newID IDGen) *Users {
	if newID == nil {
		newID = randomID
	}
	set := make(map[string]struct{}, len(knownDepts))
	for _, d := range knownDepts {
		set[d.Code] = struct{}{}
	}
	return &Users{repo: repo, auth: auth, newID: newID, depts: set}
}

// CreateInput describes a new account.
type CreateInput struct {
	Username string
	Email    string
	Name     string
	Password string
	Super    bool
	Active   *bool // defaults to true when nil
	Roles    map[string]domain.Role
}

// Create validates and persists a new user, returning it without password data.
func (s *Users) Create(ctx context.Context, in CreateInput) (domain.User, error) {
	in.Username = strings.TrimSpace(in.Username)
	if in.Username == "" {
		return domain.User{}, ErrUsernameRequired
	}
	if in.Password == "" {
		return domain.User{}, ErrPasswordRequired
	}
	if len(in.Password) < 6 {
		return domain.User{}, ErrPasswordTooShort
	}
	if err := s.validateRoles(in.Roles); err != nil {
		return domain.User{}, err
	}
	hash, err := passwd.Hash(in.Password)
	if err != nil {
		return domain.User{}, err
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}
	u := domain.User{
		ID:           s.newID(),
		Username:     in.Username,
		Email:        strings.TrimSpace(in.Email),
		Name:         strings.TrimSpace(in.Name),
		Super:        in.Super,
		Active:       active,
		Roles:        in.Roles,
		PasswordHash: hash,
	}
	if err := s.repo.CreateUser(ctx, u); err != nil {
		return domain.User{}, err
	}
	return sanitize(u), nil
}

// EnsureUser creates the account if its username is free, and reports whether
// it was created. An already-present username is treated as success (no-op),
// which makes it safe to call on every startup for idempotent seeding.
func (s *Users) EnsureUser(ctx context.Context, in CreateInput) (created bool, err error) {
	if _, err := s.repo.UserByUsername(ctx, in.Username); err == nil {
		return false, nil
	} else if !errors.Is(err, repository.ErrNotFound) {
		return false, err
	}
	if _, err := s.Create(ctx, in); err != nil {
		if errors.Is(err, repository.ErrUsernameTaken) { // raced with another instance
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// UpdateInput describes a partial update. Nil fields are left unchanged; a
// non-nil Roles map replaces the whole grant set.
type UpdateInput struct {
	Email    *string
	Name     *string
	Password *string
	Super    *bool
	Active   *bool
	Roles    map[string]domain.Role
}

// Update applies changes to an existing user.
func (s *Users) Update(ctx context.Context, id string, in UpdateInput) (domain.User, error) {
	u, err := s.repo.UserByID(ctx, id)
	if err != nil {
		return domain.User{}, err
	}
	if in.Email != nil {
		u.Email = strings.TrimSpace(*in.Email)
	}
	if in.Name != nil {
		u.Name = strings.TrimSpace(*in.Name)
	}
	if in.Super != nil {
		u.Super = *in.Super
	}
	if in.Active != nil {
		u.Active = *in.Active
	}
	if in.Roles != nil {
		if err := s.validateRoles(in.Roles); err != nil {
			return domain.User{}, err
		}
		u.Roles = in.Roles
	}
	if in.Password != nil {
		if len(*in.Password) < 6 {
			return domain.User{}, ErrPasswordTooShort
		}
		if u.PasswordHash, err = passwd.Hash(*in.Password); err != nil {
			return domain.User{}, err
		}
	}
	u.UpdatedAt = time.Now()
	if err := s.repo.UpdateUser(ctx, u); err != nil {
		return domain.User{}, err
	}
	// A password change or deactivation should drop existing sessions.
	if in.Password != nil || (in.Active != nil && !*in.Active) {
		_ = s.repo.RevokeAllForUser(ctx, u.ID)
	}
	return sanitize(u), nil
}

// Delete removes a user.
func (s *Users) Delete(ctx context.Context, id string) error {
	return s.repo.DeleteUser(ctx, id)
}

// List returns all users without password data.
func (s *Users) List(ctx context.Context) ([]domain.User, error) {
	users, err := s.repo.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range users {
		users[i] = sanitize(users[i])
	}
	return users, nil
}

// Get returns a single user without password data.
func (s *Users) Get(ctx context.Context, id string) (domain.User, error) {
	u, err := s.repo.UserByID(ctx, id)
	if err != nil {
		return domain.User{}, err
	}
	return sanitize(u), nil
}

// SetRole grants or updates a single department role for a user.
func (s *Users) SetRole(ctx context.Context, userID, dept string, role domain.Role) error {
	if _, ok := s.depts[dept]; !ok {
		return ErrUnknownDept
	}
	if !role.Valid() {
		return ErrInvalidRole
	}
	if err := s.repo.SetMembership(ctx, userID, dept, role); err != nil {
		return err
	}
	_ = s.repo.RevokeAllForUser(ctx, userID) // force re-issue with new role
	return nil
}

// RemoveRole revokes a user's access to a department.
func (s *Users) RemoveRole(ctx context.Context, userID, dept string) error {
	if err := s.repo.RemoveMembership(ctx, userID, dept); err != nil {
		return err
	}
	_ = s.repo.RevokeAllForUser(ctx, userID)
	return nil
}

// Departments returns the known department catalogue.
func (s *Users) Departments(ctx context.Context) ([]domain.Department, error) {
	return s.repo.ListDepartments(ctx)
}

func (s *Users) validateRoles(roles map[string]domain.Role) error {
	for dept, role := range roles {
		if _, ok := s.depts[dept]; !ok {
			return ErrUnknownDept
		}
		if !role.Valid() {
			return ErrInvalidRole
		}
	}
	return nil
}

// sanitize strips password material before returning a user to clients.
func sanitize(u domain.User) domain.User {
	u.PasswordHash = ""
	if u.Roles == nil {
		u.Roles = map[string]domain.Role{}
	}
	return u
}
