package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/engine"
)

type Arg struct {
	Name, Description string
	Required          bool
}

var AdminTools = map[string][]Arg{
	"regate_status":           {},
	"regate_upstreams":        {},
	"regate_inspect":          {{"name", "Upstream name", false}},
	"regate_config_get":       {},
	"regate_draft_create":     {},
	"regate_draft_get":        {{"draft_id", "Draft identifier", true}},
	"regate_draft_patch":      {{"draft_id", "Draft identifier", true}, {"source", "Complete HCL source", true}},
	"regate_validate":         {{"source", "HCL to validate", false}, {"draft_id", "Existing draft identifier", false}},
	"regate_diff":             {{"draft_id", "Draft identifier", true}},
	"regate_test":             {{"draft_id", "Draft to test", false}, {"surface", "Surface name", true}, {"tool", "Tool name", true}, {"arguments", "Input object", false}},
	"regate_simulate":         {{"draft_id", "Draft to simulate", false}, {"surface", "Surface name", true}, {"tool", "Tool name", true}, {"arguments", "Input object", false}},
	"regate_activate":         {{"draft_id", "Validated draft", true}},
	"regate_rollback":         {{"revision", "Revision to restore", true}},
	"regate_graph_get":        {{"pipeline", "Pipeline name", true}},
	"regate_graph_patch":      {{"draft_id", "Draft identifier", true}, {"pipeline", "Pipeline name", true}, {"source", "Replacement HCL pipeline block", true}},
	"regate_attribute_patch":  {{"draft_id", "Draft identifier", true}, {"path", "Block path e.g. pipeline/customer_summary/node/fetch", true}, {"attribute", "Existing attribute name", true}, {"expression", "HCL2 expression source", true}},
	"regate_attribute_add":    {{"draft_id", "Draft identifier", true}, {"path", "Block path e.g. pipeline/customer_summary/node/fetch", true}, {"attribute", "New attribute name", true}, {"expression", "HCL2 expression source", true}},
	"regate_hook_inspect":     {{"name", "Hook name", true}},
	"regate_pipeline_trace":   {{"run_id", "Filter by run", false}},
	"regate_impact_analyze":   {{"draft_id", "Draft identifier", true}},
	"regate_upstream_refresh": {{"name", "Upstream name", true}},
	"regate_run":              {{"surface", "Surface", true}, {"tool", "Tool", true}, {"arguments", "Arguments", false}},
	"regate_resume":           {{"run_id", "Suspended run", true}, {"value", "Resume result", false}},
	"regate_revisions":        {},
	"regate_effects":          {},
}

func AdminDefs() []map[string]any {
	out := []map[string]any{}
	names := make([]string, 0, len(AdminTools))
	for n := range AdminTools {
		names = append(names, n)
	}
	sortStrings(names)
	for _, name := range names {
		props := map[string]any{}
		required := []string{}
		for _, arg := range AdminTools[name] {
			typ := "string"
			if arg.Name == "arguments" || arg.Name == "value" {
				typ = "object"
			}
			props[arg.Name] = map[string]any{"type": typ, "description": arg.Description}
			if arg.Required {
				required = append(required, arg.Name)
			}
		}
		out = append(out, map[string]any{"name": name, "description": "Regate management: " + strings.ReplaceAll(strings.TrimPrefix(name, "regate_"), "_", " "), "inputSchema": map[string]any{"type": "object", "properties": props, "required": required}})
	}
	return out
}
func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
func str(a map[string]any, key string) string { s, _ := a[key].(string); return s }
func obj(a map[string]any, key string) map[string]any {
	x, _ := a[key].(map[string]any)
	if x == nil {
		x = map[string]any{}
	}
	return x
}
func (s *Service) Admin(ctx context.Context, name string, args map[string]any) (any, error) {
	cfg, rev := s.Current()
	switch name {
	case "regate_status":
		return s.Status(), nil
	case "regate_upstreams":
		return cfg.Upstreams, nil
	case "regate_inspect":
		return s.Inspect(str(args, "name")), nil
	case "regate_config_get":
		return map[string]any{"revision": rev.ID, "source": cfg.Source}, nil
	case "regate_draft_create":
		return s.Store.DraftCreate()
	case "regate_draft_get":
		return s.Store.DraftGet(str(args, "draft_id"))
	case "regate_draft_patch":
        source := str(args, "source")
        // A draft is editable HCL source, not an activated program. Allow
        // unknown blocks/expressions so forward-compatible opaque fragments
        // survive editing; activation still requires full strict compilation.
        if _, e := config.ParseForEdit(source); e != nil { return nil, e }
        return s.Store.DraftUpdate(str(args, "draft_id"), source)
	case "regate_validate":
		source := str(args, "source")
		if source == "" {
			d, e := s.Store.DraftGet(str(args, "draft_id"))
			if e != nil {
				return nil, e
			}
			source = d.Source
		}
		c, e := s.Validate(source)
		if e != nil {
			return map[string]any{"valid": false, "error": e.Error()}, nil
		}
		return map[string]any{"valid": true, "pipelines": len(c.Pipelines), "surfaces": len(c.Surfaces), "hooks": len(c.Hooks)}, nil
	case "regate_diff", "regate_impact_analyze":
		d, e := s.Store.DraftGet(str(args, "draft_id"))
		if e != nil {
			return nil, e
		}
		previous, e := config.Decode(cfg.Source)
		if e != nil {
			return nil, e
		}
		next, e := config.Decode(d.Source)
		if e != nil {
			return nil, e
		}
		return impact(previous, next, d.Base, rev.ID), nil
	case "regate_activate":
		return s.Activate(str(args, "draft_id"))
	case "regate_rollback":
		return s.Rollback(str(args, "revision"))
	case "regate_revisions":
		return s.Store.History(), nil
	case "regate_effects":
		return s.Store.Effects(), nil
	case "regate_hook_inspect":
		h, ok := cfg.Hooks[str(args, "name")]
		if !ok {
			return nil, fmt.Errorf("unknown hook")
		}
		return h, nil
	case "regate_graph_get":
		p, ok := cfg.Pipelines[str(args, "pipeline")]
		if !ok {
			return nil, fmt.Errorf("unknown pipeline")
		}
		return p, nil
	case "regate_graph_patch":
		d, e := s.Store.DraftGet(str(args, "draft_id"))
		if e != nil {
			return nil, e
		}
		source, e := config.ReplaceBlock(d.Source, "pipeline", str(args, "pipeline"), str(args, "source"))
		if e != nil {
			return nil, e
		}
		return s.Store.DraftUpdate(d.ID, source)
	case "regate_attribute_patch", "regate_attribute_add":
		d, e := s.Store.DraftGet(str(args, "draft_id"))
		if e != nil {
			return nil, e
		}
		parts := strings.Split(str(args, "path"), "/")
		if len(parts)%2 != 0 || len(parts) == 0 {
			return nil, fmt.Errorf("block path must contain type/label pairs")
		}
		selectors := make([]config.BlockSelector, 0, len(parts)/2)
		for i := 0; i < len(parts); i += 2 {
			if parts[i] == "" || parts[i+1] == "" {
				return nil, fmt.Errorf("empty block path component")
			}
			selectors = append(selectors, config.BlockSelector{Type: parts[i], Label: parts[i+1]})
		}
		var source string
		if name == "regate_attribute_add" {
			source, e = config.AddAttribute(d.Source, selectors, str(args, "attribute"), str(args, "expression"))
		} else {
			source, e = config.SetAttribute(d.Source, selectors, str(args, "attribute"), str(args, "expression"))
		}
		if e != nil {
			return nil, e
		}
		return s.Store.DraftUpdate(d.ID, source)
	case "regate_pipeline_trace":
		traces := s.Store.Traces()
		id := str(args, "run_id")
		if id == "" {
			return traces, nil
		}
		filtered := []any{}
		for _, t := range traces {
			if t.RunID == id {
				filtered = append(filtered, t)
			}
		}
		return filtered, nil
	case "regate_upstream_refresh":
		c, e := s.Discover(ctx, str(args, "name"), true)
		return c, e
	case "regate_test", "regate_simulate", "regate_run":
		selected := cfg
		id := rev.ID
		if did := str(args, "draft_id"); did != "" {
			d, e := s.Store.DraftGet(did)
			if e != nil {
				return nil, e
			}
			selected, e = s.Validate(d.Source)
			if e != nil {
				return nil, e
			}
			id = "draft:" + did
		}
		dry := name != "regate_run"
		out, e := s.Engine.Run(ctx, id, selected, str(args, "surface"), str(args, "tool"), obj(args, "arguments"), dry)
		if e != nil {
			return out, e
		}
		return out, nil
	case "regate_resume":
		// Resume against its original immutable revision rather than today's config.
		var cp engine.Checkpoint
		if e := s.Store.GetSuspended(str(args, "run_id"), &cp); e != nil {
			return nil, e
		}
		source, e := s.Store.Source(cp.Revision)
		if e != nil {
			return nil, e
		}
		old, e := s.Validate(source)
		if e != nil {
			return nil, e
		}
		return s.Engine.Resume(ctx, old, cp.Revision, cp.RunID, args["value"])
	}
	return nil, fmt.Errorf("unknown management tool %s", name)
}
func impact(a, b *config.Config, base, current string) map[string]any {
	changed := []string{}
	details := map[string]any{}
	av, _ := json.Marshal(a)
	bv, _ := json.Marshal(b)
	if string(av) != string(bv) {
		changed = append(changed, "configuration")
	}
	for name, p := range b.Pipelines {
		old, ok := a.Pipelines[name]
		o, _ := json.Marshal(old)
		n, _ := json.Marshal(p)
		if !ok || string(o) != string(n) {
			changed = append(changed, "pipeline:"+name)
		}
	}
	for name := range a.Pipelines {
		if _, ok := b.Pipelines[name]; !ok {
			changed = append(changed, "removed-pipeline:"+name)
		}
	}
	for name, s := range b.Surfaces {
		old, ok := a.Surfaces[name]
		o, _ := json.Marshal(old)
		n, _ := json.Marshal(s)
		if !ok || string(o) != string(n) {
			changed = append(changed, "surface:"+name)
		}
	}
	for name := range a.Surfaces {
		if _, ok := b.Surfaces[name]; !ok {
			changed = append(changed, "removed-surface:"+name)
		}
	}
	details["base_revision"] = base
	details["current_revision"] = current
	details["conflict"] = base != current
	details["changes"] = changed
	semantic, err := config.SemanticDiff(a, b)
	if err == nil {
		details["semantic_changes"] = semantic
	}
	details["warning"] = "Impact is structural only: upstream schemas and runtime permissions need live verification"
	return details
}
func (s *Service) Skills() []map[string]any {
	results := []map[string]any{}
	for _, name := range []string{"regate-config", "regate-authoring"} {
		entry, e := s.skillEntry(name)
		if e == nil {
			results = append(results, entry)
		}
	}
	return results
}
func (s *Service) Skill(uri string) (map[string]any, error) {
	name, err := skillNameFromURI(uri)
	if err != nil {
		return nil, err
	}
	entry, err := s.skillEntry(name)
	if err != nil {
		return nil, err
	}
	return map[string]any{"skill": entry, "ttlMs": 300000, "cacheScope": "private"}, nil
}
func (s *Service) skillEntry(name string) (map[string]any, error) {
	b, e := os.ReadFile(filepath.Join(skillsDir(), name, "SKILL.md"))
	if e != nil {
		return nil, e
	}
	description := ""
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "description:") {
			description = strings.TrimSpace(strings.TrimPrefix(line, "description:"))
			break
		}
	}
	if description == "" {
		return nil, fmt.Errorf("invalid skill frontmatter for %s", name)
	}
	sum := sha256.Sum256(b)
	uri := "skill://" + name + "/SKILL.md"
	return map[string]any{"uri": uri, "frontmatter": map[string]any{"name": name, "description": description}, "resources": []any{map[string]any{"uri": uri, "digest": fmt.Sprintf("sha256:%x", sum[:]), "size": len(b)}}}, nil
}
func skillNameFromURI(uri string) (string, error) {
	for _, name := range []string{"regate-config", "regate-authoring"} {
		if uri == "skill://"+name+"/SKILL.md" {
			return name, nil
		}
	}
	return "", fmt.Errorf("skill URI not found")
}
func (s *Service) SkillContent(uri string) (string, error) {
	name, e := skillNameFromURI(uri)
	if e != nil {
		return "", e
	}
	b, e := os.ReadFile(filepath.Join(skillsDir(), name, "SKILL.md"))
	return string(b), e
}
func timeoutContext(ctx context.Context, ms int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
}

// skillsDir is explicit in deployed installations, and defaults to the
// repository's skill directory when running the development tests.
func skillsDir() string {
	if v := os.Getenv("REGATE_SKILLS_DIR"); v != "" {
		return v
	}
	if st, e := os.Stat("skills"); e == nil && st.IsDir() {
		return "skills"
	}
	return "../../skills"
}
