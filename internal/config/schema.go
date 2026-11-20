package config

import (
	"fmt"
	"math"
	"strings"
)

// The executable schema is closed: unknown attributes or blocks cannot be
// silently ignored. This also prevents misspelled policy/security settings
// from weakening published configurations. Opaque source may be retained by
// patch operations, but must not be activated until the compiler understands it.
var blockAttributes = map[string]map[string]string{
	"regate":      {"listen": "string", "max_active_tasks": "positive-int", "max_queued_tasks": "nonnegative-int", "max_queued_per_owner": "positive-int"},
    "oauth":        {"issuer": "string", "audience": "string", "jwks_url": "string", "introspection_url": "string", "client_id_env": "string", "client_secret_env": "string", "token_endpoint": "string", "exchange_client_id_env": "string", "exchange_client_secret_env": "string"},
	"upstream":    {"url": "string", "auth_env": "string", "timeout_ms": "positive-int", "trust_level": "string", "oauth_exchange": "string", "oauth_audience": "string", "oauth_scopes": "string-list", "tool_schema_pins": "object"},
	"hook":        {"transport": "string", "url": "string", "upstream": "string", "tool": "string", "auth_env": "string", "contract": "string", "on_error": "string", "timeout_ms": "positive-int", "trust_level": "string", "oauth_exchange": "string", "oauth_audience": "string", "oauth_scopes": "string-list"},
	"policy":      {"decision_hook": "string", "scope": "string"},
    "readback":    {"write_operation":"string","upstream":"string","read_tool":"string","intent_pointer":"string","read_argument":"string","result_pointer":"string","match_intent_pointer":"string","match_result_pointer":"string","write_token_argument":"string","result_token_pointer":"string","safe_read":"bool"},
    "compensation":{"for_operation":"string","upstream":"string","tool":"string","intent_pointer":"string","argument":"string","enabled":"bool"},
	"pipeline":    {"entry": "string", "timeout_ms": "positive-int", "concurrency": "positive-int"},
	"surface":     {"path": "string", "oauth": "string", "required_scopes": "string-list"},
	"tool":        {"description": "string", "pipeline": "string", "source": "string", "input_schema": "object", "output_schema": "object", "read_only": "bool", "required_scopes": "string-list", "data_classification": "string", "allow_unmasked": "bool"},
	"resource":    {"uri": "string", "source": "string", "text": "string", "mime_type": "string", "required_scopes": "string-list"},
	"prompt":      {"description": "string", "text": "string", "required_scopes": "string-list"},
	"interceptor": {"stage": "string", "hook": "string", "surface": "string", "tool": "string", "order": "int"},
	"edge":        {"from": "string", "to": "string", "when": "string", "kind": "string", "from_port": "string", "to_port": "string"},
}
var nodeAttributes = map[string]map[string]string{
	"input":        {"kind": "string"},
	"output":       {"kind": "string", "value": "expression", "mapping": "expression"},
	"transform":    {"kind": "string", "value": "expression", "mapping": "expression"},
	"filter":       {"kind": "string", "value": "expression", "fields": "string-list"},
	"validate":     {"kind": "string", "value": "expression", "required": "string-list"},
	"condition":    {"kind": "string", "test": "expression"},
	"parallel":     {"kind": "string", "value": "expression", "mapping": "expression"},
	"join":         {"kind": "string", "mode": "string", "quorum": "positive-int", "value": "expression", "mapping": "expression"},
	"map":          {"kind": "string", "items": "expression", "mapping": "expression", "pipeline": "string", "allow_effects": "bool", "allow_suspend": "bool", "max_items": "positive-int", "parallelism": "positive-int"},
	"pipeline.call": {"kind": "string", "pipeline": "string", "arguments": "object-or-expression", "allow_effects": "bool", "allow_suspend": "bool"},
	"mcp.call":     {"kind": "string", "upstream": "string", "tool": "string", "effect": "string", "arguments": "object-or-expression", "mock": "any", "mock_structured": "any", "mock_content": "any", "retry": "object"},
	"hook.call":    {"kind": "string", "hook": "string", "value": "expression"},
	"http.call":    {"kind": "string", "url": "string", "auth_env": "string", "timeout_ms": "positive-int", "value": "expression", "trust_level": "string"},
	"policy.check": {"kind": "string", "policy": "string", "value": "expression", "mandatory": "bool"},
	"suspend":      {"kind": "string"},
	"mask.pii":     {"kind": "string", "value": "expression"},
}
var nestedBlocks = map[string]map[string]bool{
	"pipeline": {"node": true, "edge": true},
	"surface":  {"tool": true, "resource": true, "prompt": true},
}
var requiredBlockAttrs = map[string][]string{
	"oauth": {"issuer", "audience"}, "readback": {"write_operation","upstream","read_tool","intent_pointer","read_argument","result_pointer","safe_read"}, "compensation":{"for_operation","upstream","tool","intent_pointer","argument","enabled"}, "upstream": {"url"}, "hook": {"transport", "contract"}, "pipeline": {"entry"},
	"surface": {"path"}, "node": {"kind"}, "edge": {"from", "to"},
}

func ValidateBlockSchema(blocks []*Block) error {
	for _, b := range blocks {
		if err := validateBlock(b, "", true); err != nil {
			return err
		}
	}
	return nil
}
func validateBlock(b *Block, parent string, top bool) error {
	where := fmt.Sprintf("%s %s at line %d", b.Type, strings.Join(b.Labels, ":"), b.Line)
	if top {
		switch b.Type {
		case "regate", "oauth", "upstream", "hook", "policy", "readback", "compensation", "pipeline", "surface", "interceptor":
		default:
			return fmt.Errorf("%s: unknown top-level block", where)
		}
	} else if !nestedBlocks[parent][b.Type] {
		return fmt.Errorf("%s: block is not permitted inside %s", where, parent)
	}
	if b.Type == "regate" || b.Type == "edge" {
		if len(b.Labels) != 0 {
			return fmt.Errorf("%s: labels are not allowed", where)
		}
	} else if len(b.Labels) != 1 {
		return fmt.Errorf("%s: exactly one label required", where)
	}
	attrSpec := blockAttributes[b.Type]
	if b.Type == "node" {
		kind, ok := b.Attrs["kind"].(string)
		if !ok {
			return fmt.Errorf("%s: kind must be string", where)
		}
		var present bool
		attrSpec, present = nodeAttributes[kind]
		// Node contracts describe the effective data entering and leaving this step.
		// The schemas are compiled before activation and checked at runtime.
		merged := make(map[string]string, len(attrSpec)+2)
		for name, typ := range attrSpec {
			merged[name] = typ
		}
		merged["input_schema"] = "object"
		merged["output_schema"] = "object"
		merged["input_ports"] = "object"
		merged["output_ports"] = "object"
		attrSpec = merged
		if !present {
			return fmt.Errorf("%s: unsupported kind %q", where, kind)
		}
	}
	for name, value := range b.Attrs {
		expected, ok := attrSpec[name]
		if !ok {
			return fmt.Errorf("%s: unknown attribute %q", where, name)
		}
		if err := validateAttrType(value, expected); err != nil {
			return fmt.Errorf("%s: %s: %w", where, name, err)
		}
	}
	for _, name := range requiredBlockAttrs[b.Type] {
		if _, ok := b.Attrs[name]; !ok {
			return fmt.Errorf("%s: missing required attribute %q", where, name)
		}
	}
	if b.Type == "node" {
		k := b.String("kind")
		for _, name := range map[string][]string{"condition": {"test"}, "map": {"items", "max_items"}, "pipeline.call": {"pipeline", "arguments"}, "mcp.call": {"upstream", "tool"}, "hook.call": {"hook"}, "policy.check": {"policy"}, "http.call": {"url"}}[k] {
			if _, ok := b.Attrs[name]; !ok {
				return fmt.Errorf("%s: %s requires %q", where, k, name)
			}
		}
		if k == "map" {
			_, expression := b.Attrs["mapping"]
			_, child := b.Attrs["pipeline"]
			if _, enabled := b.Attrs["parallelism"]; enabled &&
				(b.Attrs["allow_effects"] == true || b.Attrs["allow_suspend"] == true) {
				return fmt.Errorf("%s: map parallelism requires an effect-free, non-suspendable mapping", where)
			}
			if expression == child {
				return fmt.Errorf("%s: map requires exactly one of mapping or pipeline", where)
			}
			if b.Attrs["allow_effects"] == true && !child {
				return fmt.Errorf("%s: map allow_effects requires pipeline", where)
			}
			if b.Attrs["allow_suspend"] == true &&
				(!child || b.Attrs["allow_effects"] != true) {
				return fmt.Errorf("%s: map allow_suspend requires pipeline and allow_effects=true", where)
			}
		}
		if k == "join" && b.String("mode") != "" {
			switch b.String("mode") {
			case "all", "any", "quorum":
			default:
				return fmt.Errorf("%s: invalid join mode", where)
			}
			if b.String("mode") == "quorum" && b.Int("quorum", 0) == 0 {
				return fmt.Errorf("%s: quorum join needs positive quorum", where)
			}
		}
		if k == "mcp.call" && b.String("effect") != "" {
			switch b.String("effect") {
			case "read", "write", "external_transfer", "unknown":
			default:
				return fmt.Errorf("%s: invalid effect", where)
			}
		}
	}
	if b.Type == "hook" {
		if b.String("on_error") != "" && b.String("on_error") != "deny" {
			return fmt.Errorf("%s: unsupported on_error (only deny is implemented)", where)
		}
	}
	for _, child := range b.Children {
		if err := validateBlock(child, b.Type, false); err != nil {
			return err
		}
	}
	return nil
}
func validateAttrType(value any, kind string) error {
	switch kind {
	case "any", "expression":
		return nil
	case "string":
		if _, ok := value.(string); ok {
			return nil
		}
	case "bool":
		if _, ok := value.(bool); ok {
			return nil
		}
	case "int":
		if _, ok := value.(int64); ok {
			return nil
		}
	case "positive-int":
		if n, ok := value.(int64); ok && n > 0 && n <= math.MaxInt {
			return nil
		}
    case "nonnegative-int":
        if n,ok:=value.(int64);ok && n>=0 && n<=math.MaxInt {return nil}
	case "object":
		if _, ok := value.(map[string]any); ok {
			return nil
		}
	case "object-or-expression":
		switch value.(type) {
		case map[string]any, string:
			return nil
		}
	case "string-list":
		if arr, ok := value.([]any); ok {
			for _, v := range arr {
				if _, ok := v.(string); !ok {
					return fmt.Errorf("expected list(string)")
				}
			}
			return nil
		}
	}
	return fmt.Errorf("expected %s, got %T", kind, value)
}
