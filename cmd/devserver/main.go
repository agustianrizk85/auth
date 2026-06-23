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
	"os"
	"strings"
	"sync"

	"greenpark/auth/internal/ai"
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

	// Seed the real Greenpark roster. Passwords MATCH each department backend's
	// seed so the dashboard can bridge a native data token per logged-in person.
	// Role strings use each department's own vocabulary (kadep/drafter/staff/…)
	// which the dashboard UI reads directly; backend write-gating is unaffected.
	active := true
	allViewer := map[string]domain.Role{}
	for _, d := range deptList {
		allViewer[d.Code] = domain.RoleViewer
	}
	r := func(dept, role string) map[string]domain.Role { return map[string]domain.Role{dept: domain.Role(role)} }
	seed := []service.CreateInput{
		// Auth admin API.
		{Username: "superadmin", Name: "Super Admin", Password: "superadmin123", Super: true, Active: &active},
		// ── Direktur (akses SEMUA divisi) ──
		{Username: "ceo@greenpark.id", Email: "ceo@greenpark.id", Name: "Direktur Utama", Password: "ceo123", Active: &active, Roles: allViewer},          // overview-only
		{Username: "dirops@greenpark.id", Email: "dirops@greenpark.id", Name: "Direktur Operasional", Password: "dirops123", Super: true, Active: &active}, // boleh approve
		// ── Perencanaan ──
		{Username: "kadep", Name: "Kepala Dept. Perencanaan", Password: "kadep123", Active: &active, Roles: r("perencanaan", "kadep")},
		{Username: "randi", Name: "Randi", Password: "randi123", Active: &active, Roles: r("perencanaan", "drafter")},
		{Username: "ananto", Name: "Ananto", Password: "ananto123", Active: &active, Roles: r("perencanaan", "drafter")},
		{Username: "agus", Name: "Agus", Password: "agus123", Active: &active, Roles: r("perencanaan", "drafter")},
		// ── Legal & Perizinan (Permit) ──
		{Username: "kadep@greenpark.id", Email: "kadep@greenpark.id", Name: "Kepala Dept. Legal", Password: "kadep123", Active: &active, Roles: r("legalpermit", "kadep")},
		{Username: "legal@greenpark.id", Email: "legal@greenpark.id", Name: "Staf Legal Permit", Password: "legal123", Active: &active, Roles: r("legalpermit", "legal_permit")},
		// ── Marketing ──
		{Username: "marketing@greenpark.id", Email: "marketing@greenpark.id", Name: "Kepala Dept. Marketing", Password: "kadep123", Active: &active, Roles: r("marketing", "kadep")},
		{Username: "ichsan@greenpark.id", Email: "ichsan@greenpark.id", Name: "Ichsan", Password: "yqfZ2hWtMQ", Active: &active, Roles: r("marketing", "staff")},
		{Username: "sohee@greenpark.id", Email: "sohee@greenpark.id", Name: "Sohee", Password: "ByxZQnQ7Rc", Active: &active, Roles: r("marketing", "staff")},
		{Username: "mila@greenpark.id", Email: "mila@greenpark.id", Name: "Mila", Password: "QpkdKGfZcf", Active: &active, Roles: r("marketing", "staff")},
		{Username: "hilman@greenpark.id", Email: "hilman@greenpark.id", Name: "Hilman", Password: "PPWrxk7stW", Active: &active, Roles: r("marketing", "staff")},
		{Username: "hakim@greenpark.id", Email: "hakim@greenpark.id", Name: "Hakim", Password: "MazUSccPKC", Active: &active, Roles: r("marketing", "staff")},
		{Username: "hanif@greenpark.id", Email: "hanif@greenpark.id", Name: "Hanif", Password: "vrnzxpPsMg", Active: &active, Roles: r("marketing", "staff")},
		{Username: "ivan@greenpark.id", Email: "ivan@greenpark.id", Name: "Ivan", Password: "AVqhqec2ca", Active: &active, Roles: r("marketing", "staff")},
		{Username: "fatimah@greenpark.id", Email: "fatimah@greenpark.id", Name: "Fatimah", Password: "agHYVXCArP", Active: &active, Roles: r("marketing", "staff")},
		{Username: "rahadian@greenpark.id", Email: "rahadian@greenpark.id", Name: "Rahadian", Password: "38fpPu2GtU", Active: &active, Roles: r("marketing", "staff")},
		// ── Sales ──
		{Username: "sales@greenpark.id", Email: "sales@greenpark.id", Name: "Kepala Dept. Sales", Password: "sales123", Active: &active, Roles: r("sales", "kadep")},
		{Username: "viewer@greenpark.id", Email: "viewer@greenpark.id", Name: "Sales Viewer", Password: "viewer123", Active: &active, Roles: r("sales", "viewer")},
		// ── Keuangan ──
		{Username: "keuangan@greenpark.id", Email: "keuangan@greenpark.id", Name: "Kepala Dept. Keuangan", Password: "keuangan123", Active: &active, Roles: r("finance", "kadep")},
	}
	for _, in := range seed {
		if _, err := usersSvc.EnsureUser(ctx, in); err != nil {
			log.Fatalf("devserver: seed %q: %v", in.Username, err)
		}
	}
	log.Printf("devserver: seeded %d Greenpark accounts (superadmin + direktur + 5 divisi)", len(seed))

	handler := httptransport.NewHandler(authSvc, usersSvc, signer)
	keyFile := os.Getenv("OPENROUTER_KEY_FILE")
	if keyFile == "" {
		keyFile = "data/openrouter.key"
	}
	aiClient := ai.New(os.Getenv("OPENROUTER_API_KEY"), os.Getenv("OPENROUTER_MODEL"), os.Getenv("OPENROUTER_SITE")).WithPersist(keyFile)
	handler.SetAI(aiClient)
	if aiClient.Configured() {
		log.Printf("devserver: AI assistant ENABLED (model %s)", aiClient.Model())
	} else {
		log.Printf("devserver: AI assistant key belum diset — atur dari UI (PUT /api/ai/config) atau OPENROUTER_API_KEY")
	}
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
