package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// GrantKeyUsageReport exposes only anonymous key IDs and aggregate counts.
// It neither reveals credential bytes nor claims that the IdP will accept an
// encrypted refresh credential.
type GrantKeyUsageReport struct {
	ActiveKey string         `json:"active_key"`
	ByKey     map[string]int `json:"by_key"`
	Total     int            `json:"total"`
	Blocked   int            `json:"blocked"`
}

type GrantRewrapReport struct {
	GrantKeyUsageReport
	Rewrapped       int `json:"rewrapped"`
	RemainingOldKey int `json:"remaining_old_key"`
}

// grantKeyUsageSQL counts only retained ciphertext. Revoked and
// needs_reauth records with erased ciphertext do not require the retired key.
// A live working task, expired credential, or indeterminate refresh cannot be
// safely re-encrypted with a new generation.
const grantKeyUsageSQL = `SELECT g.key_id,COUNT(*),
	COALESCE(SUM(CASE
	    WHEN g.state != 'active' OR t.status != 'input_required'
	      OR g.expires_ns <= ? OR
	      COALESCE(json_extract(t.record,'$.execution_phase'),'') != ''
	    THEN 1 ELSE 0 END),0)
	FROM task_grants g JOIN tasks t ON t.task_id=g.task_id
	WHERE length(g.ciphertext)>0
	GROUP BY g.key_id ORDER BY g.key_id`

func readGrantKeyUsage(rows *sql.Rows, active string) (GrantKeyUsageReport, error) {
	report := GrantKeyUsageReport{ActiveKey: active, ByKey: map[string]int{}}
	defer rows.Close()
	for rows.Next() {
		var key string
		var count, blocked int
		if err := rows.Scan(&key, &count, &blocked); err != nil {
			return GrantKeyUsageReport{}, ErrGrantUnavailable
		}
		if count < 0 || blocked < 0 || blocked > count {
			return GrantKeyUsageReport{}, ErrGrantUnavailable
		}
		report.ByKey[key] = count
		report.Total += count
		report.Blocked += blocked
	}
	if err := rows.Err(); err != nil {
		return GrantKeyUsageReport{}, ErrGrantUnavailable
	}
	if err := rows.Close(); err != nil {
		return GrantKeyUsageReport{}, ErrGrantUnavailable
	}
	return report, nil
}

// GrantKeyUsage returns non-secret inventory for key-retirement planning.
// Historical SQLite backups and WAL pages must be audited separately.
func (s *Store) GrantKeyUsage(ctx context.Context) (GrantKeyUsageReport, error) {
	active := ""
	if s.grantKeys != nil {
		active = s.grantKeys.active
	}
	rows, err := s.db.QueryContext(ctx, grantKeyUsageSQL, time.Now().UnixNano())
	if err != nil {
		return GrantKeyUsageReport{}, ErrGrantUnavailable
	}
	return readGrantKeyUsage(rows, active)
}

type grantRewrapCandidate struct {
	meta       GrantMetadata
	nonce      []byte
	ciphertext []byte
	taskJSON   string
	taskState  string
}

// RewrapGrantsBatch authenticates and reseals only quiescent, suspended,
// active OAuth grants, under a new active encryption key. Updating the AAD
// generation also fences a stale in-memory continuation. It must NEVER
// rewrap a working task or a refresh whose remote outcome might be unknown.
//
// Selection, decryption, generation CAS, encryption, and key-usage accounting
// are one execution-leader-fenced SQLite write transaction. Failure is atomic:
// no partially rewrapped batch or unguarded OAuth request can result.
func (s *Store) RewrapGrantsBatch(ctx context.Context, limit int) (GrantRewrapReport, error) {
	var empty GrantRewrapReport
	if s.grantKeys == nil {
		return empty, ErrGrantKeyUnavailable
	}
	if limit < 1 || limit > 64 {
		return empty, fmt.Errorf("grant rewrap batch size must be between 1 and 64")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	if err := s.checkExecutionLeaseTx(tx); err != nil {
		return empty, err
	}
	// Strictly limit to suspended tasks with no active authorization phase.
	// A blocked old-key grant remains in the usage inventory; operators must
	// resolve it before retiring the old key. Do not silently drop it.
	rows, err := tx.QueryContext(ctx, `SELECT
		g.task_id,g.owner_digest,g.revision,g.provider,g.issuer,
		g.audience,g.scopes_digest,g.generation,g.expires_ns,g.state,
		g.key_id,g.nonce,g.ciphertext,t.record,t.status
		FROM task_grants g JOIN tasks t ON t.task_id=g.task_id
		WHERE g.key_id<>? AND g.state='active'
		  AND g.expires_ns>? AND t.status='input_required'
		  AND COALESCE(json_extract(t.record,'$.execution_phase'),'')=''
		  AND length(g.ciphertext)>0
		ORDER BY g.task_id LIMIT ?`,
		s.grantKeys.active, time.Now().UnixNano(), limit)
	if err != nil {
		return empty, ErrGrantUnavailable
	}
	candidates := make([]grantRewrapCandidate, 0, limit)
	for rows.Next() {
		var item grantRewrapCandidate
		var expires int64
		err = rows.Scan(&item.meta.TaskID, &item.meta.OwnerDigest,
			&item.meta.Revision, &item.meta.Provider, &item.meta.Issuer,
			&item.meta.Audience, &item.meta.ScopesDigest, &item.meta.Generation,
			&expires, &item.meta.State, &item.meta.KeyID,
			&item.nonce, &item.ciphertext, &item.taskJSON, &item.taskState)
		if err != nil {
			break
		}
		item.meta.ExpiresAt = time.Unix(0, expires).UTC()
		candidates = append(candidates, item)
	}
	if err == nil {
		err = rows.Err()
	}
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return empty, ErrGrantUnavailable
	}

	report := GrantRewrapReport{}
	for _, item := range candidates {
		var task TaskRecord
		if json.Unmarshal([]byte(item.taskJSON), &task) != nil ||
			task.Status != item.taskState ||
			task.Status != "input_required" || task.Phase != "" ||
			task.TaskID != item.meta.TaskID ||
			task.OwnerDigest != item.meta.OwnerDigest ||
			task.Revision != item.meta.Revision ||
			task.Delegation == nil ||
			task.Delegation.Provider != item.meta.Provider ||
			task.Delegation.Issuer != item.meta.Issuer ||
			task.Delegation.Audience != item.meta.Audience ||
			digestTaskScopes(task.RequiredScopes) != item.meta.ScopesDigest ||
			len(task.RequiredScopes) == 0 ||
			item.meta.Generation < 1 ||
			item.meta.State != "active" ||
			!item.meta.ExpiresAt.After(time.Now()) ||
			(task.TTLMS > 0 &&
				time.Since(task.CreatedAt) > time.Duration(task.TTLMS)*time.Millisecond) {
			return empty, ErrGrantUnavailable
		}
		token, err := s.grantKeys.open(item.meta, item.nonce, item.ciphertext)
		if err != nil {
			return empty, ErrGrantKeyUnavailable
		}
		next := item.meta
		next.Generation++
		next.KeyID = s.grantKeys.active
		nonce, sealed, err := s.grantKeys.seal(next, token)
		if err != nil {
			return empty, ErrGrantKeyUnavailable
		}
		updated, err := tx.ExecContext(ctx, `UPDATE task_grants
			SET key_id=?,generation=?,nonce=?,ciphertext=?,updated_ns=?
			WHERE task_id=? AND owner_digest=? AND revision=?
			  AND generation=? AND state='active' AND key_id=?`,
			next.KeyID, next.Generation, nonce, sealed, time.Now().UnixNano(),
			item.meta.TaskID, item.meta.OwnerDigest, item.meta.Revision,
			item.meta.Generation, item.meta.KeyID)
		if err != nil {
			return empty, err
		}
		affected, err := updated.RowsAffected()
		if err != nil || affected != 1 {
			return empty, ErrGrantConflict
		}
		report.Rewrapped++
	}
	usage, err := tx.QueryContext(ctx, grantKeyUsageSQL, time.Now().UnixNano())
	if err != nil {
		return empty, ErrGrantUnavailable
	}
	report.GrantKeyUsageReport, err = readGrantKeyUsage(usage, s.grantKeys.active)
	if err != nil {
		return empty, err
	}
	for key, count := range report.ByKey {
		if key != report.ActiveKey {
			report.RemainingOldKey += count
		}
	}
	if err := tx.Commit(); err != nil {
		return empty, err
	}
	return report, nil
}
