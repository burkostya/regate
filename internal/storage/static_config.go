package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

var ErrStaticConfigConflict = errors.New("static HCL is managed by deployment; preserve its prefix and use a revision from the current deployment")

func (s *Store) StaticConfigPrefix() (string, error) {
	var prefix string
	err := s.db.QueryRow("SELECT prefix FROM static_config WHERE id=1").Scan(&prefix)
	return prefix, err
}

func checkStaticSource(tx *sql.Tx, source string) error {
	var prefix string
	if err := tx.QueryRow("SELECT prefix FROM static_config WHERE id=1").Scan(&prefix); err != nil {
		return err
	}
	if !strings.HasPrefix(source, prefix) {
		return ErrStaticConfigConflict
	}
	return nil
}

// ActivateStaticConfig replaces the existing ownership boundary and active
// revision together. Only the execution leader can publish a deployment.
func (s *Store) ActivateStaticConfig(base, source, prefix string) (Revision, error) {
	if !strings.HasPrefix(source, prefix) {
		return Revision{}, ErrStaticConfigConflict
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return Revision{}, err
	}
	defer tx.Rollback()
	if err := s.checkExecutionLeaseTx(tx); err != nil {
		return Revision{}, err
	}
	var active string
	if err := tx.QueryRow("SELECT id FROM revisions ORDER BY seq DESC LIMIT 1").Scan(&active); err != nil {
		return Revision{}, err
	}
	if base != active {
		return Revision{}, errors.New("active revision changed during static configuration publication")
	}
	revision, err := activateTx(tx, source, active)
	if err != nil {
		return Revision{}, err
	}
	if err := recordStaticConfig(tx, prefix, revision.ID); err != nil {
		return Revision{}, err
	}
	if err := tx.Commit(); err != nil {
		return Revision{}, err
	}
	return revision, nil
}

func recordStaticConfig(tx *sql.Tx, prefix, revision string) error {
	if _, err := tx.Exec("UPDATE static_config SET prefix=?,since_seq=(SELECT seq FROM revisions WHERE id=?) WHERE id=1", prefix, revision); err != nil {
		return err
	}
	return appendAuditTx(tx, "management", "regate_static_config_apply", "succeeded")
}
