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
    "github.com/burkostya/regate/internal/expr"
    "github.com/burkostya/regate/internal/identity"
    "github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/storage"
)

var running sync.Map // task id -> context.CancelFunc; one-process cancellation
// OAuth task owners are identities, not access-token bytes. A renewed token
// from the same issuer/audience/subject can resume a durable task; a token for
// a different principal can never inherit another owner's state.
func ownerIdentity(ctx context.Context,raw string) string {
    if principal,ok:=identity.FromContext(ctx);ok {
        return "oauth:"+principal.Issuer+"\x00"+principal.Audience+"\x00"+principal.Subject
    }
    return raw
}
func fingerprint(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}
func allowedOwner(task storage.TaskRecord, owner string) bool {
	return subtle.ConstantTimeCompare([]byte(task.OwnerDigest), []byte(fingerprint(owner))) == 1
}
func outcomeResult(out engine.Outcome, err error) map[string]any {
	if err != nil {
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": expr.RedactDiagnostic(err.Error())}}, "isError": true, "resultType": "complete"}
	}
	if out.MCP != nil {
		out.MCP["resultType"] = "complete"
		return out.MCP
	}
	return map[string]any{"resultType": "complete", "content": []any{map[string]any{"type": "text", "text": fmt.Sprint(out.Value)}}, "structuredContent": out.Value, "isError": false}
}
func (s *Service) CreateTask(ctx context.Context, surface, tool, owner string, args map[string]any) (map[string]any, error) {
    cfg,rev:=s.Current()
    return s.createTaskFor(ctx,cfg,rev.ID,surface,tool,owner,args)
}

func (s *Service) createTaskFor(ctx context.Context,cfg *config.Config,revision string,surface,tool,owner string,args map[string]any) (map[string]any,error) {
    published,ok:=cfg.Surfaces[surface]
    if !ok {return nil,fmt.Errorf("surface not found")}
    var selected *config.Tool
    for i:=range published.Tools {if published.Tools[i].Name==tool {selected=&published.Tools[i];break}}
    if selected==nil {return nil,fmt.Errorf("tool not found")}
    if published.OAuth!="" {
        if _,verified:=identity.FromContext(ctx);!verified {return nil,identity.ErrInvalidToken}
    }
    if err:=identity.RequireScopes(ctx,selected.RequiredScopes);err!=nil {return nil,err}
    if err:=cfg.ToolContracts[surface][tool].Input.Validate(args);err!=nil {
        return nil,fmt.Errorf("invalid tool arguments")
    }
    task := storage.TaskRecord{
        TaskID: storage.ID(),Status:"working",StatusMessage:"Executing pipeline",
        OwnerDigest:fingerprint(ownerIdentity(ctx,owner)),Surface:surface,Tool:tool,Revision:revision,
        RequiredScopes:append([]string(nil),selected.RequiredScopes...),
        RedactPII:published.OAuth!="" && !selected.AllowUnmasked,
        TTLMS:3600000,PollIntervalMS:1000,
    }
	if e := s.Store.TaskPut(task); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	running.Store(task.TaskID, context.CancelFunc(cancel))
	go func() {
		defer cancel()
		defer running.Delete(task.TaskID)
		out, err := s.Engine.Run(ctx, revision, cfg, surface, tool, args, false)
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
			current.Result = taskSafeResult(current,out,err)
		}
		_ = s.Store.TaskPut(current)
	}()
	result := task.Public()
	result["resultType"] = "task"
	return result, nil
}
// OAuth async tasks are stored with the sensitivity of the original tool.
 // A successful task must not bypass the masking applied to synchronous calls.
func taskSafeResult(task storage.TaskRecord,out engine.Outcome,err error) map[string]any {
    if task.RedactPII && err!=nil {
        return map[string]any{"content":[]any{map[string]any{"type":"text","text":"operation failed"}},"isError":true,"resultType":"complete"}
    }
    result:=outcomeResult(out,err)
    if task.RedactPII {
        if sanitized,ok:=expr.MaskPII(result).(map[string]any);ok{return sanitized}
    }
    return result
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
			updated.Result = taskSafeResult(updated,out,err)
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
