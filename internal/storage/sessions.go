package storage

import (
	"context"
	"database/sql"
	"errors"
)

// Spec: Phase 2 research facts S-1/S-6/S-7/S-10 (session design): the raw
// 256-bit session ID lives ONLY in the cookie; this table stores its
// SHA-256 hash as the PK. The auth middleware generates IDs and enforces
// the 30 min idle / 12 h absolute windows from these timestamps
// (server-side, never cookie-trusted).

// Session is one WebUI session row (hashed at rest).
type Session struct {
	IDHash            string // SHA-256 of the raw cookie ID — never the raw ID
	AdminID           string
	CreatedAt         int64 // unix seconds
	LastSeenAt        int64 // unix seconds; the idle window restarts here
	AbsoluteExpiresAt int64 // unix seconds; activity never extends this
}

// InsertSession creates one session row. A duplicate ID hash errors via the
// PK — never a silent overwrite (that would transfer the admin identity).
func (s *Store) InsertSession(ctx context.Context, sess Session) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO webui_sessions (id_hash, admin_id, created_at, last_seen_at, absolute_expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		sess.IDHash, sess.AdminID, sess.CreatedAt, sess.LastSeenAt, sess.AbsoluteExpiresAt)
	return err
}

// GetSession reads one session row by ID hash. Unknown hashes are
// ErrNotFound — the middleware treats that exactly like "no cookie".
func (s *Store) GetSession(ctx context.Context, idHash string) (*Session, error) {
	var sess Session
	err := s.db.QueryRowContext(ctx, `
		SELECT id_hash, admin_id, created_at, last_seen_at, absolute_expires_at
		FROM webui_sessions WHERE id_hash = ?`, idHash).
		Scan(&sess.IDHash, &sess.AdminID, &sess.CreatedAt, &sess.LastSeenAt, &sess.AbsoluteExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

// TouchSession updates last_seen_at only — the idle window restarts per
// request while the absolute cap stays fixed (S-6).
func (s *Store) TouchSession(ctx context.Context, idHash string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE webui_sessions SET last_seen_at = ? WHERE id_hash = ?`, now, idHash)
	return err
}

// DeleteSession removes one session row (logout, S-10). Idempotent: the
// outcome "logged out" is reached whether or not the row still existed.
func (s *Store) DeleteSession(ctx context.Context, idHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM webui_sessions WHERE id_hash = ?`, idHash)
	return err
}

// DeleteSessionsForAdmin removes every session of one admin — the S-8
// server-side revocation on privilege/password change — and returns the
// number removed.
func (s *Store) DeleteSessionsForAdmin(ctx context.Context, adminID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM webui_sessions WHERE admin_id = ?`, adminID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PurgeExpiredSessions removes rows whose absolute expiry has passed and
// returns the number removed — the housekeeping sweep that keeps abandoned
// sessions (and stolen cookies) from outliving their window.
func (s *Store) PurgeExpiredSessions(ctx context.Context, now int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM webui_sessions WHERE absolute_expires_at < ?`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
