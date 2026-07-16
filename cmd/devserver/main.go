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
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"greenpark/auth/internal/ai"
	"greenpark/auth/internal/bootstrap"
	"greenpark/auth/internal/config"
	"greenpark/auth/internal/domain"
	"greenpark/auth/internal/keys"
	"greenpark/auth/internal/repository"
	"greenpark/auth/internal/service"
	"greenpark/auth/internal/token"
	httptransport "greenpark/auth/internal/transport/http"
)

func main() {
	cfg := config.Load()

	// PERSISTENT signing key (same key file cmd/server uses, keys/, gitignored).
	// Previously the devserver generated an EPHEMERAL key every start, so each
	// restart/deploy invalidated everyone's token → mass 401 "logged out". With a
	// persistent key the stateless access tokens stay valid across restarts.
	priv, created, err := keys.LoadOrCreate(cfg.PrivateKeyPath, cfg.PublicKeyPath)
	if err != nil {
		log.Fatalf("devserver: load/create signing key: %v", err)
	}
	signer := token.NewSigner(priv)
	if created {
		log.Printf("devserver: generated NEW persistent signing key (kid=%s) at %s", signer.KID(), cfg.PrivateKeyPath)
	} else {
		log.Printf("devserver: loaded persistent signing key (kid=%s)", signer.KID())
	}

	// Long-lived access tokens: the devserver has no frontend refresh flow and its
	// (in-memory) refresh tokens don't survive a restart anyway, so a 15-min TTL
	// would just log everyone out repeatedly. Honor an explicit AUTH_ACCESS_TTL if
	// it's already long; otherwise default to 30 days.
	accessTTL := cfg.AccessTTL
	if accessTTL < 24*time.Hour {
		accessTTL = 30 * 24 * time.Hour
	}
	log.Printf("devserver: access-token TTL = %s", accessTTL)

	repo := newMemRepo()
	ctx := context.Background()
	deptList := make([]domain.Department, 0, len(config.Departments))
	for _, d := range config.Departments {
		dd := domain.Department{Code: d.Code, Name: d.Name}
		deptList = append(deptList, dd)
		_ = repo.UpsertDepartment(ctx, dd)
	}
	// Seed the role catalogue (master data) when empty — mirrors cmd/server.
	if existing, _ := repo.ListRoles(ctx); len(existing) == 0 {
		for _, rd := range bootstrap.DefaultRoles() {
			_ = repo.UpsertRole(ctx, rd)
		}
	}

	authSvc := service.NewAuth(repo, signer, cfg.Issuer, accessTTL, cfg.RefreshTTL, nil)
	usersSvc := service.NewUsers(repo, authSvc, deptList, nil)

	// Seed the real Greenpark roster. Passwords MATCH each department backend's
	// seed so the dashboard can bridge a native data token per logged-in person.
	// Role strings use each department's own vocabulary (kadep/drafter/staff/…)
	// which the dashboard UI reads directly; backend write-gating is unaffected.
	active := true
	// Superadmin (auth admin API) + the shared Greenpark roster. The roster is
	// the single source of truth (internal/bootstrap), shared with cmd/server so
	// the two never drift.
	seed := append([]service.CreateInput{
		{Username: "superadmin", Name: "Super Admin", Password: "superadmin123", Super: true, Active: &active},
	}, bootstrap.GreenparkRoster(deptList)...)
	for _, in := range seed {
		if _, err := usersSvc.EnsureUser(ctx, in); err != nil {
			log.Fatalf("devserver: seed %q: %v", in.Username, err)
		}
	}
	log.Printf("devserver: seeded %d Greenpark accounts (superadmin + direktur + 6 divisi)", len(seed))

	handler := httptransport.NewHandler(authSvc, usersSvc, signer)
	// Ollama Cloud is the AI provider (env OLLAMA_*); fall back to legacy
	// OPENROUTER_* names so existing deploy env keeps working during migration.
	aiKey := os.Getenv("OLLAMA_API_KEY")
	if aiKey == "" {
		aiKey = os.Getenv("OPENROUTER_API_KEY")
	}
	aiModel := os.Getenv("OLLAMA_MODEL")
	if aiModel == "" {
		aiModel = os.Getenv("OPENROUTER_MODEL")
	}
	keyFile := os.Getenv("OLLAMA_KEY_FILE")
	if keyFile == "" {
		keyFile = os.Getenv("OPENROUTER_KEY_FILE")
	}
	if keyFile == "" {
		keyFile = "data/ollama.key"
	}
	aiClient := ai.New(aiKey, aiModel, os.Getenv("OLLAMA_ENDPOINT")).WithPersist(keyFile)
	handler.SetAI(aiClient)
	if aiClient.Configured() {
		log.Printf("devserver: AI (Ollama Cloud) ENABLED (model %s)", aiClient.Model())
	} else {
		log.Printf("devserver: AI key belum diset — atur dari UI (PUT /api/ai/config) atau OLLAMA_API_KEY")
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
	roles   map[string]domain.RoleDef
	refresh map[string]domain.RefreshToken
	byHash  map[string]string
}

func newMemRepo() *memRepo {
	return &memRepo{
		users:   map[string]domain.User{},
		byName:  map[string]string{},
		depts:   map[string]domain.Department{},
		roles:   map[string]domain.RoleDef{},
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

func (m *memRepo) DeleteDepartment(_ context.Context, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.depts, code)
	return nil
}

func (m *memRepo) ListRoles(_ context.Context) ([]domain.RoleDef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]domain.RoleDef, 0, len(m.roles))
	for _, r := range m.roles {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sort != out[j].Sort {
			return out[i].Sort < out[j].Sort
		}
		return out[i].Value < out[j].Value
	})
	return out, nil
}

func (m *memRepo) UpsertRole(_ context.Context, r domain.RoleDef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roles[r.Value] = r
	return nil
}

func (m *memRepo) DeleteRole(_ context.Context, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.roles, value)
	return nil
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
