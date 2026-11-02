package config

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// SecretVersioner is an optional interface of a SecretResolver: it reports a version of a
// Secret that changes whenever the Secret does, even if its keys do not (a rotated value). The
// version is opaque and compared only for equality. Reloader.Revision uses it when the resolver
// has it; one that does not, such as PresentSecrets, leaves Secrets out of the revision.
//
// Like Keys, it must never return a value, not in its result and not in its errors.
type SecretVersioner interface {
	// SecretVersion returns the version of the Secret, or an error wrapping ErrSecretNotFound.
	// It must not call out to a remote service: Revision calls it for every referenced Secret
	// every few seconds.
	SecretVersion(ctx context.Context, namespace, name string) (string, error)
}

// secretKey names a Secret that a snapshot's objects refer to.
type secretKey struct{ namespace, name string }

// published is what a successful reload hands to readers in one atomic step: the Snapshot, and
// what Revision needs to describe it. Readers of Current take the Snapshot out and never see
// the rest, and Revision never sees a fingerprint that belongs to a different Snapshot.
type published struct {
	snap        *Snapshot
	fingerprint string      // of the config files the snapshot was built from
	secrets     []secretKey // the Secrets the snapshot's objects refer to, sorted and unique
}

// newPublished returns the published form of snap, built from the config files that
// fingerprint describes, with the Secrets that refs name.
func newPublished(snap *Snapshot, fingerprint string, refs []secretRef) *published {
	keys := make([]secretKey, 0, len(refs))
	for _, ref := range refs {
		keys = append(keys, secretKey{ref.from.namespace, ref.name})
	}
	slices.SortFunc(keys, func(a, b secretKey) int {
		return cmp.Or(cmp.Compare(a.namespace, b.namespace), cmp.Compare(a.name, b.name))
	})
	return &published{snap: snap, fingerprint: fingerprint, secrets: slices.Compact(keys)}
}

// Revision returns an opaque digest of what the current snapshot was built from: the
// fingerprint of the config files, and the version of every Secret the snapshot's objects refer
// to. It changes when a config file changes or a referenced Secret is created, updated or
// deleted, even if the Secret's keys stay the same, and it stays the same across a recheck that
// finds nothing new, which publishes a new Snapshot all the same. Compare revisions only for
// equality.
//
// A missing Secret and one that cannot be read are two more states of a Secret, each stable,
// not errors. If the resolver is not a SecretVersioner, only the config files count. A failed
// reload leaves the snapshot, and so the config part of the revision, as it was. Before the
// first successful load the revision is one fixed value.
//
// Revision makes no API call when the resolver reads a cache, as the cluster-backed one does,
// and the digest holds Secret names and versions, never a value. It is safe to call from any
// goroutine, as often as every few seconds.
func (r *Reloader) Revision(ctx context.Context) string {
	var b strings.Builder
	if p := r.snap.Load(); p == nil {
		b.WriteString("unloaded")
	} else {
		fmt.Fprintf(&b, "config %q\n", p.fingerprint)
		if v, ok := r.secrets.(SecretVersioner); ok {
			for _, k := range p.secrets {
				state := "missing"
				version, err := v.SecretVersion(ctx, k.namespace, k.name)
				switch {
				case err == nil:
					state = fmt.Sprintf("version %q", version)
				case !errors.Is(err, ErrSecretNotFound):
					state = "unreadable"
				}
				fmt.Fprintf(&b, "secret %q %q %s\n", k.namespace, k.name, state)
			}
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
