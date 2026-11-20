package storage

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

const maxEffectHistory = 200

var ErrInvalidEffectHistoryID = errors.New("invalid effect ID")
var ErrEffectHistoryNotFound = errors.New("effect not found")

type EffectTransition struct {
	Status string    `json:"status"`
	At     time.Time `json:"at"`
}

type EffectHistory struct {
	EffectID    string             `json:"effect_id"`
	RunID       string             `json:"run_id"`
	Node        string             `json:"node"`
	Transitions []EffectTransition `json:"transitions"`
	Truncated   bool               `json:"truncated,omitempty"`
}

// EffectHistory returns the oldest transitions in commit order and the
// identifiers needed to correlate their durable state with a pipeline trace.
func (s *Store) EffectHistory(ctx context.Context, id string) (EffectHistory, error) {
	if id == "" || len(id) > 128 || !utf8.ValidString(id) || strings.ContainsAny(id, "\r\n\x00") {
		return EffectHistory{}, ErrInvalidEffectHistoryID
	}
	rows, err := s.db.QueryContext(ctx, `SELECT json_extract(record,'$.run_id'),
		json_extract(record,'$.node'), status, at FROM effect_events
		WHERE effect_id=? ORDER BY seq LIMIT ?`, id, maxEffectHistory+1)
	if err != nil {
		return EffectHistory{}, err
	}
	defer rows.Close()

	history := EffectHistory{EffectID: id}
	for rows.Next() {
		if len(history.Transitions) == maxEffectHistory {
			history.Truncated = true
			break
		}
		var run, node, status, at string
		if err := rows.Scan(&run, &node, &status, &at); err != nil {
			return EffectHistory{}, err
		}
		if len(history.Transitions) == 0 {
			history.RunID, history.Node = run, node
		}
		when, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return EffectHistory{}, err
		}
		history.Transitions = append(history.Transitions, EffectTransition{Status: status, At: when})
	}
	if err := rows.Err(); err != nil {
		return EffectHistory{}, err
	}
	if len(history.Transitions) == 0 {
		return EffectHistory{}, ErrEffectHistoryNotFound
	}
	return history, nil
}
