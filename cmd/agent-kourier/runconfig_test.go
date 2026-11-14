package main

import (
	"reflect"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/agentkourier/agent-kourier/internal/broker"
)

// runConfig hands what the environment says to the broker. The admission fields matter most: with no own connection, a list or a
// selector that did not reach the broker would leave it nothing to admit by, and it would serve every namespace.
func TestRunConfigCarriesTheEnvironment(t *testing.T) {
	const sel = `{"matchLabels":{"team":"yes"}}`
	base := env{
		"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CONFIG_DIR": "/cfg", "AGENTKOURIER_DB_PATH": "/db/k.db",
		"AGENTKOURIER_SYNC_TIMEOUT": "7s", "AGENTKOURIER_KLOG_V": "3", "AGENTKOURIER_AUDIT_RETENTION": "720h", "OTEL_SERVICE_NAME": "kourier-test",
	}
	with := func(kv ...string) env {
		e := env{}
		for k, v := range base {
			e[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			e[kv[i]] = kv[i+1]
		}
		return e
	}
	for name, e := range map[string]env{
		"nothing: every namespace":   with(),
		"an own connection":          with("AGENTKOURIER_CHAT_CONNECTION", "platform/slack"),
		"a list, no own connection":  with("AGENTKOURIER_CONNECTION_NAMESPACES", "team-a,team-b"),
		"a selector, no own":         with(selectorKey, sel),
		"{}, no own":                 with(selectorKey, "{}"),
		"own, a list and a selector": with("AGENTKOURIER_CHAT_CONNECTION", "platform/slack", "AGENTKOURIER_CONNECTION_NAMESPACES", "team-a", selectorKey, sel),
		"postgres, with a selector": with("AGENTKOURIER_STORE", "postgres", "AGENTKOURIER_POSTGRES_DSN_SECRET", "agent-kourier/pg/dsn",
			"AGENTKOURIER_LEASE_NAMESPACE", "agent-kourier", "AGENTKOURIER_LEASE_NAME", "agent-kourier-leader", "AGENTKOURIER_POD_NAME", "agent-kourier-0", selectorKey, sel),
	} {
		t.Run(name, func(t *testing.T) {
			c, err := loadEnv(e.get)
			if err != nil {
				t.Fatal(err)
			}
			if e["AGENTKOURIER_STORE"] == "postgres" && (c.store == (broker.StoreConfig{}) || c.election == (broker.ElectionConfig{})) {
				t.Fatalf("store %+v, election %+v; want both set from the environment, or the comparisons below prove nothing", c.store, c.election)
			}
			got := c.runConfig()
			if got.ConfigSource != c.configSource || got.ChatConnection != c.chatConnection {
				t.Errorf("source %q, own %+v; want %q, %+v", got.ConfigSource, got.ChatConnection, c.configSource, c.chatConnection)
			}
			if !slices.Equal(got.ConnectionNamespaces, c.connectionNamespaces) {
				t.Errorf("ConnectionNamespaces %q; want %q", got.ConnectionNamespaces, c.connectionNamespaces)
			}
			if !sameSelector(got.ConnectionNamespaceSelector, c.connectionNamespaceSelector) {
				t.Errorf("ConnectionNamespaceSelector %v; want %v", got.ConnectionNamespaceSelector, c.connectionNamespaceSelector)
			}
			for field, pair := range map[string][2]any{
				"ConfigDir": {got.ConfigDir, c.configDir}, "DBPath": {got.DBPath, c.dbPath}, "Store": {got.Store, c.store},
				"Election": {got.Election, c.election}, "SyncTimeout": {got.SyncTimeout, c.syncTimeout}, "KlogVerbosity": {got.KlogVerbosity, c.klogV},
				"Telemetry": {got.Telemetry, c.telemetry}, "AuditRetention": {got.AuditRetention, c.auditRetention},
			} {
				if !reflect.DeepEqual(pair[0], pair[1]) {
					t.Errorf("%s = %v; want %v", field, pair[0], pair[1])
				}
			}
		})
	}
}

// sameSelector says whether a and b are both nil, or both a selector with the same requirements: nil is no selector, and an empty one
// every namespace, so the two must not be confused.
func sameSelector(a, b labels.Selector) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.String() == b.String() && a.Empty() == b.Empty()
}
