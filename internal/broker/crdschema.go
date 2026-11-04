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

// What the broker found when it compared a CRD's schema hash with the one it was built with, the values of the state label of
// agentkourier_crd_schema_state. The hash is an annotation: a statement by whoever wrote the CRD, which a person can leave in place while
// changing the schema under it. It says which CRD files were applied, not what the API server serves.
const (
	// SchemaMatch: the CRD carries the hash of the schema this broker was built with.
	SchemaMatch = "match"
	// SchemaMismatch: the CRD carries another hash, so the cluster's CRDs and this broker are not from the same release.
	SchemaMismatch = "mismatch"
	// SchemaUnannotated: the CRD carries no hash. The generated CRD files carry it, so only a CRD from an older chart or file, or one made
	// by something else, has none. That is not a fault, only a CRD that cannot be compared.
	SchemaUnannotated = "unannotated"
	// SchemaUnknown: the CRD could not be read (RBAC, an outage, or it is not there), so nothing is known.
	SchemaUnknown = "unknown"
)

var schemaStates = []string{SchemaMatch, SchemaMismatch, SchemaUnannotated, SchemaUnknown}

// crdResource is the CustomResourceDefinition resource, read through the dynamic client, which the CR source has already.
var crdResource = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

const (
	// crdGetTimeout bounds the get of each CRD, so that a slow API server cannot hold a pass.
	crdGetTimeout = 10 * time.Second
	// crdSchemaEvery is how often the comparison is made again: a chart upgrade that changes the CRDs under a pinned image rolls no pod,
	// so a comparison made once would be stale. Three small gets a minute.
	crdSchemaEvery = time.Minute
)

// startSchemaWatch compares the schema of each of the three CRDs with the one this broker was built with, with the config source crds only
// (crds is nil otherwise: there is no call and the gauge has no series), and again every crdSchemaEvery until ctx ends. It starts at once and
// returns at once, so an API server that does not answer holds nothing up. The returned channel closes when the watch has stopped. Nothing
// here is a reason not to serve: a CRD of another schema is the operator's to bring in step, not a fault of the process.
func startSchemaWatch(ctx context.Context, crds *CRDSource, log *slog.Logger, m *Metrics) <-chan struct{} {
	done := make(chan struct{})
	if crds == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		watchCRDSchemas(ctx, crds.Client, log, m, crdSchemaEvery)
	}()
	return done
}

// watchCRDSchemas runs the comparison now and then every `every`, until ctx ends. It gets each CRD by name (the chart's ClusterRole allows
// exactly that), reads v1alpha1.SchemaHashAnnotation and compares it with v1alpha1.SchemaSHA256, sets the gauge on every pass, and logs only
// when what it found for a CRD changed: a mismatch is warned of once, not each minute, and a CRD that is brought back in step is said so.
func watchCRDSchemas(ctx context.Context, client dynamic.Interface, log *slog.Logger, m *Metrics, every time.Duration) {
	last := map[string]finding{}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		for _, name := range v1alpha1.CRDNames() {
			if ctx.Err() != nil {
				return
			}
			f := compareSchema(ctx, client, name)
			if ctx.Err() != nil {
				return // a stop is not a finding
			}
			m.CRDSchema(name, f.state)
			if prev, seen := last[name]; !seen || prev.key() != f.key() {
				f.log(log, name, prev.state)
				last[name] = f
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// finding is what one reading of one CRD showed.
type finding struct {
	state          string
	cluster, built string // the hashes, for a mismatch and an unannotated CRD
	reason         string // why a CRD is unknown
	err            error
}

// key is what makes two findings the same to the log: the state, and for a mismatch the hash found, so a second wrong schema is told too.
func (f finding) key() string { return f.state + "/" + f.cluster }

// compareSchema reads one CRD and says what its annotation shows.
func compareSchema(ctx context.Context, client dynamic.Interface, name string) finding {
	built, ok := v1alpha1.SchemaSHA256(name)
	if !ok {
		return finding{state: SchemaUnknown, reason: "this broker has no hash for it"} // not reachable: CRDNames and SchemaSHA256 come from the same files
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
		return finding{state: SchemaUnknown, reason: reason, err: err, built: built}
	}
	got := crd.GetAnnotations()[v1alpha1.SchemaHashAnnotation]
	switch got {
	case "":
		return finding{state: SchemaUnannotated, built: built}
	case built:
		return finding{state: SchemaMatch, cluster: got, built: built}
	}
	return finding{state: SchemaMismatch, cluster: got, built: built}
}

// log says a finding, naming what an operator needs to act on. prev is the state before it, empty at the first reading.
func (f finding) log(log *slog.Logger, name, prev string) {
	var was []any
	if prev != "" {
		was = []any{"was", prev}
	}
	switch f.state {
	case SchemaMatch:
		if prev != "" { // the first match is not news
			log.Info("the schema hash of a CRD now matches the one this broker was built with", append([]any{"crd", name}, was...)...)
		}
	case SchemaMismatch:
		log.Warn("the schema hash of a CRD is not the one this broker was built with: update the CRDs the way they were installed, to those of the chart release that matches this image (helm upgrade with crds.install, or kubectl apply --server-side -f of that release's files/crds); the hash says which files were applied, not what the API server serves",
			append([]any{"crd", name, "annotation", v1alpha1.SchemaHashAnnotation, "cluster", f.cluster, "built", f.built}, was...)...)
	case SchemaUnannotated:
		log.Info("a CRD has no schema hash, so it cannot be compared with the one this broker was built with: it comes from a chart or file that did not carry it",
			append([]any{"crd", name, "annotation", v1alpha1.SchemaHashAnnotation, "built", f.built}, was...)...)
	default:
		log.Warn("the schema of a CRD could not be compared with the one this broker was built with; its state is unknown",
			append([]any{"crd", name, "reason", f.reason, "error", f.err}, was...)...)
	}
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
