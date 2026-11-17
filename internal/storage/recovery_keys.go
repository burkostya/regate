package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"time"
)

// ErrBackupKeyAudit deliberately excludes task identity, key IDs,
// ciphertext and credential material from user-visible diagnostics.
var ErrBackupKeyAudit = errors.New("backup encrypted state authentication failed")

// BackupKeyAudit is a non-secret local recovery readiness check. It does not
// establish current OAuth authorization or replay safety for external writes.
type BackupKeyAudit struct {
	SnapshotSHA256       string `json:"snapshot_sha256"`
	QueuedAuthenticated  int    `json:"queued_authenticated"`
	GrantsAuthenticated  int    `json:"grants_authenticated"`
	GrantsNonExecutable  int    `json:"grants_non_executable"`
}

// VerifyBackupKeys authenticates every encrypted queued input and every
// retained encrypted grant in a standalone SQLite backup using the keyrings
// supplied only through process environment. It does not create a Store,
// migrate schemas, acquire a leader lease, refresh OAuth or dispatch work.
// Invalidated grants are counted but never treated as executable.
//
// The snapshot digest is checked again after auditing to reject a backup
// replaced or changed during the read. This proves local key recoverability,
// not that the IdP will honor a credential or that a remote effect is known.
func VerifyBackupKeys(path string) (BackupKeyAudit, error) {
	var report BackupKeyAudit
	manifest, err := VerifyBackup(path)
	if err != nil {
		return report, err
	}
	grantKeys, err := LoadGrantKeyRing()
	if err != nil {
		return report, ErrGrantKeyUnavailable
	}
	queueKeys, err := LoadTaskQueueKeyRing()
	if err != nil {
		return report, ErrTaskQueueKeyUnavailable
	}
	if manifest.Queued > 0 && queueKeys == nil {
		return report, ErrTaskQueueKeyUnavailable
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return report, err
	}
	uri := (&url.URL{Scheme: "file", Path: absolute}).String() + "?mode=ro"
	db, err := sql.Open("sqlite3", uri)
	if err != nil {
		return report, ErrInvalidBackup
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA query_only=ON"); err != nil {
		return report, ErrInvalidBackup
	}
	ctx := context.Background()
	if err := authenticateBackupQueue(ctx, db, queueKeys, &report); err != nil {
		return BackupKeyAudit{}, err
	}
	if err := authenticateBackupGrants(ctx, db, grantKeys, &report); err != nil {
		return BackupKeyAudit{}, err
	}
	if report.QueuedAuthenticated != manifest.Queued ||
		report.GrantsAuthenticated+report.GrantsNonExecutable != manifest.Grants {
		return BackupKeyAudit{}, ErrBackupKeyAudit
	}
	rechecked, err := VerifyBackup(path)
	if err != nil || rechecked != manifest {
		return BackupKeyAudit{}, ErrInvalidBackup
	}
	report.SnapshotSHA256 = manifest.SHA256
	return report, nil
}

func authenticateBackupQueue(ctx context.Context, db *sql.DB,
	keys *TaskQueueKeyRing, report *BackupKeyAudit) error {
	rows, err := db.QueryContext(ctx,
		"SELECT q.task_id,q.owner_digest,q.revision,q.surface,q.tool,"+
			"q.key_id,q.nonce,q.ciphertext,t.record,t.status "+
			"FROM task_queue q JOIN tasks t ON t.task_id=q.task_id ORDER BY q.seq")
	if err != nil {
		return ErrBackupKeyAudit
	}
	defer rows.Close()
	for rows.Next() {
		var id, owner, revision, surface, tool, keyID, record, status string
		var nonce, ciphertext []byte
		if err := rows.Scan(&id, &owner, &revision, &surface, &tool,
			&keyID, &nonce, &ciphertext, &record, &status); err != nil {
			return ErrBackupKeyAudit
		}
		var task TaskRecord
		if json.Unmarshal([]byte(record), &task) != nil ||
			task.TaskID != id || task.OwnerDigest != owner ||
			task.Revision != revision || task.Surface != surface ||
			task.Tool != tool || task.Status != "queued" ||
			status != "queued" || task.Phase != "queued" ||
			task.Delegation != nil || len(task.RequiredScopes) != 0 ||
			task.RedactPII {
			return ErrBackupKeyAudit
		}
		if _, err := keys.open(task, keyID, nonce, ciphertext); err != nil {
			return ErrTaskQueueKeyUnavailable
		}
		report.QueuedAuthenticated++
	}
	if rows.Err() != nil {
		return ErrBackupKeyAudit
	}
	return nil
}

func authenticateBackupGrants(ctx context.Context, db *sql.DB,
	keys *GrantKeyRing, report *BackupKeyAudit) error {
	rows, err := db.QueryContext(ctx,
		"SELECT g.task_id,g.owner_digest,g.revision,g.provider,g.issuer,"+
			"g.audience,g.scopes_digest,g.generation,g.expires_ns,g.state,"+
			"g.key_id,g.nonce,g.ciphertext,t.record "+
			"FROM task_grants g JOIN tasks t ON t.task_id=g.task_id "+
			"ORDER BY g.task_id")
	if err != nil {
		return ErrBackupKeyAudit
	}
	defer rows.Close()
	for rows.Next() {
		var metadata GrantMetadata
		var expires int64
		var nonce, ciphertext []byte
		var taskJSON string
		if err := rows.Scan(&metadata.TaskID, &metadata.OwnerDigest,
			&metadata.Revision, &metadata.Provider, &metadata.Issuer,
			&metadata.Audience, &metadata.ScopesDigest, &metadata.Generation,
			&expires, &metadata.State, &metadata.KeyID,
			&nonce, &ciphertext, &taskJSON); err != nil {
			return ErrBackupKeyAudit
		}
		metadata.ExpiresAt = time.Unix(0, expires).UTC()
		var task TaskRecord
		if json.Unmarshal([]byte(taskJSON), &task) != nil ||
			task.TaskID != metadata.TaskID ||
			task.OwnerDigest != metadata.OwnerDigest ||
			task.Revision != metadata.Revision ||
			task.Delegation == nil ||
			task.Delegation.Provider != metadata.Provider ||
			task.Delegation.Issuer != metadata.Issuer ||
			task.Delegation.Audience != metadata.Audience ||
			digestTaskScopes(task.RequiredScopes) != metadata.ScopesDigest ||
			len(task.RequiredScopes) == 0 || metadata.Generation < 1 {
			return ErrBackupKeyAudit
		}
		switch metadata.State {
		case "active", "refreshing":
			// An initial pending grant provision can be in refreshing
			// state without any credential yet. It MUST NOT be reported
			// as an authenticated, usable refresh credential.
			if metadata.State == "refreshing" && len(nonce) == 0 &&
				len(ciphertext) == 0 {
				report.GrantsNonExecutable++
				continue
			}
			if _, err := keys.open(metadata, nonce, ciphertext); err != nil {
				return ErrGrantKeyUnavailable
			}
			if metadata.State == "active" {
				report.GrantsAuthenticated++
			} else {
				// A rotating token is never safe to retry on recovery,
				// even when the stored prior ciphertext authenticates.
				report.GrantsNonExecutable++
			}
		case "revoked", "needs_reauth":
			if len(nonce) != 0 || len(ciphertext) != 0 {
				return ErrBackupKeyAudit
			}
			report.GrantsNonExecutable++
		default:
			return ErrBackupKeyAudit
		}
	}
	if rows.Err() != nil {
		return ErrBackupKeyAudit
	}
	return nil
}
