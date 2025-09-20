package userdb

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestSQLiteStoreLookup(t *testing.T) {
	db := openTestDatabase(t)
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("failed to construct store: %v", err)
	}
	defer store.Close()

	seedTestUsers(t, store.UnderlyingDB())

	ctx := context.Background()
	user, err := store.Lookup(ctx, "alice", "example.com")
	if err != nil {
		t.Fatalf("expected lookup to succeed, got error: %v", err)
	}
	if user.Username != "alice" || user.Domain != "example.com" {
		t.Fatalf("unexpected user identity: %#v", user)
	}
	if user.PasswordHash != "hashed-secret" {
		t.Fatalf("unexpected password hash: %q", user.PasswordHash)
	}
	if user.ContactURI != "sip:alice@192.0.2.10" {
		t.Fatalf("unexpected contact URI: %q", user.ContactURI)
	}
}

func TestSQLiteStoreLookupNotFound(t *testing.T) {
	db := openTestDatabase(t)
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("failed to construct store: %v", err)
	}
	defer store.Close()

	seedTestUsers(t, store.UnderlyingDB())

	ctx := context.Background()
	_, err = store.Lookup(ctx, "carol", "example.com")
	if err != ErrUserNotFound {
		t.Fatalf("expected ErrUserNotFound, got %v", err)
	}
}

func TestSQLiteStoreAllUsers(t *testing.T) {
	db := openTestDatabase(t)
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("failed to construct store: %v", err)
	}
	defer store.Close()

	seedTestUsers(t, store.UnderlyingDB())

	ctx := context.Background()
	users, err := store.AllUsers(ctx)
	if err != nil {
		t.Fatalf("AllUsers returned error: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
}

func openTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	return db
}

func seedTestUsers(t *testing.T, db *sql.DB) {
	t.Helper()
	statements := []string{
		`CREATE TABLE users (
        username TEXT NOT NULL,
        domain TEXT NOT NULL,
        password_hash TEXT,
        contact_uri TEXT,
        PRIMARY KEY (username, domain)
)`,
		`INSERT INTO users (username, domain, password_hash, contact_uri) VALUES ('alice', 'example.com', 'hashed-secret', 'sip:alice@192.0.2.10')`,
		`INSERT INTO users (username, domain, password_hash, contact_uri) VALUES ('bob', 'example.com', 'hashed-secret-2', 'sip:bob@192.0.2.20')`,
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("failed to seed database with %q: %v", stmt, err)
		}
	}
}
