// Package sqlitetest gives tests a SQLite database that is already migrated. Opening an empty one runs every migration,
// and under the race detector that is about a second of CPU: a test suite that opens a store per test spent half its time
// there. Here the migrations run once for the process, and each test gets a copy of the result.
package sqlitetest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// template is a migrated, empty database, as the bytes of its file. It is made on first use, in a directory of its own
// that is removed at once, so that nothing outlives the process and no test's temporary directory holds what others read.
// It is a write-once memo in test-support code, not state of the product.
var template = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "sqlitetest-")
	if err != nil {
		return nil, fmt.Errorf("make the directory of the template: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "template.db")
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		return nil, fmt.Errorf("migrate the template: %w", err)
	}
	if err := st.Close(); err != nil { // closing the last connection folds the write-ahead log into the file
		return nil, fmt.Errorf("close the template: %w", err)
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		return nil, errors.New("the template left a write-ahead log: its file is not the whole database")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is the template's, in a directory this function made
	if err != nil {
		return nil, fmt.Errorf("read the template: %w", err)
	}
	return raw, nil
})

// Path returns the path of a migrated, empty database in the test's temporary directory. The test opens it with
// sqlite.Open, which finds nothing to migrate, and the directory is removed when the test ends.
func Path(t testing.TB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "courier.db")
	Seed(t, path)
	return path
}

// Seed writes a migrated, empty database at path, unless a file is there already: a test that opens the same path
// twice (a restart) seeds it each time, and what it wrote stays.
func Seed(t testing.TB, path string) {
	t.Helper()
	raw, err := template()
	if err != nil {
		t.Fatalf("sqlitetest: %v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the path is the test's own, which it names
	if errors.Is(err, os.ErrExist) {
		return
	}
	if err != nil {
		t.Fatalf("sqlitetest: create %s: %v", path, err)
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		t.Fatalf("sqlitetest: write %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("sqlitetest: close %s: %v", path, err)
	}
}
