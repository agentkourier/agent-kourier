package redact

import "testing"

// benchTexts are the texts the entries mostly read: a chat message with no secret in it, an alert as Alertmanager posts it,
// a manifest with an env entry whose value is a secret and a shell line with a secret flag. The secrets are fake.
var benchTexts = []struct{ name, text string }{
	{"message", "Hey team, the deploy to prod finished at 14:02 and the dashboards look healthy. Can someone double-check " +
		"the checkout latency before we close the incident? I'll write up the timeline in the doc after lunch."},
	{"alert", "[FIRING:1] KubePodCrashLooping (production api-server-7c9d8f6b5-x2k4p)\n*Alert:* Pod is crash looping - warning\n" +
		"*Description:* Pod production/api-server-7c9d8f6b5-x2k4p (api) is in waiting state (reason: \"CrashLoopBackOff\").\n" +
		"*Details:*\n  • alertname: KubePodCrashLooping\n  • container: api\n  • namespace: production\n  • severity: warning\n" +
		"  • runbook_url: https://runbooks.example.com/alerts/KubePodCrashLooping\n"},
	{"manifest", "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n  namespace: production\nspec:\n  template:\n" +
		"    spec:\n      containers:\n        - name: api\n          image: registry.example.com/api:1.4.2\n          env:\n" +
		"            - name: LOG_LEVEL\n              value: info\n            - name: DB_PASSWORD\n              value: fake-db-password\n"},
	{"shell", "curl -sS --token fake-token-value -H 'Accept: application/json' https://api.example.com/v1/items?limit=50 | jq ."},
}

// BenchmarkEntries times each entry on each of benchTexts.
func BenchmarkEntries(b *testing.B) {
	for _, e := range leakEntries {
		for _, in := range benchTexts {
			b.Run(e.name+"/"+in.name, func(b *testing.B) {
				for b.Loop() {
					e.fn(in.text, 4000)
				}
			})
		}
	}
}
