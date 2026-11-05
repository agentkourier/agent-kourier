package config

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// asJSON is the JSON of a YAML document, as an API server hands it over.
func asJSON(t *testing.T, doc string) []byte {
	t.Helper()
	j, err := yaml.YAMLToJSONStrict([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// TestDecodeObjectToleratesUnknownFields: on the custom resource route a broker older than its CRDs meets fields it has no type for, in
// spec (a field with a default, which the API server adds to stored objects), in status and in metadata, on every kind. None of them
// refuses the object, and each is reported by path and never by value.
func TestDecodeObjectToleratesUnknownFields(t *testing.T) {
	tests := []struct {
		name, kind, doc string
		want            []string
	}{
		{
			name: "a field a newer CRD defaults, in a Binding spec",
			kind: "Binding",
			doc:  strings.Replace(bindingDoc, "channel: C0123456789 }", "channel: C0123456789, futureField: defaulted-by-the-api-server }", 1),
			want: []string{"spec.chat.futureField"},
		},
		{
			name: "a field a newer CRD defaults, in a ChatConnection spec",
			kind: "ChatConnection",
			doc:  strings.Replace(connectionDoc, "platform: slack,", "platform: slack, futureField: x,", 1),
			want: []string{"spec.futureField"},
		},
		{
			name: "a field a newer CRD defaults, in an AgentBackend spec",
			kind: "AgentBackend",
			doc:  strings.Replace(backendDoc, "dialect: kagent-v1,", "dialect: kagent-v1, futureField: 3,", 1),
			want: []string{"spec.futureField"},
		},
		{
			name: "a new status field",
			kind: "Binding",
			doc:  bindingDoc + "status:\n  foo: bar\n  conditions:\n    - { type: Ready, status: \"True\", reason: Ready, lastTransitionTime: \"2026-11-04T10:00:00Z\", observedGeneration: 1 }\n",
			want: []string{"status.foo"},
		},
		{
			name: "a new metadata field of a newer API server, on a Binding",
			kind: "Binding",
			doc:  strings.Replace(bindingDoc, "metadata: { name: sre-alerts, namespace: sre }", "metadata: { name: sre-alerts, namespace: sre, newMeta: 1 }", 1),
			want: []string{"metadata.newMeta"},
		},
		{
			name: "a new metadata field of a newer API server, on a ChatConnection",
			kind: "ChatConnection",
			doc:  strings.Replace(connectionDoc, "metadata: {", "metadata: { newMeta: 1,", 1),
			want: []string{"metadata.newMeta"},
		},
		{
			name: "several, in document order, whatever their depth",
			kind: "Binding",
			doc: strings.Replace(bindingDoc, "channel: C0123456789 }", "channel: C0123456789, a: 1 }", 1) +
				"status:\n  b: 2\n",
			want: []string{"spec.chat.a", "status.b"},
		},
		{name: "none", kind: "Binding", doc: bindingDoc},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			j := asJSON(t, tc.doc)
			var unknown []string
			var errs []error
			var name string
			switch tc.kind {
			case "Binding":
				var b *v1alpha1.Binding
				b, unknown, errs = DecodeObject[v1alpha1.Binding](j)
				if b != nil {
					name = b.Name
				}
			case "ChatConnection":
				var c *v1alpha1.ChatConnection
				c, unknown, errs = DecodeObject[v1alpha1.ChatConnection](j)
				if c != nil {
					name = c.Name
				}
			case "AgentBackend":
				var be *v1alpha1.AgentBackend
				be, unknown, errs = DecodeObject[v1alpha1.AgentBackend](j)
				if be != nil {
					name = be.Name
				}
			}
			if len(errs) > 0 {
				t.Fatalf("an unknown field refused the object: %v", errs)
			}
			if name == "" {
				t.Fatal("no object")
			}
			if !slices.Equal(unknown, tc.want) {
				t.Errorf("unknown = %q, want %q", unknown, tc.want)
			}
		})
	}
}

// TestDecodeObjectNamesNoValue: what is ignored is reported by the path of the field, so a value that is a secret never reaches a log.
func TestDecodeObjectNamesNoValue(t *testing.T) {
	const secret = "not-for-the-log-4f2a"
	doc := strings.Replace(bindingDoc, "channel: C0123456789 }", "channel: C0123456789, futureField: "+secret+" }", 1)
	_, unknown, errs := DecodeObject[v1alpha1.Binding](asJSON(t, doc))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if got := strings.Join(unknown, ";"); got != "spec.chat.futureField" || strings.Contains(got, secret) {
		t.Errorf("unknown = %q", got)
	}
}

// TestDecodeObjectStillRefusesWhatIsWrong: tolerance is for fields the type does not have. A value of the wrong type, a spec that
// fails Validate, and a spec that fails Validate beside an unknown field are all refused, and the unknown field is still reported.
func TestDecodeObjectStillRefusesWhatIsWrong(t *testing.T) {
	t.Run("a value of the wrong type", func(t *testing.T) {
		doc := strings.Replace(bindingDoc, "channel: C0123456789 }", "channel: [C0123456789] }", 1)
		b, _, errs := DecodeObject[v1alpha1.Binding](asJSON(t, doc))
		if b != nil || len(errs) != 1 {
			t.Errorf("got %v, %v; want one error and no object", b, errs)
		}
	})
	t.Run("a spec that fails validation", func(t *testing.T) {
		doc := strings.Replace(bindingDoc, "channel: C0123456789 }", "channel: not-a-channel }", 1)
		b, unknown, errs := DecodeObject[v1alpha1.Binding](asJSON(t, doc))
		if b != nil || len(errs) == 0 || len(unknown) != 0 {
			t.Errorf("got %v, %v, %v; want errors and no object", b, unknown, errs)
		}
	})
	t.Run("a spec that fails validation beside an unknown field", func(t *testing.T) {
		doc := strings.Replace(bindingDoc, "channel: C0123456789 }", "channel: not-a-channel, futureField: x }", 1)
		b, unknown, errs := DecodeObject[v1alpha1.Binding](asJSON(t, doc))
		if b != nil || len(errs) == 0 || !slices.Equal(unknown, []string{"spec.chat.futureField"}) {
			t.Errorf("got %v, %v, %v", b, unknown, errs)
		}
		for _, e := range errs {
			if strings.Contains(e.Error(), "unknown field") {
				t.Errorf("the unknown field is a problem: %v", e)
			}
		}
	})
	t.Run("not JSON of an object", func(t *testing.T) {
		if b, _, errs := DecodeObject[v1alpha1.Binding](json.RawMessage(`[1]`)); b != nil || len(errs) != 1 {
			t.Errorf("got %v, %v", b, errs)
		}
	})
}

// TestTheFileRouteStaysStrict: DecodeObject's tolerance is the custom resource route's. Load still refuses a field the type does not have,
// in every kind and in every place, and a Binding file may not carry a status.
func TestTheFileRouteStaysStrict(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"a Binding spec", strings.Replace(bindingDoc, "channel: C0123456789 }", "channel: C0123456789, futureField: x }", 1), "spec.chat.futureField: unknown field"},
		{"a ChatConnection spec", strings.Replace(connectionDoc, "platform: slack,", "platform: slack, futureField: x,", 1), "spec.futureField: unknown field"},
		{"an AgentBackend spec", strings.Replace(backendDoc, "dialect: kagent-v1,", "dialect: kagent-v1, futureField: x,", 1), "spec.futureField: unknown field"},
		{"metadata", strings.Replace(bindingDoc, "namespace: sre }", "namespace: sre, newMeta: 1 }", 1), "metadata.newMeta: unknown field"},
		{"a Binding status", bindingDoc + statusDoc, statusInFileMessage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(context.Background(), write(t, map[string]string{"x.yaml": tc.doc}))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if err := DecodeStrict(asYAMLOf(t, bindingDoc, "futureField"), &v1alpha1.Binding{}); err == nil {
		t.Error("DecodeStrict accepted an unknown field")
	}
}

// asYAMLOf is the document with a top-level key added, for DecodeStrict.
func asYAMLOf(t *testing.T, doc, key string) []byte {
	t.Helper()
	return []byte(doc + key + ": 1\n")
}
