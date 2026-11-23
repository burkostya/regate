package storage

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestToolCallRetentionAndGroupingKey(t *testing.T) {
	const source = `regate { listen = "127.0.0.1:8080" }`
	dir := t.TempDir()
	db, err := New(dir, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	now := time.Now()
	group := db.ToolCallGroup("surface", "header:x-session", "session", "owner")
	if group == db.ToolCallGroup("surface", "meta:x-session", "session", "owner") {
		t.Fatal("different session sources share a group")
	}
	insert := func(run, surface, revision string, at time.Time) {
		t.Helper()
		err := db.StartToolCall(ctx, ToolCallObservation{
			RunID: run, Surface: surface, Revision: revision, Tool: "read",
			Group: group, Explicit: true, StartedNS: at.UnixNano(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("expired", "surface", "revision", now.Add(-31*24*time.Hour))
	insert("another-surface", "another", "revision", now)
	insert("previous", "surface", "previous", now)
	insert("kept", "surface", "revision", now)
	if err := db.FinishToolCall(ctx, "kept", now.Add(time.Second), true); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishToolCall(ctx, "kept", now.Add(2*time.Second), false); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New(dir, source)
	if err != nil {
		t.Fatal(err)
	}
	if group != db.ToolCallGroup("surface", "header:x-session", "session", "owner") {
		t.Fatal("grouping key changed on restart")
	}
	rows, err := db.ToolCalls(ctx, "surface", "revision")
	if err != nil || len(rows) != 1 || rows[0].RunID != "kept" || !rows[0].Successful || rows[0].FinishedNS != now.Add(time.Second).UnixNano() {
		t.Fatalf("unexpected retained observations: %+v, %v", rows, err)
	}
	if _, err := db.db.Exec(`WITH RECURSIVE entries(n) AS (
		SELECT 1 UNION ALL SELECT n+1 FROM entries WHERE n<?
	) INSERT INTO tool_call_observations(run_id,surface,revision,tool,group_digest,explicit,started_ns)
		SELECT 'row-'||n,'surface','revision','read',?,1,? FROM entries`, maxToolCallObservations, []byte(group), now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	insert("newest", "surface", "revision", now)
	var count, old int
	if err := db.db.QueryRow(`SELECT count(*),sum(run_id IN ('previous','kept','another-surface','row-1'))
		FROM tool_call_observations`).Scan(&count, &old); err != nil {
		t.Fatal(err)
	}
	if count != maxToolCallObservations || old != 0 {
		t.Fatalf("cap retained %d rows and %d oldest calls", count, old)
	}
}

// Migration fixtures start with the current database and remove the later
// tables needed to exercise the historical upgrade.
func removeToolObservationSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"tool_call_observations", "tool_call_observation_key"} {
		if _, err := db.Exec("DROP TABLE " + table); err != nil {
			t.Fatal(err)
		}
	}
}
