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
    "github.com/burkostya/regate/internal/storage"
)

type Arg struct {
	Name, Description string
	Required          bool
}

var AdminTools = map[string][]Arg{
	"regate_status":           {},
    "regate_audit": {},
	"regate_upstreams":        {},
	"regate_inspect":          {{"name", "Upstream name", false}},
	"regate_config_get":       {},
	"regate_draft_create":     {},
	"regate_draft_get":        {{"draft_id", "Draft identifier", true}},
	"regate_draft_patch":      {{"draft_id", "Draft identifier", true}, {"source", "Complete HCL source", true}, {"expected_source_digest", "Optional SHA-256 optimistic edit precondition", false}},
	"regate_validate":         {{"source", "HCL to validate", false}, {"draft_id", "Existing draft identifier", false}},
	"regate_diff":             {{"draft_id", "Draft identifier", true}},
	"regate_test":             {{"draft_id", "Draft to test", false}, {"surface", "Surface name", true}, {"tool", "Tool name", true}, {"arguments", "Input object", false}, {"expected_source_digest", "Reviewed draft SHA-256 digest", false}, {"expected_revision", "Reviewed active revision (without draft_id)", false}},
	"regate_simulate":         {{"draft_id", "Draft to simulate", false}, {"surface", "Surface name", true}, {"tool", "Tool name", true}, {"arguments", "Input object", false}, {"expected_source_digest", "Reviewed draft SHA-256 digest", false}, {"expected_revision", "Reviewed active revision (without draft_id)", false}},
	"regate_activate":         {{"draft_id", "Validated draft", true}, {"expected_source_digest", "Optional exact reviewed source SHA-256", false}},
	"regate_rollback":         {{"revision", "Revision to restore", true}},
	"regate_graph_get":        {{"pipeline", "Pipeline name", true}},
	"regate_graph_patch":      {{"draft_id", "Draft identifier", true}, {"pipeline", "Pipeline name", true}, {"source", "Replacement HCL pipeline block", true}, {"expected_source_digest", "Optional SHA-256 optimistic edit precondition", false}},
	"regate_attribute_patch":  {{"draft_id", "Draft identifier", true}, {"path", "Block path e.g. pipeline/customer_summary/node/fetch", true}, {"attribute", "Existing attribute name", true}, {"expression", "HCL2 expression source", true}, {"expected_source_digest", "Optional SHA-256 optimistic edit precondition", false}},
	"regate_attribute_add":    {{"draft_id", "Draft identifier", true}, {"path", "Block path e.g. pipeline/customer_summary/node/fetch", true}, {"attribute", "New attribute name", true}, {"expression", "HCL2 expression source", true}, {"expected_source_digest", "Optional SHA-256 optimistic edit precondition", false}},
	"regate_hook_inspect":     {{"name", "Hook name", true}},
	"regate_pipeline_trace":   {{"run_id", "Filter by run", false}},
	"regate_impact_analyze":   {{"draft_id", "Draft identifier", true}},
	"regate_upstream_refresh": {{"name", "Upstream name", true}},
	"regate_run":              {{"surface", "Surface", true}, {"tool", "Tool", true}, {"arguments", "Arguments", false}, {"expected_revision", "Reviewed active revision (optimistic precondition)", false}},
	"regate_resume":           {{"run_id", "Suspended run", true}, {"value", "Resume result", false}},
    "regate_task_resume_grant": {{"task_id","Suspended delegated task ID",true},{"value","Explicit continuation input",false}},
	"regate_revisions":        {},
	"regate_effects":          {},
    "regate_queue_key_usage": {},
    "regate_queue_rewrap": {},
    "regate_grant_key_usage": {},
    "regate_grant_rewrap": {},
    "regate_effect_compensate": {
        {"effect_id","Previously resolved-applied effect to compensate",true},
        {"contract","Named HCL compensation contract",true},
        {"intent","Exact original write argument object",true},
        {"actor","Operator identity (audited assertion)",true},
        {"reason","Explicit compensation approval reason (stored as digest)",true},
    },
    "regate_effect_readback": {
        {"effect_id","Unknown target-bound effect ID",true},
        {"adapter","Named HCL readback contract",true},
        {"intent","Exact original writer argument object (never persisted)",true},
    },
    "regate_effect_decide": {
        {"effect_id", "Unknown effect ID", true},
        {"actor", "Operator or service identity (audited statement)", true},
        {"verdict", "applied or absent", true},
        {"reason", "Reason for explicit operator decision; secrets are masked", true},
    },
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
			if arg.Name == "arguments" || arg.Name == "value" || arg.Name == "intent" {
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
// A missing precondition is accepted for compatibility with existing MCP
// automation. The visual editor always supplies the snapshot it reviewed.
func checkExpectedDraftDigest(args map[string]any,d storage.Draft) error {
    raw,present:=args["expected_source_digest"]
    if !present {return nil}
    value,ok:=raw.(string)
    if !ok || len(value)!=64 {return fmt.Errorf("expected_source_digest must be a SHA-256 hex string")}
    if value!=d.SourceDigest {return storage.ErrDraftSourceConflict}
    return nil
}
// A caller may pin a reviewed active revision. Calls without this optional
// precondition remain supported for existing management MCP clients.
func checkExpectedExecutionRevision(args map[string]any, current string) error {
    raw, present := args["expected_revision"]
    if !present { return nil }
    expected, ok := raw.(string)
    if !ok || expected == "" {
        return fmt.Errorf("expected_revision must be a non-empty revision identifier")
    }
    if expected != current {
        return fmt.Errorf("active revision changed; refresh before execution")
    }
    return nil
}
// An administrative effect cannot begin when its durable audit admission
// record is unavailable. The second record is best effort after the
// operation, but a failure must never be presented as a successful retry.
var auditedManagementMutations = map[string]bool{
    "regate_draft_create":true,
    "regate_draft_patch":true,
    "regate_activate":true,
    "regate_rollback":true,
    "regate_graph_patch":true,
    "regate_attribute_patch":true,
    "regate_attribute_add":true,
    "regate_queue_rewrap":true,
    "regate_grant_rewrap":true,
    "regate_effect_compensate":true,
    "regate_effect_readback":true,
    "regate_effect_decide":true,
    "regate_run":true,
    "regate_resume":true,
    "regate_task_resume_grant":true,
    "regate_upstream_refresh":true,
}
func (s *Service) Admin(ctx context.Context,name string,args map[string]any)(any,error) {
    if !auditedManagementMutations[name] {return s.adminOperation(ctx,name,args)}
    if err:=s.Store.AppendAudit("management",name,"started");err!=nil {
        return nil,fmt.Errorf("management audit admission unavailable: %w",err)
    }
    result,err:=s.adminOperation(ctx,name,args)
    outcome:="succeeded"
    if err!=nil {outcome="failed"}
    if auditErr:=s.Store.AppendAudit("management",name,outcome);auditErr!=nil {
        // A successful remote effect may already exist. Never imply rollback
        // or tell clients to retry an unknown outcome.
        return nil,fmt.Errorf("management action outcome audit unavailable; inspect state before retry: %w",auditErr)
    }
    return result,err
}
func (s *Service) adminOperation(ctx context.Context, name string, args map[string]any) (any, error) {
	cfg, rev := s.Current()
	switch name {
	case "regate_status":
		return s.Status(), nil
    case "regate_audit":
        return s.Store.AuditTail(100)
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
        id:=str(args,"draft_id")
        if _,present:=args["expected_source_digest"];present {
            d,e:=s.Store.DraftGet(id)
            if e!=nil {return nil,e}
            if e=checkExpectedDraftDigest(args,d);e!=nil {return nil,e}
            return s.Store.DraftUpdateIfSource(id,d.Source,source)
        }
        return s.Store.DraftUpdate(id, source)
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
        if _,present:=args["expected_source_digest"];present {
            d,e:=s.Store.DraftGet(str(args,"draft_id"))
            if e!=nil {return nil,e}
            if e=checkExpectedDraftDigest(args,d);e!=nil {return nil,e}
        }
		return s.ActivateIfDigest(str(args, "draft_id"),str(args,"expected_source_digest"))
	case "regate_rollback":
		return s.Rollback(str(args, "revision"))
	case "regate_revisions":
		return s.Store.History(), nil
	case "regate_effects":
		return s.Store.Effects(), nil
    case "regate_queue_key_usage":
        return s.Store.QueueKeyUsage(ctx)
    case "regate_queue_rewrap":
        return s.Store.RewrapQueuedBatch(ctx,32)
    case "regate_grant_key_usage":
        return s.Store.GrantKeyUsage(ctx)
    case "regate_grant_rewrap":
        return s.Store.RewrapGrantsBatch(ctx,32)
    case "regate_effect_compensate":
        return s.Engine.CompensateMCP(ctx,cfg,
            str(args,"effect_id"),str(args,"contract"),obj(args,"intent"),
            str(args,"actor"),str(args,"reason"))
    case "regate_effect_readback":
        return s.Engine.ReconcileMCP(ctx,cfg,
            str(args,"effect_id"),str(args,"adapter"),obj(args,"intent"))
    case "regate_effect_decide":
        id:=str(args,"effect_id")
        current,err:=s.Store.EffectGet(id)
        if err!=nil{return nil,err}
        if current.Status!="unknown" {return nil,storage.ErrEffectUnresolved}
        return s.Store.ResolveByOperator(id,str(args,"actor"),str(args,"verdict"),str(args,"reason"))
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
        if e=checkExpectedDraftDigest(args,d);e!=nil {return nil,e}
		source, e := config.ReplaceBlock(d.Source, "pipeline", str(args, "pipeline"), str(args, "source"))
		if e != nil {
			return nil, e
		}
		return s.Store.DraftUpdateIfSource(d.ID, d.Source, source)
	case "regate_attribute_patch", "regate_attribute_add":
		d, e := s.Store.DraftGet(str(args, "draft_id"))
		if e != nil {
			return nil, e
		}
        if e=checkExpectedDraftDigest(args,d);e!=nil {return nil,e}
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
		return s.Store.DraftUpdateIfSource(d.ID, d.Source, source)
	case "regate_pipeline_trace":
        return s.Store.TracesFiltered(storage.TraceQuery{RunID:str(args,"run_id")}),nil
	case "regate_upstream_refresh":
		c, e := s.Discover(ctx, str(args, "name"), true)
		return c, e
	case "regate_test", "regate_simulate", "regate_run":
        // Dry-run can use an explicitly reviewed draft. LIVE effects must
        // never run unactivated source, regardless of REST/MCP argument shape.
        if name == "regate_run" {
            if _, present := args["draft_id"]; present {
                return nil, fmt.Errorf("live execution requires an activated revision; draft_id is forbidden")
            }
        }
		selected := cfg
		id := rev.ID
        if raw, present := args["draft_id"]; present {
            did, ok := raw.(string)
            if !ok || did == "" {
                return nil, fmt.Errorf("draft_id must be a non-empty string")
            }
            if _, present := args["expected_revision"]; present {
                return nil, fmt.Errorf("expected_revision cannot be combined with draft_id")
            }
			d, e := s.Store.DraftGet(did)
			if e != nil {
				return nil, e
			}
            if e := checkExpectedDraftDigest(args, d); e != nil {
                return nil, e
            }
			selected, e = s.Validate(d.Source)
			if e != nil {
				return nil, e
			}
			id = "draft:" + did
        } else {
            if _, present := args["expected_source_digest"]; present {
                return nil, fmt.Errorf("expected_source_digest requires draft_id")
            }
            if e := checkExpectedExecutionRevision(args, rev.ID); e != nil {
                return nil, e
            }
		}
		dry := name != "regate_run"
		out, e := s.Engine.Run(ctx, id, selected, str(args, "surface"), str(args, "tool"), obj(args, "arguments"), dry)
		if e != nil {
			return out, e
		}
		return out, nil
	case "regate_task_resume_grant":
        return s.ResumeTaskWithGrant(ctx,str(args,"task_id"),args["value"])
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
