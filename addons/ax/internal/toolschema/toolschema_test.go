package toolschema

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/genai"
)

// Each way a function declaration gives its parameters comes out as the same JSON Schema, in plain JSON values with lower-case
// type names.
func TestOfNormalizesEachForm(t *testing.T) {
	want := `{"properties":{"question":{"type":"string"}},"required":["question"],"type":"object"}`
	for _, tc := range []struct {
		name string
		f    *genai.FunctionDeclaration
		want string
	}{
		{"JSON Schema", &genai.FunctionDeclaration{Name: "ask_user", ParametersJsonSchema: &jsonschema.Schema{Type: "object",
			Properties: map[string]*jsonschema.Schema{"question": {Type: "string"}}, Required: []string{"question"}}}, want},
		{"genai schema, upper-case types", &genai.FunctionDeclaration{Name: "ask_user", Parameters: &genai.Schema{Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{"question": {Type: genai.TypeString}}, Required: []string{"question"}}}, want},
		{"no parameters", &genai.FunctionDeclaration{Name: "ping"}, `{"properties":{},"type":"object"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Of(tc.f)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.want {
				t.Fatalf("schema %s; want %s", raw, tc.want)
			}
		})
	}
	if _, err := Of(nil); err == nil {
		t.Fatal("a nil declaration has a schema")
	}
}
