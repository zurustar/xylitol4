package userdb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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

func TestSQLiteStoreCreateUser(t *testing.T) {
	db := openTestDatabase(t)
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("failed to construct store: %v", err)
	}
	defer store.Close()

	ensureSchema(t, store.UnderlyingDB())

	ctx := context.Background()
	err = store.CreateUser(ctx, User{Username: "dave", Domain: "example.com", PasswordHash: "hash", ContactURI: "sip:dave@example.com"})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}

	user, err := store.Lookup(ctx, "dave", "example.com")
	if err != nil {
		t.Fatalf("Lookup after create failed: %v", err)
	}
	if user.ContactURI != "sip:dave@example.com" {
		t.Fatalf("unexpected contact URI: %q", user.ContactURI)
	}
}

func TestSQLiteStoreDeleteUser(t *testing.T) {
	db := openTestDatabase(t)
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("failed to construct store: %v", err)
	}
	defer store.Close()

	seedTestUsers(t, store.UnderlyingDB())

	ctx := context.Background()
	if err := store.DeleteUser(ctx, "alice", "example.com"); err != nil {
		t.Fatalf("DeleteUser returned error: %v", err)
	}
	if _, err := store.Lookup(ctx, "alice", "example.com"); err != ErrUserNotFound {
		t.Fatalf("expected alice to be deleted, lookup error: %v", err)
	}
}

func TestSQLiteStoreUpdatePassword(t *testing.T) {
	db := openTestDatabase(t)
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("failed to construct store: %v", err)
	}
	defer store.Close()

	seedTestUsers(t, store.UnderlyingDB())

	ctx := context.Background()
	if err := store.UpdatePassword(ctx, "alice", "example.com", "new-hash"); err != nil {
		t.Fatalf("UpdatePassword returned error: %v", err)
	}
	user, err := store.Lookup(ctx, "alice", "example.com")
	if err != nil {
		t.Fatalf("Lookup after UpdatePassword failed: %v", err)
	}
	if user.PasswordHash != "new-hash" {
		t.Fatalf("password hash not updated: %q", user.PasswordHash)
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
	ensureSchema(t, db)
	statements := []string{
		`INSERT INTO users (username, domain, password_hash, contact_uri) VALUES ('alice', 'example.com', 'hashed-secret', 'sip:alice@192.0.2.10')`,
		`INSERT INTO users (username, domain, password_hash, contact_uri) VALUES ('bob', 'example.com', 'hashed-secret-2', 'sip:bob@192.0.2.20')`,
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("failed to seed database with %q: %v", stmt, err)
		}
	}
}

func ensureSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	const schema = `CREATE TABLE users (
        username TEXT NOT NULL,
        domain TEXT NOT NULL,
        password_hash TEXT,
        contact_uri TEXT,
        PRIMARY KEY (username, domain)
)`
	if _, err := db.Exec(schema); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("failed to create schema: %v", err)
		}
	}
}
