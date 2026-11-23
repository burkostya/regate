package engine

import (
	"context"
	"fmt"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/expr"
	"github.com/burkostya/regate/internal/identity"
)

type toolCallMocksKey struct{}

// WithToolCallMocks attaches synthetic outputs to a management dry run.
func WithToolCallMocks(ctx context.Context, mocks map[string]any) context.Context {
	return context.WithValue(ctx, toolCallMocksKey{}, mocks)
}

func MaskPublishedResult(surface config.Surface, tool config.Tool, value any) any {
	if surface.OAuth != "" && !tool.AllowUnmasked {
		return expr.MaskPII(value)
	}
	return value
}

func authorizeComposedTool(ctx context.Context, cfg *config.Config, surface config.Surface, tool config.Tool) error {
	composed := false
	err := cfg.VisitPipelineNodes(tool.Pipeline, func(n config.Node) error {
		composed = composed || n.Kind == "tool.call"
		return nil
	})
	if err != nil || !composed {
		return err
	}
	if _, ok := identity.FromContext(ctx); !ok {
		return identity.ErrInvalidToken
	}
	if err := identity.RequireScopes(ctx, surface.RequiredScopes); err != nil {
		return err
	}
	return identity.RequireScopes(ctx, tool.RequiredScopes)
}

func (r *Runtime) callPublishedTool(ctx context.Context, cfg *config.Config, node config.Node, pipeline, surface, revision, parentRun string, input any, dry bool) (any, error) {
	published := cfg.Surfaces[surface]
	child, found := published.FindTool(node.Str("tool"))
	if node.Str("surface") != surface || !found || published.Upstream != "" {
		return nil, fmt.Errorf("tool.call target is unavailable on this surface")
	}
	effectful, suspendable := cfg.ToolTraits(surface, child)
	if suspendable || effectful && !node.Bool("allow_effects", false) {
		return nil, fmt.Errorf("tool.call target exceeds the node permissions")
	}
	args, ok := input.(map[string]any)
	if !ok || args == nil {
		return nil, fmt.Errorf("tool.call arguments must be an object")
	}
	contract := cfg.ToolContracts[surface][child.Name]
	if dry {
		if err := contract.Input.Validate(args); err != nil {
			return nil, fmt.Errorf("tool.call input: %w", err)
		}
		mocks, _ := ctx.Value(toolCallMocksKey{}).(map[string]any)
		key := pipeline + "." + node.Name
		value, ok := mocks[key]
		if !ok {
			return nil, fmt.Errorf("tool.call needs a dry-run mock for %s", key)
		}
		if err := contract.Output.Validate(value); err != nil {
			return nil, fmt.Errorf("tool.call mock: %w", err)
		}
		return MaskPublishedResult(published, child, value), nil
	}
	if err := identity.RequireScopes(ctx, child.RequiredScopes); err != nil {
		return nil, err
	}
	out, err := r.runTool(ctx, revision, cfg, surface, child.Name, args, false, derivedSubrunID(parentRun, node.Name, child.Name))
	if err != nil {
		return nil, fmt.Errorf("tool.call %s: %w", child.Name, err)
	}
	return MaskPublishedResult(published, child, out.Value), nil
}
