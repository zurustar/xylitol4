package userdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrUserNotFound is returned when a user lookup does not yield any results.
var ErrUserNotFound = errors.New("userdb: user not found")

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
