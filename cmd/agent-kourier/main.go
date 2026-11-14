// Command Agent Kourier is the agent-kourier broker. See spec.md for the design.
//
// It is configured by environment variables, which the Deployment sets, and takes one flag, --version, and two
// subcommands: dry-run (dryrun.go), which replays a channel's history through a Binding's chat triggers and
// reads Slack with a bot token from a file or SLACK_BOT_TOKEN, and init (initcmd.go), which writes the files of a first
// install with the Helm chart (by default the platform's values and the team's custom resources, for a team that brings
// its own Slack app; with --source files one values file), checks them, and prints the commands to install with them.
// The broker reads these variables:
//
//	AGENTKOURIER_CONFIG_SOURCE  files (the default) or crds: where the resources are read from. files reads the directory
//	                       below, all or nothing. crds reads Binding, AgentBackend and ChatConnection from the cluster, one
//	                       object at a time, and ignores the directory
//	AGENTKOURIER_CONFIG_DIR     with files, the resource files (default /etc/agent-kourier)
//	AGENTKOURIER_CHAT_CONNECTION   with crds, optional: namespace/name of the broker's own ChatConnection, which it always serves.
//	                       Unset, the broker has no ChatConnection of its own and serves the namespaces the two variables below
//	                       admit, or, when neither is set, every namespace. With files it is refused, since the directory must hold
//	                       exactly one ChatConnection
//	AGENTKOURIER_CONNECTION_NAMESPACES   with crds, optional: namespaces whose ChatConnections this broker serves besides its own,
//	                       comma-separated, each a DNS-1123 label (no wildcard); spaces around an item are trimmed, empty items and
//	                       repeats dropped. Read at start. Unset or empty is no list; a value of only commas and spaces is refused.
//	                       With files it is refused
//	AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR   with crds, optional: a metav1.LabelSelector as JSON; the namespaces whose labels it
//	                       matches are admitted too, read live, so a label change admits or stops admitting a namespace with no
//	                       restart. {} admits every namespace. Unset or empty is no selector. Decoded strictly (no unknown or repeated
//	                       field, nothing after the object) and validated as Kubernetes validates a selector. With files it is refused.
//	                       With none of the three set the broker serves the ChatConnections of every namespace (spec D54)
//	AGENTKOURIER_DB_PATH        the SQLite file, on the volume (default /data/agent-kourier.db)
//	AGENTKOURIER_STORE          sqlite (the default, one replica) or postgres
//	AGENTKOURIER_POSTGRES_DSN_SECRET   with postgres, where the DSN is: namespace/name/key of a Secret, read through the
//	                       Secret caches (its namespace needs the list and watch Role the config's do)
//	AGENTKOURIER_POSTGRES_TIMEOUTS     with postgres, agent-kourier (the default: Agent Kourier sets the idle-in-transaction
//	                       and statement timeouts on each connection) or role (the database role sets them, for a pooler
//	                       that refuses startup parameters)
//	AGENTKOURIER_LEASE_NAMESPACE, AGENTKOURIER_LEASE_NAME, AGENTKOURIER_POD_NAME   with postgres, required: where the leader-election Lease
//	                       is (Agent Kourier's own namespace, one Lease for the Deployment) and the pod's name, which names the
//	                       replica as the holder. The election runs with postgres and never with sqlite, whose replica works at once
//	AGENTKOURIER_LEASE_DURATION, AGENTKOURIER_LEASE_RENEW_DEADLINE, AGENTKOURIER_LEASE_RETRY_PERIOD   with postgres, optional: the election's
//	                       timings (defaults 15s, 10s and 2s). A standby takes over about a lease duration after the leader's
//	                       last renewal; the leader gives up renewing after the renew deadline
//	AGENTKOURIER_LISTEN         the address of /healthz, /readyz and /metrics (default :8080)
//	AGENTKOURIER_SYNC_TIMEOUT   how long to wait for the Secret caches (default 30s)
//	AGENTKOURIER_KLOG_V         client-go's verbosity, held below 8 (default 0)
//	AGENTKOURIER_LOG_LEVEL      debug, info, warn or error (default info)
//	AGENTKOURIER_AUDIT_RETENTION   how long audit entries are kept: "keep" (the default: nothing is purged) or a duration of at
//	                       least 24h, such as 8760h, after which an entry is purged
//
// It exits 0 after a signal; 1 when Slack rejects the tokens of its own connection (with files, the one of its directory; with crds and
// no own connection a rejected token stops only that connection and not the process); 2 for a wrong config or wiring (or an unreadable
// variable, a namespace list that names no namespace, or a selector that is not one; with crds, also CRDs that are not installed, an own
// ChatConnection that is missing or invalid, and, with no own connection, rows that were written before connections were recorded); 3 when RBAC denies it the Secrets, or
// with crds the custom resources; 4 for any other failure; and 5 when it was the leader and lost the lease, met a stale epoch, or had a chat
// write or an agent call refused because its lease was past the renew deadline (spec D37): it ends at once, and Kubernetes restarts it
// as a standby.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	kjson "sigs.k8s.io/json"

	"github.com/agentkourier/agent-kourier/internal/telemetry"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/broker"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
	"github.com/agentkourier/agent-kourier/internal/retention"
)

// version is set at build time: -ldflags "-X main.version=<value>".
var version = "dev"

// The exit codes.
const (
	exitOK        = 0
	exitSlackAuth = 1 // Slack rejected the tokens of the broker's own connection: a restart would be rejected again
	exitUsage     = 2 // the config, the wiring or an environment variable is wrong
	exitRBAC      = 3 // the API server denied access to Secrets or, with crds, to the custom resources
	exitFailure   = 4 // anything else: a dependency failed
)

func main() {
	os.Exit(run(os.Args[1:], version, os.Stdout, os.Stderr, os.Getenv))
}

// exitCode maps what broker.Run returned to the exit code.
func exitCode(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, broker.ErrSlackAuth):
		return exitSlackAuth
	case errors.Is(err, k8ssecrets.ErrAccessDenied), errors.Is(err, crsource.ErrAccessDenied):
		return exitRBAC
	case errors.Is(err, broker.ErrWiring):
		return exitUsage
	}
	return exitFailure
}

// envConfig is what the environment says.
type envConfig struct {
	configDir, dbPath, listen   string
	configSource                broker.ConfigSource
	chatConnection              v1alpha1.ObjectRef // with crds only: the broker's own ChatConnection, zero when it has none (spec D53)
	connectionNamespaces        []string           // with crds only: the admission list, none when unset
	connectionNamespaceSelector labels.Selector    // with crds only: the admission selector, nil when unset, empty for every namespace
	store                       broker.StoreConfig
	election                    broker.ElectionConfig // with postgres only; its Identity is the pod's name, made unique by run
	syncTimeout                 time.Duration
	klogV                       int
	logLevel                    slog.Level
	auditRetention              time.Duration // zero keeps the audit log for ever
	telemetry                   telemetry.Config
}

// loadEnv reads the environment through getenv. A value it cannot read is an error wrapping
// broker.ErrWiring that names the variable; nothing is guessed.
func loadEnv(getenv func(string) string) (envConfig, error) {
	get := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	c := envConfig{configDir: get("AGENTKOURIER_CONFIG_DIR", "/etc/agent-kourier"), dbPath: get("AGENTKOURIER_DB_PATH", "/data/agent-kourier.db"), listen: get("AGENTKOURIER_LISTEN", ":8080")}
	bad := func(key string, want string, err error) error {
		return fmt.Errorf("%w: %s is %q, want %s: %w", broker.ErrWiring, key, getenv(key), want, err)
	}
	var err error
	if c.configSource, c.chatConnection, err = loadSourceEnv(getenv); err != nil {
		return c, err
	}
	if c.connectionNamespaces, err = loadConnectionNamespaces(getenv, c.configSource); err != nil {
		return c, err
	}
	// With no own connection, no list and no selector, the broker serves every namespace (spec D54): the source reads a nil selector so.
	if c.connectionNamespaceSelector, err = loadConnectionNamespaceSelector(getenv, c.configSource); err != nil {
		return c, err
	}
	if c.syncTimeout, err = time.ParseDuration(get("AGENTKOURIER_SYNC_TIMEOUT", "30s")); err != nil || c.syncTimeout <= 0 {
		return c, bad("AGENTKOURIER_SYNC_TIMEOUT", "a positive duration such as 30s", errors.Join(err, errors.New("not positive")))
	}
	if c.klogV, err = strconv.Atoi(get("AGENTKOURIER_KLOG_V", "0")); err != nil {
		return c, bad("AGENTKOURIER_KLOG_V", "a whole number", err)
	}
	levels := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}
	level, ok := levels[strings.ToLower(get("AGENTKOURIER_LOG_LEVEL", "info"))]
	if !ok {
		return c, bad("AGENTKOURIER_LOG_LEVEL", "debug, info, warn or error", errors.New("unknown level"))
	}
	c.logLevel = level
	if v := get("AGENTKOURIER_AUDIT_RETENTION", "keep"); !strings.EqualFold(v, "keep") {
		// "keep" is the only way to say keep: a zero duration, which the retention would take as keep, is refused, since it
		// could be read as either.
		if c.auditRetention, err = time.ParseDuration(v); err != nil || c.auditRetention == 0 || !retention.ValidAuditRetention(c.auditRetention) {
			return c, bad("AGENTKOURIER_AUDIT_RETENTION", "keep, or a duration of at least "+retention.MinAuditRetention.String()+" such as 8760h", errors.Join(err, errors.New("too short or not a duration")))
		}
	}
	if c.store, err = loadStoreEnv(getenv); err != nil {
		return c, err
	}
	if c.election, err = loadElectionEnv(getenv, c.store.Kind); err != nil {
		return c, err
	}
	c.telemetry, err = telemetry.LoadConfig(getenv)
	if err != nil {
		return c, fmt.Errorf("%w: %w", broker.ErrWiring, err)
	}
	return c, nil
}

// loadSourceEnv reads the variables that choose where the resources are read from. The source is files unless the variable says crds,
// and any other value is refused: there is no fallback. The broker's own connection is optional with crds, where a cluster may hold the
// connections of several brokers: unset, it is the zero reference, and the broker serves the namespaces the list and the selector admit, or
// every namespace when neither is set. It is refused with files, whose directory must hold exactly one: ignoring it would serve the one connection
// the person did not name.
func loadSourceEnv(getenv func(string) string) (broker.ConfigSource, v1alpha1.ObjectRef, error) {
	var conn v1alpha1.ObjectRef
	source := broker.ConfigSource(strings.ToLower(getenv("AGENTKOURIER_CONFIG_SOURCE")))
	if source == "" {
		source = broker.SourceFiles
	}
	ref := getenv("AGENTKOURIER_CHAT_CONNECTION")
	switch source {
	case broker.SourceFiles:
		if ref != "" {
			return "", conn, fmt.Errorf("%w: AGENTKOURIER_CHAT_CONNECTION is set with AGENTKOURIER_CONFIG_SOURCE=files, which serves the one ChatConnection of its directory: unset it", broker.ErrWiring)
		}
	case broker.SourceCRDs:
		if ref == "" {
			return source, conn, nil
		}
		ns, name, ok := strings.Cut(ref, "/")
		if !ok || ns == "" || name == "" || strings.Contains(name, "/") {
			return "", conn, fmt.Errorf("%w: AGENTKOURIER_CHAT_CONNECTION is not namespace/name (its value is not shown)", broker.ErrWiring)
		}
		conn = v1alpha1.ObjectRef{Namespace: ns, Name: name}
	default:
		return "", conn, fmt.Errorf("%w: AGENTKOURIER_CONFIG_SOURCE is %q, want files or crds", broker.ErrWiring, getenv("AGENTKOURIER_CONFIG_SOURCE"))
	}
	return source, conn, nil
}

// loadConnectionNamespaces reads the admission list: the namespaces whose ChatConnections the broker serves besides its own. Each
// item is trimmed and must be a DNS-1123 label, which leaves out a wildcard, a namespace/name and anything Kubernetes cannot hold; an
// empty item is dropped and so is a repeat. Nothing set is no list. A value of nothing but commas and spaces is refused: it is a slip in a
// template, and read as no list it would serve every namespace with no own connection and no selector (spec D54).
// It is refused with the files source, which serves the one ChatConnection of its directory: ignoring it would serve what the person
// did not mean. An error names the position of the bad item among the comma-separated ones (counting the empty), and never a value, since
// the variable is one a person may paste a token into.
func loadConnectionNamespaces(getenv func(string) string, source broker.ConfigSource) ([]string, error) {
	const key = "AGENTKOURIER_CONNECTION_NAMESPACES"
	raw := getenv(key)
	if raw == "" {
		return nil, nil
	}
	if source == broker.SourceFiles {
		return nil, fmt.Errorf("%w: %s is set with AGENTKOURIER_CONFIG_SOURCE=files, which serves the one ChatConnection of its directory: unset it", broker.ErrWiring, key)
	}
	var out []string
	for i, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if len(validation.IsDNS1123Label(item)) > 0 {
			return nil, fmt.Errorf("%w: %s item %d is not a namespace name, which is a DNS-1123 label (no wildcard; its value is not shown)", broker.ErrWiring, key, i+1)
		}
		if !slices.Contains(out, item) {
			out = append(out, item)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s is set and names no namespace (only commas and spaces): unset it, or name the namespaces", broker.ErrWiring, key)
	}
	return out, nil
}

// loadConnectionNamespaceSelector reads the admission selector: a metav1.LabelSelector as JSON, whose matching namespaces the broker serves
// the ChatConnections of besides the list's. Unset or empty is no selector, the nil Selector: never labels.Nothing(), which is what
// metav1.LabelSelectorAsSelector makes of a nil LabelSelector, and which would read namespaces to match none of them. Otherwise the value
// must be one JSON object and nothing after it, decoded as the API server decodes one (field names case-sensitive, no field a
// LabelSelector does not have and none twice: a repeated matchLabels would otherwise keep the last, which may be {}, every namespace), and
// pass metav1.LabelSelectorAsSelector, which validates each key, value and operator. {}, or any selector that requires nothing, is the
// empty Selector: every namespace. It is refused with the files source, as the list is.
//
// An error says what is wrong and never shows the value or a piece of it, a field name or a label included: the variable is one a person
// may paste a token into. The decoders' and Kubernetes' own errors quote what they refuse, so they are not wrapped.
func loadConnectionNamespaceSelector(getenv func(string) string, source broker.ConfigSource) (labels.Selector, error) {
	const key = "AGENTKOURIER_CONNECTION_NAMESPACE_SELECTOR"
	raw := getenv(key)
	if raw == "" {
		return nil, nil
	}
	bad := func(what string) error {
		return fmt.Errorf("%w: %s %s (its value is not shown)", broker.ErrWiring, key, what)
	}
	if source == broker.SourceFiles {
		return nil, fmt.Errorf("%w: %s is set with AGENTKOURIER_CONFIG_SOURCE=files, which serves the one ChatConnection of its directory: unset it", broker.ErrWiring, key)
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	var obj json.RawMessage
	if err := dec.Decode(&obj); err != nil {
		return nil, bad("is not JSON; want a LabelSelector as a JSON object, such as {\"matchLabels\":{...}}, or {} for every namespace")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, bad("has more after the selector; want one JSON object")
	}
	if len(obj) == 0 || obj[0] != '{' { // the decoder skips the space before a value; null comes here as the text null
		return nil, bad("is not a JSON object; want a LabelSelector, or {} for every namespace (unset it for no selector)")
	}
	var ls metav1.LabelSelector
	for _, check := range []struct {
		opt  kjson.StrictOption
		what string
	}{
		{kjson.DisallowUnknownFields, "has a field that a LabelSelector does not have; want matchLabels and matchExpressions, whose items have key, operator and values (names are case-sensitive)"},
		{kjson.DisallowDuplicateFields, "has a field twice"},
	} {
		strict, err := kjson.UnmarshalStrict(obj, &ls, check.opt)
		if err != nil {
			return nil, bad("does not have the shape of a LabelSelector; want matchLabels an object of strings and matchExpressions a list of objects")
		}
		if len(strict) > 0 {
			return nil, bad(check.what)
		}
	}
	// Each part on its own first, so that the error can say which part Kubernetes refuses without quoting it.
	if _, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchLabels: ls.MatchLabels}); err != nil {
		return nil, bad("matchLabels holds a key or a value that is not a valid label key or value")
	}
	for i, req := range ls.MatchExpressions {
		switch req.Operator {
		case metav1.LabelSelectorOpIn, metav1.LabelSelectorOpNotIn, metav1.LabelSelectorOpExists, metav1.LabelSelectorOpDoesNotExist:
		default:
			return nil, bad(fmt.Sprintf("matchExpressions item %d has an operator that is not In, NotIn, Exists or DoesNotExist", i+1))
		}
		if _, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{req}}); err != nil {
			return nil, bad(fmt.Sprintf("matchExpressions item %d is not a valid requirement: its key or a value is not a valid label key or value, "+
				"or its values do not suit its operator (In and NotIn need at least one, Exists and DoesNotExist none)", i+1))
		}
	}
	sel, err := metav1.LabelSelectorAsSelector(&ls)
	if err != nil {
		return nil, bad("is not a valid label selector")
	}
	return sel, nil
}

// loadStoreEnv reads the store's variables. The DSN is a secret and its variable holds only where it is, but a person may put the DSN
// there, so no error here echoes a value.
func loadStoreEnv(getenv func(string) string) (broker.StoreConfig, error) {
	bad := func(key, want string) error {
		return fmt.Errorf("%w: %s is not %s (its value is not shown)", broker.ErrWiring, key, want)
	}
	kind := strings.ToLower(getenv("AGENTKOURIER_STORE"))
	if kind == "" {
		kind = string(broker.StoreSQLite)
	}
	c := broker.StoreConfig{}
	switch broker.StoreKind(kind) {
	case broker.StoreSQLite:
		c.Kind = broker.StoreSQLite
	case broker.StorePostgres:
		c.Kind = broker.StorePostgres
	default:
		return c, bad("AGENTKOURIER_STORE", "sqlite or postgres")
	}
	ref, timeouts := getenv("AGENTKOURIER_POSTGRES_DSN_SECRET"), getenv("AGENTKOURIER_POSTGRES_TIMEOUTS")
	if c.Kind == broker.StoreSQLite {
		// Ignoring either would run on SQLite what the person meant for Postgres.
		switch {
		case ref != "":
			return c, bad("AGENTKOURIER_POSTGRES_DSN_SECRET", "unset with AGENTKOURIER_STORE=sqlite")
		case timeouts != "":
			return c, bad("AGENTKOURIER_POSTGRES_TIMEOUTS", "unset with AGENTKOURIER_STORE=sqlite")
		}
		return c, nil
	}
	parts := strings.Split(ref, "/")
	if len(parts) != 3 || slices.Contains(parts, "") {
		return c, bad("AGENTKOURIER_POSTGRES_DSN_SECRET", "namespace/name/key, the Secret that holds the DSN")
	}
	c.PostgresDSN = broker.SecretKey{Namespace: parts[0], Name: parts[1], Key: parts[2]}
	switch timeouts {
	case "", "agent-kourier":
	case "role":
		c.PostgresRoleTimeouts = true
	default:
		return c, bad("AGENTKOURIER_POSTGRES_TIMEOUTS", "agent-kourier or role")
	}
	return c, nil
}

// loadElectionEnv reads the election's variables. With postgres the Lease and the pod are required, and the timings are
// optional (zero means the default); with sqlite there is no election, and a variable that asks for one is refused,
// since ignoring it would run one replica where the person meant two.
func loadElectionEnv(getenv func(string) string, kind broker.StoreKind) (broker.ElectionConfig, error) {
	timings := map[string]*time.Duration{}
	var c broker.ElectionConfig
	timings["AGENTKOURIER_LEASE_DURATION"], timings["AGENTKOURIER_LEASE_RENEW_DEADLINE"], timings["AGENTKOURIER_LEASE_RETRY_PERIOD"] = &c.LeaseDuration, &c.RenewDeadline, &c.RetryPeriod
	if kind != broker.StorePostgres {
		for _, key := range []string{"AGENTKOURIER_LEASE_NAMESPACE", "AGENTKOURIER_LEASE_NAME", "AGENTKOURIER_LEASE_DURATION", "AGENTKOURIER_LEASE_RENEW_DEADLINE", "AGENTKOURIER_LEASE_RETRY_PERIOD"} {
			if getenv(key) != "" {
				return broker.ElectionConfig{}, fmt.Errorf("%w: %s is set with AGENTKOURIER_STORE=sqlite, which has no leader election: unset it", broker.ErrWiring, key)
			}
		}
		return c, nil
	}
	c.Namespace, c.Name, c.Identity = getenv("AGENTKOURIER_LEASE_NAMESPACE"), getenv("AGENTKOURIER_LEASE_NAME"), getenv("AGENTKOURIER_POD_NAME")
	for _, req := range []struct{ key, value string }{{"AGENTKOURIER_LEASE_NAMESPACE", c.Namespace}, {"AGENTKOURIER_LEASE_NAME", c.Name}, {"AGENTKOURIER_POD_NAME", c.Identity}} {
		if req.value == "" {
			return c, fmt.Errorf("%w: %s is required with AGENTKOURIER_STORE=postgres: more than one replica may run, and the Lease says which leads", broker.ErrWiring, req.key)
		}
	}
	for _, key := range []string{"AGENTKOURIER_LEASE_DURATION", "AGENTKOURIER_LEASE_RENEW_DEADLINE", "AGENTKOURIER_LEASE_RETRY_PERIOD"} {
		v := getenv(key)
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("%w: %s is %q, want a positive duration such as 15s", broker.ErrWiring, key, v)
		}
		*timings[key] = d
	}
	return c, nil
}

// uniqueIdentity makes the pod's name the name of this process: a container that restarts in the same pod is a new
// process, and must not take the Lease its predecessor held for its own (broker.ElectionConfig.Identity).
func uniqueIdentity(pod string) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("make the replica's identity: %w", err)
	}
	return pod + "_" + hex.EncodeToString(b), nil
}

// run executes the CLI and returns the process exit code.
func run(args []string, ver string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) > 0 && args[0] == "dry-run" {
		return dryRunCommand(args[1:], stdout, stderr, getenv)
	}
	if len(args) > 0 && args[0] == "init" {
		return initCommand(args[1:], ver, os.Stdin, stdout, stderr, getenv)
	}
	fs := flag.NewFlagSet("agent-kourier", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	// Flag parsing stops at the first argument that is no flag, so a mistyped subcommand (dryrun, dry_run) would
	// fall through and start the broker: in a pod, a second one. The broker takes no arguments. The argument is not
	// echoed: a person may have typed a token in its place.
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, "agent-kourier: unknown command; the broker takes no arguments (did you mean dry-run or init?)")
		return exitUsage
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, ver)
		return exitOK
	}
	env, err := loadEnv(getenv)
	if err != nil {
		return fail(stderr, err)
	}
	log := slog.New(telemetry.NewJSONHandler(stderr, env.logLevel))
	// A component that falls back to slog.Default() (the outbox, the sweeps, net/http's error log) goes through the
	// same handler. The one process-wide setting; it is made here and nowhere else.
	slog.SetDefault(log)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", env.listen)
	if err != nil {
		return fail(stderr, fmt.Errorf("listen on %s: %w", env.listen, err))
	}
	defer func() { _ = ln.Close() }()

	if env.store.Kind == broker.StorePostgres {
		if env.election.Identity, err = uniqueIdentity(env.election.Identity); err != nil {
			return fail(stderr, err)
		}
	}
	cfg := broker.RunConfig{
		ConfigSource: env.configSource, ChatConnection: env.chatConnection, ConnectionNamespaces: env.connectionNamespaces, ConnectionNamespaceSelector: env.connectionNamespaceSelector, ConfigDir: env.configDir, DBPath: env.dbPath, Store: env.store, Election: env.election, Listener: ln, SyncTimeout: env.syncTimeout, KlogVerbosity: env.klogV, Logger: log, Telemetry: env.telemetry, ServiceVersion: ver, AuditRetention: env.auditRetention,
	}
	applyTestHooks(log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return fail(stderr, broker.Run(ctx, cfg))
}

// fail reports err on stderr and returns its exit code.
func fail(stderr io.Writer, err error) int {
	code := exitCode(err)
	if err != nil {
		// Run's error may quote an agent, Slack or a dependency: redacted and capped, as in a log line.
		_, _ = fmt.Fprintln(stderr, "agent-kourier:", telemetry.LogText(err))
	}
	return code
}
