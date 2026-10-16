package corpus

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const fixture = "testdata/docs"

func loadFixture(t *testing.T) *Index {
	t.Helper()
	ix, err := Load(context.Background(), fixture, Options{})
	if err != nil {
		t.Fatalf("Load(%s): %v", fixture, err)
	}
	return ix
}

// write creates the file at dir/rel with content, and its directories.
func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFixture(t *testing.T) {
	ix := loadFixture(t)
	want := []string{
		"explanation/threads.md",
		"how-to/connect-agent.md",
		"how-to/write-binding.md",
		"index.md",
		"reference/errors.md",
		"reference/limits.md",
	}
	if got := ix.Paths(); !reflect.DeepEqual(got, want) {
		t.Errorf("Paths() = %q, want %q (sorted, Markdown only, no hidden directory)", got, want)
	}
}

func TestLoadRefusals(t *testing.T) {
	t.Run("a directory that does not exist", func(t *testing.T) {
		_, err := Load(context.Background(), filepath.Join(t.TempDir(), "missing"), Options{})
		if err == nil {
			t.Fatal("Load succeeded")
		}
	})
	t.Run("a file instead of a directory", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "a.md", "# A\n")
		if _, err := Load(context.Background(), filepath.Join(dir, "a.md"), Options{}); err == nil {
			t.Fatal("Load succeeded")
		}
	})
	t.Run("no Markdown at all", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "notes.txt", "text")
		_, err := Load(context.Background(), dir, Options{})
		if err == nil || !strings.Contains(err.Error(), "no Markdown") {
			t.Fatalf("Load error = %v, want one saying there is no Markdown", err)
		}
	})
	t.Run("a file over the size limit", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "big/page.md", "# Big\n"+strings.Repeat("x", 100))
		_, err := Load(context.Background(), dir, Options{MaxFileBytes: 50})
		if err == nil || !strings.Contains(err.Error(), "big/page.md") || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("Load error = %v, want one naming big/page.md and the limit", err)
		}
	})
	t.Run("a cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Load(ctx, fixture, Options{}); err == nil {
			t.Fatal("Load succeeded with a cancelled context")
		}
	})
}

// TestLoadRefusesSymlinks pins that no symlink inside the corpus is followed or tolerated, whatever it points at:
// a link out of the corpus, a link to a directory out of it, and a link to a file inside it.
func TestLoadRefusesSymlinks(t *testing.T) {
	outside := t.TempDir()
	write(t, outside, "secret.md", "# Secret\nthe outside file\n")
	tests := []struct {
		name string
		link func(t *testing.T, corpus string) // creates the link at the path in want
		want string
	}{
		{"file link out of the corpus", func(t *testing.T, c string) {
			mustSymlink(t, filepath.Join(outside, "secret.md"), filepath.Join(c, "link.md"))
		}, "link.md"},
		{"directory link out of the corpus", func(t *testing.T, c string) {
			mustSymlink(t, outside, filepath.Join(c, "sub", "dir"))
		}, "sub/dir"},
		{"file link inside the corpus", func(t *testing.T, c string) {
			mustSymlink(t, filepath.Join(c, "real.md"), filepath.Join(c, "alias.md"))
		}, "alias.md"},
		{"dangling link", func(t *testing.T, c string) {
			mustSymlink(t, filepath.Join(c, "nowhere.md"), filepath.Join(c, "dangling.md"))
		}, "dangling.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, "real.md", "# Real\n")
			write(t, dir, "sub/page.md", "# Sub\n")
			tt.link(t, dir)
			_, err := Load(context.Background(), dir, Options{})
			if err == nil {
				t.Fatal("Load succeeded; want the symlink refused")
			}
			msg := filepath.ToSlash(err.Error())
			if !strings.Contains(msg, "symlink") || !strings.Contains(msg, tt.want) {
				t.Errorf("Load error = %q, want one naming the symlink at %q", msg, tt.want)
			}
			if strings.Contains(msg, outside) {
				t.Errorf("Load error %q names where the link points", msg)
			}
		})
	}
}

// TestLoadFollowsTheRootLink pins that the directory the operator names may itself be a link (a mount does that):
// only what is inside it is held to the rule.
func TestLoadFollowsTheRootLink(t *testing.T) {
	target := t.TempDir()
	write(t, target, "a.md", "# A\n")
	link := filepath.Join(t.TempDir(), "docs")
	mustSymlink(t, target, link)
	ix, err := Load(context.Background(), link, Options{})
	if err != nil {
		t.Fatalf("Load through a root link: %v", err)
	}
	if got := ix.Paths(); !reflect.DeepEqual(got, []string{"a.md"}) {
		t.Errorf("Paths() = %q", got)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
}

func TestLoadExclude(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "keep.md", "# Keep\n")
	write(t, dir, "brand/logo.md", "# Logo\n")
	write(t, dir, "brand/deep/more.md", "# More\n")
	write(t, dir, "guides/draft.md", "# Draft\n")
	write(t, dir, "guides/final.md", "# Final\n")
	write(t, dir, "brandy.md", "# Not excluded: only a whole path segment matches\n")
	ix, err := Load(context.Background(), dir, Options{Exclude: []string{"brand", "guides/draft.md"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"brandy.md", "guides/final.md", "keep.md"}
	if got := ix.Paths(); !reflect.DeepEqual(got, want) {
		t.Errorf("Paths() = %q, want %q", got, want)
	}
	for _, bad := range []string{"/abs", "../up", "a/../b", "", "a//b"} {
		if _, err := Load(context.Background(), dir, Options{Exclude: []string{bad}}); err == nil {
			t.Errorf("Exclude %q was accepted", bad)
		}
	}
}

func TestSplitList(t *testing.T) {
	if got := SplitList(" a, b ,,c/d ,"); !reflect.DeepEqual(got, []string{"a", "b", "c/d"}) {
		t.Errorf("SplitList = %q", got)
	}
	if got := SplitList(""); got != nil {
		t.Errorf("SplitList(\"\") = %q, want nil", got)
	}
}
