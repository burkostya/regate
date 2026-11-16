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

// A delegated task can continue only while a verified subject bearer is
// live in this process. Neither the token nor a refresh token is persisted.
// On a later resumed request, the same principal must supply a NEW verified
// bearer. Bound the in-process context by the original bearer expiration.
func taskExecutionContext(ctx context.Context, delegated bool)(context.Context,context.CancelFunc,error) {
    if !delegated {
        run,cancel:=context.WithTimeout(context.Background(),90*time.Second)
        return run,cancel,nil
    }
    if !identity.HasLiveSubjectToken(ctx) {
        return nil,nil,identity.ErrTokenExchange
    }
    principal,_:=identity.FromContext(ctx)
    expiry:=principal.ExpiresAt.Add(-2*time.Second)
    deadline:=time.Now().Add(90*time.Second)
    if expiry.Before(deadline) {deadline=expiry}
    if !deadline.After(time.Now()) {return nil,nil,identity.ErrTokenExchange}
    run,cancel:=context.WithDeadline(context.WithoutCancel(ctx),deadline)
    return run,cancel,nil
}

func immutableTaskScopes(surface,tool []string)[]string {
    seen:=map[string]bool{}
    var all []string
    for _,scope:=range append(append([]string(nil),surface...),tool...) {
        if scope!="" && !seen[scope] {all=append(all,scope);seen[scope]=true}
    }
    return all
}
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
    delegated:=engine.ToolNeedsDelegation(cfg,surface,*selected)
    var requirement *storage.DelegationRequirement
    if delegated {
        principal,ok:=identity.FromContext(ctx)
        provider,configured:=cfg.OAuth[published.OAuth]
        if !ok || !configured || !identity.HasLiveSubjectToken(ctx) ||
            principal.Issuer!=provider.Issuer || principal.Audience!=provider.Audience {
            return nil,identity.ErrTokenExchange
        }
        requirement=&storage.DelegationRequirement{
            Provider:published.OAuth,Issuer:provider.Issuer,Audience:provider.Audience,
        }
    }
    if err:=cfg.ToolContracts[surface][tool].Input.Validate(args);err!=nil {
        return nil,fmt.Errorf("invalid tool arguments")
    }
    runctx,cancel,err:=taskExecutionContext(ctx,delegated)
    if err!=nil{return nil,err}
    task := storage.TaskRecord{
        TaskID: storage.ID(),Status:"working",StatusMessage:"Executing pipeline",
        OwnerDigest:fingerprint(ownerIdentity(ctx,owner)),Surface:surface,Tool:tool,Revision:revision,
        RequiredScopes:immutableTaskScopes(published.RequiredScopes,selected.RequiredScopes),
        Delegation:requirement,
        RedactPII:published.OAuth!="" && !selected.AllowUnmasked,
        TTLMS:3600000,PollIntervalMS:1000,
    }
    // Only nondelegated, unscoped, non-OAuth requests can enter an encrypted
    // waiting queue. Protected tasks still need a fresh user authorization or
    // a specifically delegated task grant and therefore apply backpressure.
    eligibleQueue:=published.OAuth=="" &&
        !delegated && len(task.RequiredScopes)==0 && !task.RedactPII
    if eligibleQueue {
        queued,err:=s.Store.AdmitOrQueueTaskWithOwnerLimit(ctx,task,args,
            cfg.MaxActiveTasks,cfg.MaxQueuedTasks,cfg.MaxQueuedPerOwner)
        if err!=nil {cancel();return nil,err}
        if queued {
            cancel()
            s.startQueueDispatcher()
            committed,err:=s.Store.TaskGet(task.TaskID)
            if err!=nil{return nil,err}
            result:=committed.Public()
            result["resultType"]="task"
            return result,nil
        }
    } else if e:=s.Store.AdmitWorkingTask(ctx,task,cfg.MaxActiveTasks);e!=nil {
        cancel()
        return nil,e
    }
	running.Store(task.TaskID, context.CancelFunc(cancel))
	go func() {
		defer cancel()
		defer running.Delete(task.TaskID)
        defer s.signalQueue()
		out, err := s.Engine.Run(runctx, revision, cfg, surface, tool, args, false)
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
    // Compatibility entry point: callers without a verified request context
    // may still resume nondelegated tasks, but never delegated work.
    return s.TaskUpdateWithContext(context.Background(),id,owner,inputResponses)
}
func (s *Service) TaskUpdateWithContext(ctx context.Context,id, owner string, inputResponses map[string]any) (map[string]any, error) {
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()
	t, e := s.Store.TaskGet(id)
	if e != nil {
		return nil, e
	}
	if !allowedOwner(t, owner) {
		return nil, fmt.Errorf("task not found")
	}
    usingGrant:=t.Status=="working" && t.Phase=="refreshing_grant" &&
        engine.HasGrantExecution(ctx)
    if t.Status!="input_required" && !usingGrant {
        return map[string]any{},nil
    }
    if usingGrant {
        if err:=s.Engine.CheckGrantExecution(ctx);err!=nil{return nil,err}
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
    // Every resumed effect uses the original immutable revision and must
    // reacquire authorization from a newly verified inbound bearer. Neither
    // a task owner digest alone nor an expired credential is a delegation.
    selected:=config.Tool{}
    found:=false
    published,exists:=cfg.Surfaces[t.Surface]
    if !exists {return nil,identity.ErrTokenExchange}
    for _,candidate:=range published.Tools {
        if candidate.Name==t.Tool {selected=candidate;found=true;break}
    }
    if !found {return nil,identity.ErrTokenExchange}
    needsDelegation:=engine.ToolNeedsDelegation(cfg,t.Surface,selected)
    if needsDelegation!=(t.Delegation!=nil) {
        return nil,identity.ErrTokenExchange
    }
    if t.Delegation!=nil {
        principal,ok:=identity.FromContext(ctx)
        requirement:=t.Delegation
        if !ok || !identity.HasLiveSubjectToken(ctx) ||
            requirement.Provider!=published.OAuth ||
            requirement.Issuer!=principal.Issuer ||
            requirement.Audience!=principal.Audience ||
            !allowedOwner(t,ownerIdentity(ctx,owner)) {
            return nil,identity.ErrTokenExchange
        }
        if err:=identity.RequireScopes(ctx,t.RequiredScopes);err!=nil {
            return nil,err
        }
    }
    runctx,cancel,err:=taskExecutionContext(ctx,needsDelegation)
    if err!=nil{return nil,err}
	t.Status = "working"
    t.Phase=""
	t.InputRequests = nil
	t.StatusMessage = "Continuing pipeline"
	if !usingGrant {
        if e:=s.Store.ClaimTaskContinuation(ctx,t,cfg.MaxActiveTasks);e!=nil {
            cancel()
            return nil,e
        }
    } else if e:=s.Store.TaskPut(t);e!=nil {
        cancel()
        return nil,e
    }
	running.Store(id, context.CancelFunc(cancel))
	go func() {
		defer cancel()
		defer running.Delete(id)
        defer s.signalQueue()
		out, err := s.Engine.Resume(runctx, cfg, t.Revision, t.RunID, value)
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
    if t.Status=="queued" {
        if err:=s.Store.CancelQueuedTask(context.Background(),id,t.OwnerDigest);err!=nil {
            return nil,err
        }
        s.signalQueue()
        return map[string]any{},nil
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
        // Cancellation revokes any outstanding autonomous grant. A worker
        // holding an old access token must check task/grant state before
        // initiating the next protected external operation.
        if t.Delegation!=nil {
            _=s.Store.RevokeTaskGrant(context.Background(),id,t.OwnerDigest)
        }
        s.signalQueue()
	}
	return map[string]any{}, nil
}
