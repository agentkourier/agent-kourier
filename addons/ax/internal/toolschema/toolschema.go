// Package toolschema turns the parameters of an ADK function declaration into the JSON Schema that a model provider takes
// for a tool, so that every provider of the add-on sends the same schema.
package toolschema

import (
	"encoding/json"
	"errors"
	"strings"

	"google.golang.org/genai"
)

// Of returns the JSON Schema of f's parameters as plain JSON values: ParametersJsonSchema, or else Parameters, with
// every type name in lower case (genai.Schema writes OBJECT and STRING, which JSON Schema does not know), or an object
// with no properties when f declares neither. ADK's schema types have JSON marshalers of their own, so the schema is
// marshalled and read back rather than walked.
func Of(f *genai.FunctionDeclaration) (map[string]any, error) {
	var schema any
	switch {
	case f == nil:
		return nil, errors.New("no function declaration")
	case f.ParametersJsonSchema != nil:
		schema = f.ParametersJsonSchema
	case f.Parameters != nil:
		schema = f.Parameters
	default:
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, errors.New("invalid tool schema")
	}
	var mapped map[string]any
	if json.Unmarshal(raw, &mapped) != nil {
		return nil, errors.New("invalid tool schema")
	}
	if mapped == nil {
		// A schema that encodes as null, such as a nil pointer, declares no parameters.
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	}
	lowerTypes(mapped)
	return mapped, nil
}

// lowerTypes writes each "type" of the schema v, at any depth, in lower case.
func lowerTypes(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "type" {
				if t, ok := val.(string); ok {
					x[k] = strings.ToLower(t)
				}
			} else {
				lowerTypes(val)
			}
		}
	case []any:
		for _, val := range x {
			lowerTypes(val)
		}
	}
}
