package engine

import (
    "context"

    "github.com/burkostya/regate/internal/storage"
)

type queuedTaskExecutionKey struct{}
type queuedTaskExecution struct {
    taskID string
    ownerDigest string
}

// Only trusted Regate task workers set this context. It never contains the
// queued input or a bearer token. The SQLite task status is checked at every
// potentially external operation before dispatch.
func WithQueuedTaskExecution(ctx context.Context,id,owner string)context.Context {
    return context.WithValue(ctx,queuedTaskExecutionKey{},queuedTaskExecution{
        taskID:id,ownerDigest:owner,
    })
}
func (r *Runtime) CheckQueuedTaskExecution(ctx context.Context)error {
    binding,ok:=ctx.Value(queuedTaskExecutionKey{}).(queuedTaskExecution)
    if !ok{return nil}
    if binding.taskID=="" || binding.ownerDigest=="" || r.Store==nil {
        return storage.ErrTaskTransition
    }
    return r.Store.RequireWorkingTask(ctx,binding.taskID,binding.ownerDigest)
}
