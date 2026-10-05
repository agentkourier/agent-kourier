package storetest

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

func newAudit(subject string, at time.Time) store.AuditEntry {
	return store.AuditEntry{
		At: at, Kind: store.AuditInteractionDecision, User: "U1", Email: "u1@example.com", Identity: "sa/investigator",
		Binding: "ns/b", Subject: subject, Outcome: "approved",
	}
}

func sameAudit(a, b store.AuditEntry) bool {
	return a.Sequence == b.Sequence && a.At.Equal(b.At) && a.Kind == b.Kind && a.User == b.User && a.Email == b.Email &&
		a.Identity == b.Identity && a.Binding == b.Binding && a.Subject == b.Subject && a.Outcome == b.Outcome
}

func auditSubjects(l []store.AuditEntry) []string {
	var out []string
	for _, e := range l {
		out = append(out, e.Subject)
	}
	return out
}

func (su suite) audit(t *testing.T) {
	// Append order is the order of the listing, whatever the entries' times say.
	su.run(t, "entries are listed in the order they were appended, with a sequence the store assigns", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		if l, err := s.ListAudit(ctx, time.Time{}, 0, 10); err != nil || len(l) != 0 {
			t.Fatalf("empty log = %+v, %v; want none", l, err)
		}
		// Appended out of time order: listing follows append order.
		for i, e := range []store.AuditEntry{newAudit("a", t0.Add(2*time.Hour)), newAudit("b", t0), newAudit("c", t0.Add(time.Hour))} {
			e.Sequence = 99 // ignored
			noErr(t, s.AppendAudit(ctx, e), fmt.Sprint("append ", i))
		}
		subjects := func(since time.Time, after int64, limit int) []string {
			t.Helper()
			l, err := s.ListAudit(ctx, since, after, limit)
			noErr(t, err, "ListAudit")
			return auditSubjects(l)
		}
		all, err := s.ListAudit(ctx, time.Time{}, 0, 10)
		first := newAudit("a", t0.Add(2*time.Hour))
		first.Sequence = all[0].Sequence
		if err != nil || len(all) != 3 || !sameAudit(all[0], first) || all[0].Sequence == 99 || all[0].Sequence >= all[1].Sequence || all[1].Sequence >= all[2].Sequence {
			t.Fatalf("list = %+v, %v; want a, b, c with increasing sequences the store chose", all, err)
		}
		for _, c := range []struct {
			name  string
			since time.Time
			after int64
			limit int
			want  []string
		}{
			{"since filters by time", t0.Add(time.Hour), 0, 10, []string{"a", "c"}},
			{"since includes its own instant", t0.Add(2 * time.Hour), 0, 10, []string{"a"}},
			{"since is exclusive of earlier instants", t0.Add(2*time.Hour + time.Microsecond), 0, 10, nil},
			{"page 1", time.Time{}, 0, 2, []string{"a", "b"}},
			{"page 2", time.Time{}, all[1].Sequence, 2, []string{"c"}},
			{"past the end", time.Time{}, all[2].Sequence, 2, nil},
			{"the cursor and since together", t0.Add(time.Hour), all[0].Sequence, 10, []string{"c"}},
			{"a limit of one", time.Time{}, 0, 1, []string{"a"}},
			{"a limit of 1000", time.Time{}, 0, 1000, []string{"a", "b", "c"}},
		} {
			if got := subjects(c.since, c.after, c.limit); !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s = %v; want %v", c.name, got, c.want)
			}
		}
		for _, limit := range []int{0, -1, 1001} {
			_, err := s.ListAudit(ctx, time.Time{}, 0, limit)
			isErr(t, err, store.ErrInvalid, fmt.Sprintf("limit %d", limit))
		}
	})

	su.run(t, "an entry is validated: a kind, a time, a subject and a short outcome; the rest is optional", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		for name, mutate := range map[string]func(*store.AuditEntry){
			"unknown kind":  func(e *store.AuditEntry) { e.Kind = "bogus" },
			"zero time":     func(e *store.AuditEntry) { e.At = time.Time{} },
			"empty subject": func(e *store.AuditEntry) { e.Subject = "" },
			"empty outcome": func(e *store.AuditEntry) { e.Outcome = "" },
			"long outcome":  func(e *store.AuditEntry) { e.Outcome = strings.Repeat("x", store.MaxAuditOutcome+1) },
		} {
			bad := newAudit("s", t0)
			mutate(&bad)
			isErr(t, s.AppendAudit(ctx, bad), store.ErrInvalid, name+": append")
		}
		for _, k := range []store.AuditKind{store.AuditEvent, store.AuditAgentCall, store.AuditInteractionDecision, store.AuditConfigChange} {
			e := newAudit("s", t0)
			e.Kind = k
			noErr(t, s.AppendAudit(ctx, e), "append of kind "+string(k))
		}
		ok := newAudit("s", t0)
		ok.User, ok.Email, ok.Identity, ok.Binding = "", "", "", "" // actor and binding are optional
		ok.Outcome = strings.Repeat("x", store.MaxAuditOutcome)
		noErr(t, s.AppendAudit(ctx, ok), "append at the outcome limit")
		l, err := s.ListAudit(ctx, time.Time{}, 0, 100)
		if err != nil || len(l) != 5 {
			t.Fatalf("log = %d entries, %v; want the four kinds and the entry at the limit, and none of the refused", len(l), err)
		}
	})

	// Implicit in the interface: the store redacts the entry before it checks it, so the limit is on the
	// text that is kept, and a long secret that redaction shortens fits.
	su.run(t, "the outcome limit is measured after redaction", func(t *testing.T, s store.Store) {
		e := newAudit("s", t0)
		e.Outcome = "denied Bearer " + strings.Repeat("x", 2*store.MaxAuditOutcome)
		noErr(t, s.AppendAudit(t.Context(), e), "append of a long secret")
		l, err := s.ListAudit(t.Context(), time.Time{}, 0, 10)
		if err != nil || len(l) != 1 || l[0].Outcome != "denied [redacted]" {
			t.Errorf("log = %+v, %v; want the outcome kept as redacted", l, err)
		}
	})

	su.run(t, "the log is append-only: entries already listed never change", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		for _, subject := range []string{"a", "b"} {
			noErr(t, s.AppendAudit(ctx, newAudit(subject, t0)), "append "+subject)
		}
		before, err := s.ListAudit(ctx, time.Time{}, 0, 10)
		noErr(t, err, "ListAudit")
		for _, subject := range []string{"a", "c"} { // a again: the same subject is a new entry, not a change
			noErr(t, s.AppendAudit(ctx, newAudit(subject, t0.Add(time.Hour))), "append "+subject)
		}
		after, err := s.ListAudit(ctx, time.Time{}, 0, 10)
		noErr(t, err, "ListAudit")
		if len(after) != 4 || !sameAudit(after[0], before[0]) || !sameAudit(after[1], before[1]) {
			t.Fatalf("log = %+v after %+v; want the first two entries unchanged and two more after them", after, before)
		}
		if after[2].Sequence <= before[1].Sequence || after[3].Sequence <= after[2].Sequence {
			t.Errorf("sequences = %d, %d, %d, %d; want them to keep increasing", after[0].Sequence, after[1].Sequence, after[2].Sequence, after[3].Sequence)
		}
	})

	su.run(t, "secrets are redacted before an entry is stored", func(t *testing.T, s store.Store) {
		e := newAudit("call with Bearer FAKE-TOKEN", t0)
		e.Outcome = "denied xoxb-FAKE-TOKEN"
		noErr(t, s.AppendAudit(t.Context(), e), "append")
		l, err := s.ListAudit(t.Context(), time.Time{}, 0, 10)
		if err != nil || len(l) != 1 || l[0].Subject != "call with [redacted]" || l[0].Outcome != "denied [redacted]" || l[0].Email != "u1@example.com" {
			t.Fatalf("stored = %+v, %v; want secrets redacted and the email as given", l, err)
		}
		want, got := e.Redacted(), l[0]
		if got.User != want.User || got.Identity != want.Identity || got.Binding != want.Binding || got.Subject != want.Subject || got.Outcome != want.Outcome {
			t.Errorf("stored = %+v; want what Redacted gives, %+v", got, want)
		}
	})

	su.run(t, "no secret survives in any free-text field", auditCanaries)

	su.run(t, "a reader paging with a sequence never misses an entry appended at the same time", func(t *testing.T, s store.Store) {
		const writers, perWriter, page = 8, 20, 7
		var finished atomic.Bool
		seen := map[string]bool{}
		pagerDone := make(chan struct{})
		go func() {
			defer close(pagerDone)
			var cursor int64
			for {
				done := finished.Load() // read before the query: every append is committed if it is true
				l, err := s.ListAudit(t.Context(), time.Time{}, cursor, page)
				if err != nil {
					t.Errorf("ListAudit: %v", err)
					return
				}
				for _, e := range l {
					if e.Sequence <= cursor { // a store that does not page on would loop for ever
						t.Errorf("ListAudit after %d returned sequence %d", cursor, e.Sequence)
						return
					}
					seen[e.Subject] = true
					cursor = e.Sequence
				}
				if len(l) == 0 {
					if done {
						return
					}
					runtime.Gosched()
				}
			}
		}()
		parallel(writers, func(w int) {
			for i := range perWriter {
				if err := s.AppendAudit(t.Context(), newAudit(fmt.Sprintf("w%d-%02d", w, i), t0)); err != nil {
					t.Errorf("append: %v", err)
				}
			}
		})
		finished.Store(true)
		<-pagerDone
		if want := writers * perWriter; len(seen) != want {
			t.Errorf("the pager saw %d of %d entries: an entry that committed after a later one was skipped", len(seen), want)
		}
	})
}

// auditCanaries appends each canary in every free
// text field and reads the rows back from the store: no secret may survive in any
// column. Email is an identity field by design (spec 8.5) and must survive.
func auditCanaries(t *testing.T, s store.Store) {
	const (
		token  = "CANARYtok3n"
		b64    = "Q0FOQVJZOnNlY3JldDEyMw=="
		jwtSeg = "eyJzdWIiOiJDQU5BUlkifQ"
	)
	type canary struct{ name, in, secret string }
	var cs []canary
	for _, g := range []string{" ", "\t", "\n", "\v", "\u00a0", "\u2003", "\u3000", "\u0085", "\u2028", "\u200b", "\u200c", "\u200d", "\u2060", "\ufeff", "\u00ad", " \u200b\u00a0\t", "\x00", "\x1b", "\u0080", "\u009f", ":", ": ", "%20"} {
		cs = append(cs,
			canary{fmt.Sprintf("Bearer %U", []rune(g)), "denied Bearer" + g + token + " by policy", token},
			canary{fmt.Sprintf("bearer %U", []rune(g)), "bearer" + g + token, token},
			canary{fmt.Sprintf("Basic %U", []rune(g)), "Authorization: Basic" + g + b64, b64})
	}
	cs = append(cs,
		canary{"glued bearer", "bearer" + token, token},
		canary{"glued BEARER", "BEARER" + token, token},
		canary{"format char in scheme", "Bea\u200brer " + token, token},
		canary{"short glued bearer", "BearerTOKEN12", "TOKEN12"},
		canary{"access_token", "cb?access_token=" + token + "&x=1", token},
		canary{"refresh_token", "refresh_token=" + token, token},
		canary{"id_token", "id_token: " + token, token},
		canary{"client_secret", "client_secret=" + token, token},
		canary{"oauth json", `{"token_type":"bearer","access_token":"` + token + `"}`, token},
		canary{"oauth json client secret", `{"client_secret": "` + token + `"}`, token},
		canary{"basic in lower case", "basic " + b64, b64},
		canary{"basic without padding", "Basic " + b64[:len(b64)-2], b64[:len(b64)-2]},
		canary{"jwt", "eyJhbGciOiJIUzI1NiJ9." + jwtSeg + ".c2lnbmF0dXJl", jwtSeg},
		canary{"jwt after bearer", "Bearer eyJhbGciOiJIUzI1NiJ9." + jwtSeg + ".c2lnbmF0dXJl", jwtSeg})
	for _, prefix := range []string{"xoxb", "xoxp", "xoxa", "xoxr", "xoxs", "xapp"} {
		tok := prefix + "-1-" + token
		cs = append(cs,
			canary{"slack " + prefix, "slack " + tok, token},
			canary{"slack " + prefix + " with format char in prefix", "slack " + prefix[:2] + "\u200b" + prefix[2:] + "\u2060-1-" + token, token})
	}
	// OAuth body forms (WP-23d). survivor is text that must outlive the
	// redaction: the neighbouring field, or the word after an empty value.
	survivors := map[string]string{}
	add := func(name, in, secret, survivor string) {
		cs = append(cs, canary{name, in, secret})
		survivors[name] = survivor
	}
	for _, key := range []string{"access_token", "refresh_token", "id_token", "client_secret", "accessToken", "refreshToken", "idToken", "clientSecret", "access-token", "refresh-token", "id-token", "client-secret", "ACCESSTOKEN"} {
		add("json "+key, `{"`+key+`":"`+token+`","user":"U1"}`, token, `"user":"U1"`)
		add("escaped-quote json "+key, `{\"`+key+`\":\"`+token+`\",\"user\":\"U1\"}`, token, `\"user\":\"U1\"`)
		add("double-escaped json "+key, `{\\\"`+key+`\\\":\\\"`+token+`\\\",\\\"user\\\":\\\"U1\\\"}`, token, `\\\"user\\\":\\\"U1\\\"`)
		add("%q of json "+key, fmt.Sprintf("%q", `{"`+key+`":"`+token+`","user":"U1"}`), token, `\"user\":\"U1\"`)
		add("form "+key, key+"="+token+"&user=U1", token, "&user=U1")
	}
	add("compact json bearer", `{"auth":"Bearer `+token+`","user":"U1"}`, token, `"user":"U1"`)
	add("escaped-quote json bearer", `{\"auth\":\"Bearer `+token+`\",\"user\":\"U1\"}`, token, `\"user\":\"U1\"`)
	add("compact json bearer with a colon", `{"auth":"Bearer: `+token+`","user":"U1"}`, token, `"user":"U1"`)
	add("bearer before a semicolon", "Bearer "+token+";user=U1", token, ";user=U1")
	add("compact json xoxb", `{"auth":"xoxb-1-`+token+`","user":"U1"}`, token, `"user":"U1"`)
	add("escaped-quote json xapp", `{\"auth\":\"xapp-1-`+token+`\",\"user\":\"U1\"}`, token, `\"user\":\"U1\"`)
	add("empty value keeps the next word", "client_secret= nextword", "", "nextword")
	add("empty json value keeps the next field", `{"access_token":"","user":"U1"}`, "", `"user":"U1"`)
	// Every canary again as Go prints it with %q and %+q (and %#v of a map),
	// which turns invisible gap characters into literal escapes (\t, \x00,
	// \u00a0, \u200b). The survivor rows are skipped: quoting rewrites their text.
	for _, c := range append([]canary(nil), cs...) {
		if survivors[c.name] != "" {
			continue
		}
		cs = append(cs,
			canary{c.name + " as %q", fmt.Sprintf("%q", c.in), c.secret},
			canary{c.name + " as %+q", fmt.Sprintf("%+q", c.in), c.secret},
			canary{c.name + " as %#v of a map", fmt.Sprintf("%#v", map[string]string{"k": c.in}), c.secret})
	}

	ctx := t.Context()
	var fields []string // free-text fields: everything but the validated Kind and the identity field Email
	typ := reflect.TypeOf(store.AuditEntry{})
	for i := 0; i < typ.NumField(); i++ {
		if f := typ.Field(i); f.Type.Kind() == reflect.String && f.Name != "Kind" && f.Name != "Email" {
			fields = append(fields, f.Name)
		}
	}
	if len(fields) != 5 { // User, Identity, Binding, Subject, Outcome
		t.Fatalf("free-text fields = %v; want User, Identity, Binding, Subject, Outcome", fields)
	}
	for _, c := range cs {
		e := newAudit("s", t0)
		for _, f := range fields {
			reflect.ValueOf(&e).Elem().FieldByName(f).SetString(c.in)
		}
		if err := s.AppendAudit(ctx, e); err != nil {
			t.Fatalf("%s: append: %v", c.name, err)
		}
	}
	rows, err := s.ListAudit(ctx, time.Time{}, 0, 1000)
	if err != nil || len(rows) != len(cs) {
		t.Fatalf("ListAudit = %d rows, %v; want %d", len(rows), err, len(cs))
	}
	for i, row := range rows {
		if row.Email != "u1@example.com" {
			t.Errorf("%s: Email = %q; want it stored as given", cs[i].name, row.Email)
		}
		for _, f := range fields {
			got := reflect.ValueOf(row).FieldByName(f).String()
			if cs[i].secret != "" && strings.Contains(got, cs[i].secret) {
				t.Errorf("%s: %s = %q still holds the secret", cs[i].name, f, got)
			}
			if s := survivors[cs[i].name]; !strings.Contains(got, s) {
				t.Errorf("%s: %s = %q lost %q to over-redaction", cs[i].name, f, got, s)
			}
		}
	}
}
