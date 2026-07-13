// Package bootstrap holds the canonical initial Greenpark account roster, shared
// by the Postgres server (persisted, idempotent) and the in-memory devserver so
// the two never drift. Passwords match each department backend's own seed so a
// migrated dashboard can bridge to its native data token per logged-in person.
// Role strings use each department's own vocabulary (kadep/arsitek/drafter/
// staff/…) which the dashboard UI reads directly.
package bootstrap

import (
	"greenpark/auth/internal/domain"
	"greenpark/auth/internal/service"
)

// GreenparkRoster returns every initial account EXCEPT the auth superadmin,
// which each entrypoint provisions its own way (Postgres via AUTH_BOOTSTRAP_*,
// devserver inline). depts is the known department catalogue, used to grant the
// directors viewer access across all divisions.
func GreenparkRoster(depts []domain.Department) []service.CreateInput {
	active := true
	allViewer := map[string]domain.Role{}
	for _, d := range depts {
		allViewer[d.Code] = domain.RoleViewer
	}
	r := func(dept, role string) map[string]domain.Role {
		return map[string]domain.Role{dept: domain.Role(role)}
	}
	return []service.CreateInput{
		// ── Direktur (akses SEMUA divisi) ──
		{Username: "ceo@greenpark.id", Email: "ceo@greenpark.id", Name: "Direktur Utama", Password: "ceo123", Active: &active, Roles: allViewer},
		{Username: "dirops@greenpark.id", Email: "dirops@greenpark.id", Name: "Direktur Operasional", Password: "dirops123", Super: true, Active: &active},
		// ── Perencanaan ──
		{Username: "kadep", Name: "Kepala Dept. Perencanaan", Password: "kadep123", Active: &active, Roles: r("perencanaan", "kadep")},
		{Username: "randi", Name: "Surandi Yanda Saputra", Password: "randi123", Active: &active, Roles: r("perencanaan", "arsitek")},
		{Username: "ananto", Name: "Ananto", Password: "ananto123", Active: &active, Roles: r("perencanaan", "arsitek")},
		{Username: "agus", Name: "Agus Priyanta", Password: "agus123", Active: &active, Roles: r("perencanaan", "drafter")},
		{Username: "rio", Name: "Rio Zakaria", Password: "rio123", Active: &active, Roles: r("perencanaan", "drafter")},
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
		// ── Teknik ──
		{Username: "teknik@greenpark.id", Email: "teknik@greenpark.id", Name: "Kepala Dept. Teknik", Password: "teknik123", Active: &active, Roles: r("teknik", "kadep")},
		// ── CSO (Customer Complaint) ──
		{Username: "cso@greenpark.id", Email: "cso@greenpark.id", Name: "Kepala Dept. CSO", Password: "cso123", Active: &active, Roles: r("cso", "kadep")},
	}
}
