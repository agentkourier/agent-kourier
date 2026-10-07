package postgres

import (
	"strconv"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// Dialect is how Postgres writes the policy predicates of package store: numbered placeholders, typed where nothing
// else types them, a time as itself and a time never set as NULL, and text in the C collation, the byte order, where a
// range of the outbox's event index is exactly a prefix.
type Dialect struct{}

var _ store.Dialect = Dialect{}

// Placeholder implements store.Dialect.
func (Dialect) Placeholder(n int, k store.ArgKind) string {
	switch k {
	case store.ArgText:
		return "$" + strconv.Itoa(n) + "::text"
	case store.ArgTime:
		return "$" + strconv.Itoa(n) + "::timestamptz"
	}
	return "$" + strconv.Itoa(n)
}

// TimeArg implements store.Dialect.
func (Dialect) TimeArg(t time.Time) any { return t }

// NoCutoff implements store.Dialect: NULL is before nothing.
func (Dialect) NoCutoff() any { return nil }

// IsSet implements store.Dialect.
func (Dialect) IsSet(col string) string { return col + " IS NOT NULL" }

// IsUnset implements store.Dialect.
func (Dialect) IsUnset(col string) string { return col + " IS NULL" }

// Bytewise implements store.Dialect.
func (Dialect) Bytewise(col string) string { return col + ` COLLATE "C"` }

// NonEmpty implements store.Dialect.
func (Dialect) NonEmpty(col string) string { return "octet_length(" + col + ") > 0" }
