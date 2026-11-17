package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrUnsafeRestoreTarget = errors.New("restore requires an empty private state directory")
var ErrRestoreQuarantined = errors.New("restored SQLite state is quarantined: offline operator acknowledgment required")

const restoreQuarantineFile = ".regate-restore-quarantine"
const restoreReceiptFile = ".regate-restore-acknowledgment.json"

type restoreQuarantineRecord struct {
	SnapshotSHA256 string `json:"snapshot_sha256"`
	RestoredAt time.Time `json:"restored_at"`
}

// A restored snapshot cannot be opened by Store.New or acquire execution
// leadership until offline reconciliation has explicitly invalidated all
// potentially replayable historical tasks and credentials.
func checkRestoreQuarantine(dir string) error {
	_,err:=os.Lstat(filepath.Join(dir,restoreQuarantineFile))
	if err==nil{return ErrRestoreQuarantined}
	if errors.Is(err,os.ErrNotExist){return nil}
	return err
}

// RestoreVerifiedBackup publishes a verified offline snapshot into an existing,
// empty, private state directory. It never opens the destination as a Store:
// doing so could run migrations, recovery or external execution too early.
//
// The archive is independently verified both before and after copying. The
// second check and byte digest comparison reject replacement or truncation of
// the source during transfer. The SQLite filename is published with a
// no-overwrite hard link, so failure cannot replace a live database.
//
// This operation does NOT fence an external Regate worker or prove that
// encryption keyrings for queued tasks and OAuth grants are available.
// Operators must fence the old worker and keep its remote effects isolated
// before acknowledging the quarantine. Restored nonterminal work and
// grants will be invalidated, not blindly replayed, at acknowledgment.
func RestoreVerifiedBackup(archive, stateDir string) (BackupManifest, error) {
	empty := BackupManifest{}
	if strings.TrimSpace(archive) == "" || strings.TrimSpace(stateDir) == "" {
		return empty, ErrUnsafeRestoreTarget
	}
	stateInfo, err := os.Lstat(stateDir)
	if err != nil {
		return empty, err
	}
	if !stateInfo.IsDir() || stateInfo.Mode().Perm()&0077 != 0 {
		return empty, ErrUnsafeRestoreTarget
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return empty, err
	}
	if len(entries) != 0 {
		return empty, ErrUnsafeRestoreTarget
	}

	original, err := VerifyBackup(archive)
	if err != nil {
		return empty, err
	}
	// A fixed, exclusive staging directory is also a filesystem-level
	// reservation. Competing restores cannot each open their own staging
	// directory and invalidate one another's final empty-state check.
	// A crash leaves this reservation for explicit operator inspection.
	stagingDir := filepath.Join(stateDir, ".regate-restore-staging")
	if err := os.Mkdir(stagingDir, 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return empty, ErrUnsafeRestoreTarget
		}
		return empty, err
	}
	defer os.RemoveAll(stagingDir)
	if err := os.Chmod(stagingDir, 0700); err != nil {
		return empty, err
	}
	stagedPath := filepath.Join(stagingDir, "snapshot.sqlite")
	input, err := os.Open(archive)
	if err != nil {
		return empty, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return empty, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 ||
		info.Size() != original.SizeBytes {
		return empty, ErrInvalidBackup
	}
	output, err := os.OpenFile(stagedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return empty, err
	}
	// Limit copied bytes: a concurrently modified archive cannot grow the
	// restore without bound. A short or oversized copy fails verification.
	count, copyErr := io.Copy(output, io.LimitReader(input, original.SizeBytes+1))
	if copyErr == nil && count == original.SizeBytes {
		copyErr = output.Sync()
	}
	if closeErr := output.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return empty, copyErr
	}
	if count != original.SizeBytes {
		return empty, ErrInvalidBackup
	}
	staged, err := VerifyBackup(stagedPath)
	if err != nil {
		return empty, err
	}
	if staged != original {
		return empty, ErrInvalidBackup
	}
	// Reject any competing writer's files, even if it began after the
	// initial empty-directory check. os.Link atomically refuses a target
	// filename created after this check as well.
	entries, err = os.ReadDir(stateDir)
	if err != nil {
		return empty, err
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(stagingDir) {
		return empty, ErrUnsafeRestoreTarget
	}
	// Publish the durable quarantine sentinel FIRST. A crash can leave a
	// blocked directory, but can never expose an executable restored DB.
	markerPath := filepath.Join(stateDir, restoreQuarantineFile)
	marker, err := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return empty, err
	}
	markerError := json.NewEncoder(marker).Encode(restoreQuarantineRecord{
		SnapshotSHA256: staged.SHA256, RestoredAt: time.Now().UTC(),
	})
	if markerError == nil { markerError = marker.Sync() }
	if closeErr := marker.Close(); markerError == nil { markerError = closeErr }
	if markerError != nil {
		_ = os.Remove(markerPath)
		return empty, markerError
	}
	databasePublished := false
	defer func() {
		if !databasePublished { _ = os.Remove(markerPath) }
	}()
	published := filepath.Join(stateDir, databaseName)
	if err = os.Link(stagedPath, published); err != nil {
		return empty, fmt.Errorf("restore cannot publish over existing state: %w", err)
	}
	databasePublished = true
	parent, err := os.Open(stateDir)
	if err != nil {
		return empty, err
	}
	err = parent.Sync()
	if closeErr := parent.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		// A verified file may already be visible; do not delete it or
		// pretend that a failed fsync established durable publication.
		return empty, err
	}
	return staged, nil
}
