package storage

import (
	"context"
	"database/sql"
	"errors"
)

// Spec: schema (webui_admins: id, username UNIQUE, password_hash),
// Argon2id login. The hash is opaque
// here: the auth layer derives it via Argon2Hasher; storage persists and
// resolves it.

// Admin is one WebUI administrator row.
type Admin struct {
	ID           string
	Username     string
	PasswordHash string
}

// CountAdmins reports the number of admin rows — the first-run bootstrap
// signal for the WebUI setup flow ("create the first admin").
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM webui_admins`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// CreateAdmin inserts one admin row. A duplicate username errors via the
// UNIQUE constraint — never a silent second row.
func (s *Store) CreateAdmin(ctx context.Context, id, username, passwordHash string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO webui_admins (id, username, password_hash) VALUES (?, ?, ?)`,
		id, username, passwordHash)
	return err
}

// EnsureFirstAdmin creates the first admin row only, closing the
// TOCTOU window of a count-then-insert bootstrap: the count and the insert
// run in ONE transaction, and SQLite serializes writers transaction-wide —
// so of two concurrent bootstrap attempts exactly one wins. The loser
// gets ErrConflict, never a silent second row. The id/username uniqueness
// constraints remain the last line of defense on top.
func (s *Store) EnsureFirstAdmin(ctx context.Context, id, username, passwordHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM webui_admins`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO webui_admins (id, username, password_hash) VALUES (?, ?, ?)`,
		id, username, passwordHash); err != nil {
		return err
	}
	return tx.Commit()
}

// GetAdminByID resolves one admin row by id (the WebUI /me + password
// change path; the session middleware resolves the row the same way
// GetAdminByUsername does for login). Unknown ids are ErrNotFound.
func (s *Store) GetAdminByID(ctx context.Context, id string) (*Admin, error) {
	var a Admin
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash FROM webui_admins WHERE id = ?`, id).
		Scan(&a.ID, &a.Username, &a.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// GetAdminByUsername resolves the login lookup. Unknown usernames are
// ErrNotFound — a single not-found shape the auth layer maps to a generic
// 401 that never reveals whether the username exists.
func (s *Store) GetAdminByUsername(ctx context.Context, username string) (*Admin, error) {
	var a Admin
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash FROM webui_admins WHERE username = ?`, username).
		Scan(&a.ID, &a.Username, &a.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// UpdateAdminPassword replaces one admin's password hash (the WebUI
// password-change path). Unknown ids are ErrNotFound.
func (s *Store) UpdateAdminPassword(ctx context.Context, id, passwordHash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE webui_admins SET password_hash = ? WHERE id = ?`, passwordHash, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAdminByUsername removes one admin row. There is NO admin-API path
// to it — it exists for the dead-state test (a session whose admin row
// vanished must answer the uniform 401) and the future account-delete
// path; a live instance never calls it on the request path.
func (s *Store) DeleteAdminByUsername(ctx context.Context, username string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webui_admins WHERE username = ?`, username)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
