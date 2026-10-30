package storage

import "context"

// Ping reports whether the database answers a trivial round trip. /healthz
// uses it so a full volume or a corrupt/locked database turns into a 503
// instead of a permanently "ok" container.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}
