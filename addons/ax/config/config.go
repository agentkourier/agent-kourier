// Package config reads the agent's settings from its environment and checks them, so that a setting the agent cannot
// use stops it at start with an error that names the variable. An error never shows the value of a variable that may
// hold a secret (POSTGRES_DSN, AUTH_HEADER, OIDC_ISSUER, RUNNER_URL).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// StoreKind names where the agent keeps its state.
type StoreKind string

// The stores.
const (
	// StoreSQLite is one file, opened by one agent at a time.
	StoreSQLite StoreKind = "sqlite"
	// StorePostgres is a database that several replicas share.
	StorePostgres StoreKind = "postgres"
)

// The model providers.
const (
	// ProviderBedrock is Amazon Bedrock's Converse API, in MODEL_REGION.
	ProviderBedrock = "bedrock"
	// ProviderAnthropic is the Anthropic Messages API, whose SDK reads its own credentials (ANTHROPIC_API_KEY) and
	// ANTHROPIC_BASE_URL.
	ProviderAnthropic = "anthropic"
)

// jitterFactor is how much a retry period may stretch; the renew deadline must exceed the retry period times it, as
// the broker's election requires.
const jitterFactor = 1.2

// Lease holds the leader election's timings. They apply to Postgres only.
type Lease struct {
	// Duration is how long a lease lasts without renewal.
	Duration time.Duration
	// RenewDeadline is how long the leader keeps trying to renew before it gives up.
	RenewDeadline time.Duration
	// RetryPeriod is how often a replica tries to take or renew the lease.
	RetryPeriod time.Duration
}

// Config is the agent's settings.
type Config struct {
	// ModelProvider is the model's provider: ProviderBedrock or ProviderAnthropic.
	ModelProvider string
	// ModelRegion is the Bedrock region; Bedrock only.
	ModelRegion string
	// ModelName is the model: Bedrock's model or inference profile ID, or the Anthropic API's model ID.
	ModelName string
	// ListenAddr is where the agent listens.
	ListenAddr string
	// PublicURL is the JSON-RPC endpoint the agent card names.
	PublicURL string
	// RunnerURL is the AX task runner's metadata server, to which /readyz and /metadata/ are proxied. Empty runs the
	// agent standalone: it answers /readyz itself.
	RunnerURL string
	// AuthTokenFile holds the accepted tokens, one per line. Set for the token check.
	AuthTokenFile string
	// AuthHeader is the header that carries the token, in canonical form. Authorization carries "Bearer <token>"; any
	// other header carries the token as its whole value. The OIDC check reads Authorization alone.
	AuthHeader string
	// AuthDisabled turns the caller check off. It is accepted only with a loopback ListenAddr.
	AuthDisabled bool
	// OIDCIssuer, OIDCAudience and OIDCSubject are the OIDC check: a request carries an ID token from the issuer, for
	// the audience, whose subject is OIDCSubject. Set all three for the OIDC check, none for another.
	OIDCIssuer, OIDCAudience, OIDCSubject string
	// Store is where the agent keeps its state.
	Store StoreKind
	// SQLitePath is the SQLite file. Used with StoreSQLite.
	SQLitePath string
	// PostgresDSN is the Postgres connection string. Required with StorePostgres. It may hold a password: never log it.
	PostgresDSN string
	// TaskRetention is how long a settled task and its message IDs are kept; zero keeps them.
	TaskRetention time.Duration
	// SessionIdleTTL is how long a context may go without a task update before its ADK session and its tasks are
	// deleted; zero keeps them.
	SessionIdleTTL time.Duration
	// Lease is the leader election's timings. Postgres only.
	Lease Lease
	// AdvertiseURL is where the other replicas reach this one. Required with StorePostgres.
	AdvertiseURL string
	// PodName names this replica as a lease holder.
	PodName string
	// LogLevel is the lowest level logged.
	LogLevel slog.Level
}

// OIDC reports whether the caller check is the OIDC one.
func (c Config) OIDC() bool { return c.OIDCIssuer != "" }

// Standalone reports whether the agent runs without the AX task runner beside it, and so answers /readyz itself.
func (c Config) Standalone() bool { return c.RunnerURL == "" }

// Parse reads the settings through getenv (os.Getenv in production), where an empty value is an unset one, and checks
// them.
func Parse(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	c := Config{
		ModelProvider: strings.ToLower(get("MODEL_PROVIDER", ProviderBedrock)),
		ModelRegion:   getenv("MODEL_REGION"),
		ModelName:     getenv("MODEL_NAME"),
		ListenAddr:    get("LISTEN_ADDR", "127.0.0.1:8090"),
		RunnerURL:     getenv("RUNNER_URL"),
		AuthTokenFile: getenv("AUTH_TOKEN_FILE"),
		OIDCIssuer:    getenv("OIDC_ISSUER"),
		OIDCAudience:  getenv("OIDC_AUDIENCE"),
		OIDCSubject:   getenv("OIDC_SUBJECT"),
		SQLitePath:    get("SQLITE_PATH", "ax-addon.db"),
		PostgresDSN:   getenv("POSTGRES_DSN"),
		AdvertiseURL:  getenv("ADVERTISE_URL"),
		PodName:       getenv("POD_NAME"),
	}
	var errs []error
	fail := func(err error) { errs = append(errs, err) }
	switch c.ModelProvider {
	case ProviderBedrock:
		if c.ModelRegion == "" {
			fail(errors.New("MODEL_REGION is required with MODEL_PROVIDER=bedrock"))
		}
	case ProviderAnthropic:
		if c.ModelRegion != "" {
			fail(errors.New("MODEL_REGION is set with MODEL_PROVIDER=anthropic, which has no region: unset it"))
		}
	default:
		fail(fmt.Errorf("MODEL_PROVIDER %q is not bedrock or anthropic", getenv("MODEL_PROVIDER")))
	}
	if c.ModelName == "" {
		fail(errors.New("MODEL_NAME is required"))
	}
	if getenv("STATE_DIR") != "" {
		fail(errors.New("STATE_DIR is no longer read: set SQLITE_PATH to the database file instead " +
			"(STATE_DIR plus /agent.db, such as /workspace/agent-kourier-state/agent.db) and unset STATE_DIR"))
	}
	host, _, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		fail(fmt.Errorf("LISTEN_ADDR %q is not host:port", c.ListenAddr))
	}
	c.PublicURL = get("PUBLIC_URL", "http://"+c.ListenAddr+"/a2a")
	if err := httpURL(c.PublicURL); err != nil {
		if getenv("PUBLIC_URL") == "" {
			fail(fmt.Errorf("PUBLIC_URL is unset and the one made from LISTEN_ADDR, %q, %w: set PUBLIC_URL to the URL callers reach the A2A endpoint at", c.PublicURL, err))
		} else {
			fail(fmt.Errorf("PUBLIC_URL %q %w", c.PublicURL, err))
		}
	}
	if c.RunnerURL != "" {
		if err := originURL(c.RunnerURL); err != nil {
			fail(fmt.Errorf("RUNNER_URL %w (its value is not shown)", err))
		}
	}
	for _, d := range []struct {
		key, def string
		into     *time.Duration
	}{
		{"TASK_RETENTION", "720h", &c.TaskRetention},
		{"SESSION_IDLE_TTL", "2160h", &c.SessionIdleTTL},
	} {
		v, err := duration(get(d.key, d.def), false)
		if err != nil {
			fail(fmt.Errorf("%s %q %w", d.key, getenv(d.key), err))
		}
		*d.into = v
	}
	if err := c.parseAuth(getenv, host); err != nil {
		fail(err)
	}
	if err := c.parseStore(getenv); err != nil {
		fail(err)
	}
	level, ok := map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError}[strings.ToLower(get("LOG_LEVEL", "info"))]
	if !ok {
		fail(fmt.Errorf("LOG_LEVEL %q is not debug, info, warn or error", getenv("LOG_LEVEL")))
	}
	c.LogLevel = level
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return c, nil
}

// parseAuth reads the caller check: the token file, OIDC, or none (AUTH_DISABLED, on a loopback listenHost only).
// Exactly one of them.
func (c *Config) parseAuth(getenv func(string) string, listenHost string) error {
	var errs []error
	if v := getenv("AUTH_DISABLED"); v != "" {
		disabled, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("AUTH_DISABLED %q is not true or false", v))
		}
		c.AuthDisabled = disabled
	}
	oidcSet := c.OIDCIssuer != "" || c.OIDCAudience != "" || c.OIDCSubject != ""
	if oidcSet {
		errs = append(errs, c.checkOIDC()...)
	}
	switch {
	case c.AuthDisabled && !loopback(listenHost):
		errs = append(errs, fmt.Errorf("AUTH_DISABLED=true needs a loopback LISTEN_ADDR, and %q is not one: "+
			"anyone who reaches the agent could run it", c.ListenAddr))
	case c.AuthDisabled && c.AuthTokenFile != "":
		errs = append(errs, errors.New("AUTH_TOKEN_FILE is set with AUTH_DISABLED=true: unset one of them"))
	case c.AuthDisabled && oidcSet:
		errs = append(errs, errors.New("OIDC_ISSUER, OIDC_AUDIENCE or OIDC_SUBJECT is set with AUTH_DISABLED=true: unset them or AUTH_DISABLED"))
	case c.AuthTokenFile != "" && oidcSet:
		errs = append(errs, errors.New("AUTH_TOKEN_FILE and OIDC_ISSUER, OIDC_AUDIENCE or OIDC_SUBJECT are both set: "+
			"the caller check is the token file or OIDC, not both"))
	case !c.AuthDisabled && c.AuthTokenFile == "" && !oidcSet:
		errs = append(errs, errors.New("a caller check is required: AUTH_TOKEN_FILE, or OIDC_ISSUER, OIDC_AUDIENCE and OIDC_SUBJECT "+
			"(or AUTH_DISABLED=true on a loopback LISTEN_ADDR)"))
	}
	header := getenv("AUTH_HEADER")
	if header == "" {
		header = "Authorization"
	}
	switch {
	case !headerName(header):
		errs = append(errs, errors.New("AUTH_HEADER is not a header name (its value is not shown)"))
	case reservedHeader(header):
		errs = append(errs, errors.New("AUTH_HEADER names a header that Agent Kourier never fills with a secret "+
			"(Proxy-Authorization, Cookie, X-User-Id, X-Agent-Name, X-Share-Token, X-Kagent-Insecure-Runtime-Identity): "+
			"leave it unset for Authorization, or name another header"))
	case oidcSet && !strings.EqualFold(header, "Authorization"):
		errs = append(errs, errors.New("AUTH_HEADER is set with OIDC, which reads the ID token from Authorization alone: unset AUTH_HEADER"))
	}
	c.AuthHeader = http.CanonicalHeaderKey(header)
	return errors.Join(errs...)
}

// checkOIDC reports what is wrong with a partly or wholly set OIDC check.
func (c *Config) checkOIDC() []error {
	var errs []error
	for _, v := range []struct{ key, value string }{
		{"OIDC_ISSUER", c.OIDCIssuer}, {"OIDC_AUDIENCE", c.OIDCAudience}, {"OIDC_SUBJECT", c.OIDCSubject},
	} {
		if v.value == "" {
			errs = append(errs, fmt.Errorf("%s is required with the OIDC check: set OIDC_ISSUER, OIDC_AUDIENCE and OIDC_SUBJECT, or none of them", v.key))
		}
	}
	if c.OIDCIssuer != "" {
		u, err := url.Parse(c.OIDCIssuer)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			errs = append(errs, errors.New("OIDC_ISSUER is not an http or https URL with a host and no user info, query or fragment (its value is not shown)"))
		}
	}
	return errs
}

// reservedHeader reports whether name is a header Agent Kourier reserves (IsReservedHeader in its api/v1alpha1) other
// than Authorization: an AgentBackend may not set one, so no valueSecretRef ever sends a token in it, and X-User-Id
// carries the Binding's userId, which is not a secret.
func reservedHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Proxy-Authorization", "Cookie", "X-User-Id", "X-Agent-Name", "X-Share-Token", "X-Kagent-Insecure-Runtime-Identity":
		return true
	}
	return false
}

// parseStore reads the store and the leader election's settings.
func (c *Config) parseStore(getenv func(string) string) error {
	var errs []error
	c.Store = StoreKind(strings.ToLower(getenv("STORE")))
	if c.Store == "" {
		c.Store = StoreSQLite
	}
	leaseKeys := []string{"LEASE_DURATION", "LEASE_RENEW_DEADLINE", "LEASE_RETRY_PERIOD"}
	c.Lease = Lease{Duration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second}
	switch c.Store {
	case StoreSQLite:
		if strings.TrimSpace(c.SQLitePath) == "" {
			errs = append(errs, errors.New("SQLITE_PATH is blank"))
		}
		if c.PostgresDSN != "" {
			errs = append(errs, errors.New("POSTGRES_DSN is set with STORE=sqlite: set STORE=postgres or unset it"))
		}
		for _, key := range leaseKeys {
			if getenv(key) != "" {
				errs = append(errs, fmt.Errorf("%s is set with STORE=sqlite, which runs one replica and elects no leader: unset it", key))
			}
		}
	case StorePostgres:
		if c.PostgresDSN == "" {
			errs = append(errs, errors.New("POSTGRES_DSN is required with STORE=postgres"))
		}
		c.AdvertiseURL = bracketIPv6(c.AdvertiseURL)
		if c.AdvertiseURL == "" {
			errs = append(errs, errors.New("ADVERTISE_URL is required with STORE=postgres"))
		} else if err := httpURL(c.AdvertiseURL); err != nil {
			errs = append(errs, fmt.Errorf("ADVERTISE_URL %q %w", c.AdvertiseURL, err))
		}
		if err := c.parseLease(getenv, leaseKeys); err != nil {
			errs = append(errs, err)
		}
		if c.PodName == "" {
			name, err := os.Hostname()
			if err != nil || name == "" {
				errs = append(errs, errors.New("POD_NAME is unset and the hostname cannot be read"))
			}
			c.PodName = name
		}
	default:
		errs = append(errs, fmt.Errorf("STORE %q is not sqlite or postgres", getenv("STORE")))
	}
	if c.PodName == "" {
		c.PodName, _ = os.Hostname() // only a lease holder's name, which sqlite does not use
	}
	return errors.Join(errs...)
}

// parseLease reads the lease timings over their defaults and checks the ordering the election needs.
func (c *Config) parseLease(getenv func(string) string, keys []string) error {
	into := []*time.Duration{&c.Lease.Duration, &c.Lease.RenewDeadline, &c.Lease.RetryPeriod}
	for i, key := range keys {
		v := getenv(key)
		if v == "" {
			continue
		}
		d, err := duration(v, true)
		if err != nil {
			return fmt.Errorf("%s %q %w", key, v, err)
		}
		*into[i] = d
	}
	switch l := c.Lease; {
	case l.Duration <= l.RenewDeadline:
		return fmt.Errorf("LEASE_DURATION (%s) must be greater than LEASE_RENEW_DEADLINE (%s)", l.Duration, l.RenewDeadline)
	case l.RenewDeadline <= time.Duration(jitterFactor*float64(l.RetryPeriod)):
		return fmt.Errorf("LEASE_RENEW_DEADLINE (%s) must be greater than LEASE_RETRY_PERIOD (%s) times %.1f", l.RenewDeadline, l.RetryPeriod, jitterFactor)
	}
	return nil
}

// duration parses a Go duration of zero or more, or of more than zero when positive.
func duration(v string, positive bool) (time.Duration, error) {
	d, err := time.ParseDuration(v)
	switch {
	case err != nil:
		return 0, errors.New("is not a Go duration such as 90s or 720h")
	case d < 0, positive && d == 0:
		if positive {
			return 0, errors.New("is not a duration of more than zero")
		}
		return 0, errors.New("is not a duration of zero or more")
	}
	return d, nil
}

// httpURL reports why v is not an absolute http or https URL with a host.
func httpURL(v string) error {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errors.New("is not an http or https URL with a host")
	}
	return nil
}

// originURL reports why v is not an http or https origin: a scheme and a host, with no user info, path, query or
// fragment (a trailing slash is allowed).
func originURL(v string) error {
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("is not an http or https URL with a host and no user info, path, query or fragment")
	}
	return nil
}

// bracketIPv6 puts the brackets a URL needs around an IPv6 address that has none, as http://$(POD_IP):8090 leaves one
// when the pod's IP is IPv6. An unbracketed host whose last colon is followed by a port and preceded by an IPv6 address
// is read as that address and port; one that is an IPv6 address whole, as the address alone. Anything else is returned
// as it came, for httpURL to judge.
func bracketIPv6(v string) string {
	scheme, rest, ok := strings.Cut(v, "://")
	if !ok {
		return v
	}
	host, tail := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		host, tail = rest[:i], rest[i:]
	}
	if strings.HasPrefix(host, "[") || strings.Count(host, ":") < 2 {
		return v
	}
	if i := strings.LastIndex(host, ":"); i > 0 {
		if addr, err := netip.ParseAddr(host[:i]); err == nil && addr.Is6() {
			if port, err := strconv.ParseUint(host[i+1:], 10, 16); err == nil && port > 0 {
				return scheme + "://" + net.JoinHostPort(host[:i], host[i+1:]) + tail
			}
		}
	}
	if addr, err := netip.ParseAddr(host); err == nil && addr.Is6() {
		return scheme + "://[" + host + "]" + tail
	}
	return v
}

// loopback reports whether host, of a host:port, is a loopback address or localhost. An empty host listens on every
// interface.
func loopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// headerName reports whether v is an HTTP header name (an RFC 9110 token).
func headerName(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		if r > 0x7e || r <= ' ' || strings.ContainsRune(`"(),/:;<=>?@[\]{}`, r) {
			return false
		}
	}
	return true
}
