// Command server starts the Greenpark master authentication (SSO) service.
//
// It owns every staff account and their per-department roles, and issues
// Ed25519-signed JWT access tokens (short-lived) plus revocable refresh tokens.
// Each department backend verifies access tokens locally using the public key
// published at /.well-known/jwks.json — the signing key never leaves here.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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

	if cfg.DatabaseURL == "" {
		log.Fatal("auth: AUTH_DATABASE_URL is required (Postgres-only storage)")
	}

	// Signing key: load the Ed25519 key pair, generating one on first run.
	priv, created, err := keys.LoadOrCreate(cfg.PrivateKeyPath, cfg.PublicKeyPath)
	if err != nil {
		log.Fatalf("auth: signing key: %v", err)
	}
	signer := token.NewSigner(priv)
	if created {
		log.Printf("auth: generated new Ed25519 key pair (kid=%s) at %s", signer.KID(), cfg.PrivateKeyPath)
	} else {
		log.Printf("auth: loaded signing key (kid=%s)", signer.KID())
	}

	// Persistence.
	repo, err := repository.NewPostgres(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("auth: postgres: %v", err)
	}
	defer func() { _ = repo.Close() }()
	log.Println("auth: using PostgreSQL store")

	// Services.
	authSvc := service.NewAuth(repo, signer, cfg.Issuer, cfg.AccessTTL, cfg.RefreshTTL, nil)
	deptList := departments()
	userSvc := service.NewUsers(repo, authSvc, deptList, nil)

	// One-time seeding: department catalogue + bootstrap superadmin + optional
	// cross-department demo accounts.
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 15*time.Second)
	seedDepartments(bootCtx, repo, deptList)
	bootstrapSuperadmin(bootCtx, repo, userSvc, cfg)
	if cfg.SeedDemo {
		seedDemoUsers(bootCtx, userSvc, deptList, cfg)
	}
	bootCancel()

	// Background cleanup of expired/revoked refresh tokens.
	stopGC := startRefreshGC(repo)
	defer stopGC()

	handler := httptransport.NewHandler(authSvc, userSvc, signer)
	router := httptransport.NewRouter(handler, cfg.Origins())

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("auth API listening on http://localhost:%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("auth: server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("auth: shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("auth: graceful shutdown failed: %v", err)
	}
	log.Println("auth: stopped")
}

// departments converts the configured catalogue into domain values.
func departments() []domain.Department {
	out := make([]domain.Department, 0, len(config.Departments))
	for _, d := range config.Departments {
		out = append(out, domain.Department{Code: d.Code, Name: d.Name})
	}
	return out
}

// seedDepartments upserts the known department catalogue so the admin UI and
// role validation have a populated list.
func seedDepartments(ctx context.Context, repo repository.Repository, depts []domain.Department) {
	for _, d := range depts {
		if err := repo.UpsertDepartment(ctx, d); err != nil {
			log.Printf("auth: seed department %q: %v", d.Code, err)
		}
	}
}

// bootstrapSuperadmin creates the first superadmin account when the user table
// is empty, using AUTH_BOOTSTRAP_* config. It is a no-op once any user exists.
func bootstrapSuperadmin(ctx context.Context, repo repository.Repository, users *service.Users, cfg config.Config) {
	n, err := repo.CountUsers(ctx)
	if err != nil {
		log.Printf("auth: bootstrap: count users: %v", err)
		return
	}
	if n > 0 {
		return
	}
	if cfg.BootstrapPassword == "" {
		log.Printf("auth: no users yet and AUTH_BOOTSTRAP_PASSWORD is unset — "+
			"set it once to create the initial superadmin %q", cfg.BootstrapUsername)
		return
	}
	active := true
	u, err := users.Create(ctx, service.CreateInput{
		Username: cfg.BootstrapUsername,
		Name:     cfg.BootstrapName,
		Password: cfg.BootstrapPassword,
		Super:    true,
		Active:   &active,
	})
	if err != nil {
		log.Printf("auth: bootstrap superadmin: %v", err)
		return
	}
	log.Printf("auth: bootstrapped superadmin %q (id=%s)", u.Username, u.ID)
}

// seedDemoUsers ensures two cross-department accounts exist so dashboards
// migrated to SSO keep working with familiar credentials: "admin" (admin role
// in every department) and "viewer" (viewer in every department). It is
// idempotent — existing accounts are left untouched.
func seedDemoUsers(ctx context.Context, users *service.Users, depts []domain.Department, cfg config.Config) {
	adminRoles := make(map[string]domain.Role, len(depts))
	viewerRoles := make(map[string]domain.Role, len(depts))
	for _, d := range depts {
		adminRoles[d.Code] = domain.RoleAdmin
		viewerRoles[d.Code] = domain.RoleViewer
	}
	active := true
	seeds := []service.CreateInput{
		{Username: "admin", Name: "Admin Demo", Password: cfg.SeedAdminPassword, Active: &active, Roles: adminRoles},
		{Username: "viewer", Name: "Viewer Demo", Password: cfg.SeedViewerPassword, Active: &active, Roles: viewerRoles},
	}
	for _, in := range seeds {
		created, err := users.EnsureUser(ctx, in)
		if err != nil {
			log.Printf("auth: seed demo user %q: %v", in.Username, err)
			continue
		}
		if created {
			log.Printf("auth: seeded demo user %q (roles in %d departments)", in.Username, len(in.Roles))
		}
	}
}

// startRefreshGC periodically purges expired and revoked refresh tokens. It
// returns a stop function to cancel the loop on shutdown.
func startRefreshGC(repo repository.Repository) func() {
	ticker := time.NewTicker(time.Hour)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if err := repo.DeleteExpiredRefresh(ctx); err != nil {
					log.Printf("auth: refresh GC: %v", err)
				}
				cancel()
			}
		}
	}()
	return func() {
		ticker.Stop()
		close(done)
	}
}
