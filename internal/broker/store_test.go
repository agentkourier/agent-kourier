package broker

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/agentkourier/agent-kourier/internal/config/k8ssecrets"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// fakePassword is a password that the tests look for in every error and log line. It is made up.
const fakePassword = "fake-pg-password-7Qz"

// deadDSN names a server nobody listens on, so that opening it fails at once, with a password in it.
const deadDSN = "postgres://agentkourier:" + fakePassword + "@127.0.0.1:1/agentkourier?sslmode=disable&connect_timeout=2"

var dsnRef = SecretKey{Namespace: "agent-kourier", Name: "agent-kourier-pg", Key: "dsn"}

// values is a secretValues that serves one value and records what it was asked for.
type values struct {
	dsn   string
	asked []SecretKey
	err   error
}

func (v *values) Value(_ context.Context, namespace, name, key string) (secret.Value, error) {
	v.asked = append(v.asked, SecretKey{namespace, name, key})
	if v.err != nil {
		return secret.Value{}, v.err
	}
	return secret.New(v.dsn), nil
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestOpenStoreDefaultsToSQLiteOnTheGivenPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-kourier.db")
	for name, kind := range map[string]StoreKind{"unset": "", "named": StoreSQLite} {
		t.Run(name, func(t *testing.T) {
			st, err := openStore(t.Context(), StoreConfig{Kind: kind}, path, &values{}, discard())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			if _, err := os.Stat(path); err != nil {
				t.Errorf("the SQLite file: %v", err)
			}
			var _ store.Store = st // the one interface Run uses, whichever store is chosen
		})
	}
}

func TestOpenStoreSQLiteReadsNoSecret(t *testing.T) {
	v := &values{}
	st, err := openStore(t.Context(), StoreConfig{}, filepath.Join(t.TempDir(), "agent-kourier.db"), v, discard())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if len(v.asked) != 0 {
		t.Errorf("SQLite read the Secrets %v", v.asked)
	}
}

func TestOpenStorePostgresAsksForTheConfiguredSecretKey(t *testing.T) {
	v := &values{dsn: deadDSN}
	_, err := openStore(t.Context(), StoreConfig{Kind: StorePostgres, PostgresDSN: dsnRef}, "", v, discard())
	if err == nil {
		t.Fatal("openStore on a server nobody listens on succeeded")
	}
	if len(v.asked) != 1 || v.asked[0] != dsnRef {
		t.Errorf("asked for %v, want exactly %v", v.asked, dsnRef)
	}
	if errors.Is(err, ErrWiring) {
		t.Errorf("an unreachable server is a failure of a dependency, not a wiring error: %v", err)
	}
}

// What the DSN says never reaches an error, whether the server cannot be reached or the DSN cannot be read.
func TestOpenStorePostgresErrorsCarryNoPasswordAndNoDSN(t *testing.T) {
	for name, dsn := range map[string]string{
		"unreachable":     deadDSN,
		"bad port":        "postgres://agentkourier:" + fakePassword + "@127.0.0.1:notaport/agentkourier",
		"bad escape":      "postgres://agentkourier:" + fakePassword + "%zz@127.0.0.1:1/agentkourier",
		"key and value":   "host=127.0.0.1 port=1 user=agentkourier password=" + fakePassword + " dbname=agentkourier sslmode=disable connect_timeout=2",
		"unknown setting": "postgres://agentkourier:" + fakePassword + "@127.0.0.1:1/agentkourier?no_such_setting=1",
	} {
		t.Run(name, func(t *testing.T) {
			var logs strings.Builder
			log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			_, err := openStore(t.Context(), StoreConfig{Kind: StorePostgres, PostgresDSN: dsnRef}, "", &values{dsn: dsn}, log)
			if err == nil {
				t.Fatal("openStore succeeded")
			}
			for _, leak := range []string{fakePassword, dsn} {
				if strings.Contains(err.Error(), leak) || strings.Contains(logs.String(), leak) {
					t.Errorf("a secret reached the error or the log:\nerror: %v\nlog: %s", err, logs.String())
				}
			}
		})
	}
}

// A Secret that cannot be read is a config problem: an error that wraps ErrWiring and names the Secret and
// the key, never the DSN.
func TestOpenStorePostgresRefusesASecretThatCannotBeRead(t *testing.T) {
	opaque := func(data map[string][]byte) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "agent-kourier", Name: "agent-kourier-pg"}, Data: data}
	}
	for name, tc := range map[string]struct {
		objects []*corev1.Secret
		want    error
	}{
		"no such Secret":  {nil, nil},
		"no such key":     {[]*corev1.Secret{opaque(map[string][]byte{"other": []byte(deadDSN)})}, secret.ErrKeyNotFound},
		"empty value":     {[]*corev1.Secret{opaque(map[string][]byte{"dsn": {}})}, secret.ErrEmptyValue},
		"only whitespace": {[]*corev1.Secret{opaque(map[string][]byte{"dsn": []byte(" \n")})}, secret.ErrEmptyValue},
		"not Opaque": {[]*corev1.Secret{{
			ObjectMeta: metav1.ObjectMeta{Namespace: "agent-kourier", Name: "agent-kourier-pg"},
			Type:       corev1.SecretTypeTLS, Data: map[string][]byte{"dsn": []byte(deadDSN)},
		}}, secret.ErrUnsupportedType},
	} {
		t.Run(name, func(t *testing.T) {
			client := fake.NewClientset()
			for _, s := range tc.objects {
				if _, err := client.CoreV1().Secrets(s.Namespace).Create(t.Context(), s, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			r := k8ssecrets.New(client, []string{"agent-kourier"})
			if err := r.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(r.Stop)

			_, err := openStore(t.Context(), StoreConfig{Kind: StorePostgres, PostgresDSN: dsnRef}, "", r, discard())

			if !errors.Is(err, ErrWiring) || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Errorf("openStore = %v, want ErrWiring wrapping %v", err, tc.want)
			}
			msg := err.Error()
			if !strings.Contains(msg, "agent-kourier/agent-kourier-pg") || !strings.Contains(msg, `"dsn"`) {
				t.Errorf("the error does not name the Secret and the key: %v", err)
			}
			if strings.Contains(msg, fakePassword) || strings.Contains(msg, "postgres://") {
				t.Errorf("the error carries the DSN: %v", err)
			}
		})
	}
}

func TestOpenStoreRefusesWhatItDoesNotKnow(t *testing.T) {
	_, err := openStore(t.Context(), StoreConfig{Kind: "mysql"}, "x", &values{}, discard())
	if !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "mysql") {
		t.Errorf("openStore = %v, want ErrWiring naming the store", err)
	}
}

// startupParams is a server that records the parameters of the first connection's startup message and then closes it, so
// that a test can see what the store asks of a server without one. It returns the DSN of the server and the parameters
// once a connection has come.
func startupParams(t *testing.T) (dsn string, params func() map[string]string) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan map[string]string, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			func() {
				defer func() { _ = conn.Close() }()
				var length [4]byte
				if _, err := io.ReadFull(conn, length[:]); err != nil {
					return
				}
				body := make([]byte, binary.BigEndian.Uint32(length[:])-4)
				if _, err := io.ReadFull(conn, body); err != nil {
					return
				}
				fields := strings.Split(string(body[4:]), "\x00") // after the protocol version: name, value, name, value, ...
				p := map[string]string{}
				for i := 0; i+1 < len(fields); i += 2 {
					p[fields[i]] = fields[i+1]
				}
				got <- p
			}()
		}
	}()
	dsn = "postgres://agentkourier:" + fakePassword + "@" + ln.Addr().String() + "/agentkourier?sslmode=disable&connect_timeout=2"
	return dsn, func() map[string]string {
		select {
		case p := <-got:
			return p
		case <-time.After(5 * time.Second):
			t.Fatal("no connection reached the server")
			return nil
		}
	}
}

// With the role setting the timeouts (behind a pooler that refuses startup parameters) Agent Kourier sends none; otherwise
// it sends its own.
func TestPostgresSendsItsTimeoutsUnlessTheRoleSetsThem(t *testing.T) {
	for name, tc := range map[string]struct {
		role bool
		want bool
	}{"agent-kourier's own": {false, true}, "the role's": {true, false}} {
		t.Run(name, func(t *testing.T) {
			dsn, params := startupParams(t)
			_, err := openStore(t.Context(), StoreConfig{Kind: StorePostgres, PostgresDSN: dsnRef, PostgresRoleTimeouts: tc.role}, "", &values{dsn: dsn}, discard())
			if err == nil {
				t.Fatal("openStore on a server that closes every connection succeeded")
			}
			p := params()
			for _, k := range []string{"idle_in_transaction_session_timeout", "statement_timeout"} {
				if _, sent := p[k]; sent != tc.want {
					t.Errorf("%s sent = %t, want %t (startup parameters %v)", k, sent, tc.want, p)
				}
			}
		})
	}
}
