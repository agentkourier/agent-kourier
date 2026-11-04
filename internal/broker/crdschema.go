package broker

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// What the broker found when it compared a CRD's schema with the one it was built with, the values of the state label of
// agentkourier_crd_schema_state.
const (
	// SchemaMatch: the CRD carries the hash of the schema this broker was built with.
	SchemaMatch = "match"
	// SchemaMismatch: the CRD carries another hash, so the cluster's CRDs and this broker are not from the same release.
	SchemaMismatch = "mismatch"
	// SchemaUnannotated: the CRD carries no hash. The chart puts it on the CRDs it renders, so a CRD applied from the files by
	// hand, or by a chart that did not stamp it, has none. That is not a fault, only a CRD that cannot be compared.
	SchemaUnannotated = "unannotated"
	// SchemaUnknown: the CRD could not be read (RBAC, an outage, or it is not there), so nothing is known.
	SchemaUnknown = "unknown"
)

var schemaStates = []string{SchemaMatch, SchemaMismatch, SchemaUnannotated, SchemaUnknown}

// crdResource is the CustomResourceDefinition resource, read through the dynamic client, which the CR source has already.
var crdResource = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

// crdGetTimeout bounds the get of each CRD, so that a slow API server cannot hold the start.
const crdGetTimeout = 10 * time.Second

// checkCRDSchemas compares the schema of each of the three CRDs with the one this broker was built with, once, at start, with the
// config source crds only. It gets each CRD by name (the chart's ClusterRole allows exactly that), reads the annotation
// v1alpha1.SchemaHashAnnotation and compares it with v1alpha1.SchemaSHA256. A mismatch is a warning that names the CRD and both hashes, and
// a state of the gauge; so is a CRD that cannot be read, which never stops the broker. It never returns an error, because nothing here is
// a reason not to serve: a CRD of another schema is the operator's to bring in step, not a fault of the process.
func checkCRDSchemas(ctx context.Context, client dynamic.Interface, log *slog.Logger, m *Metrics) {
	for _, name := range v1alpha1.CRDNames() {
		state := schemaState(ctx, client, log, name)
		m.CRDSchema(name, state)
	}
}

// schemaState reads one CRD and says what its annotation shows, logging what an operator needs to act on.
func schemaState(ctx context.Context, client dynamic.Interface, log *slog.Logger, name string) string {
	built, ok := v1alpha1.SchemaSHA256(name)
	if !ok {
		return SchemaUnknown // not reachable: CRDNames and SchemaSHA256 are generated from the same files
	}
	ctx, cancel := context.WithTimeout(ctx, crdGetTimeout)
	defer cancel()
	crd, err := client.Resource(crdResource).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		reason := "the CRD could not be read"
		switch {
		case apierrors.IsForbidden(err):
			reason = "the ServiceAccount may not get the CRD (the chart's ClusterRole allows it with config.source crds)"
		case apierrors.IsNotFound(err):
			reason = "the CRD is not there"
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			reason = "the API server did not answer in time"
		}
		log.Warn("the schema of a CRD could not be compared with the one this broker was built with; its state is unknown", "crd", name, "reason", reason, "error", err)
		return SchemaUnknown
	}
	got, has := crd.GetAnnotations()[v1alpha1.SchemaHashAnnotation]
	switch {
	case !has || got == "":
		log.Info("a CRD has no schema hash, so its schema cannot be compared with the one this broker was built with: it was not applied by the chart (the chart stamps the hash)",
			"crd", name, "annotation", v1alpha1.SchemaHashAnnotation, "built", built)
		return SchemaUnannotated
	case got != built:
		log.Warn("the schema of a CRD is not the one this broker was built with: apply the CRDs of the chart release that matches this image (crds.install, or kubectl apply --server-side -f files/crds of that release)",
			"crd", name, "annotation", v1alpha1.SchemaHashAnnotation, "cluster", got, "built", built)
		return SchemaMismatch
	}
	return SchemaMatch
}

// CRDSchema sets the state of one CRD's schema comparison: 1 for state, 0 for the others, so that a CRD has all its series once it
// has any. A state outside the closed set is read as unknown.
func (m *Metrics) CRDSchema(crd, state string) {
	if !slices.Contains(schemaStates, state) {
		state = SchemaUnknown
	}
	for _, s := range schemaStates {
		v := 0.0
		if s == state {
			v = 1
		}
		m.crdSchema.WithLabelValues(crd, s).Set(v)
	}
}
