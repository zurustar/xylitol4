package userdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrUserNotFound is returned when a user lookup does not yield any results.
var ErrUserNotFound = errors.New("userdb: user not found")

// ErrBroadcastRuleNotFound indicates that a broadcast ringing rule could not be located.
var ErrBroadcastRuleNotFound = errors.New("userdb: broadcast rule not found")

// User models a SIP user entry stored in the registrar database.
type User struct {
	Username     string
	Domain       string
	PasswordHash string
	ContactURI   string
}

// SQLiteStore provides read access to user records backed by SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// BroadcastRule describes an address that should ring a collection of downstream contacts.
type BroadcastRule struct {
	ID          int64
	Address     string
	Description string
	Targets     []BroadcastTarget
}

// BroadcastTarget records an individual contact URI associated with a broadcast rule.
type BroadcastTarget struct {
	ID         int64
	RuleID     int64
	ContactURI string
	Priority   int
}

// OpenSQLite opens a new SQLite backed store using the provided datasource path.
// The datasource may be a filename or any SQLite connection string supported by
// modernc.org/sqlite.
func OpenSQLite(path string) (*SQLiteStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("userdb: sqlite path is required")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("userdb: open sqlite: %w", err)
	}
	store, err := NewSQLiteStore(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

// NewSQLiteStore wraps an existing database handle with user store helpers.
func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fmt.Errorf("userdb: db handle is nil")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("userdb: ping sqlite: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// Close releases the underlying database resources.
func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Lookup returns a user entry by username and domain.
func (s *SQLiteStore) Lookup(ctx context.Context, username, domain string) (*User, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("userdb: store is not initialised")
	}
	const query = `SELECT username, domain, password_hash, contact_uri FROM users WHERE username = ? AND domain = ? LIMIT 1`
	row := s.db.QueryRowContext(ctx, query, username, domain)
	var user User
	var password sql.NullString
	var contact sql.NullString
	if err := row.Scan(&user.Username, &user.Domain, &password, &contact); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("userdb: lookup user: %w", err)
	}
	if password.Valid {
		user.PasswordHash = password.String
	}
	if contact.Valid {
		user.ContactURI = contact.String
	}
	return &user, nil
}

// AllUsers returns every user entry stored in the database.
func (s *SQLiteStore) AllUsers(ctx context.Context) ([]User, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("userdb: store is not initialised")
	}
	const query = `SELECT username, domain, password_hash, contact_uri FROM users`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("userdb: query users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var user User
		var password sql.NullString
		var contact sql.NullString
		if err := rows.Scan(&user.Username, &user.Domain, &password, &contact); err != nil {
			return nil, fmt.Errorf("userdb: scan user: %w", err)
		}
		if password.Valid {
			user.PasswordHash = password.String
		}
		if contact.Valid {
			user.ContactURI = contact.String
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("userdb: iterate users: %w", err)
	}
	return users, nil
}

// CreateUser inserts a new user entry into the database.
func (s *SQLiteStore) CreateUser(ctx context.Context, user User) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("userdb: store is not initialised")
	}
	if strings.TrimSpace(user.Username) == "" {
		return fmt.Errorf("userdb: username is required")
	}
	if strings.TrimSpace(user.Domain) == "" {
		return fmt.Errorf("userdb: domain is required")
	}
	const query = `INSERT INTO users (username, domain, password_hash, contact_uri) VALUES (?, ?, ?, ?)`
	if _, err := s.db.ExecContext(ctx, query, user.Username, user.Domain, user.PasswordHash, user.ContactURI); err != nil {
		return fmt.Errorf("userdb: create user: %w", err)
	}
	return nil
}

// DeleteUser removes a user entry from the database.
func (s *SQLiteStore) DeleteUser(ctx context.Context, username, domain string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("userdb: store is not initialised")
	}
	const query = `DELETE FROM users WHERE username = ? AND domain = ?`
	res, err := s.db.ExecContext(ctx, query, username, domain)
	if err != nil {
		return fmt.Errorf("userdb: delete user: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("userdb: delete user rows affected: %w", err)
	}
	if affected == 0 {
		return ErrUserNotFound
	}
	return nil
}

// UpdatePassword updates the stored password hash for a user.
func (s *SQLiteStore) UpdatePassword(ctx context.Context, username, domain, passwordHash string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("userdb: store is not initialised")
	}
	const query = `UPDATE users SET password_hash = ? WHERE username = ? AND domain = ?`
	res, err := s.db.ExecContext(ctx, query, passwordHash, username, domain)
	if err != nil {
		return fmt.Errorf("userdb: update password: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("userdb: update password rows affected: %w", err)
	}
	if affected == 0 {
		return ErrUserNotFound
	}
	return nil
}

// UnderlyingDB exposes the raw database handle. It is primarily intended for
// testing purposes where schema initialisation is required.
func (s *SQLiteStore) UnderlyingDB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// ListBroadcastRules returns all broadcast ringing rules with their associated targets.
func (s *SQLiteStore) ListBroadcastRules(ctx context.Context) ([]BroadcastRule, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("userdb: store is not initialised")
	}
	const rulesQuery = `SELECT id, address, description FROM broadcast_rules`
	rows, err := s.db.QueryContext(ctx, rulesQuery)
	if err != nil {
		return nil, fmt.Errorf("userdb: query broadcast rules: %w", err)
	}
	defer rows.Close()

	var rules []BroadcastRule
	for rows.Next() {
		var rule BroadcastRule
		var description sql.NullString
		if err := rows.Scan(&rule.ID, &rule.Address, &description); err != nil {
			return nil, fmt.Errorf("userdb: scan broadcast rule: %w", err)
		}
		if description.Valid {
			rule.Description = description.String
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("userdb: iterate broadcast rules: %w", err)
	}
	for i := range rules {
		targets, err := s.targetsForRule(ctx, rules[i].ID)
		if err != nil {
			return nil, err
		}
		rules[i].Targets = targets
	}
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Address == rules[j].Address {
			return rules[i].ID < rules[j].ID
		}
		return rules[i].Address < rules[j].Address
	})
	return rules, nil
}

// CreateBroadcastRule inserts a new broadcast rule and optional targets.
func (s *SQLiteStore) CreateBroadcastRule(ctx context.Context, rule BroadcastRule) (*BroadcastRule, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("userdb: store is not initialised")
	}
	if strings.TrimSpace(rule.Address) == "" {
		return nil, fmt.Errorf("userdb: broadcast rule address is required")
	}
	exists, err := s.broadcastRuleIDByAddress(ctx, rule.Address)
	if err != nil && !errors.Is(err, ErrBroadcastRuleNotFound) {
		return nil, err
	}
	if err == nil && exists > 0 {
		return nil, fmt.Errorf("userdb: broadcast rule for address %q already exists", rule.Address)
	}
	const insertRule = `INSERT INTO broadcast_rules (address, description) VALUES (?, ?)`
	if _, err := s.db.ExecContext(ctx, insertRule, rule.Address, rule.Description); err != nil {
		return nil, fmt.Errorf("userdb: create broadcast rule: %w", err)
	}
	ruleID, err := s.broadcastRuleIDByAddress(ctx, rule.Address)
	if err != nil {
		return nil, err
	}
	created := &BroadcastRule{ID: ruleID, Address: rule.Address, Description: rule.Description}
	if len(rule.Targets) > 0 {
		if err := s.ReplaceBroadcastTargets(ctx, ruleID, rule.Targets); err != nil {
			return nil, err
		}
		targets, err := s.targetsForRule(ctx, ruleID)
		if err != nil {
			return nil, err
		}
		created.Targets = targets
	}
	return created, nil
}

// UpdateBroadcastRule modifies an existing broadcast rule's address or description.
func (s *SQLiteStore) UpdateBroadcastRule(ctx context.Context, rule BroadcastRule) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("userdb: store is not initialised")
	}
	if rule.ID <= 0 {
		return fmt.Errorf("userdb: broadcast rule id is required")
	}
	if strings.TrimSpace(rule.Address) == "" {
		return fmt.Errorf("userdb: broadcast rule address is required")
	}
	const updateRule = `UPDATE broadcast_rules SET address = ?, description = ? WHERE id = ?`
	res, err := s.db.ExecContext(ctx, updateRule, rule.Address, rule.Description, rule.ID)
	if err != nil {
		return fmt.Errorf("userdb: update broadcast rule: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("userdb: update broadcast rule rows affected: %w", err)
	}
	if affected == 0 {
		return ErrBroadcastRuleNotFound
	}
	return nil
}

// DeleteBroadcastRule removes a broadcast rule and its associated targets.
func (s *SQLiteStore) DeleteBroadcastRule(ctx context.Context, ruleID int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("userdb: store is not initialised")
	}
	if ruleID <= 0 {
		return fmt.Errorf("userdb: broadcast rule id is required")
	}
	const deleteTargets = `DELETE FROM broadcast_targets WHERE rule_id = ?`
	if _, err := s.db.ExecContext(ctx, deleteTargets, ruleID); err != nil {
		return fmt.Errorf("userdb: delete broadcast targets: %w", err)
	}
	const deleteRule = `DELETE FROM broadcast_rules WHERE id = ?`
	res, err := s.db.ExecContext(ctx, deleteRule, ruleID)
	if err != nil {
		return fmt.Errorf("userdb: delete broadcast rule: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("userdb: delete broadcast rule rows affected: %w", err)
	}
	if affected == 0 {
		return ErrBroadcastRuleNotFound
	}
	return nil
}

// ReplaceBroadcastTargets overwrites the contact list for the given broadcast rule.
func (s *SQLiteStore) ReplaceBroadcastTargets(ctx context.Context, ruleID int64, targets []BroadcastTarget) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("userdb: store is not initialised")
	}
	if ruleID <= 0 {
		return fmt.Errorf("userdb: broadcast rule id is required")
	}
	// Ensure the rule exists before modifying its targets.
	if _, err := s.broadcastRuleByID(ctx, ruleID); err != nil {
		return err
	}
	const deleteTargets = `DELETE FROM broadcast_targets WHERE rule_id = ?`
	if _, err := s.db.ExecContext(ctx, deleteTargets, ruleID); err != nil {
		return fmt.Errorf("userdb: clear broadcast targets: %w", err)
	}
	if len(targets) == 0 {
		return nil
	}
	const insertTarget = `INSERT INTO broadcast_targets (rule_id, contact_uri, priority) VALUES (?, ?, ?)`
	for i, target := range targets {
		contact := strings.TrimSpace(target.ContactURI)
		if contact == "" {
			return fmt.Errorf("userdb: broadcast target contact URI is required")
		}
		priority := target.Priority
		if priority == 0 {
			priority = i
		}
		if _, err := s.db.ExecContext(ctx, insertTarget, ruleID, contact, priority); err != nil {
			return fmt.Errorf("userdb: insert broadcast target: %w", err)
		}
	}
	return nil
}

// LookupBroadcastTargets returns the contacts configured for the given broadcast address.
func (s *SQLiteStore) LookupBroadcastTargets(ctx context.Context, address string) ([]BroadcastTarget, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("userdb: store is not initialised")
	}
	ruleID, err := s.broadcastRuleIDByAddress(ctx, address)
	if err != nil {
		return nil, err
	}
	targets, err := s.targetsForRule(ctx, ruleID)
	if err != nil {
		return nil, err
	}
	return targets, nil
}

func (s *SQLiteStore) targetsForRule(ctx context.Context, ruleID int64) ([]BroadcastTarget, error) {
	const targetsQuery = `SELECT id, rule_id, contact_uri, priority FROM broadcast_targets WHERE rule_id = ?`
	rows, err := s.db.QueryContext(ctx, targetsQuery, ruleID)
	if err != nil {
		return nil, fmt.Errorf("userdb: query broadcast targets: %w", err)
	}
	defer rows.Close()

	var targets []BroadcastTarget
	for rows.Next() {
		var target BroadcastTarget
		if err := rows.Scan(&target.ID, &target.RuleID, &target.ContactURI, &target.Priority); err != nil {
			return nil, fmt.Errorf("userdb: scan broadcast target: %w", err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("userdb: iterate broadcast targets: %w", err)
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Priority == targets[j].Priority {
			return targets[i].ID < targets[j].ID
		}
		return targets[i].Priority < targets[j].Priority
	})
	return targets, nil
}

func (s *SQLiteStore) broadcastRuleIDByAddress(ctx context.Context, address string) (int64, error) {
	const query = `SELECT id FROM broadcast_rules WHERE address = ? LIMIT 1`
	row := s.db.QueryRowContext(ctx, query, address)
	var id int64
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrBroadcastRuleNotFound
		}
		return 0, fmt.Errorf("userdb: lookup broadcast rule id: %w", err)
	}
	return id, nil
}

func (s *SQLiteStore) broadcastRuleByID(ctx context.Context, id int64) (*BroadcastRule, error) {
	const query = `SELECT id, address, description FROM broadcast_rules WHERE id = ? LIMIT 1`
	row := s.db.QueryRowContext(ctx, query, id)
	var rule BroadcastRule
	var description sql.NullString
	if err := row.Scan(&rule.ID, &rule.Address, &description); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrBroadcastRuleNotFound
		}
		return nil, fmt.Errorf("userdb: lookup broadcast rule: %w", err)
	}
	if description.Valid {
		rule.Description = description.String
	}
	return &rule, nil
}
