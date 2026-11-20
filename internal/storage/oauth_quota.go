package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const maxOAuthQuotaPrincipals = 4096

// AdmitOAuthRequest accounts for one request against a verified principal's
// shared burst budget. A positive delay reports when to retry. An immediate
// SQLite transaction serializes admission across process and proxy changes.
func (s *Store) AdmitOAuthRequest(ctx context.Context, principal [32]byte, interval time.Duration, burst int) (time.Duration, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	var ready int64
	err = tx.QueryRowContext(ctx, "SELECT ready_ns FROM oauth_quota WHERE principal=?", principal[:]).Scan(&ready)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, "DELETE FROM oauth_quota WHERE ready_ns<=?", now); err != nil {
			return 0, err
		}
		var count int
		var earliest sql.NullInt64
		if err := tx.QueryRowContext(ctx, "SELECT count(*),min(ready_ns) FROM oauth_quota").Scan(&count, &earliest); err != nil {
			return 0, err
		}
		if count >= maxOAuthQuotaPrincipals {
			return time.Duration(earliest.Int64 - now), nil
		}
	} else if err != nil {
		return 0, err
	}
	// Each accepted request advances the theoretical arrival time by one
	// interval. An idle principal regains its full burst once ready <= now.
	earliest := ready - int64(interval)*int64(burst-1)
	if earliest > now {
		return time.Duration(earliest - now), nil
	}
	next := max(now, ready) + int64(interval)
	if _, err := tx.ExecContext(ctx, `INSERT INTO oauth_quota (principal,ready_ns) VALUES (?,?)
		ON CONFLICT(principal) DO UPDATE SET ready_ns=excluded.ready_ns`, principal[:], next); err != nil {
		return 0, err
	}
	return 0, tx.Commit()
}
