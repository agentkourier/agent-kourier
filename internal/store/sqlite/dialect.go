package sqlite

import (
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// Dialect is how SQLite writes the policy predicates of package store: positional placeholders, a time as the
// nanoseconds of its Unix time and a time never set as 0, and text in the binary order that SQLite compares it in.
type Dialect struct{}

var _ store.Dialect = Dialect{}

// Placeholder implements store.Dialect. Placeholders are positional, so each place in a statement takes an argument.
func (Dialect) Placeholder(int, store.ArgKind) string { return "?" }

// TimeArg implements store.Dialect.
func (Dialect) TimeArg(t time.Time) any { return ts(t) }

// NoCutoff implements store.Dialect: no stored time is before 0.
func (Dialect) NoCutoff() any { return int64(0) }

// IsSet implements store.Dialect.
func (Dialect) IsSet(col string) string { return col + " <> 0" }

// IsUnset implements store.Dialect.
func (Dialect) IsUnset(col string) string { return col + " = 0" }

// Bytewise implements store.Dialect: SQLite's default order of text is binary.
func (Dialect) Bytewise(col string) string { return col }

// NonEmpty implements store.Dialect.
func (Dialect) NonEmpty(col string) string { return "length(" + col + ") > 0" }
