package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"slices"
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
        base := context.Background()
        if principal, verified := identity.FromContext(ctx); verified {
            base = identity.WithPrincipal(base, principal)
        }
        run,cancel:=context.WithTimeout(base,90*time.Second)
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

// continuationRequest publishes the key the client must echo. Multiple
// independent suspensions also require a path alongside action and content.
func continuationRequest(paths [][]string, key string, first bool) map[string]any {
	message:="Additional continuation data"
	if first { message="Provide the data required to resume the Regate pipeline" }
	if len(paths)>1 { message="Select one of the pending suspension paths and provide its data" }
	return map[string]any{key:map[string]any{
		"method":"elicitation/create",
		"params":map[string]any{"mode":"form","message":message,
			"requestedSchema":map[string]any{"type":"object","additionalProperties":true}},
		"paths":paths,
	}}
}

// continuationRecoveryRequest is not an elicitation for another answer:
 // the final reply has already been committed, and recovering must use that
 // immutable value. The caller must explicitly request recovery.
func continuationRecoveryRequest(key string) map[string]any {
	return map[string]any{key:map[string]any{
		"method":"elicitation/create",
		"params":map[string]any{"mode":"form",
			"message":"The final answer was already durably accepted. Verify external effect outcomes, then request recovery without sending another answer.",
			"requestedSchema":map[string]any{"type":"object","additionalProperties":false}},
		"recovery":true,
	}}
}

func parseContinuationPath(response map[string]any)([]string,error) {
	raw,found:=response["path"]
	if !found {return nil,nil}
	var segments []string
	switch path:=raw.(type) {
	case []string:
		segments=path
	case []any:
		for _,part:=range path {
			name,ok:=part.(string)
			if !ok {return nil,fmt.Errorf("invalid suspension path segment")}
			segments=append(segments,name)
		}
	default:
		return nil,fmt.Errorf("suspension path must be an array of node names")
	}
	if len(segments)==0 {return nil,fmt.Errorf("empty suspension path")}
	for _,part:=range segments {
		if part=="" {return nil,fmt.Errorf("empty suspension path segment")}
	}
	return segments,nil
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
    if run, ok := ctx.Value(observedRunKey{}).(*observedRun); ok && run.id != "" {
        task.RunID = run.id
        task.ToolCallObserved = true
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
		out, err := s.runTaskTool(runctx, cfg, task, args)
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
			current.InputRequests = continuationRequest(out.SuspensionPaths, "resume", true)
		} else {
			current.Status = "completed"
			current.StatusMessage = "Execution completed"
			current.Result = taskSafeResult(current,out,err)
		}
		s.finishTaskObservation(current)
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
	responseKey := "resume" // Server-initiated grant recovery retains its internal key.
	if !usingGrant {
		if len(t.InputRequests) != 1 {
			return map[string]any{}, nil
		}
		for key := range t.InputRequests {
			responseKey = key
		}
	}
	response, ok := inputResponses[responseKey].(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	if response["action"] != "accept" {
		return map[string]any{}, nil
	}
	value := response["content"]
	path, err := parseContinuationPath(response)
	if err!=nil {return nil,err}
	// Resolve and authorize a target before claiming the task. An ambiguous,
	// duplicate or malformed path must not turn an input_required task into a
	// completed error task; it can be corrected without replaying any effect.
	var checkpoint engine.Checkpoint
	if err:=s.Store.GetSuspended(t.RunID,&checkpoint);err!=nil {return nil,err}
	waiting:=engine.PendingSuspensionPaths(&checkpoint)
	if len(waiting)==0 {
		// Once all leaves are answered, clients cannot overwrite an accepted
		// value. Recovery is a separate explicit intent, with no new content.
		_,hasContent:=response["content"]
		if response["recover"]!=true || len(path)!=0 || hasContent {
			return nil,fmt.Errorf("recovery requires recover=true and no new continuation input")
		}
		value=nil
	} else {
		if response["recover"]==true {
			return nil,fmt.Errorf("recovery forbidden while a suspension awaits input")
		}
		if len(path)==0 && len(waiting)!=1 {
			return nil,fmt.Errorf("explicit suspension path required")
		}
		if len(path)>0 {
			valid:=false
			for _,candidate:=range waiting {if slices.Equal(path,candidate) {valid=true;break}}
			if !valid {return nil,fmt.Errorf("unknown or already answered suspension path")}
		}
	}
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
		out, err := s.Engine.ResumeAt(runctx, cfg, t.Revision, t.RunID, path, value)
		s.tasksMu.Lock()
		defer s.tasksMu.Unlock()
		updated, e := s.Store.TaskGet(id)
		if e != nil || updated.Status == "cancelled" {
			return
		}
		updated.RunID = out.RunID
		if out.Status == "suspended" {
			updated.Status = "input_required"
			updated.StatusMessage = "Additional input needed to resume pipeline"
			updated.Result = nil
			updated.Error = nil
			updated.InputRequests = continuationRequest(out.SuspensionPaths, "resume-"+storage.ID(), false)
		} else {
			// A rejected leaf value, grant/lease failure, or an incomplete
			// graph failure can leave the *original* checkpoint intact. Do not
			// permanently complete the task when the engine has deliberately
			// retained an operator-correctable continuation. Never redispatch
			// effects automatically: only a fresh authorized response may retry.
			var pending engine.Checkpoint
			pendingErr := fmt.Errorf("no preserved checkpoint")
			if err != nil {
				pendingErr = s.Store.GetSuspended(t.RunID, &pending)
			}
			if err != nil && pendingErr == nil {
				updated.Status = "input_required"
				updated.Result = nil
				updated.Error = nil
				paths := engine.PendingSuspensionPaths(&pending)
				if len(paths) > 0 {
					updated.StatusMessage = "Continuation rejected; checkpoint retained. Check external effects before retrying."
					key := responseKey
					if usingGrant || !slices.EqualFunc(waiting, paths, slices.Equal[[]string]) {
						key = "resume-" + storage.ID()
					}
					updated.InputRequests = continuationRequest(paths, key, false)
				} else {
					// A final answer was durably stored before downstream work
					// failed. Never solicit or accept a replacement answer.
					updated.StatusMessage = "Accepted answer retained; verify external effects before recovery."
					key := responseKey
					if usingGrant || len(waiting) != 0 {
						key = "resume-" + storage.ID()
					}
					updated.InputRequests = continuationRecoveryRequest(key)
				}
			} else {
				updated.Status = "completed"
				updated.StatusMessage = "Execution completed"
				updated.InputRequests = nil
				updated.Result = taskSafeResult(updated,out,err)
			}
		}
		s.finishTaskObservation(updated)
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
        t.Status = "cancelled"
        s.finishTaskObservation(t)
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
