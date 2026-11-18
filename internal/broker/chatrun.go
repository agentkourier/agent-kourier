package broker

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/identity"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

var (
	// ErrChatAuth is wrapped by the error RunPlatform returns when the chat platform rejected the credentials for good: for the
	// broker's own connection the process exits (cmd/agent-kourier maps it to its exit code), with the platform and its code in the
	// error, because no retry would help. Its own text is the one of a platform that is not named; the error that wraps it names it.
	ErrChatAuth = errors.New("the chat platform rejected the tokens")
	// ErrWiring is wrapped by an error that means the broker was put together wrongly, or its
	// config is invalid or asks for what v1 does not do: a usage error, not a failure of a dependency.
	ErrWiring = errors.New("the broker cannot be wired")
)

// PlatformRunner is a chat platform as RunPlatform runs it: chat.Platform. Run holds the connection until it fails or
// ctx ends, and Classify says which of its errors no retry cures (chat.FatalAuth).
type PlatformRunner interface {
	Run(ctx context.Context, sink chat.Sink) error
	Classify(op chat.Op, err error) chat.Verdict
}

// The defaults of RunPolicy. They are the limits docs/reference/limits.md states.
const (
	defaultRunBase       = time.Second
	defaultRunMax        = time.Minute
	defaultRunResetAfter = 120 * time.Second
	// retryAfterMax is the longest wait that a platform's own answer (Classify's RetryAfter) may ask for. A longer one is cut to it:
	// slack-go reads Retry-After as any number of seconds, so a faulty or hostile answer would park the connection for good.
	retryAfterMax = 5 * time.Minute
)

// RunPolicy is how RunPlatform paces its retries. Zero fields take their defaults.
type RunPolicy struct {
	// Base is the first pause after a failure, doubling up to Max. Defaults: 1 s and 60 s.
	Base, Max time.Duration
	// ResetAfter is how long a connection must have been up for the next failure to start the
	// backoff again from Base. Default 120 s.
	ResetAfter time.Duration
	// Now and After are the clock; a test injects its own. Defaults: time.Now and time.After.
	Now    func() time.Time
	After  func(time.Duration) <-chan time.Time
	Logger *slog.Logger // slog.Default() if nil
	// Platform is the connection's platform as ChatConnection.spec.platform spells it ("slack"), which the warning of a connection that
	// ended and the error of a rejection name. Empty, they say the chat platform.
	Platform string
	// Random is the jitter's source: a number in [0, 1). The wait is the pause cut by that share of its upper half, so
	// between half the pause and the pause. A test injects its own, one per connection when it runs several, and a Random
	// of 0 cuts nothing. Default: math/rand/v2's Float64. A number outside [0, 1) is held to its bounds.
	Random func() float64
}

// RunPlatform runs the chat connection for as long as ctx lives. An adapter reconnects by itself only for a refresh that
// the platform asks for and for the first attempt after a loss, so when Run returns, the connection was lost and could not
// be opened again, or never opened. An error that the platform classifies as credentials it rejects (chat.FatalAuth) ends
// RunPlatform at once with ErrChatAuth, naming p.Platform, since no retry cures it. Any other error is retried after a wait. The pause
// doubles from Base to Max and starts over from Base once a run has lasted ResetAfter; the wait is the pause cut at random
// by up to half of it, so that connections that fail together (a network that blips) do not retry together. Max is the
// longest wait, except for a wait the platform asks for (Classify's RetryAfter, as for a rate limit), which is waited
// for when it is longer, up to retryAfterMax. It returns an error wrapping ctx's when ctx ends: a shutdown, not a failure. The errors
// it logs come from Run, which keeps tokens out of them.
func RunPlatform(ctx context.Context, runner PlatformRunner, sink chat.Sink, p RunPolicy) error {
	base, limit, reset := cmp.Or(p.Base, defaultRunBase), cmp.Or(p.Max, defaultRunMax), cmp.Or(p.ResetAfter, defaultRunResetAfter)
	now, after, log, random := p.Now, p.After, p.Logger, p.Random
	if now == nil {
		now = time.Now
	}
	if after == nil {
		after = time.After
	}
	if log == nil {
		log = slog.Default()
	}
	if random == nil {
		random = rand.Float64
	}
	pause := min(base, limit)
	for {
		started := now()
		err := runner.Run(ctx, sink)
		if ctx.Err() != nil {
			return stopped(ctx, p.Platform)
		}
		v := runner.Classify(chat.OpOther, err)
		if v.Class == chat.FatalAuth {
			return &authRejected{platform: p.Platform, code: v.Code}
		}
		if now().Sub(started) >= reset {
			pause = min(base, limit)
		}
		wait := max(jittered(pause, random()), min(v.RetryAfter, retryAfterMax))
		// The platform is in the message, not in an attribute, so that a Slack connection's line stays the one that log alerts match.
		log.WarnContext(ctx, cmp.Or(p.Platform, "chat")+" connection ended; connecting again", "error", err, "after", wait)
		select {
		case <-ctx.Done():
			return stopped(ctx, p.Platform)
		case <-after(wait):
		}
		pause = min(pause*2, limit)
	}
}

// jittered is pause cut by the share cut of its upper half: between half of pause (cut 1) and pause (cut 0). A cut outside [0, 1]
// is held to it.
func jittered(pause time.Duration, cut float64) time.Duration {
	return pause - time.Duration(min(max(cut, 0), 1)*float64(pause/2))
}

// ChooseConnection returns the ChatConnection this broker runs, with the keys of its credentials Secret that its
// platform needs (v1alpha1.CredentialKeys) read through secrets. With the custom resources own is the broker's own connection
// (RunConfig.ChatConnection), found by its reference: the snapshot may hold the ChatConnections of the admitted namespaces
// besides it, and this reads none of their Secrets and cannot fail for them (ChooseConnections reads them). With the files
// own is the zero value, and the directory must hold exactly one ChatConnection, as the broker serves one. The connection must be on a platform that has
// an adapter (platformFor knows it). Anything else is an error wrapping ErrWiring. The tokens are those of the snapshot at the call: Run reads them at start, and again when the
// leader half starts, since a standby may lead long after it started. A change of the connection or of its Secret applies
// to a running leader on restart, and nothing flags it as a failed reload (the stale flag is for reloads that failed): the
// verification of the Bindings tells of tokens that differ from these (ChatCredentialsChanged), and logs a Binding that a reload moved
// to another connection when it stops verifying it.
func ChooseConnection(ctx context.Context, snap *config.Snapshot, own v1alpha1.ObjectRef, secrets identity.SecretReader) (Connection, error) {
	if snap == nil {
		return Connection{}, errors.New("broker: the config has not loaded yet")
	}
	conns := snap.ChatConnections()
	if own != (v1alpha1.ObjectRef{}) {
		i := slices.IndexFunc(conns, func(c *v1alpha1.ChatConnection) bool { return c.Namespace == own.Namespace && c.Name == own.Name })
		if i < 0 {
			return Connection{}, fmt.Errorf("%w: ChatConnection %s is not in the config: AGENTKOURIER_CHAT_CONNECTION names the connection this broker runs", ErrWiring, v1alpha1.NamespacedName(own.Namespace, own.Name))
		}
		conns = conns[i : i+1]
	}
	if len(conns) != 1 {
		names := make([]string, len(conns))
		for i, c := range conns {
			names[i] = v1alpha1.NamespacedName(c.Namespace, c.Name)
		}
		return Connection{}, fmt.Errorf("%w: v1 serves exactly one ChatConnection, and the config has %d [%s]", ErrWiring, len(conns), strings.Join(names, ", "))
	}
	return connectionOf(ctx, conns[0], secrets)
}

// errNoAdapter is wrapped by the error of a ChatConnection whose platform this broker has no adapter for (platformFor does not know it).
var errNoAdapter = errors.New("a platform this broker has no adapter for")

// errTeamsSocket is wrapped by the error of a Microsoft Teams ChatConnection in socket mode, which the API takes and no broker runs yet.
var errTeamsSocket = errors.New("spec.teams.mode is socket, and Socket mode is not supported yet: use http")

// connectionOf is the Connection of c with the credentials of its platform and mode (v1alpha1.CredentialKeys) read through secrets. A
// platform with no adapter is an error wrapping ErrWiring and errNoAdapter, a Microsoft Teams connection in socket mode one wrapping
// ErrWiring and errTeamsSocket, and a Secret, key or value that is missing one wrapping ErrWiring; a Secret that cannot be read just
// now is not.
func connectionOf(ctx context.Context, c *v1alpha1.ChatConnection, secrets identity.SecretReader) (Connection, error) {
	name := v1alpha1.NamespacedName(c.Namespace, c.Name)
	if _, ok := platformFor(c.Spec.Platform); !ok {
		return Connection{}, fmt.Errorf("%w: ChatConnection %s is %q, %w", ErrWiring, name, c.Spec.Platform, errNoAdapter)
	}
	var teamsSpec v1alpha1.TeamsConnectionSpec
	if c.Spec.Platform == v1alpha1.PlatformTeams {
		if c.Spec.Teams == nil { // Validate refuses it; this keeps the adapter from a connection with no tenant
			return Connection{}, fmt.Errorf("%w: ChatConnection %s is a Microsoft Teams connection with no spec.teams", ErrWiring, name)
		}
		if c.Spec.TeamsMode() != v1alpha1.TeamsModeHTTP {
			return Connection{}, fmt.Errorf("%w: ChatConnection %s: %w", ErrWiring, name, errTeamsSocket)
		}
		teamsSpec = *c.Spec.Teams
	}
	read := func(key string) (secret.Value, error) {
		v, err := secrets.Value(ctx, c.Namespace, c.Spec.CredentialsSecretRef.Name, key)
		if err == nil && v.IsEmpty() {
			err = secret.ErrEmptyValue
		}
		if err != nil {
			// A Secret, key or value that is missing is the operator's to fix, so a usage error. One that
			// cannot be read just now (a cache that has not synced) is not.
			err = fmt.Errorf("ChatConnection %s: read %s of Secret %s failed: %w", name, key, v1alpha1.NamespacedName(c.Namespace, c.Spec.CredentialsSecretRef.Name), err)
			if errors.Is(err, secret.ErrKeyNotFound) || errors.Is(err, secret.ErrEmptyValue) || errors.Is(err, config.ErrSecretNotFound) {
				err = fmt.Errorf("%w: %w", ErrWiring, err)
			}
			return secret.Value{}, err
		}
		return v, nil
	}
	creds := map[string]secret.Value{}
	for _, key := range v1alpha1.CredentialKeys(c.Spec) { // in its order: the first key missing is the one the error names
		v, err := read(key)
		if err != nil {
			return Connection{}, err
		}
		creds[key] = v
	}
	return Connection{Ref: v1alpha1.ObjectRef{Namespace: c.Namespace, Name: c.Name}, Platform: c.Spec.Platform, Credentials: creds, Teams: teamsSpec}, nil
}

// Connections are the ChatConnections a broker runs: its own, if it has one, and the team connections it serves besides (spec D52, D53).
type Connections struct {
	// Own is the broker's own connection, which it always runs. It is nil when the broker has none (spec D53): every connection is then a team
	// connection, and the zero Connection is not a stand-in.
	Own *Connection
	// Team are the other ChatConnections of the snapshot, the served ones of the admitted namespaces, oldest first (the order
	// the source serves them in, and the one that decides which of two connections of one Slack app is served).
	Team []TeamConnection
}

// TeamConnection is a served ChatConnection of an admitted namespace, with what starting it takes, or why it cannot start yet.
type TeamConnection struct {
	// Connection is the connection and its credentials. The credentials are empty when Err is set.
	Connection
	// UID, Generation and Created are the ChatConnection's metadata.uid, metadata.generation and creationTimestamp. The first two
	// with the credentials say whether it is the connection a runtime was started for; the last is its age.
	UID        string
	Generation int64
	Created    time.Time
	// Err is why its credentials could not be read, its platform is not one the broker has an adapter for, or it is a Microsoft Teams
	// connection in socket mode, which the broker does not run yet. It is no failure of the broker: the connection is left to start
	// later, when the Secret is there, or left out. It holds no Secret value.
	Err error
}

// Name is "namespace/name", the connection as keys and rows name it.
func (t TeamConnection) Name() string { return v1alpha1.NamespacedName(t.Ref.Namespace, t.Ref.Name) }

// fingerprint says which ChatConnection and credentials a runtime was started with: a digest of the object's UID and generation, its
// platform and credentials Secret, and the credentials themselves (their content, so that a rotated token is another fingerprint). It is
// kept in memory and compared, never logged or stored.
func (t TeamConnection) fingerprint() string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%q %d %q %q\n", t.UID, t.Generation, t.Platform, t.Name()) // a hash.Hash never fails to write
	for _, key := range slices.Sorted(maps.Keys(t.Credentials)) {
		v := t.Credentials[key].Reveal()
		_, _ = fmt.Fprintf(h, "%q %d:%s", key, len(v), v)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ChooseConnections returns the broker's own connection, as ChooseConnection does and with the failures it has (the own connection
// missing or invalid, or its Secret missing, is an error wrapping ErrWiring; one that cannot be read is an error), and the team
// connections of the snapshot besides it. It never fails for a team connection: one whose Secret is not readable now (a takeover
// before the Secret informer has synced it, say), or whose platform has no adapter, comes back with its Err set, and the supervisor
// starts it when it can. With the files there are none.
func ChooseConnections(ctx context.Context, snap *config.Snapshot, own v1alpha1.ObjectRef, secrets identity.SecretReader) (Connections, error) {
	ownConn, err := ChooseConnection(ctx, snap, own, secrets)
	if err != nil {
		return Connections{}, err
	}
	return Connections{Own: &ownConn, Team: teamConnections(ctx, snap, ownConn.Ref, secrets)}, nil
}

// ChooseTeamConnections returns the connections of a broker with no ChatConnection of its own (spec D53): no own, and every ChatConnection of the
// snapshot as a team connection, oldest first. It has no failure to report, as no connection is the broker's to refuse to start for: one whose
// Secret is not readable now, or whose platform has no adapter, comes back with its Err, and the supervisor starts it when it can. It is not
// ChooseConnections with a zero reference, which means the files, where the directory holds exactly one ChatConnection.
func ChooseTeamConnections(ctx context.Context, snap *config.Snapshot, secrets identity.SecretReader) Connections {
	return Connections{Team: teamConnections(ctx, snap, v1alpha1.ObjectRef{}, secrets)}
}

// teamConnections are the ChatConnections of snap other than own, oldest first (creationTimestamp, then namespace and name, the
// source's own rule), with their credentials read through secrets. A zero own leaves out none, as no ChatConnection has an empty name. A
// snapshot that is not loaded has none.
func teamConnections(ctx context.Context, snap *config.Snapshot, own v1alpha1.ObjectRef, secrets identity.SecretReader) []TeamConnection {
	if snap == nil {
		return nil
	}
	var out []TeamConnection
	for _, c := range snap.ChatConnections() {
		if c.Namespace == own.Namespace && c.Name == own.Name {
			continue
		}
		t := TeamConnection{UID: string(c.UID), Generation: c.Generation, Created: c.CreationTimestamp.Time}
		t.Connection, t.Err = connectionOf(ctx, c, secrets)
		if t.Err != nil {
			t.Connection = Connection{Ref: v1alpha1.ObjectRef{Namespace: c.Namespace, Name: c.Name}, Platform: c.Spec.Platform}
		}
		out = append(out, t)
	}
	slices.SortStableFunc(out, func(a, b TeamConnection) int {
		return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.Ref.Namespace, b.Ref.Namespace), cmp.Compare(a.Ref.Name, b.Ref.Name))
	})
	return out
}
