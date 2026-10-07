package store

import (
	"math"
	"time"
)

// Dialect is what a backend says about the SQL it speaks, for the few things in a policy predicate that differ between
// backends: how an argument is written, what is bound for a time, how a time that was never set is stored and tested,
// and how text is compared byte by byte. A backend gives its own, and every predicate in this package is written
// once and rendered through it, so that the backends cannot drift apart on what a predicate means.
type Dialect interface {
	// Placeholder is the placeholder of the nth argument of a statement, counted from 1. A backend that must type its
	// placeholders (a CASE has no other way to know the type of what it compares) does so by kind.
	Placeholder(n int, k ArgKind) string
	// TimeArg is the value to bind for the time t.
	TimeArg(t time.Time) any
	// NoCutoff is the value to bind for a cutoff that is not in effect (see Binder.Cutoff): the one that no time
	// column is before.
	NoCutoff() any
	// IsSet is the test that the column col, a time that may be unset, holds a time.
	IsSet(col string) string
	// IsUnset is the test that the column col, a time that may be unset, holds none.
	IsUnset(col string) string
	// Bytewise is the column col, to be compared in the byte order of its text: the order in which a range of the
	// outbox's event index is exactly a prefix.
	Bytewise(col string) string
	// NonEmpty is the test that the byte column col holds at least one byte.
	NonEmpty(col string) string
}

// ArgKind says what an argument is, for a Dialect that types its placeholders.
type ArgKind int

const (
	// ArgAny is an argument whose type the statement makes plain, such as the one a column is compared with.
	ArgAny ArgKind = iota
	// ArgText is a text argument that appears where nothing says it is one, as the operand of a CASE.
	ArgText
	// ArgTime is a time argument that appears where nothing says it is one, as the result of a CASE.
	ArgTime
)

// Cond is a condition of a statement, written once. It renders itself through a Binder, which numbers and collects
// the arguments it binds, so a statement is the adapter's text with the conditions rendered into it.
type Cond interface {
	// SQL writes the condition and binds its arguments, in the order they appear in the text it returns.
	SQL(b *Binder) string
}

// Fixed is a Cond that binds no argument, so it is the same text whatever statement it is part of.
type Fixed func(d Dialect) string

// SQL implements Cond.
func (f Fixed) SQL(b *Binder) string { return f(b.d) }

// Text writes f in the dialect d, for a statement that has no Binder of its own.
func (f Fixed) Text(d Dialect) string { return f(d) }

// condFunc is a Cond written as a function.
type condFunc func(b *Binder) string

// SQL implements Cond.
func (f condFunc) SQL(b *Binder) string { return f(b) }

// Ref is how a statement names a table: its alias, or "" for a statement that uses the bare column names.
type Ref string

// col is the column name of the table r.
func (r Ref) col(name string) string {
	if r == "" {
		return name
	}
	return string(r) + "." + name
}

// Binder renders a statement's arguments: it numbers them in the order the statement's text binds them, in the
// placeholders of a Dialect, and keeps their values. A statement's text and its arguments must be written in the same
// order, so an adapter binds its own arguments through Arg, as it writes them, along with the conditions it renders.
type Binder struct {
	d    Dialect
	args []any
}

// NewBinder returns a Binder for a statement in the dialect d.
func NewBinder(d Dialect) *Binder { return &Binder{d: d} }

// Arg binds v and returns its placeholder. Each place in the text needs a call of its own: a placeholder may be written
// twice only in a backend whose placeholders are numbered, and the adapter that does so says so.
func (b *Binder) Arg(v any) string { return b.bind(ArgAny, v) }

// Time binds the time t, as the dialect binds one, and returns its placeholder.
func (b *Binder) Time(t time.Time) string { return b.bind(ArgAny, b.d.TimeArg(t)) }

// Cutoff binds t as a cutoff and returns its placeholder. A cutoff is a time that a column is compared "before": a time
// after the Unix epoch is one in effect, and any other (the zero time most of all) is one that is not, which no column is
// before, so it takes over, or deletes, nothing. The backends tell a time that was never set from one that was in
// different ways (0, NULL), so this is the one place that says which times are not cutoffs. A cutoff later than
// time.Unix(0, math.MaxInt64) is that time, which is after every row either backend can hold: SQLite keeps nanoseconds
// in 64 bits, and a later time would wrap to an earlier one.
func (b *Binder) Cutoff(t time.Time) string {
	if !t.After(time.Unix(0, 0)) {
		return b.bind(ArgAny, b.d.NoCutoff())
	}
	if last := time.Unix(0, math.MaxInt64); t.After(last) {
		t = last
	}
	return b.bind(ArgAny, b.d.TimeArg(t))
}

// Render writes c, binding its arguments, and returns its text.
func (b *Binder) Render(c Cond) string { return c.SQL(b) }

// Args returns the arguments bound so far, in the order of their placeholders.
func (b *Binder) Args() []any { return b.args }

// bind appends v to the arguments and returns the placeholder of its place.
func (b *Binder) bind(k ArgKind, v any) string {
	b.args = append(b.args, v)
	return b.d.Placeholder(len(b.args), k)
}
