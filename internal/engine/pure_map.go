package engine

import (
	"context"
	"fmt"
	"sync"

	"github.com/burkostya/regate/internal/config"
)

// evaluatePureMap preflights the complete input and evaluates at most
// parallelism concurrent pure CEL expressions against the same immutable
// upstream node/port snapshot. Results and errors are collected by original
// item index, never completion order. No partial result escapes on failure.
// Effectful and suspendable pipeline-backed maps do not call this function.
func evaluatePureMap(ctx context.Context, plan *config.NodeIR, items []any, vars map[string]any, parallelism int) ([]any, error) {
	if plan == nil || plan.Expressions["mapping"] == nil {
		return nil, fmt.Errorf("map mapping expression unavailable")
	}
	if parallelism < 1 || parallelism > 16 {
		return nil, fmt.Errorf("invalid pure map parallelism")
	}
	for i, item := range items {
		if err := plan.InputSchema.Validate(item); err != nil {
			return nil, fmt.Errorf("map item %d failed schema validation", i)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	values := make([]any, len(items))
	evaluate := func(i int) (any, error) {
		local := map[string]any{
			"input": items[i],
			"nodes": vars["nodes"],
			"ports": vars["ports"],
			"port_present": vars["port_present"],
		}
		return plan.Expressions["mapping"].Eval(ctx, local)
	}
	if parallelism == 1 {
		for i := range items {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			value, err := evaluate(i)
			if err != nil {
				return nil, fmt.Errorf("map item %d: %w", i, err)
			}
			values[i] = value
		}
		return values, nil
	}

	// A bounded worker pool avoids one goroutine per item. Per-index writes
	// are disjoint; no worker mutates the shared compiled CEL program or vars.
	jobs := make(chan int)
	failures := make([]error, len(items))
	workers := parallelism
	if workers > len(items) {
		workers = len(items)
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				if err := ctx.Err(); err != nil {
					failures[i] = err
					continue
				}
				values[i], failures[i] = evaluate(i)
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
	return values, nil
}
