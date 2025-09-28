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

func TestBroadcastRuleLifecycle(t *testing.T) {
	db := openTestDatabase(t)
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("failed to construct store: %v", err)
	}
	defer store.Close()

	ensureSchema(t, store.UnderlyingDB())

	ctx := context.Background()
	created, err := store.CreateBroadcastRule(ctx, BroadcastRule{
		Address:     "sip:1000@example.com",
		Description: "Support Team",
		Targets: []BroadcastTarget{
			{ContactURI: "sip:alice@example.com"},
			{ContactURI: "sip:bob@example.com", Priority: 5},
		},
	})
	if err != nil {
		t.Fatalf("CreateBroadcastRule returned error: %v", err)
	}
	if created.ID == 0 {
		t.Fatalf("expected created rule to have an ID")
	}
	if len(created.Targets) != 2 {
		t.Fatalf("expected two targets, got %d", len(created.Targets))
	}
	if created.Targets[0].ContactURI != "sip:alice@example.com" {
		t.Fatalf("unexpected first target: %#v", created.Targets[0])
	}
	if created.Targets[1].Priority != 5 {
		t.Fatalf("expected explicit priority to be preserved, got %d", created.Targets[1].Priority)
	}

	created.Address = "sip:2000@example.com"
	created.Description = "Sales"
	if err := store.UpdateBroadcastRule(ctx, *created); err != nil {
		t.Fatalf("UpdateBroadcastRule returned error: %v", err)
	}

	replacement := []BroadcastTarget{
		{ContactURI: "sip:carol@example.com"},
	}
	if err := store.ReplaceBroadcastTargets(ctx, created.ID, replacement); err != nil {
		t.Fatalf("ReplaceBroadcastTargets returned error: %v", err)
	}

	targets, err := store.LookupBroadcastTargets(ctx, "sip:2000@example.com")
	if err != nil {
		t.Fatalf("LookupBroadcastTargets returned error: %v", err)
	}
	if len(targets) != 1 || targets[0].ContactURI != "sip:carol@example.com" {
		t.Fatalf("unexpected lookup targets: %#v", targets)
	}

	rules, err := store.ListBroadcastRules(ctx)
	if err != nil {
		t.Fatalf("ListBroadcastRules returned error: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected a single rule, got %d", len(rules))
	}
	if rules[0].Address != "sip:2000@example.com" {
		t.Fatalf("unexpected rule address: %q", rules[0].Address)
	}
	if len(rules[0].Targets) != 1 || rules[0].Targets[0].ContactURI != "sip:carol@example.com" {
		t.Fatalf("unexpected rule targets: %#v", rules[0].Targets)
	}

	if err := store.DeleteBroadcastRule(ctx, created.ID); err != nil {
		t.Fatalf("DeleteBroadcastRule returned error: %v", err)
	}
	if _, err := store.LookupBroadcastTargets(ctx, "sip:2000@example.com"); err != ErrBroadcastRuleNotFound {
		t.Fatalf("expected ErrBroadcastRuleNotFound after delete, got %v", err)
	}
}

func TestLookupBroadcastTargetsNotFound(t *testing.T) {
	db := openTestDatabase(t)
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("failed to construct store: %v", err)
	}
	defer store.Close()

	ensureSchema(t, store.UnderlyingDB())
	seedBroadcastRule(t, store.UnderlyingDB(), "sip:3000@example.com", "sip:one@example.com")

	ctx := context.Background()
	if _, err := store.LookupBroadcastTargets(ctx, "sip:missing@example.com"); err != ErrBroadcastRuleNotFound {
		t.Fatalf("expected ErrBroadcastRuleNotFound, got %v", err)
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
	statements := []string{
		`CREATE TABLE users (
        username TEXT NOT NULL,
        domain TEXT NOT NULL,
        password_hash TEXT,
        contact_uri TEXT,
        PRIMARY KEY (username, domain)
)`,
		`CREATE TABLE broadcast_rules (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        address TEXT NOT NULL,
        description TEXT
)`,
		`CREATE TABLE broadcast_targets (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        rule_id INTEGER NOT NULL,
        contact_uri TEXT NOT NULL,
        priority INTEGER NOT NULL
)`,
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			if !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("failed to create schema: %v", err)
			}
		}
	}
}

func seedBroadcastRule(t *testing.T, db *sql.DB, address string, contacts ...string) int64 {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO broadcast_rules (address, description) VALUES (?, ?)`, address, ""); err != nil {
		t.Fatalf("failed to insert broadcast rule: %v", err)
	}
	row := db.QueryRow(`SELECT id FROM broadcast_rules WHERE address = ?`, address)
	var id int64
	if err := row.Scan(&id); err != nil {
		t.Fatalf("failed to fetch broadcast rule id: %v", err)
	}
	for i, contact := range contacts {
		if _, err := db.Exec(`INSERT INTO broadcast_targets (rule_id, contact_uri, priority) VALUES (?, ?, ?)`, id, contact, i); err != nil {
			t.Fatalf("failed to insert broadcast target: %v", err)
		}
	}
	return id
}
