package bootstrap

import "greenpark/auth/internal/domain"

// DefaultRoles is the initial role catalogue (master data) seeded on first run
// when auth_roles is empty. The super admin can add, edit or remove entries
// afterwards via /api/admin/roles. Values match the vocabulary the department
// dashboards read from a user's granted role. Shared by the Postgres server and
// the in-memory devserver so the two never drift.
func DefaultRoles() []domain.RoleDef {
	return []domain.RoleDef{
		{Value: "kadep", Label: "Kepala Divisi", Sort: 0},
		{Value: "admin", Label: "Admin", Sort: 1},
		{Value: "viewer", Label: "Viewer (lihat saja)", Sort: 2},
		{Value: "arsitek", Label: "Arsitek", Sort: 3},
		{Value: "drafter", Label: "Drafter", Sort: 4},
		{Value: "staff", Label: "Staff", Sort: 5},
		{Value: "legal_permit", Label: "Staf Legal Permit", Sort: 6},
		{Value: "purchasing", Label: "Purchasing", Sort: 7},
	}
}
