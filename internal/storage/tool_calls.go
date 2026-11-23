package storage

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

const maxToolCallObservations = 100_000
const toolCallRetention = 30 * 24 * time.Hour
const pruneToolCallsSQL = "DELETE FROM tool_call_observations WHERE started_ns < ?"

type ToolCallObservation struct {
	RunID, Surface, Revision, Tool string
	Group                          string
	Explicit                       bool
	StartedNS, FinishedNS          int64
	Successful                     bool
}

func (s *Store) initializeObservations(tx *sql.Tx, version int) error {
	if version < 9 {
		for _, stmt := range []string{
			`CREATE TABLE tool_call_observation_key (
				id INTEGER PRIMARY KEY CHECK (id = 1),
				key BLOB NOT NULL CHECK (length(key) = 32)
			)`,
			`CREATE TABLE tool_call_observations (
				seq INTEGER PRIMARY KEY AUTOINCREMENT,
				run_id TEXT NOT NULL UNIQUE,
				surface TEXT NOT NULL,
				revision TEXT NOT NULL,
				tool TEXT NOT NULL,
				group_digest BLOB NOT NULL CHECK (length(group_digest) = 32),
				explicit INTEGER NOT NULL CHECK (explicit IN (0, 1)),
				started_ns INTEGER NOT NULL,
				finished_ns INTEGER NOT NULL DEFAULT 0,
				successful INTEGER NOT NULL DEFAULT 0 CHECK (successful IN (0, 1))
			)`,
			"CREATE INDEX tool_call_observations_time ON tool_call_observations(started_ns)",
			"CREATE INDEX tool_call_observations_revision ON tool_call_observations(surface, revision)",
			"PRAGMA user_version=9",
		} {
			if _, err := tx.Exec(stmt); err != nil {
				return fmt.Errorf("SQLite tool call observation migration: %w", err)
			}
		}
		if _, err := rand.Read(s.observationKey[:]); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO tool_call_observation_key(id,key) VALUES(1,?)", s.observationKey[:]); err != nil {
			return err
		}
	}
	var key []byte
	if err := tx.QueryRow("SELECT key FROM tool_call_observation_key WHERE id=1").Scan(&key); err != nil {
		return err
	}
	if len(key) != len(s.observationKey) {
		return fmt.Errorf("invalid SQLite observation key")
	}
	copy(s.observationKey[:], key)
	_, err := tx.Exec(pruneToolCallsSQL, toolCallCutoff(time.Now()))
	return err
}

func toolCallCutoff(now time.Time) int64 {
	return now.Add(-toolCallRetention).UnixNano()
}

// ToolCallGroup hashes the grouping inputs without retaining them.
func (s *Store) ToolCallGroup(parts ...string) string {
	data, _ := json.Marshal(parts)
	mac := hmac.New(sha256.New, s.observationKey[:])
	_, _ = mac.Write(data)
	return string(mac.Sum(nil))
}

func (s *Store) StartToolCall(ctx context.Context, call ToolCallObservation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO tool_call_observations
		(run_id,surface,revision,tool,group_digest,explicit,started_ns) VALUES (?,?,?,?,?,?,?)`,
		call.RunID, call.Surface, call.Revision, call.Tool, []byte(call.Group), call.Explicit, call.StartedNS)
	if err != nil {
		return err
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM tool_call_observations WHERE seq<=?", seq-maxToolCallObservations); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, pruneToolCallsSQL, toolCallCutoff(time.Now())); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishToolCall(ctx context.Context, runID string, at time.Time, success bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tool_call_observations SET finished_ns=max(started_ns,?),successful=?
		WHERE run_id=? AND finished_ns=0`, at.UnixNano(), success, runID)
	return err
}

func (s *Store) ToolCalls(ctx context.Context, surface, revision string) ([]ToolCallObservation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT run_id,tool,group_digest,explicit,started_ns,finished_ns,successful
		FROM tool_call_observations WHERE surface=? AND revision=? AND started_ns>=?
		ORDER BY group_digest,started_ns,seq`, surface, revision, toolCallCutoff(time.Now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var calls []ToolCallObservation
	for rows.Next() {
		var call ToolCallObservation
		if err := rows.Scan(&call.RunID, &call.Tool, &call.Group, &call.Explicit, &call.StartedNS, &call.FinishedNS, &call.Successful); err != nil {
			return nil, err
		}
		calls = append(calls, call)
	}
	return calls, rows.Err()
}
