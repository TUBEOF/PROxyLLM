package storage

import "context"

// pruneBatch bounds one retention transaction. A long-lived install can have
// millions of old rows; one unbounded DELETE would hold SQLite's single
// writer lock long enough to starve log inserts and session touches.
const pruneBatch = 500

// PruneRequestLogs deletes request rows older than cutoff (unix seconds) and
// their child sub-operation rows in bounded batches. The child table
// references request_logs by convention, not by FK, so the children are
// deleted first. Returns the number of request rows removed.
func (s *Store) PruneRequestLogs(ctx context.Context, cutoff int64) (int64, error) {
	var total int64
	for {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return total, err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM request_child_logs WHERE parent_id IN (
				SELECT id FROM request_logs WHERE ts < ? LIMIT ?)`,
			cutoff, pruneBatch); err != nil {
			_ = tx.Rollback()
			return total, err
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM request_logs WHERE id IN (
				SELECT id FROM request_logs WHERE ts < ? LIMIT ?)`,
			cutoff, pruneBatch)
		if err != nil {
			_ = tx.Rollback()
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return total, err
		}
		if err := tx.Commit(); err != nil {
			return total, err
		}
		total += n
		if n < pruneBatch {
			return total, nil
		}
	}
}
