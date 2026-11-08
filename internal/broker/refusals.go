package broker

import (
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// refusedMessage is why a connection's Bindings are told the connection is not served when it is a second connection of a Slack app that the
// broker serves already. It names no other connection: the Bindings that read it may be in a namespace the other connection is not.
const refusedMessage = "another ChatConnection of the same Slack app is older and is the one this broker serves, and an app is served once"

// refusals are the ChatConnections that the broker holds in its Snapshot and does not run because they are a second connection of a Slack app
// (spec D51), with why. The Snapshot serves their Bindings, so the status writer would call them accepted; overlay adds the rejection that says
// otherwise to the report the writer reads, in the words it uses for a connection that is not served. It is safe for concurrent use.
type refusals struct {
	mu      sync.Mutex
	refused map[string]string // "namespace/name" of the connection -> why
	// changed is told when the set differs after set; the status writer's notify, which never blocks.
	changed func()
}

// set replaces the refusals with refused, and tells changed if that is a change.
func (r *refusals) set(refused map[string]string) {
	r.mu.Lock()
	same := maps.Equal(r.refused, refused)
	r.refused = maps.Clone(refused)
	changed := r.changed
	r.mu.Unlock()
	if !same && changed != nil {
		changed()
	}
}

// names returns the refused connections, sorted.
func (r *refusals) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Sorted(maps.Keys(r.refused))
}

// overlay returns rep with a ConnectionNotServed rejection added for each Binding that the Snapshot serves and that names a refused connection.
// The rejection carries the generation served, since the Binding is in the Snapshot and is not run. rep is not changed; with nothing refused it is returned as it is.
func (r *refusals) overlay(rep config.Report) config.Report {
	r.mu.Lock()
	refused := maps.Clone(r.refused)
	r.mu.Unlock()
	if len(refused) == 0 || rep.Snapshot == nil {
		return rep
	}
	rejections := slices.Clone(rep.Rejections)
	for _, rb := range rep.Snapshot.Bindings() {
		conn := v1alpha1.NamespacedName(rb.Connection.Namespace, rb.Connection.Name)
		why, ok := refused[conn]
		if !ok {
			continue
		}
		rejections = append(rejections, config.Rejection{
			Namespace: rb.Binding.Namespace, Name: rb.Binding.Name, Reason: config.ReasonConnectionNotServed,
			Message:    fmt.Sprintf("spec.chat.connectionRef: ChatConnection %s is not served by this broker: %s", conn, why),
			Generation: rb.Binding.Generation, Served: rb.Binding.Generation,
		})
	}
	rep.Rejections = rejections
	return rep
}
