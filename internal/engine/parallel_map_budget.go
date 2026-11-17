package engine

import "context"

// A declared parallelism limit applies at the outermost active pure-map
// boundary. Without this propagated marker, transitive pure pipeline calls
// can spawn up to 16 workers per level (16^8 is possible within the allowed
// call depth), even though each local pool is individually bounded.
//
// Nested maps run in their caller's worker goroutine. This preserves their
// configured item order, validation and error contracts without multiplying
// the number of concurrent child evaluations. Independent top-level maps
// may still run in parallel under the parent pipeline's own concurrency.
type pureMapWorkerKey struct{}

func inPureMapWorker(ctx context.Context) bool {
    active, _ := ctx.Value(pureMapWorkerKey{}).(bool)
    return active
}

func withPureMapWorker(ctx context.Context) context.Context {
    if inPureMapWorker(ctx) { return ctx }
    return context.WithValue(ctx, pureMapWorkerKey{}, true)
}

func effectivePureMapParallelism(ctx context.Context, declared int) int {
    if declared > 1 && inPureMapWorker(ctx) {return 1}
    return declared
}
