package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/burkostya/regate/internal/engine"
	"github.com/burkostya/regate/internal/storage"
)

var running sync.Map // task id -> context.CancelFunc; one-process cancellation
func fingerprint(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}
func allowedOwner(task storage.TaskRecord, owner string) bool {
	return subtle.ConstantTimeCompare([]byte(task.OwnerDigest), []byte(fingerprint(owner))) == 1
}
func outcomeResult(out engine.Outcome, err error) map[string]any {
	if err != nil {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true, "resultType": "complete"}
	}
	if out.MCP != nil {
		out.MCP["resultType"] = "complete"
		return out.MCP
	}
	return map[string]any{"resultType": "complete", "content": []any{map[string]any{"type": "text", "text": fmt.Sprint(out.Value)}}, "structuredContent": out.Value, "isError": false}
}
func (s *Service) CreateTask(surface, tool, owner string, args map[string]any) (map[string]any, error) {
	cfg, rev := s.Current()
	task := storage.TaskRecord{TaskID: storage.ID(), Status: "working", StatusMessage: "Executing pipeline", OwnerDigest: fingerprint(owner), Surface: surface, Revision: rev.ID, TTLMS: 3600000, PollIntervalMS: 1000}
	if e := s.Store.TaskPut(task); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	running.Store(task.TaskID, context.CancelFunc(cancel))
	go func() {
		defer cancel()
		defer running.Delete(task.TaskID)
		out, err := s.Engine.Run(ctx, rev.ID, cfg, surface, tool, args, false)
		// Persist status under a per-task mutex to avoid races with cancel/update.
		s.tasksMu.Lock()
		defer s.tasksMu.Unlock()
		current, e := s.Store.TaskGet(task.TaskID)
		if e != nil {
			return
		}
		if current.Status == "cancelled" {
			return
		}
		current.RunID = out.RunID
		if out.Status == "suspended" {
			current.Status = "input_required"
			current.StatusMessage = "Agent or user input needed to resume pipeline"
			current.InputRequests = map[string]any{"resume": map[string]any{"method": "elicitation/create", "params": map[string]any{"mode": "form", "message": "Provide the data required to resume the Regate pipeline", "requestedSchema": map[string]any{"type": "object", "additionalProperties": true}}}}
		} else {
			current.Status = "completed"
			current.StatusMessage = "Execution completed"
			current.Result = outcomeResult(out, err)
		}
		_ = s.Store.TaskPut(current)
	}()
	result := task.Public()
	result["resultType"] = "task"
	return result, nil
}
func (s *Service) TaskGet(id, owner string) (map[string]any, error) {
	t, e := s.Store.TaskGet(id)
	if e != nil {
		return nil, e
	}
	if !allowedOwner(t, owner) {
		return nil, fmt.Errorf("task not found")
	}
	return t.Public(), nil
}
func (s *Service) TaskUpdate(id, owner string, inputResponses map[string]any) (map[string]any, error) {
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	t, e := s.Store.TaskGet(id)
	if e != nil {
		return nil, e
	}
	if !allowedOwner(t, owner) {
		return nil, fmt.Errorf("task not found")
	}
	if t.Status != "input_required" {
		return map[string]any{}, nil
	}
	response, ok := inputResponses["resume"].(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	if response["action"] != "accept" {
		return map[string]any{}, nil
	}
	value := response["content"]
	source, e := s.Store.Source(t.Revision)
	if e != nil {
		return nil, e
	}
	cfg, e := s.Validate(source)
	if e != nil {
		return nil, e
	}
	t.Status = "working"
	t.InputRequests = nil
	t.StatusMessage = "Continuing pipeline"
	if e := s.Store.TaskPut(t); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	running.Store(id, context.CancelFunc(cancel))
	go func() {
		defer cancel()
		defer running.Delete(id)
		out, err := s.Engine.Resume(ctx, cfg, t.Revision, t.RunID, value)
		s.tasksMu.Lock()
		defer s.tasksMu.Unlock()
		updated, e := s.Store.TaskGet(id)
		if e != nil || updated.Status == "cancelled" {
			return
		}
		updated.RunID = out.RunID
		if out.Status == "suspended" {
			updated.Status = "input_required"
			updated.InputRequests = map[string]any{"resume_next": map[string]any{"method": "elicitation/create", "params": map[string]any{"mode": "form", "message": "Additional continuation data", "requestedSchema": map[string]any{"type": "object"}}}}
		} else {
			updated.Status = "completed"
			updated.Result = outcomeResult(out, err)
		}
		_ = s.Store.TaskPut(updated)
	}()
	return map[string]any{}, nil
}
func (s *Service) TaskCancel(id, owner string) (map[string]any, error) {
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	t, e := s.Store.TaskGet(id)
	if e != nil {
		return nil, e
	}
	if !allowedOwner(t, owner) {
		return nil, fmt.Errorf("task not found")
	}
	if t.Status == "working" || t.Status == "input_required" {
		if v, ok := running.Load(id); ok {
			v.(context.CancelFunc)()
		}
		t.Status = "cancelled"
		t.StatusMessage = "Cancellation requested; external effects may still have occurred"
		if e := s.Store.TaskPut(t); e != nil {
			return nil, e
		}
	}
	return map[string]any{}, nil
}
