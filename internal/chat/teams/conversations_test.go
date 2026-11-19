package teams

import (
	"context"
	"testing"
)

// TestMemoryConversationsMerge: a record fills in what it says and leaves what it does not say as it was, so that an invoke, which has
// no aadGroupId, does not erase the one an install event or a message gave; records are kept per connection and channel.
func TestMemoryConversationsMerge(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryConversations()
	if _, ok, err := s.Lookup(ctx, testConn, testChannel); err != nil || ok {
		t.Fatalf("an empty store: ok %v, err %v", ok, err)
	}
	full := Conversation{Channel: testChannel, ServiceURL: testService, TeamID: testChannel, AADGroupID: testGroup, TenantID: testTenant}
	steps := []struct {
		name string
		rec  Conversation
		want Conversation
	}{
		{"the install event", full, full},
		{"an invoke with no group", Conversation{Channel: testChannel, ServiceURL: testService, TeamID: testChannel, TenantID: testTenant}, full},
		{"a new service URL", Conversation{Channel: testChannel, ServiceURL: "https://smba.trafficmanager.net/emea/"},
			Conversation{Channel: testChannel, ServiceURL: "https://smba.trafficmanager.net/emea/", TeamID: testChannel, AADGroupID: testGroup, TenantID: testTenant}},
	}
	for _, st := range steps {
		if err := s.Remember(ctx, testConn, st.rec); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		got, ok, err := s.Lookup(ctx, testConn, testChannel)
		if err != nil || !ok || got != st.want {
			t.Errorf("%s: got %+v, %v, %v; want %+v", st.name, got, ok, err, st.want)
		}
	}
	if _, ok, _ := s.Lookup(ctx, "ns/other", testChannel); ok {
		t.Error("another connection sees the record")
	}
	if err := s.Remember(ctx, testConn, Conversation{}); err == nil {
		t.Error("a record with no channel is kept")
	}
}
