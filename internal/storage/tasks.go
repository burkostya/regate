package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// TaskRecord is durable; OwnerDigest never appears in public task responses.
type TaskRecord struct {
	TaskID         string         `json:"taskId"`
	Status         string         `json:"status"`
	StatusMessage  string         `json:"statusMessage,omitempty"`
	OwnerDigest    string         `json:"owner_digest"`
	Surface        string         `json:"surface"`
    Tool           string         `json:"tool,omitempty"`
    RequiredScopes []string       `json:"required_scopes,omitempty"`
    RedactPII      bool           `json:"redact_pii,omitempty"`
	RunID          string         `json:"run_id,omitempty"`
	Revision       string         `json:"revision"`
	CreatedAt      time.Time      `json:"createdAt"`
	LastUpdatedAt  time.Time      `json:"lastUpdatedAt"`
	TTLMS          int64          `json:"ttlMs"`
	PollIntervalMS int64          `json:"pollIntervalMs"`
	Result         map[string]any `json:"result,omitempty"`
	Error          map[string]any `json:"error,omitempty"`
	InputRequests  map[string]any `json:"inputRequests,omitempty"`
}

func (s *Store) TaskPut(task TaskRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(task.TaskID) != 24 {
		return fmt.Errorf("invalid task id")
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now().UTC()
	}
	task.LastUpdatedAt = time.Now().UTC()
	b, e := json.MarshalIndent(task, "", "  ")
	if e != nil {
		return e
	}
	return atomicWrite(filepath.Join(s.Dir, "tasks", task.TaskID+".json"), b)
}
func (s *Store) TaskGet(id string) (TaskRecord, error) {
	if len(id) != 24 {
		return TaskRecord{}, fmt.Errorf("invalid task id")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, e := os.ReadFile(filepath.Join(s.Dir, "tasks", id+".json"))
	if e != nil {
		return TaskRecord{}, e
	}
	var t TaskRecord
	if e = json.Unmarshal(b, &t); e != nil {
		return TaskRecord{}, e
	}
	if t.TTLMS > 0 && time.Since(t.CreatedAt) > time.Duration(t.TTLMS)*time.Millisecond {
		return TaskRecord{}, fmt.Errorf("task expired")
	}
	return t, nil
}
func (t TaskRecord) Public() map[string]any {
	out := map[string]any{"taskId": t.TaskID, "status": t.Status, "createdAt": t.CreatedAt.Format(time.RFC3339Nano), "lastUpdatedAt": t.LastUpdatedAt.Format(time.RFC3339Nano), "ttlMs": t.TTLMS, "pollIntervalMs": t.PollIntervalMS}
	if t.StatusMessage != "" {
		out["statusMessage"] = t.StatusMessage
	}
	if t.Status == "completed" && t.Result != nil {
		out["result"] = t.Result
	}
	if t.Status == "failed" && t.Error != nil {
		out["error"] = t.Error
	}
	if t.Status == "input_required" && t.InputRequests != nil {
		out["inputRequests"] = t.InputRequests
	}
	return out
}
func (s *Store) RecoverInterruptedTasks() error {
	items, e := os.ReadDir(filepath.Join(s.Dir, "tasks"))
	if e != nil {
		return e
	}
	for _, entry := range items {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := entry.Name()[:len(entry.Name())-5]
		t, e := s.TaskGet(id)
		if e != nil {
			continue
		}
		if t.Status == "working" {
			t.Status = "failed"
			t.StatusMessage = "Server restarted: reconcile external effects before retry"
			t.Error = map[string]any{"code": -32000, "message": "Interrupted execution; outcome of external effects may be unknown"}
			if e := s.TaskPut(t); e != nil {
				return e
			}
		}
	}
	return nil
}
