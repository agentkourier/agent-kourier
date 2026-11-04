package v1alpha1

// SchemaHashAnnotation is the annotation the Helm chart puts on each CustomResourceDefinition it renders: the SHA-256, in hex, of the
// generated CRD file (SchemaSHA256 is the one this package was built with). The broker reads it to tell whether the CRDs of its
// cluster are the ones it was built with.
const SchemaHashAnnotation = Group + "/schema-sha256"

// CRDNames returns the names of this package's three CustomResourceDefinitions, as the chart renders them.
func CRDNames() []string {
	return []string{"agentbackends." + Group, "bindings." + Group, "chatconnections." + Group}
}
