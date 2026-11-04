package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Source is where a Reloader reads the config objects from, and how it tells that they changed.
// The Reloader owns the polling, the resolving against Secrets and the last good snapshot; a
// Source owns only reading and fingerprinting. It makes no assumption about what a Source reads:
// a directory of files is one (the default), and anything that can name its current state is
// another.
//
// A Reloader calls the methods of its Source from one goroutine at a time.
type Source interface {
	// Set reads the objects and returns them with the fingerprint of the state it read them
	// from. It returns the fingerprint on an error too, so that a state that fails to read is
	// recognised on the next poll and not tried again until it changes. The Set is the caller's:
	// Resolve takes ownership of it and the Snapshot keeps pointers into it, so each call must
	// return a Set of its own, never one that is read or changed again.
	Set(ctx context.Context) (*Set, string, error)
	// Fingerprint summarizes the source's current state without reading the objects, so that
	// the Reloader can poll it cheaply. It is equal to what Set returned for the same state and
	// differs after a change, and it is opaque: the Reloader compares it only for equality. It
	// cannot fail. A state that cannot be read fingerprints as that failure, so that it looks
	// the same on every poll and fixing it looks like a change.
	Fingerprint(ctx context.Context) string
}

// NewDirSource returns the Source of the config directory dir, which is what a Reloader reads when it is given Dir and no
// Source. It is for a test of what two sources have in common (sourcetest).
func NewDirSource(dir string) Source { return dirSource{dir} }

// dirSource is the Source of a config directory, read as Load does. It holds no state.
type dirSource struct{ dir string }

// Set loads one generation of the directory; see generation. A change to the directory while
// it loads is for the caller to detect by comparing Fingerprint with the one Set returned.
func (s dirSource) Set(ctx context.Context) (*Set, string, error) {
	root, gen := generation(s.dir)
	fp := fingerprint(root, gen)
	set, err := Load(ctx, root)
	return set, fp, err
}

// Fingerprint summarizes the directory as it is now.
func (s dirSource) Fingerprint(context.Context) string {
	return fingerprint(generation(s.dir))
}

// generation returns the directory to read config from. A mounted ConfigMap replaces its
// files by pointing the ..data symlink at a new directory, and files read through the
// top-level links can come from two generations. Reading the target directory itself, which
// never changes, cannot. Without ..data, the directory is read as it is, and gen is empty.
func generation(dir string) (root, gen string) {
	gen, err := os.Readlink(filepath.Join(dir, "..data"))
	if err != nil {
		return dir, ""
	}
	if filepath.IsAbs(gen) {
		return gen, gen
	}
	return filepath.Join(dir, gen), gen
}

// fingerprint summarizes the config files of root by name, size, and modification time, and
// the generation, so that a change to any of them changes it. It cannot fail: a directory or
// file that cannot be read fingerprints as its error, so that a broken state looks the same
// on every poll and fixing it looks like a change.
func fingerprint(root, gen string) string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "!" + err.Error()
	}
	var b strings.Builder
	b.WriteString(gen)
	for _, e := range entries {
		if !isConfigFile(e.Name()) {
			continue
		}
		fi, err := os.Stat(filepath.Join(root, e.Name())) // follows symlinks
		if err != nil {
			fmt.Fprintf(&b, "|%s:!%v", e.Name(), err)
			continue
		}
		fmt.Fprintf(&b, "|%s:%d:%d", e.Name(), fi.Size(), fi.ModTime().UnixNano())
	}
	return b.String()
}
