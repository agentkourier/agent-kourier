package store_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func TestListMigrations(t *testing.T) {
	ms, err := store.ListMigrations(fstest.MapFS{
		"migrations/0010_ten.sql": {}, "migrations/0002_two.sql": {}, "migrations/0001_one.sql": {},
	})
	if err != nil || len(ms) != 3 || ms[0].Version != 1 || ms[1].Version != 2 || ms[2].Version != 10 {
		t.Fatalf("ListMigrations = %+v, %v; want 1, 2, 10 in numeric order", ms, err)
	}
	if ms[0].Name != "migrations/0001_one.sql" {
		t.Errorf("Name = %q; want the file's path", ms[0].Name)
	}
	// Unpadded, the file names sort 10 before 9: only a numeric sort puts 9 first.
	ms, err = store.ListMigrations(fstest.MapFS{"migrations/10_b.sql": {}, "migrations/9_a.sql": {}})
	if err != nil || len(ms) != 2 || ms[0].Version != 9 || ms[1].Version != 10 {
		t.Errorf("ListMigrations = %+v, %v; want 9 then 10", ms, err)
	}
	if _, err := store.ListMigrations(fstest.MapFS{"migrations/0001_a.sql": {}, "migrations/1_b.sql": {}}); err == nil {
		t.Error("two files with one version were accepted")
	}
	if _, err := store.ListMigrations(fstest.MapFS{"migrations/x_a.sql": {}}); err == nil {
		t.Error("a file with no numeric version was accepted")
	}
}

func TestWarnIfSchemaNewer(t *testing.T) {
	known := []store.Migration{{Version: 1, Name: "migrations/0001_a.sql"}, {Version: 2, Name: "migrations/0002_b.sql"}}
	tests := []struct {
		name   string
		known  []store.Migration
		newest int
		warn   bool
	}{
		{"database is newer", known, 3, true},
		{"database is at the newest known", known, 2, false},
		{"database is behind", known, 1, false},
		{"database is empty", known, 0, false},
		{"binary knows none", nil, 5, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			log := slog.New(slog.NewTextHandler(&out, nil))
			store.WarnIfSchemaNewer(context.Background(), log, tt.known, tt.newest)
			got := out.String()
			if tt.warn != (got != "") {
				t.Fatalf("log = %q; want a warning: %v", got, tt.warn)
			}
			if !tt.warn {
				return
			}
			for _, want := range []string{
				"level=WARN", `msg="the database schema is newer than this build of Agent Kourier"`,
				"newest_known_migration=2", "newest_in_database=3",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("log = %q; want it to contain %q", got, want)
				}
			}
		})
	}
}
