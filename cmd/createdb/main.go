// Command createdb creates the auth database named in AUTH_DATABASE_URL when it
// does not yet exist, using the same pgx driver as the server. It connects to
// the maintenance database ("postgres") to issue CREATE DATABASE. The password
// stays inside AUTH_DATABASE_URL — this tool never prints it.
//
//	AUTH_DATABASE_URL=... go run ./cmd/createdb
package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var dbInPath = regexp.MustCompile(`/([^/?]+)(\?|$)`)

func main() {
	dsn := os.Getenv("AUTH_DATABASE_URL")
	if dsn == "" {
		log.Fatal("createdb: AUTH_DATABASE_URL not set")
	}
	m := dbInPath.FindStringSubmatch(dsn)
	if m == nil {
		log.Fatal("createdb: cannot find database name in AUTH_DATABASE_URL")
	}
	target := m[1]
	maint := dbInPath.ReplaceAllString(dsn, "/postgres$2")

	db, err := sql.Open("pgx", maint)
	if err != nil {
		log.Fatalf("createdb: open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Ping(); err != nil {
		log.Fatalf("createdb: connect: %v", err)
	}

	var exists bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, target).Scan(&exists); err != nil {
		log.Fatalf("createdb: check: %v", err)
	}
	if exists {
		fmt.Printf("database %q already exists — nothing to do\n", target)
		return
	}
	if _, err := db.Exec(`CREATE DATABASE "` + strings.ReplaceAll(target, `"`, `""`) + `"`); err != nil {
		log.Fatalf("createdb: create: %v", err)
	}
	fmt.Printf("created database %q\n", target)
}
