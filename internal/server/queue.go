package server

import (
    "context"
    "database/sql"
    "errors"
    "time"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/engine"
    "github.com/burkostya/regate/internal/storage"
)

// The dispatcher exists only after the SQLite execution lease is acquired.
// A periodic wake is needed to recover previously queued work after restart;
// task completion also nudges it immediately to release available slots.
func (s *Service) startQueueDispatcher() {
    s.queueOnce.Do(func(){
        s.queueWake=make(chan struct{},1)
        go s.queueDispatchLoop()
    })
    s.signalQueue()
}
func (s *Service) signalQueue() {
    if s.queueWake==nil{return}
    select {
    case s.queueWake<-struct{}{}:
    default:
    }
}

func (s *Service) queueDispatchLoop() {
    ticker:=time.NewTicker(200*time.Millisecond)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
        case <-s.queueWake:
        }
        // As soon as leadership is released or the store is closed, queued
        // work must never be dispatched by this process.
        ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second)
        err:=s.Store.RequireExecutionLeader(ctx)
        cancel()
        if err!=nil{return}

        cfg,_:=s.Current()
        for {
            ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second)
            task,args,err:=s.Store.ClaimNextQueued(ctx,cfg.MaxActiveTasks)
            cancel()
            if errors.Is(err,storage.ErrTaskQueueExpired){continue}
            if errors.Is(err,storage.ErrTaskCapacity) || errors.Is(err,sql.ErrNoRows) {
                break
            }
            if err!=nil {
                // Malformed/corrupt or undecryptable queued input remains
                // encrypted and intact for operator inspection. Do not skip
                // over it or create an unbounded retry loop.
                break
            }
            source,err:=s.Store.Source(task.Revision)
            if err!=nil {
                s.failClaimedQueuedTask(task.TaskID)
                continue
            }
            original,err:=config.Decode(source)
            if err!=nil {
                s.failClaimedQueuedTask(task.TaskID)
                continue
            }
            published,valid:=original.Surfaces[task.Surface]
            if !valid || published.OAuth!="" || task.Delegation!=nil ||
                len(task.RequiredScopes)>0 || task.RedactPII {
                s.failClaimedQueuedTask(task.TaskID)
                continue
            }
            toolFound:=false
            for _,tool:=range published.Tools {
                if tool.Name==task.Tool {
                    toolFound=true
                    if engine.ToolNeedsDelegation(original,task.Surface,tool) {
                        toolFound=false
                    }
                    break
                }
            }
            if !toolFound {
                s.failClaimedQueuedTask(task.TaskID)
                continue
            }
            s.executeClaimedQueuedTask(task,args,original)
        }
    }
}

func (s *Service) failClaimedQueuedTask(id string) {
    s.tasksMu.Lock()
    defer s.tasksMu.Unlock()
    task,err:=s.Store.TaskGet(id)
    if err!=nil || task.Status!="working"{return}
    task.Status="failed"
    task.StatusMessage="Queued task could not validate its original revision"
    task.Error=map[string]any{
        "code":-32000,
        "message":"Queued execution not available",
    }
    _=s.Store.TaskPut(task)
    s.signalQueue()
}

// Claimed queue entries are never restarted without reconciliation. A worker
// executes its original immutable HCL revision, not the current active one.
// The context carries only task/owner identity; authorization at each
// outbound effect checks SQLite state, avoiding post-cancel dispatch.
func (s *Service) executeClaimedQueuedTask(task storage.TaskRecord,
    args map[string]any,cfg *config.Config) {
    ctx,cancel:=context.WithTimeout(context.Background(),90*time.Second)
    ctx=engine.WithQueuedTaskExecution(ctx,task.TaskID,task.OwnerDigest)
    running.Store(task.TaskID,context.CancelFunc(cancel))
    go func() {
        defer cancel()
        defer running.Delete(task.TaskID)
        defer s.signalQueue()
        out,err:=s.Engine.Run(ctx,task.Revision,cfg,task.Surface,task.Tool,args,false)
        s.tasksMu.Lock()
        defer s.tasksMu.Unlock()
        current,e:=s.Store.TaskGet(task.TaskID)
        if e!=nil || current.Status!="working" {return}
        current.RunID=out.RunID
        if out.Status=="suspended" {
            current.Status="input_required"
            current.StatusMessage="Input required to continue queued pipeline"
            current.InputRequests=map[string]any{
                "resume":map[string]any{
                    "method":"elicitation/create",
                    "params":map[string]any{
                        "mode":"form",
                        "message":"Provide continuation data",
                        "requestedSchema":map[string]any{
                            "type":"object","additionalProperties":true,
                        },
                    },
                },
            }
        } else {
            current.Status="completed"
            current.StatusMessage="Execution completed"
            current.Result=taskSafeResult(current,out,err)
        }
        _=s.Store.TaskPut(current)
    }()
}
