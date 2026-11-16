package engine

import (
    "context"

    "github.com/burkostya/regate/internal/storage"
)

type grantExecutionKey struct{}
type grantExecution struct {
    taskID string
    ownerDigest string
    generation int64
}

// WithGrantExecution carries only an immutable task/grant binding, never a
// credential. It is set solely by trusted server-side authorization after a
// verified refresh and durable CAS commit.
func WithGrantExecution(ctx context.Context,taskID,ownerDigest string,generation int64)context.Context {
    return context.WithValue(ctx,grantExecutionKey{},grantExecution{
        taskID:taskID,ownerDigest:ownerDigest,generation:generation,
    })
}
func HasGrantExecution(ctx context.Context)bool {
    value,ok:=ctx.Value(grantExecutionKey{}).(grantExecution)
    return ok && value.taskID!="" && value.ownerDigest!="" && value.generation>0
}
func (r *Runtime) CheckGrantExecution(ctx context.Context)error {
    value,ok:=ctx.Value(grantExecutionKey{}).(grantExecution)
    if !ok{return nil}
    if value.taskID=="" || value.ownerDigest=="" || value.generation<1 || r.Store==nil {
        return storage.ErrGrantUnavailable
    }
    return r.Store.RequireActiveTaskGrant(ctx,value.taskID,value.ownerDigest,value.generation)
}
