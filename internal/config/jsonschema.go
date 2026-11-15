package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// SchemaContract is compiled once with the HCL revision. It is immutable
// during a run. Validation implements JSON Schema 2020-12, including local
// references; remote schema fetching is prohibited at compile time.
type SchemaContract struct {
	Raw      map[string]any     `json:"-"`
	Compiled *jsonschema.Schema `json:"-"`
}

type denySchemaLoader struct{}

func (denySchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external JSON Schema resource %q is forbidden; inline schemas and local $defs are supported", url)
}
func compileSchema(raw map[string]any) (*SchemaContract, error) {
	if raw == nil {
		return nil, nil
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("encode JSON Schema: %w", err)
	}
	// Normalize HCL numbers and composite types to standard JSON values.
	var data any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err = decoder.Decode(&data); err != nil {
		return nil, err
	}
	const address = "https://regate.invalid/embedded-schema.json"
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(denySchemaLoader{})
	if err = compiler.AddResource(address, data); err != nil {
		return nil, err
	}
	compiled, err := compiler.Compile(address)
	if err != nil {
		return nil, err
	}
	return &SchemaContract{Raw: raw, Compiled: compiled}, nil
}
func (s *SchemaContract) Validate(value any) error {
	if s == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode data for JSON Schema validation: %w", err)
	}
	var normalized any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err = decoder.Decode(&normalized); err != nil {
		return fmt.Errorf("decode data for JSON Schema validation: %w", err)
	}
	if err = s.Compiled.Validate(normalized); err != nil {
		return fmt.Errorf("JSON Schema: %w", err)
	}
	return nil
}
func (s *SchemaContract) Types() []string {
	if s == nil {
		return nil
	}
	return schemaTypes(s.Raw)
}
func schemaTypes(raw map[string]any) []string {
	if raw == nil {
		return nil
	}
	switch x := raw["type"].(type) {
	case string:
		return []string{x}
	case []any:
		types := []string{}
		for _, v := range x {
			if t, ok := v.(string); ok {
				types = append(types, t)
			}
		}
		sort.Strings(types)
		return types
	}
	return nil
}
func disjointTypes(given, needed []string) bool {
	if len(given) == 0 || len(needed) == 0 {
		return false
	}
	for _, a := range given {
		for _, b := range needed {
			if a == b || a == "integer" && b == "number" || a == "number" && b == "integer" {
				return false
			}
		}
	}
	return true
}

// CheckKnownCompatibility rejects only proven-incompatible producer/consumer
// contracts. JSON Schema subtyping in full is undecidable for arbitrary refs,
// conditionals and custom vocabularies. All allowed uncertain connections are
// still validated against the consumer schema at execution time.
func CheckKnownCompatibility(producer, consumer *SchemaContract) error {
	if producer == nil || consumer == nil {
		return nil
	}
	if disjointTypes(producer.Types(), consumer.Types()) {
		return fmt.Errorf("incompatible port types: producer %v cannot feed consumer %v", producer.Types(), consumer.Types())
	}
	if containsString(producer.Types(), "object") && containsString(consumer.Types(), "object") {
		required, _ := consumer.Raw["required"].([]any)
		properties, _ := producer.Raw["properties"].(map[string]any)
		closed, closedOK := producer.Raw["additionalProperties"].(bool)
		if closedOK && !closed {
			for _, v := range required {
				key, _ := v.(string)
				if _, ok := properties[key]; !ok {
					return fmt.Errorf("producer forbids required consumer field %q", key)
				}
			}
		}
		expected, _ := consumer.Raw["properties"].(map[string]any)
		for key, a := range properties {
			b, ok := expected[key]
			if !ok {
				continue
			}
			left, lok := a.(map[string]any)
			right, rok := b.(map[string]any)
			if lok && rok && disjointTypes(schemaTypes(left), schemaTypes(right)) {
				// An optional producer property may be omitted, hence the connection is
				// not impossible unless the producer guarantees it will be present.
				if isRequired(producer.Raw, key) {
					return fmt.Errorf("incompatible property %q: %v to %v", key, schemaTypes(left), schemaTypes(right))
				}
			}
		}
	}
	return nil
}
func containsString(v []string, needle string) bool {
	for _, x := range v {
		if x == needle {
			return true
		}
	}
	return false
}
func isRequired(schema map[string]any, key string) bool {
	xs, _ := schema["required"].([]any)
	for _, v := range xs {
		if v == key {
			return true
		}
	}
	return false
}
func schemaRefFromExpression(expression string) (string, bool) {
	const prefix = "nodes."
	expression = strings.TrimSpace(expression)
	if !strings.HasPrefix(expression, prefix) || !strings.HasSuffix(expression, ".value") {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(expression, prefix), ".value")
	if name == "" || strings.ContainsAny(name, " ./[\t\n'") {
		return "", false
	}
	return name, true
}
