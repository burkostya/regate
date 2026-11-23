package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/storage"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

var errDerivedToolNameConflict = errors.New("tool or generated pipeline name already exists")
var derivedToolName = regexp.MustCompile("^[A-Za-z0-9_.-]{1,128}$")

func surfaceInsertion(source, name string) (int, error) {
	blocks, err := config.ParseForEdit(source)
	if err != nil {
		return 0, err
	}
	for _, block := range blocks {
		if block.Type == "surface" && len(block.Labels) == 1 && block.Labels[0] == name {
			brace := block.End - 1
			line := strings.LastIndexByte(source[:brace], '\n') + 1
			if strings.TrimSpace(source[line:brace]) == "" {
				return line, nil
			}
			return brace, nil
		}
	}
	return 0, fmt.Errorf("surface is unavailable in the active source")
}

func (s *Service) createCallChainDraft(ctx context.Context, cfg *config.Config, revision string, args map[string]any) (storage.Draft, error) {
	expected := str(args, "expected_revision")
	if expected == "" {
		return storage.Draft{}, fmt.Errorf("expected_revision is required")
	}
	if expected != revision {
		return storage.Draft{}, storage.ErrDraftSourceConflict
	}
	name := str(args, "tool_name")
	if !derivedToolName.MatchString(name) {
		return storage.Draft{}, fmt.Errorf("tool_name must be 1–128 characters using letters, digits, underscores, hyphens or periods")
	}
	surfaceName := str(args, "surface")
	surface := cfg.Surfaces[surfaceName]
	pipeline := "derive." + surfaceName + "." + name
	if _, exists := surface.FindTool(name); exists {
		return storage.Draft{}, errDerivedToolNameConflict
	}
	if _, exists := cfg.Pipelines[pipeline]; exists {
		return storage.Draft{}, errDerivedToolNameConflict
	}
	chainID := str(args, "chain_id")
	if chainID == "" {
		return storage.Draft{}, fmt.Errorf("chain_id is required")
	}
	search, err := s.findCallChains(ctx, cfg, surfaceName, revision, "", chainID)
	if err != nil {
		return storage.Draft{}, err
	}
	if len(search.Chains) == 0 {
		return storage.Draft{}, fmt.Errorf("chain is no longer available on this revision")
	}
	chain := search.Chains[0]
	if chain.DraftUnavailable != "" {
		return storage.Draft{}, errors.New(chain.DraftUnavailable)
	}
	source, err := buildCallChainSource(cfg.Source, surface, name, pipeline, chain.Steps)
	if err != nil {
		return storage.Draft{}, err
	}
	if _, err := s.Validate(source); err != nil {
		return storage.Draft{}, err
	}
	return s.Store.DraftCreateFromRevision(revision, source)
}

func buildCallChainSource(source string, surface config.Surface, name, pipeline string, steps []chainStep) (string, error) {
	at, err := surfaceInsertion(source, surface.Name)
	if err != nil {
		return "", err
	}
	toolFile, pipelineFile := hclwrite.NewEmptyFile(), hclwrite.NewEmptyFile()
	toolBody := toolFile.Body().AppendNewBlock("tool", []string{name}).Body()
	toolBody.SetAttributeValue("pipeline", cty.StringVal(pipeline))
	pipelineBody := pipelineFile.Body().AppendNewBlock("pipeline", []string{pipeline}).Body()
	pipelineBody.SetAttributeValue("entry", cty.StringVal("step_1"))
	pipelineBody.SetAttributeValue("timeout_ms", cty.NumberIntVal(publicToolTimeout.Milliseconds()))
	pipelineBody.SetAttributeValue("concurrency", cty.NumberIntVal(1))

	properties := map[string]any{}
	definitions := map[string]any{}
	definitionFor := map[string]string{}
	var required []string
	scopes := map[string]cty.Value{}
	restricted, readOnly := false, true
	for i, step := range steps {
		id := fmt.Sprintf("step_%d", i+1)
		child, _ := surface.FindTool(step.Tool)
		schema := maps.Clone(child.InputSchema)
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		ref, exists := definitionFor[child.Name]
		if !exists {
			ref = "#/$defs/" + id
			// Give an embedded source schema its own resource root, so refs
			// such as #/$defs/item still resolve inside the source schema.
			if _, exists := schema["$id"]; !exists {
				schema["$id"] = "https://regate.invalid/derive/" + id + ".json"
			}
			definitions[id] = schema
			definitionFor[child.Name] = ref
		}
		properties[id] = map[string]any{"$ref": ref}
		required = append(required, id)
		for _, scope := range child.RequiredScopes {
			scopes[scope] = cty.StringVal(scope)
		}
		restricted = restricted || child.DataClassification == "restricted"
		readOnly = readOnly && !step.AllowEffects

		pipelineBody.AppendNewline()
		node := pipelineBody.AppendNewBlock("node", []string{id}).Body()
		node.SetAttributeValue("kind", cty.StringVal("tool.call"))
		node.SetAttributeValue("surface", cty.StringVal(surface.Name))
		node.SetAttributeValue("tool", cty.StringVal(child.Name))
		node.SetAttributeValue("arguments", cty.StringVal("input."+id))
		if step.AllowEffects {
			node.SetAttributeValue("allow_effects", cty.True)
		}
	}
	toolBody.SetAttributeValue("read_only", cty.BoolVal(readOnly))
	if restricted {
		toolBody.SetAttributeValue("data_classification", cty.StringVal("restricted"))
	}
	if len(scopes) != 0 {
		values := make([]cty.Value, 0, len(scopes))
		for _, key := range slices.Sorted(maps.Keys(scopes)) {
			values = append(values, scopes[key])
		}
		toolBody.SetAttributeValue("required_scopes", cty.ListVal(values))
	}
	schema, err := json.Marshal(map[string]any{
		"type": "object", "properties": properties, "$defs": definitions,
		"required": required, "additionalProperties": false,
	})
	if err != nil {
		return "", err
	}
	schemaType, err := ctyjson.ImpliedType(schema)
	if err != nil {
		return "", err
	}
	attribute, err := ctyjson.Unmarshal(schema, schemaType)
	if err != nil {
		return "", err
	}
	toolBody.SetAttributeValue("input_schema", attribute)
	pipelineBody.AppendNewline()
	output := pipelineBody.AppendNewBlock("node", []string{"result"}).Body()
	output.SetAttributeValue("kind", cty.StringVal("output"))
	output.SetAttributeValue("value", cty.StringVal("nodes."+required[len(required)-1]+".value"))
	pipelineBody.AppendNewline()
	successors := append(slices.Clone(required[1:]), "result")
	for i, successor := range successors {
		edge := pipelineBody.AppendNewBlock("edge", nil).Body()
		edge.SetAttributeValue("from", cty.StringVal(required[i]))
		edge.SetAttributeValue("to", cty.StringVal(successor))
	}
	insertion := "  " + strings.ReplaceAll(strings.TrimSuffix(string(toolFile.Bytes()), "\n"), "\n", "\n  ")
	return source[:at] + "\n" + insertion + "\n" + source[at:] + "\n\n" + string(pipelineFile.Bytes()), nil
}

type toolCallImpact struct {
	Surface        string   `json:"surface"`
	Tool           string   `json:"tool"`
	Node           string   `json:"node"`
	Target         string   `json:"target"`
	AllowEffects   bool     `json:"allow_effects"`
	RequiredScopes []string `json:"required_scopes"`
}

func composedToolImpact(cfg *config.Config) []toolCallImpact {
	out := []toolCallImpact{}
	for _, name := range slices.Sorted(maps.Keys(cfg.Surfaces)) {
		surface := cfg.Surfaces[name]
		for _, tool := range surface.Tools {
			start := len(out)
			_ = cfg.VisitPipelineNodes(tool.Pipeline, func(node config.Node) error {
				if node.Kind != "tool.call" {
					return nil
				}
				child, _ := surface.FindTool(node.Str("tool"))
				scopes := append(slices.Clone(surface.RequiredScopes), child.RequiredScopes...)
				slices.Sort(scopes)
				out = append(out, toolCallImpact{
					Surface: name, Tool: tool.Name, Node: node.Name, Target: child.Name,
					AllowEffects: node.Bool("allow_effects", false), RequiredScopes: slices.Compact(scopes),
				})
				return nil
			})
			slices.SortFunc(out[start:], func(a, b toolCallImpact) int {
				return strings.Compare(a.Node, b.Node)
			})
		}
	}
	return out
}
