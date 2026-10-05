package sqlitetest_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
)

// migrationCount is how many migrations the store has, from the files that it embeds.
func migrationCount(t *testing.T) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migration files = %v, %v; want some", files, err)
	}
	return len(files)
}

// recorded is the versions that the database at path says it has migrated to.
func recorded(t *testing.T, path string) []int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(t.Context(), `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestPathIsAMigratedDatabaseThatOpens: what a test gets is a database with every migration applied, which the store
// opens, as it opens one that it made.
func TestPathIsAMigratedDatabaseThatOpens(t *testing.T) {
	path := sqlitetest.Path(t)
	if got := recorded(t, path); len(got) != migrationCount(t) {
		t.Errorf("migrations recorded = %v; want %d of them", got, migrationCount(t))
	}
	st, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open of a database from Path: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		t.Errorf("a write-ahead log stands beside the copy; it was taken from a database that was not closed")
	}
}

// TestEveryPathIsADatabaseOfItsOwn: a write to one is in no other, nor in the ones made after it, which are copies of
// what was made once.
func TestEveryPathIsADatabaseOfItsOwn(t *testing.T) {
	a, b := sqlitetest.Path(t), sqlitetest.Path(t)
	if a == b {
		t.Fatalf("two Paths are both %s", a)
	}
	db, err := sql.Open("sqlite", a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO schema_migrations (version, applied_at) VALUES (99999, 0)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got := recorded(t, a); got[len(got)-1] != 99999 {
		t.Fatalf("the write is not in the database it was made in: %v", got)
	}
	for name, path := range map[string]string{"the other": b, "a later one": sqlitetest.Path(t)} {
		if got := recorded(t, path); len(got) != migrationCount(t) {
			t.Errorf("%s database holds %v; want the migrations alone", name, got)
		}
	}
}

// TestSeedLeavesAFileThatIsThere: a test that reopens a database by its path seeds it each time, and what it wrote stays.
func TestSeedLeavesAFileThatIsThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mine.db")
	sqlitetest.Seed(t, path)
	if got := recorded(t, path); len(got) != migrationCount(t) {
		t.Fatalf("a seeded path holds %v; want every migration", got)
	}
	if err := os.WriteFile(path, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	sqlitetest.Seed(t, path)
	if got, err := os.ReadFile(path); err != nil || string(got) != "not a database" {
		t.Errorf("after a second Seed the file holds %q, %v; want it as it was", got, err)
	}
}
