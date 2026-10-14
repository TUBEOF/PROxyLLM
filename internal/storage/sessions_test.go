package storage

import (
	"context"
	"errors"
	"testing"
)

// Spec: the admin API sits behind session auth; the session design is
// 256-bit CSPRNG session IDs, SHA-256-hashed at rest; server-side
// idle/absolute expiry enforcement; hash-only persistence (the raw ID lives
// only in the cookie); logout = row delete. Storage stores and
// resolves hashes; the auth middleware generates IDs, enforces the 30 min
// idle / 12 h absolute windows from the timestamps stored here.

func insertTestSession(t *testing.T, s *Store, idHash, adminID string, now int64) {
	t.Helper()
	ctx := context.Background()
	// webui_sessions.admin_id is FK-bound to webui_admins — sessions only
	// exist for real admins (ON DELETE CASCADE mirrors that). ON CONFLICT
	// keeps the helper reusable for several sessions of the same admin.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO webui_admins (id, username, password_hash) VALUES (?, ?, 'x')
		 ON CONFLICT(id) DO NOTHING`, adminID, "user-"+adminID); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	err := s.InsertSession(ctx, Session{
		IDHash:            idHash,
		AdminID:           adminID,
		CreatedAt:         now,
		LastSeenAt:        now,
		AbsoluteExpiresAt: now + 12*3600,
	})
	if err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
}

func TestSession_RoundTrip(t *testing.T) {
	s := openTokenStore(t)

	insertTestSession(t, s, "hash-of-abc", "adm-1", 1000)
	got, err := s.GetSession(context.Background(), "hash-of-abc")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.AdminID != "adm-1" || got.CreatedAt != 1000 || got.LastSeenAt != 1000 {
		t.Errorf("round-trip: got %+v", got)
	}
	if got.AbsoluteExpiresAt != 1000+12*3600 {
		t.Errorf("absolute expiry = %d, want created+12h", got.AbsoluteExpiresAt)
	}
}

func TestSession_UnknownIdNotFound(t *testing.T) {
	s := openTokenStore(t)

	// Login check path: a cookie with no row must be a single ErrNotFound
	// — the middleware treats it exactly like "no cookie" (401, redirect to
	// login), never an internal error.
	_, err := s.GetSession(context.Background(), "no-such-hash")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session: got %v, want ErrNotFound", err)
	}
}

func TestSession_TouchUpdatesLastSeen(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	insertTestSession(t, s, "h", "adm-1", 1000)
	if err := s.TouchSession(ctx, "h", 2000); err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	got, err := s.GetSession(ctx, "h")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSeenAt != 2000 {
		t.Errorf("last_seen = %d, want 2000 (idle window restarts per request)", got.LastSeenAt)
	}
	// Touch must not disturb the absolute window (activity never
	// extends the 12 h cap).
	if got.AbsoluteExpiresAt != 1000+12*3600 {
		t.Errorf("absolute expiry changed by touch: %d", got.AbsoluteExpiresAt)
	}
}

func TestSession_DeleteIsIdempotent(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// Logout: the client may not resend the cookie, a proxy purge may
	// have already removed the row — either way the outcome is "logged
	// out", so a second delete must not error and the cookie is cleared.
	insertTestSession(t, s, "h", "adm-1", 1000)
	if err := s.DeleteSession(ctx, "h"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteSession(ctx, "h"); err != nil {
		t.Fatalf("second delete: %v (want idempotent nil)", err)
	}
	if _, err := s.GetSession(ctx, "h"); !errors.Is(err, ErrNotFound) {
		t.Errorf("session readable after delete: %v", err)
	}
}

func TestSession_DeleteForAdminRemovesAllSessions(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// Password change = privilege change → all of that admin's sessions
	// are revoked server-side. Count returned so callers can audit.
	insertTestSession(t, s, "h1", "adm-1", 1000)
	insertTestSession(t, s, "h2", "adm-1", 1000)
	insertTestSession(t, s, "h3", "adm-2", 1000)

	n, err := s.DeleteSessionsForAdmin(ctx, "adm-1")
	if err != nil {
		t.Fatalf("DeleteSessionsForAdmin: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d, want 2", n)
	}
	if _, err := s.GetSession(ctx, "h1"); !errors.Is(err, ErrNotFound) {
		t.Error("adm-1 session h1 survived")
	}
	if _, err := s.GetSession(ctx, "h2"); !errors.Is(err, ErrNotFound) {
		t.Error("adm-1 session h2 survived")
	}
	// Other admins are unaffected.
	if _, err := s.GetSession(ctx, "h3"); err != nil {
		t.Errorf("adm-2 session destroyed by adm-1 purge: %v", err)
	}
}

func TestSession_PurgeExpired(t *testing.T) {
	s := openTokenStore(t)
	ctx := context.Background()

	// The housekeeping sweep removes rows whose absolute expiry has
	// passed, so abandoned sessions cannot accumulate (and a stolen cookie
	// dies with its row even if never presented).
	insertTestSession(t, s, "expired-1", "adm-1", 1000) // expires at 1000+12h
	expired := Session{
		IDHash:            "expired-2",
		AdminID:           "adm-1",
		CreatedAt:         0,
		LastSeenAt:        0,
		AbsoluteExpiresAt: 500,
	}
	if err := s.InsertSession(ctx, expired); err != nil {
		t.Fatal(err)
	}
	insertTestSession(t, s, "live", "adm-1", 1000)

	n, err := s.PurgeExpiredSessions(ctx, 10000)
	if err != nil {
		t.Fatalf("PurgeExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d, want 1 (only expired-2)", n)
	}
	if _, err := s.GetSession(ctx, "live"); err != nil {
		t.Error("live session purged before its absolute expiry")
	}
	if _, err := s.GetSession(ctx, "expired-2"); !errors.Is(err, ErrNotFound) {
		t.Error("expired session survived purge")
	}
}

func TestSession_DuplicateIdRejected(t *testing.T) {
	s := openTokenStore(t)

	// The PK is the hash — a collision must fail loudly, never overwrite an
	// existing session (that would silently transfer the admin identity).
	insertTestSession(t, s, "h", "adm-1", 1000)
	err := s.InsertSession(context.Background(), Session{
		IDHash: "h", AdminID: "adm-2", CreatedAt: 2000, LastSeenAt: 2000, AbsoluteExpiresAt: 3000,
	})
	if err == nil {
		t.Error("duplicate session id accepted")
	}
}
