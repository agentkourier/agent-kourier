package v1alpha1

// SchemaHashAnnotation is the annotation that each generated CustomResourceDefinition file carries (so whatever applies it applies it): the SHA-256, in hex,
// of the file without that line (SchemaSHA256 is the one this package was built with). The broker reads it to tell whether the CRDs of its
// cluster are the ones it was built with; it is a statement by whoever wrote the CRD, not proof of what the API server serves.
const SchemaHashAnnotation = Group + "/schema-sha256"

// CRDNames returns the names of this package's three CustomResourceDefinitions, as the chart renders them.
func CRDNames() []string {
	return []string{"agentbackends." + Group, "bindings." + Group, "chatconnections." + Group}
}
