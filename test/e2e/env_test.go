package e2e

import "testing"

func TestNewEnv(t *testing.T) {
	vars := map[string]string{"E2E_SLACK_URL": "http://s", "E2E_SINK_URL": "http://k", "E2E_STUB_URL": "http://u", "E2E_ALERTMANAGER_URL": "http://a", "E2E_COURIER_URL": "http://c"}
	for _, tc := range []struct {
		name string
		env  map[string]string
		want Env
	}{
		{"defaults are the fast tier's Services, and courier has none until WP-35", nil, Env{
			SlackURL:        "http://fakeslack.e2e.svc.cluster.local:8080",
			SinkURL:         "http://sink.e2e.svc.cluster.local:8080",
			StubURL:         "http://stubagent.e2e.svc.cluster.local:8080",
			AlertmanagerURL: "http://alertmanager.monitoring.svc.cluster.local:9093",
		}},
		{"every variable overrides", vars, Env{SlackURL: "http://s", SinkURL: "http://k", StubURL: "http://u", AlertmanagerURL: "http://a", CourierURL: "http://c"}},
	} {
		if got := NewEnv(func(k string) string { return tc.env[k] }); got != tc.want {
			t.Errorf("%s: NewEnv() = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
