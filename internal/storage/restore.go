package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var ErrUnsafeRestoreTarget = errors.New("restore requires an empty private state directory")

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
// before allowing a restored worker to acquire execution leadership.
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
	stagingDir, err := os.MkdirTemp(stateDir, ".regate-restore-*")
	if err != nil {
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
	published := filepath.Join(stateDir, databaseName)
	if err = os.Link(stagedPath, published); err != nil {
		return empty, fmt.Errorf("restore cannot publish over existing state: %w", err)
	}
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
