package server

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/engine"
)

func (s *Service) testPublishedContent(ctx context.Context, cfg *config.Config, revision string, args map[string]any) (any, error) {
	surface := str(args, "surface")
	pub, ok := cfg.Surfaces[surface]
	if !ok || pub.Upstream != "" {
		return nil, fmt.Errorf("resource and prompt tests require an assembled surface")
	}
	resource, hasResource := args["resource"]
	prompt, hasPrompt := args["prompt"]
	if _, tool := args["tool"]; tool || hasResource == hasPrompt ||
		(hasResource && str(args, "resource") == "") || (hasPrompt && str(args, "prompt") == "") {
		return nil, fmt.Errorf("exactly one of tool, resource or prompt is required")
	}
	var fixtures engine.ContentMocks
	if raw, present := args["mocks"]; present {
		mocks, ok := raw.(map[string]any)
		if !ok || mocks == nil {
			return nil, fmt.Errorf("content mocks must be an object")
		}
		for key, value := range mocks {
			object, ok := value.(map[string]any)
			if !ok || object == nil {
				return nil, fmt.Errorf("content mocks %s must be an object", key)
			}
			switch key {
			case "source":
				fixtures.Source = object
			case "hooks":
				fixtures.Hooks = object
			default:
				return nil, fmt.Errorf("content mocks accept source and hooks")
			}
		}
	}
	if hasResource {
		if _, present := args["arguments"]; present {
			return nil, fmt.Errorf("resource tests select the URI through the published resource")
		}
		for _, target := range pub.Resources {
			if target.Name == resource {
				return s.Engine.ReadPublishedResource(ctx, cfg, revision, surface, target, &fixtures)
			}
		}
		return nil, fmt.Errorf("unknown published resource %q", resource)
	}

	var arguments map[string]any
	if raw, present := args["arguments"]; present {
		arguments, ok = raw.(map[string]any)
		if !ok || arguments == nil {
			return nil, fmt.Errorf("prompt arguments must be an object")
		}
	}
	for _, target := range pub.Prompts {
		if target.Name == prompt {
			return s.Engine.GetPublishedPrompt(ctx, cfg, revision, surface, target, arguments, &fixtures)
		}
	}
	return nil, fmt.Errorf("unknown published prompt %q", prompt)
}

type contentImpactTarget struct {
	Surface string `json:"surface"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
}

type contentImpactStage struct {
	Name     string `json:"name"`
	Stage    string `json:"stage"`
	Action   string `json:"action"`
	Hook     string `json:"hook,omitempty"`
	Policy   string `json:"policy,omitempty"`
	External bool   `json:"external"`
}

type contentImpactView struct {
	Source string               `json:"source,omitempty"`
	Scopes []string             `json:"required_scopes,omitempty"`
	Stages []contentImpactStage `json:"stages"`
}

type contentImpactEntry struct {
	contentImpactTarget
	Before *contentImpactView `json:"before,omitempty"`
	After  *contentImpactView `json:"after,omitempty"`
}

type contentImpactSnapshot struct {
	view       contentImpactView
	references []any
}

func contentSnapshot(cfg *config.Config) map[contentImpactTarget]contentImpactSnapshot {
	result := map[contentImpactTarget]contentImpactSnapshot{}
	for surface, published := range cfg.Surfaces {
		add := func(kind, name, source string, scopes []string, definition any) {
			key := contentImpactTarget{surface, kind, name}
			view := contentImpactView{
				Source: source,
				Scopes: append(append([]string(nil), published.RequiredScopes...), scopes...),
				Stages: []contentImpactStage{},
			}
			refs := []any{definition, cfg.OAuth[published.OAuth]}
			if upstream, _, ok := strings.Cut(source, "."); ok {
				u := cfg.Upstreams[upstream]
				refs = append(refs, u, cfg.OAuth[u.OAuthExchange])
			}
			for _, i := range cfg.ContentInterceptors(surface, kind, name) {
				stage := contentImpactStage{Name: i.Name, Stage: i.Stage, Hook: i.Hook, Policy: i.Policy, Action: "hook"}
				switch {
				case i.Policy != "":
					stage.Action = "policy"
					policy := cfg.Policies[i.Policy]
					stage.Hook = policy.DecisionHook
					refs = append(refs, policy)
				case i.Check != "":
					stage.Action = "check"
				case i.Transform != "":
					stage.Action = "transform"
				}
				h := cfg.Hooks[stage.Hook]
				stage.External = h.Transport == "http" || h.Transport == "mcp"
				// The source expressions and destination definitions affect the
				// comparison. The impact summary only includes their names.
				refs = append(refs, i.Order, i.Input, i.Check, i.Transform, h)
				if h.Transport == "http" {
					refs = append(refs, cfg.OAuth[h.OAuthExchange])
				} else if h.Transport == "mcp" {
					u := cfg.Upstreams[h.Upstream]
					refs = append(refs, u, cfg.OAuth[u.OAuthExchange])
				}
				view.Stages = append(view.Stages, stage)
			}
			result[key] = contentImpactSnapshot{view, refs}
		}
		for _, r := range published.Resources {
			r.Contract = config.ToolContract{}
			add("resource", r.Name, r.Source, r.RequiredScopes, r)
		}
		for _, p := range published.Prompts {
			p.Contract = config.ToolContract{}
			add("prompt", p.Name, p.Source, p.RequiredScopes, p)
		}
	}
	return result
}

func contentImpact(a, b *config.Config) []contentImpactEntry {
	before, after := contentSnapshot(a), contentSnapshot(b)
	changes := []contentImpactEntry{}
	for key, prior := range before {
		if next, ok := after[key]; !ok {
			changes = append(changes, contentImpactEntry{contentImpactTarget: key, Before: &prior.view})
		} else if !reflect.DeepEqual(prior, next) {
			changes = append(changes, contentImpactEntry{key, &prior.view, &next.view})
		}
	}
	for key, next := range after {
		if _, ok := before[key]; !ok {
			changes = append(changes, contentImpactEntry{contentImpactTarget: key, After: &next.view})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		x, y := changes[i], changes[j]
		if x.Surface != y.Surface {
			return x.Surface < y.Surface
		}
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		return x.Name < y.Name
	})
	return changes
}
