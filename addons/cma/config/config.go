// Package config reads the adapter's settings from its environment and checks them, so that a setting the adapter
// cannot use stops it at start with an error that names the variable. An error never shows the value of a variable
// that may hold a secret (POSTGRES_DSN, AUTH_HEADER).
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

// StoreKind names where the adapter keeps its state.
type StoreKind string

// The stores.
const (
	// StoreSQLite is one file, opened by one adapter at a time.
	StoreSQLite StoreKind = "sqlite"
	// StorePostgres is a database that several replicas share.
	StorePostgres StoreKind = "postgres"
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

// Config is the adapter's settings.
type Config struct {
	// AgentID and EnvironmentID name the CMA agent the adapter runs and the environment its sessions run in.
	AgentID, EnvironmentID string
	// ListenAddr is where the adapter listens.
	ListenAddr string
	// PublicURL is the JSON-RPC endpoint the agent card names.
	PublicURL string
	// SessionIdleTTL is how long a context may go without a turn before its session is archived; zero never archives.
	SessionIdleTTL time.Duration
	// ToolResultTimeout is how long a turn waits, with no session event, for a sandbox tool's result; zero waits for ever.
	ToolResultTimeout time.Duration
	// AuthTokenFile holds the accepted tokens, one per line. Empty only when AuthDisabled.
	AuthTokenFile string
	// AuthHeader is the header that carries the token, in canonical form. Authorization carries "Bearer <token>"; any
	// other header carries the token as its whole value.
	AuthHeader string
	// AuthDisabled turns the caller check off. It is accepted only with a loopback ListenAddr.
	AuthDisabled bool
	// Store is where the adapter keeps its state.
	Store StoreKind
	// SQLitePath is the SQLite file. Used with StoreSQLite.
	SQLitePath string
	// PostgresDSN is the Postgres connection string. Required with StorePostgres. It may hold a password: never log it.
	PostgresDSN string
	// TaskRetention is how long a settled task and its message IDs are kept; zero keeps them.
	TaskRetention time.Duration
	// Lease is the leader election's timings. Postgres only.
	Lease Lease
	// AdvertiseURL is where the other replicas reach this one. Required with StorePostgres.
	AdvertiseURL string
	// PodName names this replica as a lease holder.
	PodName string
	// LogLevel is the lowest level logged.
	LogLevel slog.Level
}

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
		AgentID:       getenv("CMA_AGENT_ID"),
		EnvironmentID: getenv("CMA_ENVIRONMENT_ID"),
		ListenAddr:    get("LISTEN_ADDR", "127.0.0.1:8090"),
		AuthTokenFile: getenv("AUTH_TOKEN_FILE"),
		SQLitePath:    get("SQLITE_PATH", "cma-addon.db"),
		PostgresDSN:   getenv("POSTGRES_DSN"),
		AdvertiseURL:  getenv("ADVERTISE_URL"),
		PodName:       getenv("POD_NAME"),
	}
	var errs []error
	fail := func(err error) { errs = append(errs, err) }
	if c.AgentID == "" {
		fail(errors.New("CMA_AGENT_ID is required"))
	}
	if c.EnvironmentID == "" {
		fail(errors.New("CMA_ENVIRONMENT_ID is required"))
	}
	host, _, err := net.SplitHostPort(c.ListenAddr)
	if err != nil {
		fail(fmt.Errorf("LISTEN_ADDR %q is not host:port", c.ListenAddr))
	}
	c.PublicURL = get("PUBLIC_URL", "http://"+c.ListenAddr+"/a2a")
	if err := httpURL(c.PublicURL); err != nil {
		fail(fmt.Errorf("PUBLIC_URL %q %w", c.PublicURL, err))
	}
	for _, d := range []struct {
		key, def string
		into     *time.Duration
	}{
		{"SESSION_IDLE_TTL", "2160h", &c.SessionIdleTTL},
		{"TOOL_RESULT_TIMEOUT", "10m", &c.ToolResultTimeout},
		{"TASK_RETENTION", "720h", &c.TaskRetention},
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

// parseAuth reads the caller check. listenHost is LISTEN_ADDR's host, which must be loopback for AUTH_DISABLED.
func (c *Config) parseAuth(getenv func(string) string, listenHost string) error {
	var errs []error
	if v := getenv("AUTH_DISABLED"); v != "" {
		disabled, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("AUTH_DISABLED %q is not true or false", v))
		}
		c.AuthDisabled = disabled
	}
	switch {
	case c.AuthDisabled && !loopback(listenHost):
		errs = append(errs, fmt.Errorf("AUTH_DISABLED=true needs a loopback LISTEN_ADDR, and %q is not one: "+
			"anyone who reaches the adapter could run the agent", c.ListenAddr))
	case c.AuthDisabled && c.AuthTokenFile != "":
		errs = append(errs, errors.New("AUTH_TOKEN_FILE is set with AUTH_DISABLED=true: unset one of them"))
	case !c.AuthDisabled && c.AuthTokenFile == "":
		errs = append(errs, errors.New("AUTH_TOKEN_FILE is required (or AUTH_DISABLED=true on a loopback LISTEN_ADDR)"))
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
	}
	c.AuthHeader = http.CanonicalHeaderKey(header)
	return errors.Join(errs...)
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
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("is not an http or https URL with a host")
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
