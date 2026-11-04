package v1alpha1_test

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"k8s.io/client-go/util/jsonpath"
	"sigs.k8s.io/yaml"
)

// bindingCRD is the part of the generated Binding CRD that the status tests read.
type bindingCRD struct {
	Spec struct {
		Versions []struct {
			Name         string `json:"name"`
			Subresources struct {
				Status *struct{} `json:"status"`
			} `json:"subresources"`
			AdditionalPrinterColumns []struct {
				Name     string `json:"name"`
				Type     string `json:"type"`
				JSONPath string `json:"jsonPath"`
			} `json:"additionalPrinterColumns"`
			Schema struct {
				OpenAPIV3Schema struct {
					Properties struct {
						Status struct {
							Properties struct {
								Conditions struct {
									Type        string   `json:"type"`
									MaxItems    *int64   `json:"maxItems"`
									ListType    string   `json:"x-kubernetes-list-type"`
									ListMapKeys []string `json:"x-kubernetes-list-map-keys"`
								} `json:"conditions"`
							} `json:"properties"`
						} `json:"status"`
					} `json:"properties"`
				} `json:"openAPIV3Schema"`
			} `json:"schema"`
		} `json:"versions"`
	} `json:"spec"`
}

func readBindingCRD(t *testing.T) bindingCRD {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(crdsDir, "agentkourier.dev_bindings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd bindingCRD
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) != 1 || crd.Spec.Versions[0].Name != "v1alpha1" {
		t.Fatalf("Binding CRD versions = %+v, want v1alpha1 only", crd.Spec.Versions)
	}
	return crd
}

// The Binding CRD serves status as a subresource, so a write to it cannot change the spec and a write to the spec
// cannot change it, and its conditions merge by type.
func TestBindingCRDHasAStatusSubresourceOfConditionsKeyedByType(t *testing.T) {
	v := readBindingCRD(t).Spec.Versions[0]
	if v.Subresources.Status == nil {
		t.Error("the Binding CRD has no status subresource")
	}
	c := v.Schema.OpenAPIV3Schema.Properties.Status.Properties.Conditions
	if c.Type != "array" || c.ListType != "map" || !slices.Equal(c.ListMapKeys, []string{"type"}) {
		t.Errorf("status.conditions = %+v, want an array that is a list map keyed by type", c)
	}
	// The plan writes five condition types (Ready, BackendReachable, ChannelJoined, Accepted, ApproversReadable). The
	// bound lets CEL rules walk the list within the API server's cost budget.
	if c.MaxItems == nil || *c.MaxItems != 16 {
		t.Errorf("status.conditions maxItems = %v, want 16", c.MaxItems)
	}
}

// printerColumn evaluates a printer column's JSONPath on obj as the API server does: a path that matches nothing,
// as when status is absent, gives an empty cell and no error.
func printerColumn(t *testing.T, path string, obj map[string]any) string {
	t.Helper()
	j := jsonpath.New("column").AllowMissingKeys(true)
	if err := j.Parse("{" + path + "}"); err != nil {
		t.Fatalf("printer column path %q does not parse: %v", path, err)
	}
	var out bytes.Buffer
	if err := j.Execute(&out, obj); err != nil {
		t.Fatalf("printer column path %q: %v", path, err)
	}
	return out.String()
}

func TestBindingPrinterColumns(t *testing.T) {
	v := readBindingCRD(t).Spec.Versions[0]
	type column struct{ Name, Type, JSONPath string }
	var got []column
	paths := map[string]string{}
	for _, c := range v.AdditionalPrinterColumns {
		got = append(got, column{c.Name, c.Type, c.JSONPath})
		paths[c.Name] = c.JSONPath
	}
	want := []column{
		{"Ready", "string", `.status.conditions[?(@.type=="Ready")].status`},
		{"Reason", "string", `.status.conditions[?(@.type=="Ready")].reason`},
		{"Channel", "string", `.spec.chat.channel`},
		{"Age", "date", `.metadata.creationTimestamp`},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("printer columns = %+v, want %+v", got, want)
	}

	meta := map[string]any{"creationTimestamp": "2026-11-04T10:00:00Z"}
	spec := map[string]any{"chat": map[string]any{"channel": "C0123456789"}}
	for _, tc := range []struct {
		name string
		obj  map[string]any
		want map[string]string
	}{
		{"no status", map[string]any{"metadata": meta, "spec": spec},
			map[string]string{"Ready": "", "Reason": "", "Channel": "C0123456789", "Age": "2026-11-04T10:00:00Z"}},
		{"the Ready condition among others", map[string]any{"metadata": meta, "spec": spec, "status": map[string]any{"conditions": []any{
			map[string]any{"type": "BackendReachable", "status": "True", "reason": "Reachable"},
			map[string]any{"type": "Ready", "status": "False", "reason": "Accepted:ChannelConflict"},
		}}}, map[string]string{"Ready": "False", "Reason": "Accepted:ChannelConflict", "Channel": "C0123456789", "Age": "2026-11-04T10:00:00Z"}},
		{"conditions with no Ready", map[string]any{"metadata": meta, "spec": spec, "status": map[string]any{"conditions": []any{
			map[string]any{"type": "BackendReachable", "status": "True", "reason": "Reachable"},
		}}}, map[string]string{"Ready": "", "Reason": "", "Channel": "C0123456789", "Age": "2026-11-04T10:00:00Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for col, want := range tc.want {
				if got := printerColumn(t, paths[col], tc.obj); got != want {
					t.Errorf("column %s = %q, want %q", col, got, want)
				}
			}
		})
	}
}
