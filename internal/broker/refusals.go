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
// (spec D52), with why. The Snapshot serves their Bindings, so the status writer would call them accepted; overlay adds the rejection that says
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

// served says whether the connection conn is not one of those refused: this broker holds its Bindings in the Snapshot and does not run it. It is
// the part of "served" that the Snapshot cannot say, since a refused connection's Bindings are in it (connectionServed adds the rest).
func (r *refusals) served(conn string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, refused := r.refused[conn]
	return !refused
}

// connectionServed returns the Served of the timeout sweep: whether this broker serves the chat connection conn ("namespace/name"). The own
// connection is served, as it runs for as long as the process does; own is "" when the broker has none (spec D53), and then no name is the own's,
// the empty one included. Another is served when the Snapshot of report has it and the supervisor has not
// refused it. One the Snapshot lacks is not in the config (deleted, invalid with no good generation, of a namespace not admitted, or past the 16) and cannot
// run while it stays so, whichever connection the Bindings name now: a Binding can be pointed at another connection while a pause waits on a thread of the first. A connection that
// is served and does not run for now (it has not started, or its credentials were rejected) is served, and its pauses wait for it. Before the first
// load of the config nothing is known, and every connection is served.
func connectionServed(report func() config.Report, own string, r *refusals) func(conn string) bool {
	return func(conn string) bool {
		if own != "" && conn == own {
			return true
		}
		snap := report().Snapshot
		if snap == nil {
			return true
		}
		for _, c := range snap.ChatConnections() {
			if v1alpha1.NamespacedName(c.Namespace, c.Name) == conn {
				return r.served(conn)
			}
		}
		return false
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
