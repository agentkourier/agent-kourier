package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// servedRecord knows which chat connections this broker serves, and which it served and does not now (spec D51). A broker that stops
// serving a connection that still exists must tell the connection's Bindings so, once, because the last status a broker wrote
// (Ready=True, perhaps) would otherwise stay over a Binding that nobody serves, and must keep their sessions, whose threadTTL it does not
// know. While it runs it sees the connection leave the Snapshot; but the admission list is read at start, so a namespace taken off it is a
// restart, and a broker cannot remember what it served before one. The store does.
//
// The record in the store holds the connections served now and those served before that still exist in the cluster. A connection that the
// record has and the Snapshot has not, and that still exists, is stopped: it stays in the record, so that every later start finds it
// stopped too and a Binding made on it afterwards is told, until it is served again (it is then served and stays in the record) or its
// ChatConnection is deleted (it is then pruned; its Bindings are everyone's to report as ConnectionNotFound).
//
// The leader makes one and the status writer and the retention of sessions both read it, so both see the same stopped connections: the
// writer tells their Bindings, and the retention gives their sessions the horizon of an unserved Binding. The connections that the cap
// leaves out are in the report already, and are not told twice. It is safe for use from several goroutines.
type servedRecord struct {
	store  store.ConnectionServing
	naming func(namespace, name string) (crsource.ConnectionBindings, error)
	now    func() time.Time

	mu    sync.Mutex
	known []string        // what the store holds, once it has been read or written
	read  bool            // known is what the store held
	gone  map[string]bool // the connections recorded as served that the Snapshot does not have and that exist, "namespace/name"
}

// stoppedMessage is the Message of the UnservedConnection of a connection that the broker served and does not now.
const stoppedMessage = "this broker served it before and does not serve it now"

// newServedRecord returns a record kept in s, which finds the Bindings of a connection with naming
// ((*crsource.Source).ConnectionBindings) and dates what it records with now (time.Now if nil).
func newServedRecord(s store.ConnectionServing, naming func(namespace, name string) (crsource.ConnectionBindings, error), now func() time.Time) *servedRecord {
	if now == nil {
		now = time.Now
	}
	return &servedRecord{store: s, naming: naming, now: now, gone: map[string]bool{}}
}

// compare returns the connections that were recorded as served and are not served by the Snapshot of rep, that still exist, with
// their Bindings, and the names of those that are served. The caller holds r.mu. A failure to read the record, or to find the Bindings of
// one connection, comes back with what was found for the rest; a connection whose Bindings could not be found is kept.
func (r *servedRecord) compare(ctx context.Context, rep config.Report) (stopped []config.UnservedConnection, current []string, err error) {
	for _, c := range rep.Snapshot.ChatConnections() {
		current = append(current, c.Namespace+"/"+c.Name)
	}
	slices.Sort(current) // the record is sorted byte for byte, and the Snapshot is sorted by namespace and then name
	if !r.read {
		known, err := r.store.ServedConnections(ctx)
		if err != nil {
			return nil, current, fmt.Errorf("read the record of the served connections: %w", err)
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
	for _, k := range slices.Sorted(maps.Keys(r.gone)) {
		if left[k] {
			continue // the report says so already, with its own reason
		}
		namespace, name, _ := strings.Cut(k, "/")
		found, err := r.naming(namespace, name)
		if err != nil {
			errs = append(errs, fmt.Errorf("find the Bindings of the ChatConnection %s: %w", k, err))
			continue
		}
		if !found.Exists {
			delete(r.gone, k) // deleted: it leaves the record at the next write
			continue
		}
		stopped = append(stopped, config.UnservedConnection{Namespace: namespace, Name: name, Reason: config.UnservedStopped, Message: stoppedMessage,
			Bindings: found.Bindings, Admitted: found.Admitted})
	}
	return stopped, current, errors.Join(errs...)
}

// update compares the connections the Snapshot of rep serves with the record, and returns the stopped ones with their Bindings. It then
// records the connections served now and those stopped that still exist, if that differs from the record. Both steps are tried at every
// call: an error from one is returned with what the other found, and the call after it tries again.
func (r *servedRecord) update(ctx context.Context, rep config.Report) ([]config.UnservedConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stopped, current, err := r.compare(ctx, rep)
	if !r.read {
		return stopped, err
	}
	want := slices.Clone(current)
	for k := range r.gone {
		want = append(want, k)
	}
	slices.Sort(want)
	if !slices.Equal(want, r.known) {
		if werr := r.store.ReplaceServedConnections(ctx, want, r.now()); werr != nil {
			err = errors.Join(err, fmt.Errorf("record the served connections: %w", werr))
		} else {
			r.known = want
		}
	}
	return stopped, err
}

// stopped is update without the write: the connections the broker served and does not now, for the retention of sessions, which must
// know them before the writer's first pass has made one.
func (r *servedRecord) stopped(ctx context.Context, rep config.Report) ([]config.UnservedConnection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stopped, _, err := r.compare(ctx, rep)
	return stopped, err
}

// retentionKnows adds the Bindings of the stopped connections to the Bindings that known says exist and are not served, so that the
// retention gives their sessions the horizon of an unserved Binding and not that of a deleted one: their threadTTL is unknown, and may
// exceed the longest that is served. The report is read apart from known's, which is safe: a Binding that is served by one is served
// for the retention, which lets the served horizon win. If the stopped connections cannot be found, nothing is known, as before a config
// has loaded, and the retention deletes no session this tick.
func (r *servedRecord) retentionKnows(ctx context.Context, report func() config.Report, log *slog.Logger,
	known func() (map[string]time.Duration, []string, bool)) func() (map[string]time.Duration, []string, bool) {
	return func() (map[string]time.Duration, []string, bool) {
		ttls, unserved, ok := known()
		if !ok {
			return nil, nil, false
		}
		stopped, err := r.stopped(ctx, report())
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("retention: the connections this broker served before and does not now cannot be found, so no session is deleted this time", "error", err)
			}
			return nil, nil, false
		}
		for _, u := range stopped {
			unserved = append(unserved, u.Bindings...)
		}
		slices.Sort(unserved)
		return ttls, slices.Compact(unserved), true
	}
}
