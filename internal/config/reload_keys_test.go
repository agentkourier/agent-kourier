package config

import (
	"context"
	"fmt"
	"testing"
)

// keyedSecrets is a watchingSecrets that is also told which keys of each Secret the config reads.
type keyedSecrets struct {
	watchingSecrets
}

func (k *keyedSecrets) WatchKeys(_ context.Context, reads []SecretRead) error {
	k.record(fmt.Sprint("watchkeys ", reads))
	return nil
}

func (k *keyedSecrets) RetainKeys(reads []SecretRead) { k.record(fmt.Sprint("retainkeys ", reads)) }

// A resolver that is told the keys is asked for them, not only for the names: the keys of each Secret that the config reads, each
// once and sorted, whichever objects name the Secret, so that it can keep those and drop the rest of the Secret. It is asked
// instead of the names, never as well as them.
func TestReloaderTellsAKeyedWatcherWhichKeysTheConfigReads(t *testing.T) {
	reads := fmt.Sprint([]SecretRead{
		{SecretName{"agent-kourier-system", "slack-main"}, []string{"appToken", "botToken"}},
		{SecretName{"sre", "agent-kourier-sre-alerts-oidc"}, []string{"token"}},
	})
	secrets := &keyedSecrets{watchingSecrets{fakeSecrets: specSecrets()}}
	r := newTestReloader(specDir(t), secrets)

	mustReload(t, r, true)
	calls := secrets.take()
	if len(calls) < 3 || calls[0] != "watchkeys "+reads || calls[len(calls)-1] != "retainkeys "+reads {
		t.Fatalf("a good reload asked %v; want watchkeys, the checks, then retainkeys, each with %s", calls, reads)
	}
	for _, c := range calls[1 : len(calls)-1] {
		if c != "keys" {
			t.Errorf("between watchkeys and retainkeys the Reloader asked %q", c)
		}
	}
}

// Two references to one Secret ask for the keys of both, once each.
func TestSecretReadsMergeTheKeysOfEveryReferenceToASecret(t *testing.T) {
	from := identity{"Binding", "sre", "a"}
	refs := []secretRef{
		{from, "p1", "creds", []string{"token"}},
		{from, "p2", "other", []string{"x"}},
		{identity{"AgentBackend", "sre", "b"}, "p3", "creds", []string{"alt", "token"}},
		{identity{"Binding", "ops", "c"}, "p4", "creds", []string{"token"}},
	}
	got := fmt.Sprint(secretReads(refs))
	want := fmt.Sprint([]SecretRead{
		{SecretName{"ops", "creds"}, []string{"token"}},
		{SecretName{"sre", "creds"}, []string{"alt", "token"}},
		{SecretName{"sre", "other"}, []string{"x"}},
	})
	if got != want {
		t.Errorf("secretReads = %s; want %s", got, want)
	}
}
