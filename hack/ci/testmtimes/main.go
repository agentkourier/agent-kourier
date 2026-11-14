// Command testmtimes sets the modification time of every file git tracks in a working tree to one derived from the
// file's blob hash, and of every directory that holds a tracked path to one fixed time, so that go test's result cache
// keys a file by its contents in every checkout. CI runs it before go test (ci.yml, hack/ci/testshards' test):
//
//	go run ./hack/ci/testmtimes [DIR]
//
// DIR is any directory of the working tree (the current one by default); the command works on the whole tree.
//
// go test reuses a package's result only if every file inside the module that its tests opened or stat'ed has the
// size, mode and modification time it had when the result was saved, and refuses to save one if such a file was
// modified less than 2 s before the test read it. It does not hash contents. A checkout writes every file with the
// current time, so without this a package whose tests read a file of the repository (testdata, a golden file, a
// page under docs/) runs again in every CI run, whether or not that file changed.
//
// A regular file's time is 2001-01-01T00:00:00Z plus the first 58 bits of its blob hash as nanoseconds: a time from
// 2001 to early 2010, the same for the same contents in every checkout and, but for odds of 2^-58 per changed file,
// different for different contents. A changed file that kept its size and drew the same time would reuse a stale result;
// RELEASING.md gives the odds. A symbolic link's own time comes from its blob (its target) in the same way, and is set
// without following it: Stat through a link reads its target's time, and Lstat, which a directory listing reads, its
// own. A directory's time is 2000-01-01T00:00:00Z: when a test opens a directory, the cache hashes the name, size,
// mode and time of every entry, so an added, removed or changed entry still changes the key, and a time derived from
// the directory's contents would add only misses for changes deeper down.
//
// It leaves alone every path git does not track (generated and untracked files), a submodule, a tracked file whose
// working copy differs from the index or is missing, and a file in a conflict: their times stay as the checkout or the
// edit left them, so the cache treats them as it did before. It reads the index (git ls-files), never history, so a
// shallow checkout works, and refreshes the index's stat data afterwards so that git sees no change. The top
// directory's .git entry gets the directories' time too: a test that lists the top directory sees it.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The times the command sets, in seconds since the Unix epoch.
const (
	fileEpoch = 978307200 // 2001-01-01T00:00:00Z, the earliest time a file or a symbolic link gets
	dirTime   = 946684800 // 2000-01-01T00:00:00Z, every directory's time
	hashBits  = 58        // the bits of the blob hash in a file's time: 2^58 ns is about 9.1 years
)

// Git's file modes, as git ls-files -s prints them.
const (
	modeFile       = "100644"
	modeExecutable = "100755"
	modeSymlink    = "120000"
)

// entry is a path of the index, as git ls-files -s lists it.
type entry struct {
	mode  string
	hash  string
	stage string
	path  string // slash-separated, relative to the top of the working tree
}

// summary counts what a run did.
type summary struct {
	files, symlinks, dirs int
	skipped               int // changed from the index, missing, in a conflict, or a submodule
	inexact               int // files whose time the file system did not keep to the nanosecond
}

func main() {
	dir := "."
	switch len(os.Args) {
	case 1:
	case 2:
		dir = os.Args[1]
	default:
		fmt.Fprintln(os.Stderr, "usage: testmtimes [DIR]")
		os.Exit(2)
	}
	s, err := run(context.Background(), dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testmtimes: %v\n", err)
		os.Exit(1)
	}
	report(s)
}

func report(s summary) {
	fmt.Printf("testmtimes: set %d files, %d symbolic links and %d directories from the index; left %d paths alone\n",
		s.files, s.symlinks, s.dirs, s.skipped)
	if s.inexact > 0 {
		fmt.Printf("testmtimes: the file system kept %d of the times to a coarser step than the nanosecond: those files' "+
			"times carry fewer than %d bits of their hash\n", s.inexact, hashBits)
	}
}

// run sets the times in the working tree that holds dir.
func run(ctx context.Context, dir string) (summary, error) {
	top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return summary{}, err
	}
	root := strings.TrimSuffix(string(top), "\n")
	// Refresh first, so that diff-files lists the files whose contents differ from the index and not those whose stat
	// data alone is stale. -q: a file that needs updating is not an error.
	if _, err := git(ctx, root, "update-index", "-q", "--refresh"); err != nil {
		return summary{}, err
	}
	listing, err := git(ctx, root, "ls-files", "-s", "-z")
	if err != nil {
		return summary{}, err
	}
	entries, err := parseEntries(listing)
	if err != nil {
		return summary{}, err
	}
	changed, err := git(ctx, root, "diff-files", "--name-only", "-z")
	if err != nil {
		return summary{}, err
	}
	dirty := map[string]bool{}
	for p := range bytes.SplitSeq(changed, []byte{0}) {
		if len(p) > 0 {
			dirty[string(p)] = true
		}
	}
	s, err := apply(root, entries, dirty)
	if err != nil {
		return s, err
	}
	// The times changed, so the index's stat data is stale for every file set: refresh it, or a plumbing command such
	// as diff-files would report each as modified.
	if _, err := git(ctx, root, "update-index", "-q", "--refresh"); err != nil {
		return s, err
	}
	// A test that lists the top directory (internal/limitsdoc walks the tree) hashes its .git entry too, whose time the
	// checkout set and every write of the index moves: give it the directories' time, after the refresh wrote the index.
	dotGit := filepath.Join(root, ".git")
	if info, err := os.Lstat(dotGit); err == nil && info.Mode()&fs.ModeSymlink == 0 {
		d := time.Unix(dirTime, 0)
		if err := os.Chtimes(dotGit, d, d); err != nil {
			return s, fmt.Errorf("set the time of .git: %w", err)
		}
	}
	return s, nil
}

// parseEntries reads git ls-files -s -z: "<mode> <hash> <stage>\t<path>", NUL-terminated.
func parseEntries(listing []byte) ([]entry, error) {
	var entries []entry
	for rec := range bytes.SplitSeq(listing, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, p, ok := strings.Cut(string(rec), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || p == "" {
			return nil, fmt.Errorf("git ls-files -s: malformed entry %q", rec)
		}
		entries = append(entries, entry{mode: fields[0], hash: fields[1], stage: fields[2], path: p})
	}
	return entries, nil
}

// apply sets the time of every entry not in dirty, and of every directory that holds an entry, under root.
func apply(root string, entries []entry, dirty map[string]bool) (summary, error) {
	var s summary
	dirs := map[string]bool{".": true}
	for _, e := range entries {
		for d := path.Dir(e.path); d != "."; d = path.Dir(d) {
			if dirs[d] {
				break
			}
			dirs[d] = true
		}
		if e.stage != "0" || dirty[e.path] {
			s.skipped++
			continue
		}
		name := filepath.Join(root, filepath.FromSlash(e.path))
		info, err := os.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			s.skipped++ // not checked out, as with a sparse checkout
			continue
		}
		if err != nil {
			return s, fmt.Errorf("stat %s: %w", e.path, err)
		}
		t, err := fileTime(e.hash)
		if err != nil {
			return s, fmt.Errorf("%s: %w", e.path, err)
		}
		switch {
		case (e.mode == modeFile || e.mode == modeExecutable) && info.Mode().IsRegular():
			if err := os.Chtimes(name, t, t); err != nil {
				return s, fmt.Errorf("set the time of %s: %w", e.path, err)
			}
			s.files++
		case e.mode == modeSymlink && info.Mode()&fs.ModeSymlink != 0:
			ts := unix.NsecToTimespec(t.UnixNano())
			if err := unix.UtimesNanoAt(unix.AT_FDCWD, name, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return s, fmt.Errorf("set the time of the symbolic link %s: %w", e.path, err)
			}
			s.symlinks++
		default:
			s.skipped++ // a submodule, or a path whose type differs from the index's
			continue
		}
		after, err := os.Lstat(name)
		if err != nil {
			return s, fmt.Errorf("stat %s: %w", e.path, err)
		}
		if !after.ModTime().Equal(t) {
			s.inexact++
		}
	}
	d := time.Unix(dirTime, 0)
	for p := range dirs {
		name := filepath.Join(root, filepath.FromSlash(p))
		if info, err := os.Lstat(name); err != nil || !info.IsDir() {
			continue // removed, or replaced by a file, in the working tree: not the index's directory
		}
		if err := os.Chtimes(name, d, d); err != nil {
			return s, fmt.Errorf("set the time of the directory %s: %w", p, err)
		}
		s.dirs++
	}
	return s, nil
}

// fileTime is the time for a blob: fileEpoch plus the first hashBits bits of its hash, as nanoseconds.
func fileTime(hash string) (time.Time, error) {
	if len(hash) < 16 {
		return time.Time{}, fmt.Errorf("blob hash %q is shorter than 64 bits", hash)
	}
	v, err := strconv.ParseUint(hash[:16], 16, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("blob hash %q: %w", hash, err)
	}
	return time.Unix(fileEpoch, int64(v>>(64-hashBits))).UTC(), nil
}

// git runs git in dir and returns its standard output.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // git with this command's own arguments
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}
