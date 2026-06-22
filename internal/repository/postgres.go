package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx database/sql driver ("pgx")

	"greenpark/auth/internal/domain"
)

// Postgres is the PostgreSQL-backed Repository implementation.
type Postgres struct {
	db *sql.DB
}

// NewPostgres opens the database, applies the schema, and returns the store.
func NewPostgres(dsn string) (*Postgres, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	p := &Postgres{db: db}
	if err := p.migrate(ctx); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return p, nil
}

func (p *Postgres) Close() error { return p.db.Close() }

func (p *Postgres) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS auth_departments (
	code text PRIMARY KEY,
	name text NOT NULL
);
CREATE TABLE IF NOT EXISTS auth_users (
	id            text PRIMARY KEY,
	username      text NOT NULL UNIQUE,
	email         text NOT NULL DEFAULT '',
	name          text NOT NULL DEFAULT '',
	super         boolean NOT NULL DEFAULT false,
	active        boolean NOT NULL DEFAULT true,
	password_hash text NOT NULL,
	created_at    timestamptz NOT NULL DEFAULT now(),
	updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS auth_users_username_lower ON auth_users (lower(username));
CREATE TABLE IF NOT EXISTS auth_memberships (
	user_id text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
	dept    text NOT NULL,
	role    text NOT NULL,
	PRIMARY KEY (user_id, dept)
);
CREATE TABLE IF NOT EXISTS auth_refresh_tokens (
	id         text PRIMARY KEY,
	user_id    text NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
	hash       text NOT NULL UNIQUE,
	expires_at timestamptz NOT NULL,
	revoked    boolean NOT NULL DEFAULT false,
	created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS auth_refresh_user ON auth_refresh_tokens (user_id);
`
	_, err := p.db.ExecContext(ctx, schema)
	return err
}

/* ------------------------------- users ------------------------------- */

func (p *Postgres) CreateUser(ctx context.Context, u domain.User) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO auth_users (id, username, email, name, super, active, password_hash, created_at, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7, now(), now())`,
		u.ID, u.Username, u.Email, u.Name, u.Super, u.Active, u.PasswordHash)
	if isUniqueViolation(err) {
		return ErrUsernameTaken
	}
	if err != nil {
		return err
	}
	return p.replaceMemberships(ctx, u.ID, u.Roles)
}

func (p *Postgres) UpdateUser(ctx context.Context, u domain.User) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE auth_users
		    SET username=$2, email=$3, name=$4, super=$5, active=$6, password_hash=$7, updated_at=now()
		  WHERE id=$1`,
		u.ID, u.Username, u.Email, u.Name, u.Super, u.Active, u.PasswordHash)
	if isUniqueViolation(err) {
		return ErrUsernameTaken
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return p.replaceMemberships(ctx, u.ID, u.Roles)
}

// replaceMemberships rewrites all of a user's department grants atomically.
func (p *Postgres) replaceMemberships(ctx context.Context, userID string, roles map[string]domain.Role) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_memberships WHERE user_id=$1`, userID); err != nil {
		return err
	}
	for dept, role := range roles {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO auth_memberships (user_id, dept, role) VALUES ($1,$2,$3)`,
			userID, dept, string(role)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (p *Postgres) DeleteUser(ctx context.Context, id string) error {
	res, err := p.db.ExecContext(ctx, `DELETE FROM auth_users WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) UserByID(ctx context.Context, id string) (domain.User, error) {
	return p.queryUser(ctx, `WHERE u.id=$1`, id)
}

func (p *Postgres) UserByUsername(ctx context.Context, username string) (domain.User, error) {
	return p.queryUser(ctx, `WHERE lower(u.username)=lower($1)`, username)
}

// queryUser loads a single user plus their memberships using the given WHERE
// clause and argument.
func (p *Postgres) queryUser(ctx context.Context, where string, arg any) (domain.User, error) {
	var u domain.User
	err := p.db.QueryRowContext(ctx,
		`SELECT u.id, u.username, u.email, u.name, u.super, u.active, u.password_hash, u.created_at, u.updated_at
		   FROM auth_users u `+where, arg).
		Scan(&u.ID, &u.Username, &u.Email, &u.Name, &u.Super, &u.Active, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, ErrNotFound
	}
	if err != nil {
		return domain.User{}, err
	}
	u.Roles, err = p.membershipsOf(ctx, u.ID)
	return u, err
}

func (p *Postgres) membershipsOf(ctx context.Context, userID string) (map[string]domain.Role, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT dept, role FROM auth_memberships WHERE user_id=$1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	roles := make(map[string]domain.Role)
	for rows.Next() {
		var dept, role string
		if err := rows.Scan(&dept, &role); err != nil {
			return nil, err
		}
		roles[dept] = domain.Role(role)
	}
	return roles, rows.Err()
}

func (p *Postgres) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT u.id, u.username, u.email, u.name, u.super, u.active, u.password_hash, u.created_at, u.updated_at
		   FROM auth_users u ORDER BY u.username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []domain.User
	for rows.Next() {
		var u domain.User
		if err := rows.Scan(&u.ID, &u.Username, &u.Email, &u.Name, &u.Super, &u.Active,
			&u.PasswordHash, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Attach memberships per user. Volume here is small (internal staff).
	for i := range users {
		if users[i].Roles, err = p.membershipsOf(ctx, users[i].ID); err != nil {
			return nil, err
		}
	}
	return users, nil
}

func (p *Postgres) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := p.db.QueryRowContext(ctx, `SELECT count(*) FROM auth_users`).Scan(&n)
	return n, err
}

/* ---------------------------- memberships ---------------------------- */

func (p *Postgres) SetMembership(ctx context.Context, userID, dept string, role domain.Role) error {
	res, err := p.db.ExecContext(ctx,
		`INSERT INTO auth_memberships (user_id, dept, role) VALUES ($1,$2,$3)
		 ON CONFLICT (user_id, dept) DO UPDATE SET role=EXCLUDED.role`,
		userID, dept, string(role))
	if isForeignKeyViolation(err) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	_ = res
	return nil
}

func (p *Postgres) RemoveMembership(ctx context.Context, userID, dept string) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM auth_memberships WHERE user_id=$1 AND dept=$2`, userID, dept)
	return err
}

/* ---------------------------- departments ---------------------------- */

func (p *Postgres) UpsertDepartment(ctx context.Context, d domain.Department) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO auth_departments (code, name) VALUES ($1,$2)
		 ON CONFLICT (code) DO UPDATE SET name=EXCLUDED.name`, d.Code, d.Name)
	return err
}

func (p *Postgres) ListDepartments(ctx context.Context) ([]domain.Department, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT code, name FROM auth_departments ORDER BY code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Department
	for rows.Next() {
		var d domain.Department
		if err := rows.Scan(&d.Code, &d.Name); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

/* -------------------------- refresh tokens --------------------------- */

func (p *Postgres) StoreRefresh(ctx context.Context, t domain.RefreshToken) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO auth_refresh_tokens (id, user_id, hash, expires_at, revoked, created_at)
		 VALUES ($1,$2,$3,$4,$5, now())`,
		t.ID, t.UserID, t.Hash, t.ExpiresAt, t.Revoked)
	return err
}

func (p *Postgres) RefreshByHash(ctx context.Context, hash string) (domain.RefreshToken, error) {
	var t domain.RefreshToken
	err := p.db.QueryRowContext(ctx,
		`SELECT id, user_id, hash, expires_at, revoked, created_at
		   FROM auth_refresh_tokens WHERE hash=$1`, hash).
		Scan(&t.ID, &t.UserID, &t.Hash, &t.ExpiresAt, &t.Revoked, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.RefreshToken{}, ErrNotFound
	}
	return t, err
}

func (p *Postgres) RevokeRefresh(ctx context.Context, id string) error {
	_, err := p.db.ExecContext(ctx, `UPDATE auth_refresh_tokens SET revoked=true WHERE id=$1`, id)
	return err
}

func (p *Postgres) RevokeAllForUser(ctx context.Context, userID string) error {
	_, err := p.db.ExecContext(ctx, `UPDATE auth_refresh_tokens SET revoked=true WHERE user_id=$1`, userID)
	return err
}

func (p *Postgres) DeleteExpiredRefresh(ctx context.Context) error {
	_, err := p.db.ExecContext(ctx,
		`DELETE FROM auth_refresh_tokens WHERE expires_at < now() OR revoked = true`)
	return err
}

/* ------------------------------ helpers ------------------------------ */

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
