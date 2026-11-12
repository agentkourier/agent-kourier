package k8ssecrets

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/redact"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// The errors of Value are logged and shown in a Binding's status, which redact the value of anything that reads as a secret name followed
// by a colon. A key named botToken, or a Secret of the type kubernetes.io/service-account-token, is such a name, so no error puts a colon
// after one: the cause that follows it would be shown as [redacted].
func TestTheErrorsOfValueSurviveRedaction(t *testing.T) {
	empty := secret("sre", "empty")
	empty.Data["botToken"] = []byte{}
	account := secret("sre", "account")
	account.Type = "kubernetes.io/service-account-token"
	client := newAPI(t, empty, account)
	r := follow(t, client, sn("sre", "empty"), sn("sre", "account"))

	for _, tc := range []struct{ name, secret, key string }{
		{"a key the Secret lacks", "empty", "appToken"},
		{"a key with no value", "empty", "botToken"},
		{"a Secret of a type that is never read", "account", "token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.Value(t.Context(), "sre", tc.secret, tc.key)
			if err == nil {
				t.Fatal("Value succeeded")
			}
			if got := redact.FreeText(err.Error()); got != err.Error() {
				t.Errorf("redaction changes the error\n got %q\nwant %q", got, err.Error())
			}
		})
	}

	t.Run("a Secret whose cache has not synced", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		_, err := unsynced(t).Value(ctx, "sre", "creds", "botToken")
		if err == nil {
			t.Fatal("Value succeeded")
		}
		if got := redact.FreeText(err.Error()); got != err.Error() {
			t.Errorf("redaction changes the error\n got %q\nwant %q", got, err.Error())
		}
	})
}

// The causes that Value wraps name the Secret (a name such as slack-credentials or agent-token is itself a secret word to the redaction), so
// the colon after it must follow a word of the broker's own: not "secret team-a/slack-credentials: secret cache not synced", which shows
// as "...: [redacted] cache not synced". Each case is an error the resolver returns for a Secret with such a name, by the path that makes it.
func TestTheCausesOfAReadSurviveRedactionForASecretNamedLikeACredential(t *testing.T) {
	failing := func(t *testing.T, err error) *Resolver {
		t.Helper()
		client := fake.NewClientset()
		client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, err })
		r := New(client, WithLogger(testutil.DiscardLogger()))
		r.wait = 50 * time.Millisecond
		t.Cleanup(r.Stop)
		return r
	}
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "x", errors.New("no grant"))
	for _, name := range []string{"slack-credentials", "agent-token"} {
		n := sn("team-a", name)
		for _, tc := range []struct {
			what string
			want error
			err  func(t *testing.T) error
		}{
			{"a cache that has not synced", ErrNotSynced, func(t *testing.T) error {
				r := failing(t, errors.New("connection refused"))
				_, err := r.Value(t.Context(), n.Namespace, n.Name, "botToken")
				return err
			}},
			{"a Watch that waits for a cache that has not synced", context.DeadlineExceeded, func(t *testing.T) error {
				r := failing(t, errors.New("connection refused"))
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()
				return r.Watch(ctx, []config.SecretName{n})
			}},
			{"a read the API server denied", ErrAccessDenied, func(t *testing.T) error {
				r := failing(t, forbidden)
				_, err := r.Value(t.Context(), n.Namespace, n.Name, "botToken")
				return err
			}},
			{"a Watch the API server denied", ErrAccessDenied, func(t *testing.T) error {
				r := failing(t, forbidden)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second) // a regressed denial fails here, not at go test's timeout
				defer cancel()
				return r.Watch(ctx, []config.SecretName{n})
			}},
			{"a Secret that is not watched", ErrNotWatched, func(t *testing.T) error {
				r := New(fake.NewClientset(), WithLogger(testutil.DiscardLogger()))
				t.Cleanup(r.Stop)
				_, err := r.get(n.Namespace, n.Name, nil)
				return err
			}},
			{"a Resolver that is stopped", ErrStopped, func(t *testing.T) error {
				r := New(fake.NewClientset(), WithLogger(testutil.DiscardLogger()))
				r.Stop()
				_, err := r.Value(t.Context(), n.Namespace, n.Name, "botToken")
				return err
			}},
			{"a Secret that does not exist", config.ErrSecretNotFound, func(t *testing.T) error {
				r := follow(t, newAPI(t), n)
				_, err := r.Value(t.Context(), n.Namespace, n.Name, "botToken")
				return err
			}},
		} {
			t.Run(name+"/"+tc.what, func(t *testing.T) {
				err := tc.err(t)
				if !errors.Is(err, tc.want) {
					t.Fatalf("error = %v; want one that wraps %v", err, tc.want)
				}
				if got := redact.FreeText(err.Error()); got != err.Error() {
					t.Errorf("redaction changes the error\n got %q\nwant %q", got, err.Error())
				}
			})
		}
	}
}
