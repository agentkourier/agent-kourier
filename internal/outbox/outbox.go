// Package outbox posts to a chat platform once, even across a crash (spec 8.6). A post is written to the
// store before it is sent, as a message in the neutral model (chat.Outgoing, versioned), tagged so that the
// platform can find it again, and its message ref is recorded after. A row with no ref may have been posted, so
// it is looked up before it is posted again. The platform renders the message at delivery, and the outbox knows
// nothing of its markup.
//
// Where a caller can wait for the platform, such as the renderer's own goroutine, it uses Send and gets the
// message's Ref. Where it cannot, such as the session's Notice, which runs on the chat platform's
// acknowledgement path, it uses Enqueue, a local write, and Run delivers the row. A notice's source ID must come
// from where it is, not from the event that carried it: the Expired and BindingGone notices for a mention take
// the message's (channel, ts), because a mention arrives as two events and both reach Submit, and one ID makes
// them one row and one post.
//
// A reaction (EnqueueReaction) is a row too, with the role reaction: durable and made once per ID, with no lookup,
// since making it twice is harmless (a reaction that is there already is success) and a reaction has no message of
// its own to find. Its ref is the ID of the message it was made on. Taking one off (EnqueueUnreaction) is a row of
// the same role, with an ID of its own made from the reaction's source, so the two find each other whichever is
// enqueued first: a removal cancels a reaction that is not yet made, and a reaction enqueued after its removal is
// not made. Without that, a reaction that failed and waits for a retry would be made after the removal, and stay.
//
// Every row names the ChatConnection whose app posts it (store.OutboxEntry.Connection), and the outbox looks the platform of
// that connection up (Config.Platforms) when it delivers the row. Each connection is delivered on its own: its rows go one at
// a time, in the order they were made, and another connection's rows go meanwhile, so an app that is slow, throttled or hung
// holds up its own rows and no other's. A row whose connection no platform runs (chat.ErrConnectionDown) stays pending, is
// not a failure of any platform, and is delivered once the connection runs; Config.Retention still abandons it.
//
// A post that can never succeed is abandoned, not retried for ever: what the platform says is permanent (Classify:
// a channel that is gone, text too long, content it refuses), a message that is not valid or a stored payload that
// is not one of this version (a legacy Slack payload, which is not migrated), and a lookup that has timed out five
// times in a row. The row keeps its place and loses its text, Config.OnAbandon is called once with the reason, one
// of the neutral reasons (chat.Reason) or an Abandon constant, and one warning is logged, with the platform's own
// code; the store keeps neither. A row still unposted after Config.Retention loses its text the same way. Send of
// an abandoned row returns ErrAbandoned and never posts it again; a producer that wants the same words posted anew
// needs a new source ID.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/testhooks"
)

// ErrAbandoned is returned for a post that Agent Kourier has given up on, and wraps its cause.
var ErrAbandoned = errors.New("outbox: the post was abandoned")

// The reasons OnAbandon is given besides the neutral reasons of chat.Reason.
const (
	AbandonInvalid       = "invalid"        // the message cannot be posted at all, or the stored payload cannot be read
	AbandonLookupTimeout = "lookup_timeout" // looking for it keeps timing out, with the platform otherwise working
	AbandonExpired       = "expired"        // it was still unposted after Config.Retention
)

// storeWriteTimeout bounds the store writes that must outlive a cancelled context (recording a ref,
// abandoning a row). It is not CallTimeout, which is for calls to the platform: a test that sets that to
// a few milliseconds must not make a local write time out under load.
const storeWriteTimeout = 10 * time.Second

// lookupTimeouts is how many lookups of a row in a row must time out, over at least
// Config.LookupTimeoutFloor and with some other platform call working in between, before it is
// abandoned.
const lookupTimeouts = 5

// Platform is what the outbox asks of the chat platform of one connection: chat.Platform, or a wrapper of it.
type Platform interface {
	// Post posts o tagged with tag and returns where it landed. Its error does not say whether the message
	// was posted.
	Post(ctx context.Context, t chat.Target, o chat.Outgoing, tag string) (chat.Ref, error)
	// Find returns the message tagged with tag, posted no earlier than about since.
	Find(ctx context.Context, t chat.Target, tag string, since time.Time) (chat.Ref, bool, error)
	// React puts a reaction on a message. One that is there already is success.
	React(ctx context.Context, r chat.Ref, name string) error
	// Unreact takes a reaction off a message. One that is not there is success.
	Unreact(ctx context.Context, r chat.Ref, name string) error
	// RefAt is the Ref of the message a row recorded by its ID alone.
	RefAt(channel, thread, id string) chat.Ref
	// Fits says whether a prompt can be shown in one message, which is checked before it is stored.
	Fits(p chat.Prompt) error
	// Classify says what an error of a call means: permanent errors abandon the row.
	Classify(op chat.Op, err error) chat.Verdict
}

// Config configures an Outbox. Store, Platforms and OnAbandon are required: New refuses a Config without them, and says which.
type Config struct {
	Store store.Outbox
	// Platforms gives the platform of the connection a row names, at each delivery. A connection it has none for is down: its
	// rows wait (chat.ErrConnectionDown).
	Platforms chat.Resolver[Platform]
	Now       func() time.Time // time.Now if nil
	Logger    *slog.Logger     // slog.Default() if nil
	// CallTimeout bounds each Post, Find and React (default 30 seconds), because the connection's delivery lock is
	// held across them and an unbounded call would hold it for good.
	CallTimeout time.Duration
	// Interval is how often Run drains with no signal (default 30 seconds).
	Interval time.Duration
	// RetryBase is the delay before a row whose delivery failed is tried again (default 5 seconds).
	// It doubles with each failure up to RetryMax (default 5 minutes).
	RetryBase, RetryMax time.Duration
	// Retention is how long a post may stay unposted before its text is dropped and it is
	// abandoned (default 7 days). A connection's delivery abandons its own rows that are past it, at each pass, before it
	// looks at them for anything else: a row whose connection nothing runs is abandoned too.
	Retention time.Duration
	// LookupTimeoutFloor is how long the lookups of a row must have been timing out, from the first
	// of five in a row, before it is abandoned for them (default 1 hour). The rule is for a lookup
	// that can never finish, not for an outage that hangs every request: so it also needs some
	// other platform call to have worked since the first timeout.
	LookupTimeoutFloor time.Duration
	// DefaultConnection is the connection ("namespace/name") of a Post or a Reaction that names none: the broker's own. Every
	// producer of the broker names its connection, so this is the answer for one that does not. Empty stores such a row with
	// no connection, as rows written before connections were recorded are.
	DefaultConnection string
	// OnAbandon is called once for each row that is abandoned, with its ID and the reason:
	// a neutral reason (chat.Reason) or AbandonInvalid, AbandonLookupTimeout or AbandonExpired. It is for a
	// metric, and runs under the delivery lock of the row's connection, so it must not block. Deliveries of different
	// connections run at the same time, so it may be called from several goroutines at once. Required: NoAbandon counts
	// nothing.
	OnAbandon func(id, reason string)
}

// NoAbandon is the OnAbandon of an Outbox whose abandoned rows are logged and counted nowhere.
func NoAbandon(_, _ string) {}

// Post is one message to deliver. Source and Role give its ID: Source is the event, turn or
// interaction it belongs to, and must differ for each message of one source and role.
type Post struct {
	Source string
	Role   store.OutboxRole
	// Connection is the ChatConnection ("namespace/name") whose app posts the message; Config.DefaultConnection if empty.
	Connection      string
	Channel, Thread string // Thread is empty for a message in the channel
	// Out is what to say, in the neutral model. It is stored as it is, so it must be valid (Validate), and a
	// prompt must fit (Platform.Fits): one that does not is store.ErrInvalid, which no retry mends.
	Out chat.Outgoing
}

// RepostedSource is the source a reply is posted under when it is posted again, under a row of its own, because the row of the
// first attempt cannot be found on the platform (a stream whose message was never recorded): the source of the first attempt,
// and the mark. The rating of such a reply names the first source, and finds the row by this one too.
func RepostedSource(source string) string { return source + "/recovered" }

// MentionSource is the source of the message that notifies user that a turn's reply named them (render.MentionMessage): a notice,
// in a row of its own for each person. That the row exists says the turn has mentioned the person, whatever became of it: its
// payload is cleared when it is delivered, so the row's ID is all that lasts.
func MentionSource(source, user string) string { return source + "/mention/" + user }

// ID returns the outbox ID of a message: deterministic, so a retry of the same event names the
// same row. An interaction prompt passes its interaction ID as source, so it cannot collide with
// an agent reply (spec 8.6 step 5).
func ID(source string, role store.OutboxRole) string { return source + "/" + string(role) }

// Outbox delivers posts. It is safe for concurrent use.
type Outbox struct {
	cfg Config
	// lanesMu guards lanes, the delivery state of each connection, by its name. It is held only to find or make a lane.
	lanesMu sync.Mutex
	lanes   map[string]*lane
	// retryMu guards retry, the rows whose delivery failed. It is held only across the map, never a call.
	retryMu sync.Mutex
	retry   map[string]retryState
	// pairMu makes the rows of a reaction and of its removal come in one order whichever is enqueued first:
	// EnqueueReaction looks for the removal and writes its own row under it, as EnqueueUnreaction writes the
	// removal's, so that no reaction is written after its removal was delivered. It is held only across local
	// writes, never a platform call, and never with a lane's lock held, so Enqueue never waits for a delivery.
	pairMu sync.Mutex
	wake   chan struct{} // Enqueue's signal to Run; one slot, since a drain takes every row
}

// lane is the delivery of one connection's rows.
type lane struct {
	// mu allows one delivery at a time on the connection. A post's outcome is unknown until its ref is recorded, so
	// two deliveries of one row could post it twice. It is the connection's alone: the rows of another connection are other
	// rows, so a delivery that is slow, throttled or hung holds up this connection and no other.
	mu sync.Mutex
	// platformOK is when a call to this connection's platform last worked, which the abandoning of a lookup that keeps
	// timing out needs: another connection's platform working says nothing of this one. Guarded by mu.
	platformOK time.Time

	// stateMu guards draining, that Run has a goroutine delivering this connection's rows, and missed, that a pass found it so
	// and left rows for it, so that the goroutine asks for another pass when it ends.
	stateMu          sync.Mutex
	draining, missed bool
}

// begin says whether the caller may start Run's goroutine for the lane, and if one is running already, notes that rows
// were left for it.
func (l *lane) begin() bool {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	if l.draining {
		l.missed = true
		return false
	}
	l.draining = true
	return true
}

// end ends the lane's goroutine, and says whether a pass left rows for it meanwhile.
func (l *lane) end() (again bool) {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	l.draining, again, l.missed = false, l.missed, false
	return again
}

// lane returns the lane of conn.
func (o *Outbox) lane(conn string) *lane {
	o.lanesMu.Lock()
	defer o.lanesMu.Unlock()
	l, ok := o.lanes[conn]
	if !ok {
		l = &lane{}
		o.lanes[conn] = l
	}
	return l
}

// retryState is how often a row's delivery has failed in a row, and when it may be tried again.
type retryState struct {
	fails    int
	timeouts int // lookups in a row that timed out
	// firstTimeout is when the first of them did.
	firstTimeout time.Time
	next         time.Time
}

// New returns an Outbox over cfg. It refuses a Config without its Store, Platforms or OnAbandon, naming the field, so that a
// wiring that leaves one out fails at start-up.
func New(cfg Config) (*Outbox, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("outbox: Config.Store is required")
	case cfg.Platforms == nil:
		return nil, errors.New("outbox: Config.Platforms is required")
	case cfg.OnAbandon == nil:
		return nil, errors.New("outbox: Config.OnAbandon is required (NoAbandon counts nothing)")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = 30 * time.Second
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 5 * time.Second
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = 5 * time.Minute
	}
	if cfg.LookupTimeoutFloor <= 0 {
		cfg.LookupTimeoutFloor = time.Hour
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 7 * 24 * time.Hour
	}
	return &Outbox{cfg: cfg, wake: make(chan struct{}, 1), lanes: map[string]*lane{}, retry: map[string]retryState{}}, nil
}

// Send writes the row, posts the message and records its ID, and returns where it is. A message that
// is already recorded is returned without a call to the platform. A row that exists without a ref, left
// by a failed post or a crash, is looked up first and posted only if the platform has no such message.
// A new row is posted at once. If the lookup fails nothing is posted, and the error is returned.
func (o *Outbox) Send(ctx context.Context, p Post) (chat.Ref, error) {
	row, err := o.row(p)
	if err != nil {
		return chat.Ref{}, err
	}
	ln := o.lane(row.Connection)
	ln.mu.Lock()
	defer ln.mu.Unlock()
	created, err := o.cfg.Store.CreateOutbox(ctx, row)
	if err != nil {
		return chat.Ref{}, err
	}
	ref, err := o.deliver(ctx, ln, row.ID, !created)
	if err == nil {
		o.settle(row.ID, nil) // a row Run was backing off is delivered; forget it
	}
	return ref, err
}

// row returns the outbox row for p, with the message as its payload. A message that is not valid, or a prompt that
// does not fit, is ErrInvalid: such a row would never be delivered, and would hold its ID for good.
func (o *Outbox) row(p Post) (store.OutboxEntry, error) {
	row := store.OutboxEntry{ID: ID(p.Source, p.Role), EventID: p.Source, Role: p.Role, Connection: o.connection(p.Connection), Channel: p.Channel, Thread: p.Thread, CreatedAt: o.cfg.Now()}
	if p.Role == store.OutboxReaction {
		return store.OutboxEntry{}, fmt.Errorf("outbox post %s: a reaction is not a post, use EnqueueReaction: %w", row.ID, store.ErrInvalid)
	}
	if err := p.Out.Validate(); err != nil { // no retry mends a message that cannot be shown
		return store.OutboxEntry{}, fmt.Errorf("outbox post %s: %w: %w", row.ID, store.ErrInvalid, err)
	}
	if p.Out.Prompt != nil {
		pl, err := o.cfg.Platforms.Platform(row.Connection)
		if err != nil { // the connection is not running: the prompt cannot be checked, which is no fault of the prompt
			return store.OutboxEntry{}, fmt.Errorf("outbox post %s: %w", row.ID, err)
		}
		if err := pl.Fits(*p.Out.Prompt); err != nil {
			return store.OutboxEntry{}, fmt.Errorf("outbox post %s: %w: %w", row.ID, store.ErrInvalid, err)
		}
	}
	payload, err := json.Marshal(p.Out)
	if err != nil {
		return store.OutboxEntry{}, fmt.Errorf("outbox post %s: %w", row.ID, err)
	}
	row.Payload = string(payload)
	return row, nil
}

// Enqueue writes the row for p and returns its ID, and does nothing else: no platform call, so it is
// safe where the platform waits for an answer, such as the session's Notice. Run delivers the row. The
// same ID enqueued twice is one row and one post. A message that is not valid is ErrInvalid.
func (o *Outbox) Enqueue(ctx context.Context, p Post) (string, error) {
	row, err := o.row(p)
	if err != nil {
		return "", err
	}
	return o.enqueue(ctx, row)
}

// enqueue writes row and wakes Run.
func (o *Outbox) enqueue(ctx context.Context, row store.OutboxEntry) (string, error) {
	if _, err := o.cfg.Store.CreateOutbox(ctx, row); err != nil {
		return "", err
	}
	select {
	case o.wake <- struct{}{}:
	default: // a signal is already waiting, and the drain it starts will take this row too
	}
	return row.ID, nil
}

// Reaction is a reaction to make on the message TS in Channel, or to take off it: Name is the bare emoji
// name. Source gives the row's ID, which is ID(Source, store.OutboxReaction) for the reaction made and
// UnreactionID(Source) for the one taken back, and must differ for each reaction of one source. A reaction
// and the removal of it share a Source: that is how they find each other.
type Reaction struct {
	Source, Channel, TS, Name string
	// Connection is the ChatConnection ("namespace/name") whose app makes the reaction; Config.DefaultConnection if empty.
	Connection string
}

// reactionPayload is a reaction row's payload. Remove makes it the removal of the reaction: a payload
// with none, which is every one stored before removals, is a reaction to make.
type reactionPayload struct {
	TS     string `json:"ts"`
	Name   string `json:"name"`
	Remove bool   `json:"remove,omitempty"`
}

// UnreactionID returns the outbox ID of the removal of the reaction of source: ID(source, store.OutboxReaction)
// is the reaction itself.
func UnreactionID(source string) string { return source + "/unreaction" }

// EnqueueReaction writes the row for r and returns its ID, and does nothing else: no platform call, so
// it is safe where the platform waits for an answer. Run delivers the row. The same ID enqueued twice is
// one row and one reaction. An empty field is ErrInvalid. A reaction whose removal is enqueued already
// (EnqueueUnreaction, for the same Source) is not made at all, and gets no row, so that a removal that
// comes first is never undone by the reaction that comes after it.
func (o *Outbox) EnqueueReaction(ctx context.Context, r Reaction) (string, error) {
	id := ID(r.Source, store.OutboxReaction)
	row, err := o.reactionRow(id, r, false)
	if err != nil {
		return "", err
	}
	o.pairMu.Lock()
	defer o.pairMu.Unlock()
	switch _, err := o.cfg.Store.GetOutbox(ctx, UnreactionID(r.Source)); {
	case err == nil:
		return id, nil
	case !errors.Is(err, store.ErrNotFound):
		return "", fmt.Errorf("outbox reaction %s: look for its removal: %w", id, err)
	}
	return o.enqueue(ctx, row)
}

// EnqueueUnreaction writes the row that takes the reaction r off its message, which was or will be made with
// EnqueueReaction of the same Source, and returns its ID. Like EnqueueReaction it makes no platform call.
// Its ID is UnreactionID(r.Source), so asking twice is one row. It is safe even if the reaction was never
// enqueued, or is still waiting to be made: delivery cancels a reaction not yet made, and takes off only one
// that was or may have been (see unreact). An empty field is ErrInvalid.
func (o *Outbox) EnqueueUnreaction(ctx context.Context, r Reaction) (string, error) {
	id := UnreactionID(r.Source)
	row, err := o.reactionRow(id, r, true)
	if err != nil {
		return "", err
	}
	o.pairMu.Lock()
	defer o.pairMu.Unlock()
	return o.enqueue(ctx, row)
}

// reactionRow is the row id of r: the reaction to make, or with remove the removal of it. An empty field is
// ErrInvalid.
func (o *Outbox) reactionRow(id string, r Reaction, remove bool) (store.OutboxEntry, error) {
	if r.Source == "" || r.Channel == "" || r.TS == "" || r.Name == "" {
		return store.OutboxEntry{}, fmt.Errorf("outbox reaction %s: the source, channel, ts and name are required: %w", id, store.ErrInvalid)
	}
	payload, err := json.Marshal(reactionPayload{TS: r.TS, Name: r.Name, Remove: remove})
	if err != nil {
		return store.OutboxEntry{}, fmt.Errorf("outbox reaction %s: %w", id, err)
	}
	return store.OutboxEntry{ID: id, EventID: r.Source, Role: store.OutboxReaction, Connection: o.connection(r.Connection), Channel: r.Channel, CreatedAt: o.cfg.Now(), Payload: string(payload)}, nil
}

// connection is the connection of a row whose producer named c: c, or Config.DefaultConnection if it named none.
func (o *Outbox) connection(c string) string {
	if c != "" {
		return c
	}
	return o.cfg.DefaultConnection
}

// Run drains the outbox until ctx ends, and returns its error. It drains at once, which recovers
// what a crash left, then on each Enqueue and every Interval. Each connection with rows to deliver is drained by a
// goroutine of its own, so a connection whose platform is slow or hung holds up its own rows and no other connection's;
// Run returns once those goroutines have, which ctx ends. A row that fails is tried again after
// a delay that doubles from RetryBase up to RetryMax, so one that always fails does not spin. Call
// it once.
func (o *Outbox) Run(ctx context.Context) error {
	tick := time.NewTicker(o.cfg.Interval)
	defer tick.Stop()
	var drains sync.WaitGroup
	defer drains.Wait()
	for {
		o.dispatch(ctx, &drains)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-o.wake:
		case <-tick.C:
		}
	}
}

// dispatch is a pass of Run: it lists what is unposted, and starts a goroutine for each connection that has rows and
// has none running. A connection whose goroutine is still running is left its rows, and the goroutine asks for another pass
// when it ends.
func (o *Outbox) dispatch(ctx context.Context, drains *sync.WaitGroup) {
	byConn, err := o.unposted(ctx)
	if err != nil {
		if ctx.Err() == nil {
			o.cfg.Logger.WarnContext(ctx, "outbox: some posts were not delivered; they will be tried again", "err", err)
		}
		return
	}
	for conn, rows := range byConn {
		ln := o.lane(conn)
		if !ln.begin() {
			continue
		}
		drains.Add(1)
		go func() {
			defer drains.Done()
			err := o.drainLane(ctx, conn, rows)
			if ln.end() {
				o.signal()
			}
			if err != nil && ctx.Err() == nil {
				o.cfg.Logger.WarnContext(ctx, "outbox: some posts were not delivered; they will be tried again", "err", err)
			}
		}()
	}
}

// signal wakes Run, unless a wake is waiting already.
func (o *Outbox) signal() {
	select {
	case o.wake <- struct{}{}:
	default: // a signal is already waiting, and the drain it starts will take every row
	}
}

// settle records the outcome of delivering a row: a failure sets the delay before its next try, a
// success forgets the row.
func (o *Outbox) settle(id string, err error) {
	if err == nil || errors.Is(err, ErrAbandoned) {
		o.dropRetry(id)
		return
	}
	o.retryMu.Lock()
	defer o.retryMu.Unlock()
	r := o.retry[id]
	r.fails++
	delay := o.cfg.RetryBase
	for i := 1; i < r.fails && delay < o.cfg.RetryMax; i++ {
		delay *= 2
	}
	r.next = o.cfg.Now().Add(min(delay, o.cfg.RetryMax))
	o.retry[id] = r
}

// dropRetry forgets what is known of the failed deliveries of the row id.
func (o *Outbox) dropRetry(id string) {
	o.retryMu.Lock()
	delete(o.retry, id)
	o.retryMu.Unlock()
}

// backingOff says whether the row id failed its last delivery and is not to be tried again yet.
func (o *Outbox) backingOff(id string) bool {
	o.retryMu.Lock()
	defer o.retryMu.Unlock()
	next, ok := o.retry[id]
	return ok && o.cfg.Now().Before(next.next)
}

// Reconcile delivers every row that has a payload and no ref, oldest first, looking each up
// before it posts: it is for startup, after a crash may have left posts unrecorded, and is one
// pass of Run, made in the caller's goroutine and returning when every connection has been drained. A connection's rows go
// one at a time and the connections go at once, so one whose platform hangs holds up the return and no other connection's
// rows. A row with no payload is not one of Send's or Enqueue's (they refuse an invalid
// message): it is a stream's, whose renderer records the ref itself, and is left alone, as is a
// row still waiting out its retry delay, and a row whose connection no platform runs (chat.ErrConnectionDown), which waits
// for the connection. A row past Retention is abandoned, as every pass does it, before anything else is tried with it. It
// goes on past a row it cannot reconcile, and returns every such error joined.
func (o *Outbox) Reconcile(ctx context.Context) error {
	byConn, err := o.unposted(ctx)
	if err != nil {
		return err
	}
	var (
		errs []error
		wg   sync.WaitGroup
		mu   sync.Mutex
	)
	for conn, rows := range byConn {
		wg.Go(func() {
			if err := o.drainLane(ctx, conn, rows); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// unposted lists the rows to deliver, with a payload and no ref, by connection, each connection's oldest first.
func (o *Outbox) unposted(ctx context.Context) (map[string][]store.OutboxEntry, error) {
	rows, err := o.cfg.Store.ListUnposted(ctx)
	if err != nil {
		return nil, fmt.Errorf("reconcile outbox: %w", err)
	}
	byConn := map[string][]store.OutboxEntry{}
	for _, r := range rows {
		if r.Payload != "" {
			conn := o.connection(r.Connection)
			byConn[conn] = append(byConn[conn], r)
		}
	}
	return byConn, nil
}

// drainLane delivers rows, which are the unposted rows of conn, oldest first, one at a time under the connection's lock, and
// returns the errors of those that failed, joined. A row past Retention is abandoned first, under the lock, which keeps it from
// a post in flight; so is one whose connection is down, which is how its text goes after the retention though nothing posts it.
// It goes on past a row it cannot deliver, and stops at the first that finds the connection down: no row of it can be delivered
// before the connection runs, and nothing is counted against any platform for that. The rows after it are younger, so none of
// them is past Retention if it is not.
func (o *Outbox) drainLane(ctx context.Context, conn string, rows []store.OutboxEntry) error {
	ln := o.lane(conn)
	var errs []error
	for _, r := range rows {
		ln.mu.Lock()
		gone, err := o.expire(ctx, r)
		if err != nil {
			errs = append(errs, err)
		}
		if gone || o.backingOff(r.ID) {
			ln.mu.Unlock()
			continue
		}
		_, err = o.deliver(ctx, ln, r.ID, true)
		down := errors.Is(err, chat.ErrConnectionDown)
		if !down {
			o.settle(r.ID, err)
		}
		ln.mu.Unlock()
		if down {
			o.cfg.Logger.DebugContext(ctx, "outbox: the connection of some posts is not running; they wait for it", "connection", conn)
			break
		}
		if !errors.Is(err, ErrAbandoned) { // an abandon is reported where it happens
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// expire abandons the row r, as listed, if it is past Retention, and says whether it is no longer to be delivered: it was
// abandoned, or it has been posted or abandoned since it was listed. The caller holds the lock of the row's connection, so no
// delivery of it is in flight. A row that cannot be abandoned is delivered as any other, and the error is returned.
func (o *Outbox) expire(ctx context.Context, r store.OutboxEntry) (gone bool, err error) {
	now := o.cfg.Now()
	if !r.CreatedAt.Before(now.Add(-o.cfg.Retention)) {
		return false, nil
	}
	newly, err := o.cfg.Store.AbandonOutbox(ctx, r.ID, now)
	switch {
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound): // posted, or gone, since it was listed
		return true, nil
	case err != nil:
		return false, fmt.Errorf("drop expired outbox text of %s: %w", r.ID, err)
	}
	o.dropRetry(r.ID)
	if newly {
		o.reportAbandon(ctx, r.ID, AbandonExpired, "", nil)
	}
	return true, nil
}

// target is where a row's message goes.
func (o *Outbox) target(row store.OutboxEntry) chat.Target {
	return chat.Target{Conn: o.connection(row.Connection), Channel: row.Channel, Thread: row.Thread}
}

// permanent says whether err, of a call of the kind op, is one that no retry mends, and if so why: the neutral
// reason and the platform's own code.
func permanent(pl Platform, op chat.Op, err error) (reason, code string, ok bool) {
	v := pl.Classify(op, err)
	if v.Class != chat.Permanent {
		return "", "", false
	}
	return string(orOther(v.Reason)), v.Code, true
}

// orOther is r, or chat.ReasonOther for a permanent error that gave none.
func orOther(r chat.Reason) chat.Reason {
	if r == chat.ReasonNone {
		return chat.ReasonOther
	}
	return r
}

// decode reads the payload of a row as the message it holds. A payload that is not a message of this version is
// an error, such as a legacy Slack payload (Text and Blocks, with no version), which is not migrated.
func decode(payload string) (chat.Outgoing, error) {
	var out chat.Outgoing
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		return chat.Outgoing{}, err
	}
	if out.V != chat.OutgoingVersion {
		return chat.Outgoing{}, fmt.Errorf("%w: the payload is of version %d, not %d: a legacy payload, which is not migrated", chat.ErrInvalid, out.V, chat.OutgoingVersion)
	}
	if err := out.Validate(); err != nil {
		return chat.Outgoing{}, err
	}
	return out, nil
}

// deliver posts the row id unless it already has a ref, which it returns. With lookup it first
// asks the platform for the message, and adopts it if it is there. The platform is the one of the row's connection; with none
// running the error wraps chat.ErrConnectionDown, and nothing was tried. The caller holds ln.mu, the lane of the row's
// connection. The row is read again here, so a delivery that finished while the caller waited for the
// lock is seen.
func (o *Outbox) deliver(ctx context.Context, ln *lane, id string, lookup bool) (telemetryRef chat.Ref, telemetryErr error) {
	row, err := o.cfg.Store.GetOutbox(ctx, id)
	if err != nil {
		return chat.Ref{}, err
	}
	pl, err := o.cfg.Platforms.Platform(o.connection(row.Connection)) // a row nobody has attributed yet is the own connection's
	if err != nil {
		return chat.Ref{}, fmt.Errorf("deliver %s: %w", id, err)
	}
	if row.MessageRef != "" {
		return pl.RefAt(row.Channel, row.Thread, row.MessageRef), nil
	}
	ctx = telemetry.Restore(ctx, row.TraceParent, row.TraceState)
	ctx, op := telemetry.Start(ctx, "agentkourier.outbox.deliver", attribute.String("agentkourier.outbox.id", id), attribute.String("agentkourier.outbox.role", string(row.Role)))
	mode := "posted"
	defer func() {
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("agentkourier.delivery.mode", mode))
		if telemetryErr == nil && telemetryRef.ID != "" {
			telemetry.Observe(ctx, "agentkourier_outbox_delivery_duration_seconds", o.cfg.Now().Sub(row.CreatedAt), string(row.Role), mode)
		}
		op.Finish(telemetryErr)
	}()
	if !row.AbandonedAt.IsZero() {
		return chat.Ref{}, fmt.Errorf("deliver %s: %w", id, ErrAbandoned)
	}
	if row.Role == store.OutboxReaction {
		return o.react(ctx, ln, pl, row)
	}
	if row.Payload == "" {
		return chat.Ref{}, fmt.Errorf("deliver %s: the row has no payload: %w", id, store.ErrInvalid)
	}
	out, err := decode(row.Payload)
	if err != nil {
		return chat.Ref{}, o.abandon(ctx, row, AbandonInvalid, "", fmt.Errorf("deliver %s: payload: %w", id, err))
	}
	if lookup {
		ref, found, err := o.find(ctx, pl, row)
		if err == nil {
			ln.platformOK = o.cfg.Now()
		} else if reason, code, ok := permanent(pl, chat.OpPost, err); ok { // as for a post: it will not pass
			return chat.Ref{}, o.abandon(ctx, row, reason, code, fmt.Errorf("deliver %s: looking for the message: %w", id, err))
		}
		timedOut := err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded)
		if o.lookupGivesUp(ln, id, timedOut) {
			return chat.Ref{}, o.abandon(ctx, row, AbandonLookupTimeout, "", fmt.Errorf("deliver %s: looking for the message: %w", id, err))
		}
		if err != nil {
			return chat.Ref{}, fmt.Errorf("deliver %s: looking for the message before posting it: %w", id, err)
		}
		if found {
			mode = "adopted"
			trace.SpanFromContext(ctx).SetAttributes(attribute.Bool("agentkourier.delivery.adopted", true))
			o.cfg.Logger.InfoContext(ctx, "outbox: adopted a message posted before a crash", "outbox_id", id, "channel", row.Channel, "ts", ref.ID)
			return o.record(ctx, id, ref)
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, o.cfg.CallTimeout)
	defer cancel()
	ref, err := pl.Post(callCtx, o.target(row), out, id)
	if err != nil {
		err = fmt.Errorf("deliver %s: post: %w", id, err)
		if reason, code, ok := permanent(pl, chat.OpPost, err); ok {
			return chat.Ref{}, o.abandon(ctx, row, reason, code, err)
		}
		if errors.Is(err, store.ErrInvalid) || errors.Is(err, chat.ErrInvalid) {
			return chat.Ref{}, o.abandon(ctx, row, AbandonInvalid, "", err)
		}
		return chat.Ref{}, err
	}
	ln.platformOK = o.cfg.Now()
	testhooks.AfterPost() // a no-op unless this is a test image, where it can crash the process here
	return o.record(ctx, id, ref)
}

// react makes the reaction of row, or takes it off when the row is a removal, and records the ID of its
// message as the ref. There is no lookup: a reaction made twice is made once, one taken off twice is taken
// off once, and one made before a crash is simply found there. The caller holds ln.mu, as deliver's does.
func (o *Outbox) react(ctx context.Context, ln *lane, pl Platform, row store.OutboxEntry) (chat.Ref, error) {
	var p reactionPayload
	if err := json.Unmarshal([]byte(row.Payload), &p); err != nil || p.TS == "" || p.Name == "" {
		return chat.Ref{}, o.abandon(ctx, row, AbandonInvalid, "", fmt.Errorf("deliver %s: the reaction's payload is not usable: %w", row.ID, store.ErrInvalid))
	}
	ref := pl.RefAt(row.Channel, "", p.TS)
	call, verb := pl.React, "react"
	if p.Remove {
		call, verb = pl.Unreact, "unreact"
		needed, err := o.unreact(ctx, row)
		if err != nil {
			return chat.Ref{}, err
		}
		if !needed {
			return o.record(ctx, row.ID, ref) // nothing was made, so nothing is taken off
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, o.cfg.CallTimeout)
	defer cancel()
	if err := call(callCtx, ref, p.Name); err != nil {
		err = fmt.Errorf("deliver %s: %s: %w", row.ID, verb, err)
		if reason, code, ok := permanent(pl, chat.OpReact, err); ok {
			return chat.Ref{}, o.abandon(ctx, row, reason, code, err)
		}
		if errors.Is(err, store.ErrInvalid) || errors.Is(err, chat.ErrInvalid) {
			return chat.Ref{}, o.abandon(ctx, row, AbandonInvalid, "", err)
		}
		return chat.Ref{}, err
	}
	ln.platformOK = o.cfg.Now()
	return o.record(ctx, row.ID, ref)
}

// unreact settles the reaction that the removal row takes off, and reports whether the platform has to be
// asked to take it off. The reaction's row is the one with the removal's source (see Reaction):
//   - There is none: nothing was ever asked for, so there is nothing to take off.
//   - There is one, in any state: the platform is asked. A reaction that was made has a ref. One that is
//     unposted may have been made all the same (a call that timed out or was reset, a delivery that a crash cut
//     short), and so may one that was abandoned, whether the platform refused it or an earlier delivery of this
//     removal cancelled it and then failed to take it off. no_reaction is success, and a reaction that the
//     platform refused for good makes the removal fail for good, which does no harm.
//   - If it is still unposted it is also cancelled, so that it is never made after the removal, which would
//     leave it for good.
//
// The caller holds the connection's lock, so no delivery of the reaction is in flight.
func (o *Outbox) unreact(ctx context.Context, row store.OutboxEntry) (needed bool, err error) {
	addID := ID(row.EventID, store.OutboxReaction)
	add, err := o.cfg.Store.GetOutbox(ctx, addID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("deliver %s: read the reaction it takes off: %w", row.ID, err)
	case add.MessageRef != "" || !add.AbandonedAt.IsZero():
		return true, nil
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeWriteTimeout)
	defer cancel()
	if _, err := o.cfg.Store.AbandonOutbox(wctx, addID, o.cfg.Now()); err != nil {
		return false, fmt.Errorf("deliver %s: cancel the reaction it takes off: %w", row.ID, err)
	}
	o.dropRetry(addID)
	return true, nil
}

// lookupGivesUp notes how a lookup of id ended and reports whether the row should now be abandoned
// for its lookups: a timeout adds one to the count in a row, and any other end starts it again.
// It needs five, spread over LookupTimeoutFloor from the first, and some other platform call that has
// worked since that first one; otherwise it is the platform that is down, not this lookup that cannot
// finish. The platform is the row's connection's: the calls of another connection's say nothing of it. The caller holds ln.mu.
func (o *Outbox) lookupGivesUp(ln *lane, id string, timedOut bool) bool {
	o.retryMu.Lock()
	defer o.retryMu.Unlock()
	r, ok := o.retry[id]
	if !timedOut {
		if ok {
			r.timeouts, r.firstTimeout = 0, time.Time{}
			o.retry[id] = r
		}
		return false
	}
	now := o.cfg.Now()
	if r.timeouts == 0 {
		r.firstTimeout = now
	}
	r.timeouts++
	o.retry[id] = r
	return r.timeouts >= lookupTimeouts && now.Sub(r.firstTimeout) >= o.cfg.LookupTimeoutFloor && ln.platformOK.After(r.firstTimeout)
}

// abandon gives up on the row, which will never post, and returns an error that wraps ErrAbandoned
// and cause. If the store cannot record it, the row is still work and that error is returned alone.
// code is the platform's own code for a permanent error, for the log and nothing else. The caller holds the connection's lock.
func (o *Outbox) abandon(ctx context.Context, row store.OutboxEntry, reason, code string, cause error) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeWriteTimeout)
	defer cancel()
	newly, err := o.cfg.Store.AbandonOutbox(wctx, row.ID, o.cfg.Now())
	if err != nil {
		return fmt.Errorf("abandon %s (%s): %w", row.ID, reason, err)
	}
	o.dropRetry(row.ID)
	if newly {
		o.reportAbandon(ctx, row.ID, reason, code, cause)
	}
	return fmt.Errorf("%w (%s): %w", ErrAbandoned, reason, cause)
}

// reportAbandon logs one warning, never the text, and calls OnAbandon. The platform's code is in the log and
// not in the call, so that a metric's labels are the closed set of reasons.
func (o *Outbox) reportAbandon(ctx context.Context, id, reason, code string, cause error) {
	attrs := []any{"outbox_id", id, "reason", reason}
	if code != "" {
		attrs = append(attrs, "code", code)
	}
	if cause != nil { // an expired row has none
		attrs = append(attrs, "err", cause)
	}
	o.cfg.Logger.WarnContext(ctx, "outbox: a post was abandoned and will not be retried", attrs...)
	o.cfg.OnAbandon(id, reason)
}

// find looks for the row's message, within the call timeout.
func (o *Outbox) find(ctx context.Context, pl Platform, row store.OutboxEntry) (chat.Ref, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, o.cfg.CallTimeout)
	defer cancel()
	return pl.Find(ctx, o.target(row), row.ID, row.CreatedAt)
}

// record stores the message's ID on the row and returns its Ref. The message is already on the platform, so it
// is recorded even if ctx has ended (a shutdown right after the post), within storeWriteTimeout; a
// failure here leaves it for the next lookup to adopt.
func (o *Outbox) record(ctx context.Context, id string, ref chat.Ref) (chat.Ref, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeWriteTimeout)
	defer cancel()
	if err := o.cfg.Store.RecordMessageRef(ctx, id, ref.ID); err != nil {
		return chat.Ref{}, fmt.Errorf("deliver %s: record the message ref: %w", id, err)
	}
	return ref, nil
}
