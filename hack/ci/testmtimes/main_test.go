package main

import (
	"bytes"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFileTime(t *testing.T) {
	epoch := time.Unix(fileEpoch, 0).UTC()
	for _, tc := range []struct {
		name, hash string
		want       time.Time
	}{
		{"zero is the epoch", strings.Repeat("0", 40), epoch},
		{"the 58th bit is the lowest kept", "0000000000000040" + strings.Repeat("0", 24), epoch.Add(1)},
		{"the six bits after it are dropped", "000000000000003f" + strings.Repeat("f", 24), epoch},
		{"the top bit is the highest", "8000000000000000" + strings.Repeat("0", 24), epoch.Add(1 << 57)},
		{"all ones is the latest", strings.Repeat("f", 64), epoch.Add(1<<58 - 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fileTime(tc.hash)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("fileTime(%s) = %v, want %v", tc.hash, got, tc.want)
			}
		})
	}
	// The whole range is years in the past, so go test's 2 s cutoff for a file modified just before it was read
	// never applies, and after the directories' time, so a file never shares it.
	latest, _ := fileTime(strings.Repeat("f", 40))
	if want := time.Date(2010, 3, 1, 0, 0, 0, 0, time.UTC); !latest.Before(want) {
		t.Errorf("the latest file time %v is not before %v", latest, want)
	}
	if !time.Unix(dirTime, 0).Before(epoch) {
		t.Errorf("the directories' time is not before the files' epoch")
	}
	for _, bad := range []string{"", "0123456789abcde", "0123456789abcdeg0000"} {
		if _, err := fileTime(bad); err == nil {
			t.Errorf("fileTime(%q) took a hash that is short or not hex", bad)
		}
	}
}

func TestParseEntries(t *testing.T) {
	listing := "100644 " + strings.Repeat("a", 40) + " 0\tdir/a file.txt\x00" +
		"120000 " + strings.Repeat("b", 40) + " 0\tlink\x00"
	got, err := parseEntries([]byte(listing))
	if err != nil {
		t.Fatal(err)
	}
	want := []entry{
		{mode: modeFile, hash: strings.Repeat("a", 40), stage: "0", path: "dir/a file.txt"},
		{mode: modeSymlink, hash: strings.Repeat("b", 40), stage: "0", path: "link"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("parseEntries = %+v, want %+v", got, want)
	}
	for _, bad := range []string{"100644 abc\tpath\x00", "100644 abc 0 path\x00", "100644 abc 0\t\x00"} {
		if _, err := parseEntries([]byte(bad)); err == nil {
			t.Errorf("parseEntries(%q) took a malformed entry", bad)
		}
	}
}

// fixtureEnv keeps the fixtures' git from the user's and the system's configuration (signing, hooks) and from
// starting maintenance in the background.
func fixtureEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_COUNT":    "2",
		"GIT_CONFIG_KEY_0":    "maintenance.auto",
		"GIT_CONFIG_VALUE_0":  "false",
		"GIT_CONFIG_KEY_1":    "gc.auto",
		"GIT_CONFIG_VALUE_1":  "0",
		"GIT_AUTHOR_NAME":     "fixture",
		"GIT_AUTHOR_EMAIL":    "fixture@example.invalid",
		"GIT_COMMITTER_NAME":  "fixture",
		"GIT_COMMITTER_EMAIL": "fixture@example.invalid",
	} {
		t.Setenv(k, v)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(t.Context(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func write(t *testing.T, name, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// origin makes a repository with a file, an executable, a nested file, an empty file and a symbolic link, committed.
func origin(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin")
	gitIn(t, filepath.Dir(dir), "init", "-q", dir)
	write(t, filepath.Join(dir, "a.txt"), "alpha\n", 0o644)
	write(t, filepath.Join(dir, "run.sh"), "#!/bin/sh\n", 0o755)
	write(t, filepath.Join(dir, "sub", "deeper", "b.txt"), "bravo\n", 0o644)
	write(t, filepath.Join(dir, "empty"), "", 0o644)
	if err := os.Symlink("a.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "fixture")
	return dir
}

// clone checks origin out fresh at dst, as a CI checkout does: every file and directory gets the time of the clone.
func clone(t *testing.T, from, dst string) {
	t.Helper()
	if err := os.RemoveAll(dst); err != nil {
		t.Fatal(err)
	}
	gitIn(t, filepath.Dir(dst), "clone", "-q", "--no-local", from, dst)
}

// mtimes returns the Lstat time of each path under dir but .git, keyed by its slash-separated relative path.
func mtimes(t *testing.T, dir string) map[string]time.Time {
	t.Helper()
	got := map[string]time.Time{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			return filepath.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		got[filepath.ToSlash(rel)] = info.ModTime()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func mustRun(t *testing.T, dir string) summary {
	t.Helper()
	s, err := run(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func blobTime(t *testing.T, dir, path string) time.Time {
	t.Helper()
	hash := strings.TrimSpace(gitIn(t, dir, "rev-parse", "HEAD:"+path))
	tm, err := fileTime(hash)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestTwoClonesGetTheSameTimes(t *testing.T) {
	fixtureEnv(t)
	from := origin(t)
	base := t.TempDir()
	first, second := filepath.Join(base, "first"), filepath.Join(base, "second")
	clone(t, from, first)
	clone(t, from, second)
	before := mtimes(t, first)

	s := mustRun(t, first)
	mustRun(t, second)
	if want := (summary{files: 4, symlinks: 1, dirs: 3}); s != want {
		t.Errorf("summary = %+v, want %+v", s, want)
	}
	a, b := mtimes(t, first), mtimes(t, second)
	if !maps.EqualFunc(a, b, time.Time.Equal) {
		t.Errorf("the clones' times differ:\n first  %v\n second %v", a, b)
	}
	for p, tm := range a {
		if tm.Equal(before[p]) {
			t.Errorf("%s kept the time of its checkout", p)
		}
	}
	for _, p := range []string{"a.txt", "run.sh", "sub/deeper/b.txt", "empty", "link"} {
		if want := blobTime(t, first, p); !a[p].Equal(want) {
			t.Errorf("%s: time %v, want %v from its blob", p, a[p], want)
		}
	}
	for _, p := range []string{".", "sub", "sub/deeper"} {
		if want := time.Unix(dirTime, 0); !a[p].Equal(want) {
			t.Errorf("directory %s: time %v, want %v", p, a[p], want)
		}
	}
	// Stat follows the link to its target's time; Lstat, above, read the link's own, from its own blob.
	info, err := os.Stat(filepath.Join(first, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(a["a.txt"]) {
		t.Errorf("Stat through the link: %v, want the target's %v", info.ModTime(), a["a.txt"])
	}
	if a["link"].Equal(a["a.txt"]) {
		t.Errorf("the link's own time is its target's: it was set through the link")
	}
	// The index's stat data was refreshed: git sees no change.
	if out := gitIn(t, first, "diff-files", "--name-only"); out != "" {
		t.Errorf("git diff-files after the run lists %q", out)
	}
	// A listing of the top directory holds .git, whose time the clone and the refresh set.
	for _, dir := range []string{first, second} {
		info, err := os.Lstat(filepath.Join(dir, ".git"))
		if err != nil {
			t.Fatal(err)
		}
		if want := time.Unix(dirTime, 0); !info.ModTime().Equal(want) {
			t.Errorf("%s/.git: time %v, want %v", filepath.Base(dir), info.ModTime(), want)
		}
	}
}

func TestAChangedFileGetsAnotherTime(t *testing.T) {
	fixtureEnv(t)
	from := origin(t)
	base := t.TempDir()
	old, changed := filepath.Join(base, "old"), filepath.Join(base, "changed")
	clone(t, from, old)
	mustRun(t, old)
	// The same size, so only the time can tell the cache that a.txt changed.
	write(t, filepath.Join(from, "a.txt"), "alphA\n", 0o644)
	gitIn(t, from, "commit", "-q", "-am", "change a.txt")
	clone(t, from, changed)
	mustRun(t, changed)

	a, b := mtimes(t, old), mtimes(t, changed)
	if a["a.txt"].Equal(b["a.txt"]) {
		t.Errorf("a.txt changed but kept its time %v", a["a.txt"])
	}
	delete(a, "a.txt")
	delete(b, "a.txt")
	if !maps.EqualFunc(a, b, time.Time.Equal) {
		t.Errorf("the unchanged paths' times differ:\n old     %v\n changed %v", a, b)
	}
}

func TestPathsOutsideTheIndexAreLeftAlone(t *testing.T) {
	fixtureEnv(t)
	from := origin(t)
	dir := filepath.Join(t.TempDir(), "clone")
	clone(t, from, dir)
	write(t, filepath.Join(dir, "untracked.txt"), "u\n", 0o644)
	write(t, filepath.Join(dir, "generated", "out.txt"), "g\n", 0o644)
	write(t, filepath.Join(dir, "sub", "new.txt"), "n\n", 0o644)
	write(t, filepath.Join(dir, "sub", "deeper", "b.txt"), "bravo, edited\n", 0o644) // tracked, changed, not committed
	if err := os.Remove(filepath.Join(dir, "empty")); err != nil {                   // tracked, missing
		t.Fatal(err)
	}
	stamp := time.Date(2024, 5, 6, 7, 8, 9, 123456789, time.UTC)
	for _, p := range []string{"untracked.txt", "generated/out.txt", "generated", "sub/new.txt", "sub/deeper/b.txt"} {
		if err := os.Chtimes(filepath.Join(dir, p), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	s := mustRun(t, dir)
	if want := (summary{files: 2, symlinks: 1, dirs: 3, skipped: 2}); s != want {
		t.Errorf("summary = %+v, want %+v", s, want)
	}
	got := mtimes(t, dir)
	for _, p := range []string{"untracked.txt", "generated/out.txt", "generated", "sub/new.txt", "sub/deeper/b.txt"} {
		if !got[p].Equal(stamp) {
			t.Errorf("%s: time %v, want it left at %v", p, got[p], stamp)
		}
	}
	if want := blobTime(t, dir, "a.txt"); !got["a.txt"].Equal(want) {
		t.Errorf("a.txt: time %v, want %v", got["a.txt"], want)
	}
	// A directory that holds a tracked path gets the directories' time even when it also holds untracked files: its
	// entries' names are in the cache key whenever a test lists it.
	if want := time.Unix(dirTime, 0); !got["sub"].Equal(want) {
		t.Errorf("sub: time %v, want %v", got["sub"], want)
	}
	if out := gitIn(t, dir, "diff-files", "--name-only"); out != "empty\nsub/deeper/b.txt\n" {
		t.Errorf("git diff-files after the run lists %q, want only the changed and the missing file", out)
	}
}

func TestRunsFromASubdirectory(t *testing.T) {
	fixtureEnv(t)
	from := origin(t)
	dir := filepath.Join(t.TempDir(), "clone")
	clone(t, from, dir)
	mustRun(t, filepath.Join(dir, "sub", "deeper"))
	if got, want := mtimes(t, dir)["a.txt"], blobTime(t, dir, "a.txt"); !got.Equal(want) {
		t.Errorf("a.txt, outside the directory it ran in: time %v, want %v", got, want)
	}
}

func TestOutsideARepositoryFails(t *testing.T) {
	fixtureEnv(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// git looks no higher than dir, which is not in a repository.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	if _, err := run(t.Context(), dir); err == nil {
		t.Error("ran outside a git working tree")
	}
}

// TestGoTestReusesAResultAcrossCheckouts is the mechanism end to end: a package whose test opens a file of its
// module reports (cached) after a fresh checkout at the same path only when the command ran on both checkouts, and
// runs again when the file changes.
func TestGoTestReusesAResultAcrossCheckouts(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go test")
	}
	fixtureEnv(t)
	from := filepath.Join(t.TempDir(), "origin")
	gitIn(t, filepath.Dir(from), "init", "-q", from)
	write(t, filepath.Join(from, "go.mod"), "module example.invalid/reads\n\ngo 1.22\n", 0o644)
	write(t, filepath.Join(from, "testdata", "input.txt"), "one\n", 0o644)
	write(t, filepath.Join(from, "reads_test.go"), `package reads

import (
	"os"
	"testing"
)

func TestReadsInput(t *testing.T) {
	if _, err := os.ReadFile("testdata/input.txt"); err != nil {
		t.Fatal(err)
	}
}
`, 0o644)
	gitIn(t, from, "add", "-A")
	gitIn(t, from, "commit", "-q", "-m", "fixture")
	checkout := filepath.Join(t.TempDir(), "checkout")

	// goTest checks origin out afresh at the same path, as every CI run does, either runs the command or gives every
	// path the time of a checkout made age ago, and returns what go test printed for the package.
	goTest := func(withCommand bool, age time.Duration) string {
		t.Helper()
		clone(t, from, checkout)
		if withCommand {
			mustRun(t, checkout)
		} else {
			stamp := time.Now().Add(-age)
			if err := filepath.WalkDir(checkout, func(p string, _ fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				return os.Chtimes(p, stamp, stamp)
			}); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.CommandContext(t.Context(), "go", "test", ".")
		cmd.Dir = checkout
		cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("go test: %v\n%s%s", err, stdout.String(), stderr.String())
		}
		return stdout.String()
	}
	cached := func(out string) bool { return strings.Contains(out, "(cached)") }

	// Without the command, each checkout's times are its own, so the result saved after one never matches the next.
	goTest(false, 2*time.Hour)
	if out := goTest(false, time.Hour); cached(out) {
		t.Fatalf("a fresh checkout without the command reused a result, so this test proves nothing: %s", out)
	}
	goTest(true, 0)
	if out := goTest(true, 0); !cached(out) {
		t.Errorf("a fresh checkout with the command did not reuse the result: %s", out)
	}
	write(t, filepath.Join(from, "testdata", "input.txt"), "two\n", 0o644)
	gitIn(t, from, "commit", "-q", "-am", "change the input, not its size")
	if out := goTest(true, 0); cached(out) {
		t.Errorf("a changed input reused the result: %s", out)
	}
}
