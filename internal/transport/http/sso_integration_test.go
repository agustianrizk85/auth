package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"greenpark/auth/integration/authmw"
	"greenpark/auth/internal/domain"
	"greenpark/auth/internal/repository"
	"greenpark/auth/internal/service"
	"greenpark/auth/internal/token"

	"crypto/ed25519"
	"crypto/rand"
)

// memRepo is an in-memory repository.Repository for integration testing without
// a database. It is intentionally simple — correctness over performance.
type memRepo struct {
	mu      sync.Mutex
	users   map[string]domain.User         // id -> user (Roles populated)
	byName  map[string]string              // lower(username) -> id
	depts   map[string]domain.Department   // code -> dept
	refresh map[string]domain.RefreshToken // id -> token
	byHash  map[string]string              // hash -> id
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

// testStack wires the full auth HTTP service over a memory repo and returns a
// running httptest server plus the department-side verifier.
func testStack(t *testing.T) (*httptest.Server, *authmw.Verifier, *service.Users) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	signer := token.NewSigner(priv)
	repo := newMemRepo()
	_ = repo.UpsertDepartment(context.Background(), domain.Department{Code: "finance", Name: "Keuangan"})
	_ = repo.UpsertDepartment(context.Background(), domain.Department{Code: "sales", Name: "Sales"})

	authSvc := service.NewAuth(repo, signer, "greenpark-auth", 15*time.Minute, 720*time.Hour, nil)
	depts := []domain.Department{{Code: "finance"}, {Code: "sales"}}
	usersSvc := service.NewUsers(repo, authSvc, depts, nil)

	srv := httptest.NewServer(NewRouter(NewHandler(authSvc, usersSvc, signer), []string{"*"}))
	t.Cleanup(srv.Close)

	v, err := authmw.New(authmw.Options{
		JWKSURL:    srv.URL + "/.well-known/jwks.json",
		Department: "finance",
		Issuer:     "greenpark-auth",
	})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	return srv, v, usersSvc
}

func postJSON(t *testing.T, url string, body any, bearer string) (*http.Response, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	res.Body.Close()
	return res, out
}

// TestSSOEndToEnd exercises the real cross-service path: a user logs in at the
// auth service, a department verifies the issued token locally via JWKS, /me
// works, and refresh rotation invalidates the old refresh token.
func TestSSOEndToEnd(t *testing.T) {
	srv, verifier, users := testStack(t)
	ctx := context.Background()

	active := true
	if _, err := users.Create(ctx, service.CreateInput{
		Username: "alice",
		Email:    "alice@greenpark.id",
		Name:     "Alice",
		Password: "secret123",
		Active:   &active,
		Roles:    map[string]domain.Role{"finance": domain.RoleAdmin, "sales": domain.RoleViewer},
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// 1) Login.
	res, body := postJSON(t, srv.URL+"/api/auth/login", map[string]string{"username": "alice", "password": "secret123"}, "")
	if res.StatusCode != 200 {
		t.Fatalf("login status=%d body=%v", res.StatusCode, body)
	}
	access, _ := body["accessToken"].(string)
	refresh, _ := body["refreshToken"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("missing tokens: %v", body)
	}

	// 2) Department verifies the access token LOCALLY via the auth JWKS.
	claims, err := verifier.Verify(access)
	if err != nil {
		t.Fatalf("dept verify: %v", err)
	}
	if claims.Username != "alice" || !claims.IsAdmin("finance") || claims.CanAccess("teknik") {
		t.Fatalf("unexpected claims: %+v", claims)
	}
	if claims.Email != "alice@greenpark.id" {
		t.Fatalf("email claim missing: %+v", claims)
	}

	// 3) /me with the access token.
	meReq, _ := http.NewRequest("GET", srv.URL+"/api/auth/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+access)
	meRes, err := http.DefaultClient.Do(meReq)
	if err != nil || meRes.StatusCode != 200 {
		t.Fatalf("me failed: err=%v status=%v", err, meRes.StatusCode)
	}
	meRes.Body.Close()

	// 4) Refresh rotation: old refresh token must stop working after use.
	res, rotated := postJSON(t, srv.URL+"/api/auth/refresh", map[string]string{"refreshToken": refresh}, "")
	if res.StatusCode != 200 || rotated["accessToken"] == "" {
		t.Fatalf("refresh failed: status=%d body=%v", res.StatusCode, rotated)
	}
	res, _ = postJSON(t, srv.URL+"/api/auth/refresh", map[string]string{"refreshToken": refresh}, "")
	if res.StatusCode == 200 {
		t.Fatal("reusing a rotated refresh token should fail")
	}
}

func TestLoginRejectsBadPassword(t *testing.T) {
	srv, _, users := testStack(t)
	active := true
	_, _ = users.Create(context.Background(), service.CreateInput{
		Username: "bob", Password: "rightpass1", Active: &active,
		Roles: map[string]domain.Role{"finance": domain.RoleViewer},
	})
	res, _ := postJSON(t, srv.URL+"/api/auth/login", map[string]string{"username": "bob", "password": "wrong"}, "")
	if res.StatusCode != 401 {
		t.Fatalf("want 401 for bad password, got %d", res.StatusCode)
	}
}

func TestAdminAPIRequiresSuper(t *testing.T) {
	srv, _, users := testStack(t)
	active := true
	// A non-super admin must not reach the admin API.
	_, _ = users.Create(context.Background(), service.CreateInput{
		Username: "carol", Password: "carolpass1", Active: &active,
		Roles: map[string]domain.Role{"finance": domain.RoleAdmin},
	})
	_, body := postJSON(t, srv.URL+"/api/auth/login", map[string]string{"username": "carol", "password": "carolpass1"}, "")
	access, _ := body["accessToken"].(string)

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/users", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("admin req: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("non-super reaching admin API: want 403, got %d", res.StatusCode)
	}
}
