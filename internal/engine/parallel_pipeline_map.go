package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/storage"
)

// evaluatePurePipelineMap executes only compiled, transitive-pure called
// pipelines. The parent preflights the full immutable argument array; bounded
// workers never suspend, dispatch external writes or create checkpoints.
// Each child has a separate derived subrun identity and timeout. Results are
// validated and returned in original input order, with stable first-error
// selection and no partially visible batch result.
func (r *Runtime) evaluatePurePipelineMap(ctx context.Context, cfg *config.Config, child config.Pipeline,
	plan *config.NodeIR, items []any, parallelism int, runID, surface, toolName, revision string, dry bool) ([]any, error) {
	if parallelism < 2 || parallelism > 16 || plan.MapItemSchema == nil {
		return nil, fmt.Errorf("invalid pure pipeline map contract")
	}
    if inPureMapWorker(ctx) {
        return nil, fmt.Errorf("nested pure pipeline map fanout must be serialized")
    }
	inputContract := cfg.Plans[child.Name].Nodes[child.Entry].InputSchema
	for i, item := range items {
		args, ok := item.(map[string]any)
		if !ok || args == nil || plan.InputSchema.Validate(item) != nil ||
			inputContract.Validate(item) != nil {
			return nil, fmt.Errorf("map item %d failed pure child input validation", i)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	results := make([]any, len(items))
	failures := make([]error, len(items))
	// A single terminal returns its value; multiple typed terminals
	// become a sparse named object for each mapped item.
	outputTerminals := 0
	for _, n := range child.Nodes {
		if n.Kind == "output" { outputTerminals++ }
	}
	multiOutput := outputTerminals > 1
	evaluate := func(workerCtx context.Context, i int) (any, error) {
		subrunID := mappedIterationRunID(runID, plan.Name, child.Name, i)
		childCtx, cancel := context.WithTimeout(workerCtx, time.Duration(child.TimeoutMS)*time.Millisecond)
		defer cancel()
		childCtx = withEffectScope(childCtx, subrunID)
		if !dry && r.Store != nil {
			r.Store.WriteTrace(storage.Trace{
				RunID: runID, Revision: revision, Surface: surface, Tool: toolName,
				Node: fmt.Sprintf("%s[%d]", plan.Name, i), Status: "subrun",
				Message: "nested run " + subrunID,
			})
		}
		nested, err := r.execute(childCtx, revision, cfg, child, subrunID,
			surface, toolName, items[i].(map[string]any), dry, nil)
		if err != nil {
			return nil, fmt.Errorf("child pipeline failed: %w", err)
		}
		if nested.Status == "suspended" {
			return nil, fmt.Errorf("pure child unexpectedly suspended")
		}
		if !nested.OutputProduced {
			return nil, fmt.Errorf("child pipeline produced no output")
		}
		output := nested.Value
		if multiOutput {
			fields := make(map[string]any, len(nested.NamedOutputs))
			for name, value := range nested.NamedOutputs {
				fields[name] = value
			}
			output = fields
		}
		if err := plan.MapItemSchema.Validate(output); err != nil {
			return nil, fmt.Errorf("typed child output contract failed: %w", err)
		}
		return output, nil
	}
	jobs := make(chan int)
	workers := parallelism
	if workers > len(items) {
		workers = len(items)
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
            workerCtx := withPureMapWorker(ctx)
			for i := range jobs {
				if err := workerCtx.Err(); err != nil {
					failures[i] = err
					continue
				}
				results[i], failures[i] = evaluate(workerCtx, i)
			}
		}()
	}
enqueue:
	for i := range items {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break enqueue
		}
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i, err := range failures {
		if err != nil {
			return nil, fmt.Errorf("map item %d: %w", i, err)
		}
	}
	return results, nil
}
