package config

import (
	"fmt"
	"strings"

	"cel.dev/cel-go/cel"
	"github.com/burkostya/regate/internal/expr"
)

type ContentSchemas struct {
	InputSchema  map[string]any `json:"input_schema,omitempty"`
	OutputSchema map[string]any `json:"output_schema,omitempty"`
	Contract     ToolContract   `json:"-"`
}

func contentSchemas(block *Block) ContentSchemas {
	input, _ := block.Attrs["input_schema"].(map[string]any)
	output, _ := block.Attrs["output_schema"].(map[string]any)
	return ContentSchemas{InputSchema: input, OutputSchema: output}
}

func (s *ContentSchemas) compile() error {
	input, err := compileSchema(s.InputSchema)
	if err != nil {
		return fmt.Errorf("input_schema: %w", err)
	}
	output, err := compileSchema(s.OutputSchema)
	if err != nil {
		return fmt.Errorf("output_schema: %w", err)
	}
	s.Contract = ToolContract{Input: input, Output: output}
	return nil
}

type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

func decodePrompt(block *Block, name string) (Prompt, error) {
	prompt := Prompt{
		Name: name, Description: block.String("description"), Text: block.String("text"),
		Source: block.String("source"), RequiredScopes: stringList(block.Attrs["required_scopes"]),
		ContentSchemas: contentSchemas(block),
	}
	if _, sourced := block.Attrs["source"]; sourced {
		upstream, remote, ok := strings.Cut(prompt.Source, ".")
		_, local := block.Attrs["text"]
		if !ok || strings.TrimSpace(upstream) == "" || strings.TrimSpace(remote) == "" || local {
			return Prompt{}, fmt.Errorf("prompt %s: source must be upstream.prompt and is exclusive with text", name)
		}
	}
	items, _ := block.Attrs["arguments"].([]any)
	seen := map[string]bool{}
	for _, item := range items {
		fields := item.(map[string]any)
		for key, value := range fields {
			kind := map[string]string{"name": "string", "description": "string", "required": "bool"}[key]
			if kind == "" || validateAttrType(value, kind) != nil {
				return Prompt{}, fmt.Errorf("prompt %s: invalid argument field %q", name, key)
			}
		}
		argument, _ := fields["name"].(string)
		if strings.TrimSpace(argument) == "" || seen[argument] {
			return Prompt{}, fmt.Errorf("prompt %s: arguments need distinct, nonempty names", name)
		}
		seen[argument] = true
		description, _ := fields["description"].(string)
		required, _ := fields["required"].(bool)
		prompt.Arguments = append(prompt.Arguments, PromptArgument{argument, description, required})
	}
	return prompt, nil
}

func decodeInterceptor(block *Block, name string) (Interceptor, error) {
	i := Interceptor{
		Name: name, Stage: block.String("stage"), Hook: block.String("hook"), Surface: block.String("surface"),
		Tool: block.String("tool"), Resource: block.String("resource"), Prompt: block.String("prompt"),
		Policy: block.String("policy"), Input: block.String("input"), Check: block.String("check"),
		Transform: block.String("transform"), Order: block.Int("order", 100),
	}
	for _, attr := range []string{"resource", "prompt", "policy", "input", "check", "transform"} {
		if value, present := block.Attrs[attr]; present && strings.TrimSpace(value.(string)) == "" {
			return Interceptor{}, fmt.Errorf("interceptor %s: %s cannot be empty", name, attr)
		}
	}
	return i, nil
}

func (i Interceptor) ContentTarget() (kind, name string) {
	if i.Resource != "" {
		return "resource", i.Resource
	}
	if i.Prompt != "" {
		return "prompt", i.Prompt
	}
	return "", ""
}

func (c *Config) ContentInterceptors(surface, kind, name string) []Interceptor {
	var matched []Interceptor
	for _, i := range c.Interceptors {
		targetKind, targetName := i.ContentTarget()
		if targetKind == kind && targetName == name && (i.Surface == "" || i.Surface == surface) {
			matched = append(matched, i)
		}
	}
	return matched
}

func (c *Config) validateContentInterceptor(i Interceptor) error {
	kind, name := i.ContentTarget()
	if kind == "" {
		if i.Policy != "" || i.Input != "" || i.Check != "" || i.Transform != "" {
			return fmt.Errorf("policy and CEL require a resource or prompt selector")
		}
		if _, ok := c.Hooks[i.Hook]; !ok {
			return fmt.Errorf("missing hook %s", i.Hook)
		}
		return nil
	}
	if i.Tool != "" || (i.Resource != "" && i.Prompt != "") {
		return fmt.Errorf("resource, prompt and tool selectors are mutually exclusive")
	}
	actions := 0
	for _, action := range []string{i.Hook, i.Policy, i.Check, i.Transform} {
		if action != "" {
			actions++
		}
	}
	if actions != 1 {
		return fmt.Errorf("requires exactly one of hook, policy, check or transform")
	}
	if i.Input != "" && i.Hook == "" && i.Policy == "" {
		return fmt.Errorf("input projection requires a hook or policy")
	}
	if i.Policy != "" {
		policy, ok := c.Policies[i.Policy]
		h := c.Hooks[policy.DecisionHook]
		if !ok || h.Contract != "decision" || h.Transport == "builtin" {
			return fmt.Errorf("policy %s requires an external decision hook", i.Policy)
		}
	}
	if i.Hook != "" {
		h, ok := c.Hooks[i.Hook]
		if !ok {
			return fmt.Errorf("missing hook %s", i.Hook)
		}
		if h.Contract == "decision" && h.Transport == "builtin" {
			return fmt.Errorf("decision requires an external hook")
		}
		if (i.Stage == "on_error") != (h.Contract == "observe") || h.Contract == "action" {
			return fmt.Errorf("on_error requires an observe hook; other stages require decision or transform hooks")
		}
	}
	if i.Stage == "on_error" && (i.Hook == "" || i.Input != "") {
		return fmt.Errorf("on_error requires an observe hook with the standard diagnostic input")
	}

	found := false
	for surfaceName, surface := range c.Surfaces {
		if surface.Upstream != "" || (i.Surface != "" && i.Surface != surfaceName) {
			continue
		}
		var sources []string
		if kind == "resource" {
			for _, res := range surface.Resources {
				if res.Name != name {
					continue
				}
				for _, other := range surface.Resources {
					if other.URI == res.URI && other.Name != res.Name {
						return fmt.Errorf("resource %s has an ambiguous URI on surface %s", name, surfaceName)
					}
				}
				sources = append(sources, res.Source)
			}
		} else {
			for _, prompt := range surface.Prompts {
				if prompt.Name == name {
					sources = append(sources, prompt.Source)
				}
			}
		}
		if len(sources) > 1 {
			return fmt.Errorf("ambiguous %s %s on surface %s", kind, name, surfaceName)
		}
		if len(sources) == 1 {
			if err := c.validateContentSource(sources[0]); err != nil {
				return fmt.Errorf("%s %s: %w", kind, name, err)
			}
			if (i.Stage == "before_call" || i.Stage == "after_call") && sources[0] == "" {
				return fmt.Errorf("%s requires an upstream source for %s %s", i.Stage, kind, name)
			}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("unknown %s %s on assembled surfaces", kind, name)
	}
	return nil
}

func (c *Config) validateContentSource(source string) error {
	if source == "" {
		return nil
	}
	upstream, _, ok := strings.Cut(source, ".")
	if !ok {
		return fmt.Errorf("source must identify a configured upstream")
	}
	if _, ok := c.Upstreams[upstream]; !ok {
		return fmt.Errorf("unknown content upstream %s", upstream)
	}
	return nil
}

func (c *Config) compileContent() error {
	for name, surface := range c.Surfaces {
		for index := range surface.Resources {
			res := &surface.Resources[index]
			if res.InputSchema != nil || res.OutputSchema != nil {
				if err := c.validateContentSource(res.Source); err != nil {
					return fmt.Errorf("surface %s resource %s: %w", name, res.Name, err)
				}
				for other, entry := range surface.Resources {
					if other != index && (entry.URI == res.URI || entry.Name == res.Name) {
						return fmt.Errorf("surface %s: ambiguous resource %s with schema", name, res.Name)
					}
				}
			}
			if err := res.ContentSchemas.compile(); err != nil {
				return fmt.Errorf("surface %s resource %s: %w", name, res.Name, err)
			}
		}
		for index := range surface.Prompts {
			prompt := &surface.Prompts[index]
			if prompt.Source != "" || len(prompt.Arguments) != 0 || prompt.InputSchema != nil || prompt.OutputSchema != nil {
				for other, entry := range surface.Prompts {
					if other != index && entry.Name == prompt.Name {
						return fmt.Errorf("surface %s: ambiguous prompt %s", name, prompt.Name)
					}
				}
			}
			if err := prompt.ContentSchemas.compile(); err != nil {
				return fmt.Errorf("surface %s prompt %s: %w", name, prompt.Name, err)
			}
		}
	}
	for index := range c.Interceptors {
		i := &c.Interceptors[index]
		program, expected := i.Input, cel.DynType
		if i.Check != "" {
			program, expected = i.Check, cel.BoolType
		} else if i.Transform != "" {
			program = i.Transform
		}
		if program == "" {
			continue
		}
		var err error
		i.Program, err = expr.CompileInterceptor(program, expected)
		if err != nil {
			return fmt.Errorf("interceptor %s: %w", i.Name, err)
		}
	}
	return nil
}
