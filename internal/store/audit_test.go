package store

import (
	"reflect"
	"testing"
)

// TestAuditEntryRedactedCoversEveryTextField fails when a string field is added
// to AuditEntry and Redacted forgets it. Kind is a validated enum and Email is
// an identity field by design (spec 8.5); everything else is free text.
func TestAuditEntryRedactedCoversEveryTextField(t *testing.T) {
	const canary = "Bearer\u00a0FAKE-TOKEN"
	var e AuditEntry
	v := reflect.ValueOf(&e).Elem()
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		if f := typ.Field(i); f.Type.Kind() == reflect.String {
			v.Field(i).SetString(canary)
		}
	}
	got := reflect.ValueOf(e.Redacted())
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.String {
			continue
		}
		want := "[redacted]"
		if f.Name == "Kind" || f.Name == "Email" {
			want = canary // left as is
		}
		if s := got.Field(i).String(); s != want {
			t.Errorf("Redacted().%s = %q; want %q", f.Name, s, want)
		}
	}
}
