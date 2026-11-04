package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// SecretVersioner is an optional interface of a SecretResolver: it reports a version of a
// Secret that changes whenever the resolver learns that the Secret did, even if its keys do not
// (a rotated value). The version is opaque and compared only for equality. Reloader.Revision
// uses it when the resolver has it; one that does not, such as PresentSecrets, leaves Secrets
// out of the revision.
//
// Like Keys, it must never return a value, not in its result and not in its errors.
type SecretVersioner interface {
	// SecretVersion returns the version of the Secret, or an error wrapping ErrSecretNotFound.
	// It must not call out to a remote service: Revision calls it for every referenced Secret
	// every few seconds.
	SecretVersion(ctx context.Context, namespace, name string) (string, error)
}

// published is what a successful reload hands to readers in one atomic step: the Snapshot, and
// what Revision needs to describe it. Readers of Current take the Snapshot out and never see
// the rest, and Revision never sees a fingerprint that belongs to a different Snapshot.
type published struct {
	snap        *Snapshot
	fingerprint string       // of the config files the snapshot was built from
	secrets     []SecretName // the Secrets the snapshot's objects refer to, sorted and unique
}

// newPublished returns the published form of snap, built from the config files that
// fingerprint describes, with the Secrets that refs name.
func newPublished(snap *Snapshot, fingerprint string, refs []secretRef) *published {
	return &published{snap: snap, fingerprint: fingerprint, secrets: secretNames(refs)}
}

// Revision returns an opaque digest of what the current snapshot was built from: the
// fingerprint of the config files, and the version of every Secret the snapshot's objects refer
// to. It changes when a config file changes or when the resolver reports that a referenced
// Secret was created, updated or deleted, even if the Secret's keys stay the same, and it stays
// the same across a recheck that finds nothing new, which publishes a new Snapshot all the same.
// Compare revisions only for equality.
//
// The Secret part is as current as the resolver. The cluster-backed one reads an informer
// cache; while its list or watch is failing (RBAC revoked, the API server unreachable) the
// cache keeps its last contents, so the versions in the revision stay where they were, and so
// do the values that Value and Keys return from that cache. A Secret rotated meanwhile moves
// neither, until the list or watch succeeds again. The resolver logs each failure as a warning;
// Revision reports none.
//
// A missing Secret and one that cannot be read are two more states of a Secret, each stable,
// not errors. If the resolver is not a SecretVersioner, only the config files count. A failed
// reload leaves the snapshot, and so the config part of the revision, as it was. Before the
// first successful load the revision is one fixed value.
//
// Read Revision before Current, never after. They are separate loads, and a reload or a Secret
// update can land between them. Revision first can pair an older revision with newer state, so
// the next Revision differs and the state is checked again. Current first can pair a newer
// revision with older state, and a caller that records the revision it checked sees no change,
// so nothing the revision does prompts it to check the newer state.
//
// Revision makes no API call when the resolver reads a cache, as the cluster-backed one does.
// The digest is computed from Secret names and versions, never a value. It is safe to call from
// any goroutine, as often as every few seconds.
func (r *Reloader) Revision(ctx context.Context) string {
	var b strings.Builder
	if p := r.snap.Load(); p == nil {
		b.WriteString("unloaded")
	} else {
		fmt.Fprintf(&b, "config %q\n", p.fingerprint)
		if v, ok := r.secrets.(SecretVersioner); ok {
			for _, k := range p.secrets {
				state := "missing"
				version, err := v.SecretVersion(ctx, k.Namespace, k.Name)
				switch {
				case err == nil:
					state = fmt.Sprintf("version %q", version)
				case !errors.Is(err, ErrSecretNotFound):
					state = "unreadable"
				}
				fmt.Fprintf(&b, "secret %q %q %s\n", k.Namespace, k.Name, state)
			}
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
