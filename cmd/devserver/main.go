// Command devserver runs the master auth service with an IN-MEMORY user store
// and an ephemeral signing key — DEV ONLY, no persistence. It exists so the SSO
// login flow can be exercised end-to-end (browser → auth → department) on a
// machine without Postgres. Production uses cmd/server (Postgres-only).
//
// The HTTP router, handlers, token signing/JWKS and bcrypt are the SAME code as
// production; only the repository implementation differs.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log"
	"net/http"
	"strings"
	"sync"

	"greenpark/auth/internal/config"
	"greenpark/auth/internal/domain"
	"greenpark/auth/internal/repository"
	"greenpark/auth/internal/service"
	"greenpark/auth/internal/token"
	httptransport "greenpark/auth/internal/transport/http"
)

func main() {
	cfg := config.Load()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatalf("devserver: genkey: %v", err)
	}
	signer := token.NewSigner(priv)
	log.Printf("devserver: ephemeral signing key (kid=%s)", signer.KID())

	repo := newMemRepo()
	ctx := context.Background()
	deptList := make([]domain.Department, 0, len(config.Departments))
	for _, d := range config.Departments {
		dd := domain.Department{Code: d.Code, Name: d.Name}
		deptList = append(deptList, dd)
		_ = repo.UpsertDepartment(ctx, dd)
	}

	authSvc := service.NewAuth(repo, signer, cfg.Issuer, cfg.AccessTTL, cfg.RefreshTTL, nil)
	usersSvc := service.NewUsers(repo, authSvc, deptList, nil)

	// Seed superadmin + cross-department admin/viewer (same as AUTH_SEED_DEMO).
	active := true
	adminRoles := map[string]domain.Role{}
	viewerRoles := map[string]domain.Role{}
	for _, d := range deptList {
		adminRoles[d.Code] = domain.RoleAdmin
		viewerRoles[d.Code] = domain.RoleViewer
	}
	seed := []service.CreateInput{
		{Username: "superadmin", Name: "Super Admin", Password: "superadmin123", Super: true, Active: &active},
		{Username: "admin", Name: "Admin Demo", Password: "admin123", Active: &active, Roles: adminRoles},
		{Username: "viewer", Name: "Viewer Demo", Password: "viewer123", Active: &active, Roles: viewerRoles},
	}
	for _, in := range seed {
		if _, err := usersSvc.EnsureUser(ctx, in); err != nil {
			log.Fatalf("devserver: seed %q: %v", in.Username, err)
		}
	}
	log.Println("devserver: seeded superadmin / admin(admin123) / viewer(viewer123)")

	handler := httptransport.NewHandler(authSvc, usersSvc, signer)
	router := httptransport.NewRouter(handler, cfg.Origins())
	log.Printf("devserver (IN-MEMORY) auth API listening on http://localhost:%s", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, router); err != nil {
		log.Fatal(err)
	}
}

// memRepo is an in-memory repository.Repository (dev only).
type memRepo struct {
	mu      sync.Mutex
	users   map[string]domain.User
	byName  map[string]string
	depts   map[string]domain.Department
	refresh map[string]domain.RefreshToken
	byHash  map[string]string
}

func newMemRepo() *memRepo {
	return &memRepo{
		users:   map[string]domain.User{},
		byName:  map[string]string{},
		depts:   map[string]domain.Department{},
		refresh: map[string]domain.RefreshToken{},
		byHash:  map[string]string{},
	}
}

func (m *memRepo) CreateUser(_ context.Context, u domain.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byName[strings.ToLower(u.Username)]; ok {
		return repository.ErrUsernameTaken
	}
	if u.Roles == nil {
		u.Roles = map[string]domain.Role{}
	}
	m.users[u.ID] = u
	m.byName[strings.ToLower(u.Username)] = u.ID
	return nil
}

func (m *memRepo) UpdateUser(_ context.Context, u domain.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[u.ID]; !ok {
		return repository.ErrNotFound
	}
	m.users[u.ID] = u
	return nil
}

func (m *memRepo) DeleteUser(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return repository.ErrNotFound
	}
	delete(m.byName, strings.ToLower(u.Username))
	delete(m.users, id)
	return nil
}

func (m *memRepo) UserByID(_ context.Context, id string) (domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return domain.User{}, repository.ErrNotFound
	}
	return u, nil
}

func (m *memRepo) UserByUsername(_ context.Context, username string) (domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byName[strings.ToLower(username)]
	if !ok {
		return domain.User{}, repository.ErrNotFound
	}
	return m.users[id], nil
}

func (m *memRepo) ListUsers(_ context.Context) ([]domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.User, 0, len(m.users))
	for _, u := range m.users {
		out = append(out, u)
	}
	return out, nil
}

func (m *memRepo) CountUsers(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.users), nil
}

func (m *memRepo) SetMembership(_ context.Context, userID, dept string, role domain.Role) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[userID]
	if !ok {
		return repository.ErrNotFound
	}
	u.Roles[dept] = role
	m.users[userID] = u
	return nil
}

func (m *memRepo) RemoveMembership(_ context.Context, userID, dept string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[userID]; ok {
		delete(u.Roles, dept)
	}
	return nil
}

func (m *memRepo) UpsertDepartment(_ context.Context, d domain.Department) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.depts[d.Code] = d
	return nil
}

func (m *memRepo) ListDepartments(_ context.Context) ([]domain.Department, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.Department, 0, len(m.depts))
	for _, d := range m.depts {
		out = append(out, d)
	}
	return out, nil
}

func (m *memRepo) StoreRefresh(_ context.Context, t domain.RefreshToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh[t.ID] = t
	m.byHash[t.Hash] = t.ID
	return nil
}

func (m *memRepo) RefreshByHash(_ context.Context, hash string) (domain.RefreshToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byHash[hash]
	if !ok {
		return domain.RefreshToken{}, repository.ErrNotFound
	}
	return m.refresh[id], nil
}

func (m *memRepo) RevokeRefresh(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.refresh[id]; ok {
		t.Revoked = true
		m.refresh[id] = t
	}
	return nil
}

func (m *memRepo) RevokeAllForUser(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, t := range m.refresh {
		if t.UserID == userID {
			t.Revoked = true
			m.refresh[id] = t
		}
	}
	return nil
}

func (m *memRepo) DeleteExpiredRefresh(_ context.Context) error { return nil }
func (m *memRepo) Close() error                                 { return nil }
