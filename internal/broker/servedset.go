package broker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// servedRecord is the part of the status writer that knows which chat connections this broker serves (spec D51). A broker that stops
// serving a connection that still exists must tell the connection's Bindings so, once, because the last status a broker wrote
// (Ready=True, perhaps) would otherwise stay over a Binding that nobody serves. While it runs it sees the connection leave the
// Snapshot; but the admission list is read at start, so a namespace taken off it is a restart, and a broker cannot remember what it
// served before one. The store does: the record is the set the leader served when it last recorded it, and the leader that starts
// (or takes over, on another replica) compares it with the set it serves.
//
// A connection that the record has and the Snapshot has not, and that still exists, is stopped: the writer tells its Bindings. One that
// no longer exists was deleted, which every broker reports as ConnectionNotFound, so it is no concern of this. The record is replaced
// by the set served now as soon as it is compared, so that a later restart does not tell the Bindings again: the words stay as they
// were written, and a Binding made later on the connection gets none. The connections that the cap leaves out are in the report
// already, and are not told twice.
//
// A servedRecord belongs to the goroutine of the writer's passes.
type servedRecord struct {
	store  store.ConnectionServing
	naming func(namespace, name string) (bindings []string, exists bool, err error)
	now    func() time.Time

	known []string        // what the store holds, once it has been read or written
	read  bool            // known is what the store held
	gone  map[string]bool // the connections recorded as served that the Snapshot does not have, "namespace/name"
}

// stoppedMessage is the Message of the UnservedConnection of a connection that the broker served and does not now.
const stoppedMessage = "this broker served it before and does not serve it now"

func newServedRecord(s store.ConnectionServing, naming func(namespace, name string) ([]string, bool, error), now func() time.Time) *servedRecord {
	return &servedRecord{store: s, naming: naming, now: now, gone: map[string]bool{}}
}

// update compares the connections the Snapshot of rep serves with the record, and returns the connections that were recorded as served
// and are not, with their Bindings. It then records the set that is served, if that differs from the record. Both steps are tried
// at every call: an error from one is returned with what the other found, and the call after it tries again.
func (r *servedRecord) update(ctx context.Context, rep config.Report) ([]config.UnservedConnection, error) {
	current := make([]string, 0, len(rep.Snapshot.ChatConnections()))
	for _, c := range rep.Snapshot.ChatConnections() {
		current = append(current, c.Namespace+"/"+c.Name)
	}
	slices.Sort(current) // the record is sorted byte for byte, and the Snapshot is sorted by namespace and then name
	if !r.read {
		known, err := r.store.ServedConnections(ctx)
		if err != nil {
			return nil, fmt.Errorf("read the record of the served connections: %w", err)
		}
		r.known, r.read = known, true
	}
	for _, k := range r.known {
		if !slices.Contains(current, k) {
			r.gone[k] = true
		}
	}
	for _, k := range current {
		delete(r.gone, k) // served again
	}
	left := map[string]bool{}
	for _, u := range rep.UnservedConnections {
		left[u.Namespace+"/"+u.Name] = true
	}

	var errs []error
	var out []config.UnservedConnection
	for _, k := range slices.Sorted(maps.Keys(r.gone)) {
		if left[k] {
			continue // the report says so already, with its own reason
		}
		namespace, name, _ := strings.Cut(k, "/")
		bindings, exists, err := r.naming(namespace, name)
		if err != nil {
			errs = append(errs, fmt.Errorf("find the Bindings of the ChatConnection %s: %w", k, err))
			continue
		}
		if !exists {
			delete(r.gone, k)
			continue
		}
		out = append(out, config.UnservedConnection{Namespace: namespace, Name: name, Reason: config.UnservedStopped, Message: stoppedMessage, Bindings: bindings})
	}
	if !slices.Equal(current, r.known) {
		if err := r.store.ReplaceServedConnections(ctx, current, r.now()); err != nil {
			errs = append(errs, fmt.Errorf("record the served connections: %w", err))
		} else {
			r.known = current
		}
	}
	return out, errors.Join(errs...)
}
