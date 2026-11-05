package crsource_test

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// This file measures what the largest object that Validate accepts holds in an informer cache, which is where the sizes on
// docs/explanation/security.md come from. The test logs the figures, so run it with -v to read them (the page says what it
// measured and how), and asserts only generous bounds, so that it fails when a cap is raised or an object grows, and not for
// the noise of a heap measurement. The figures are of a test process and of the cache's decode alone (an unstructured
// object), not of a pod, and not of everything the broker holds.

const (
	etcdRequestLimit = 1536 * 1024 // etcd's default limit on one request, in bytes (from its documentation; not checked here)
)

// sizeHeapAlloc is the live heap after two collections.
func sizeHeapAlloc() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// sizeHeldMiB decodes body n times as an informer's cache holds an object, and returns the heap that each copy holds, in MiB:
// the growth of the live heap over the n copies, which are kept until it is read.
func sizeHeldMiB(t *testing.T, body []byte, n int) float64 {
	t.Helper()
	before := sizeHeapAlloc()
	keep := make([]*unstructured.Unstructured, n)
	for i := range keep {
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON(body); err != nil {
			t.Fatal(err)
		}
		keep[i] = u
	}
	after := sizeHeapAlloc()
	runtime.KeepAlive(keep)
	return float64(after-before) / float64(n) / (1 << 20)
}

// sizeMeasure marshals obj, logs its size and what a copy holds, and returns both.
func sizeMeasure(t *testing.T, what string, obj any, copies int) (jsonBytes int, heldMiB float64) {
	t.Helper()
	body, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	heldMiB = sizeHeldMiB(t, body, copies)
	t.Logf("%s: %d bytes of JSON (%.1f KiB, %.2f MiB); one copy held by an informer cache: %.3f MiB; %.0f copies hold 205 MiB, the chart's GOMEMLIMIT at 256Mi",
		what, len(body), float64(len(body))/1024, float64(len(body))/(1<<20), heldMiB, 205/heldMiB)
	return len(body), heldMiB
}

func sizeNamespaces(n int) []string {
	ns := make([]string, n)
	for i := range ns {
		ns[i] = strings.Repeat("a", 63-len(fmt.Sprint(i))) + fmt.Sprint(i)
	}
	return ns
}

// largestBackend is the AgentBackend with every cap at its maximum: 256 allowed namespaces of 63 characters, a URL of 2048
// bytes, and 32 headers with a name of 256 bytes and a value of 4096.
func largestBackend() *v1alpha1.AgentBackend {
	b := &v1alpha1.AgentBackend{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentBackend"},
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 63), Namespace: strings.Repeat("a", 63)},
		Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectA2A, URL: "http://" + strings.Repeat("a", 2048-len("http://")),
			AllowedNamespaces: sizeNamespaces(256)},
	}
	for i := range 32 {
		b.Spec.Headers = append(b.Spec.Headers, v1alpha1.Header{Name: fmt.Sprintf("X-%0254d", i), Value: strings.Repeat("v", 4096)})
	}
	return b
}

func largestConnection() *v1alpha1.ChatConnection {
	return &v1alpha1.ChatConnection{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ChatConnection"},
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 63), Namespace: strings.Repeat("a", 63)},
		Spec: v1alpha1.ChatConnectionSpec{Platform: "slack", CredentialsSecretRef: v1alpha1.SecretRef{Name: strings.Repeat("a", 253)},
			AllowedNamespaces: sizeNamespaces(256)},
	}
}

// largestBinding is a Binding with every cap at its maximum, but for the number of triggers, which is n of at most 50: each
// trigger has 6 match and 6 exclude regexes of 512 bytes, 32 extract entries of 512 bytes with names of 63, 32 closeWhen
// values of 256 bytes, and a template of 8192 bytes.
func largestBinding(n int) *v1alpha1.Binding {
	b := &v1alpha1.Binding{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Binding"},
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 63), Namespace: strings.Repeat("a", 63)},
		Spec: v1alpha1.BindingSpec{
			Agent: v1alpha1.AgentRef{BackendRef: v1alpha1.ObjectRef{Name: strings.Repeat("a", 253), Namespace: strings.Repeat("a", 63)},
				Name: strings.Repeat("a", 253), Namespace: strings.Repeat("a", 63)},
			Identity: v1alpha1.Identity{UserID: strings.Repeat("u", 256), TokenSecretRef: &v1alpha1.SecretKeyRef{Name: strings.Repeat("a", 253), Key: strings.Repeat("k", 253)}},
			Chat: v1alpha1.ChatBinding{
				ConnectionRef:  v1alpha1.ObjectRef{Name: strings.Repeat("a", 253), Namespace: strings.Repeat("a", 63)},
				Channel:        "C" + strings.Repeat("0", 31),
				Acknowledgment: &v1alpha1.ChatAcknowledgment{Emoji: strings.Repeat("e", 100)},
			},
			Interactions: v1alpha1.Interactions{ToolApprovals: true},
		},
	}
	for i := range 50 {
		b.Spec.Chat.Mentions = append(b.Spec.Chat.Mentions, v1alpha1.ChatMention{Name: fmt.Sprintf("%s%02d", strings.Repeat("m", 61), i), User: "U" + strings.Repeat("0", 20)})
	}
	for i := range 10 {
		b.Spec.Interactions.ApproverGroups = append(b.Spec.Interactions.ApproverGroups, fmt.Sprintf("S%031d", i))
	}
	b.Spec.Chat.Triggers = []v1alpha1.Trigger{{Type: v1alpha1.TriggerMention}}
	for i := 1; i < n; i++ {
		tr := v1alpha1.Trigger{
			Type:    v1alpha1.TriggerBotMessage,
			From:    &v1alpha1.BotSource{ID: fmt.Sprintf("B%031d", i), Kind: v1alpha1.SenderKindBot},
			Match:   map[v1alpha1.MessageField]string{},
			Exclude: map[v1alpha1.MessageField]string{},
			Extract: map[string]v1alpha1.Extract{},
			Thread: v1alpha1.TriggerThread{By: v1alpha1.ThreadByKey, Cooldown: metav1.Duration{Duration: 2 * time.Hour},
				CloseWhen: map[string]string{}},
			Limits: v1alpha1.TriggerLimits{RateLimit: &v1alpha1.RateLimit{MaxRuns: 5, Per: metav1.Duration{Duration: 10 * time.Minute}, MaxConcurrent: 2},
				MaxRunsPerDay: new(int32(20))},
			PromptTemplate: strings.Repeat("x", 8192),
		}
		for _, f := range []v1alpha1.MessageField{"text", "title", "link", "fallback", "footer", "body"} {
			tr.Match[f] = strings.Repeat("x", 512)
			tr.Exclude[f] = strings.Repeat("x", 512)
		}
		for j := range 32 {
			name := fmt.Sprintf("%s%02d", strings.Repeat("e", 61), j)
			if j == 0 {
				name = "key"
			}
			tr.Extract[name] = v1alpha1.Extract{Field: "text", Regex: "(?P<" + name + ">x)" + strings.Repeat("x", 512-len(name)-len("(?P<>x)"))}
			tr.Thread.CloseWhen[name] = strings.Repeat("v", 256)
		}
		b.Spec.Chat.Triggers = append(b.Spec.Chat.Triggers, tr)
	}
	return b
}

// TestLargestObjectsAreWhatTheSecurityPageSays builds the largest AgentBackend, ChatConnection and Binding that Validate accepts, and a
// Binding of about etcd's request limit, and logs what each holds in an informer cache. It fails if one is refused, or if one
// is larger than the page says by a wide margin.
func TestLargestObjectsAreWhatTheSecurityPageSays(t *testing.T) {
	validate := func(what string, obj interface {
		Default()
		Validate() error
	}) {
		t.Helper()
		obj.Default()
		if err := obj.Validate(); err != nil {
			t.Fatalf("the largest %s is refused: %.400v", what, err)
		}
	}
	backend, connection := largestBackend(), largestConnection()
	validate("AgentBackend", backend)
	validate("ChatConnection", connection)

	bytes, held := sizeMeasure(t, "largest AgentBackend", backend, 200)
	if bytes < 140<<10 || bytes > 200<<10 || held < 0.1 || held > 0.5 {
		t.Errorf("the largest AgentBackend is %d bytes of JSON and holds %.3f MiB; want 140 to 200 KiB and 0.1 to 0.5 MiB", bytes, held)
	}
	bytes, held = sizeMeasure(t, "largest ChatConnection", connection, 200)
	if bytes < 12<<10 || bytes > 32<<10 || held > 0.2 {
		t.Errorf("the largest ChatConnection is %d bytes of JSON and holds %.3f MiB; want 12 to 32 KiB and under 0.2 MiB", bytes, held)
	}

	full := largestBinding(50)
	validate("Binding", full)
	bytes, held = sizeMeasure(t, "largest Binding by the caps (50 triggers)", full, 20)
	if bytes < 2<<20 || bytes > 3<<20 || held < 2 || held > 8 {
		t.Errorf("the largest Binding is %d bytes of JSON and holds %.3f MiB; want 2 to 3 MiB and 2 to 8 MiB", bytes, held)
	}

	// A Binding of about the request limit of etcd, which is what an API server takes: the most triggers that fit.
	n := 2
	for {
		body, err := json.Marshal(largestBinding(n + 1))
		if err != nil {
			t.Fatal(err)
		}
		if len(body) > etcdRequestLimit {
			break
		}
		n++
	}
	limit := largestBinding(n)
	validate("Binding of the request limit", limit)
	bytes, held = sizeMeasure(t, fmt.Sprintf("Binding of about 1.5 MiB (%d triggers)", n), limit, 20)
	if bytes > etcdRequestLimit || bytes < 1<<20 || held < 1 || held > 6 {
		t.Errorf("a Binding of the request limit is %d bytes of JSON and holds %.3f MiB; want 1 to 1.5 MiB and 1 to 6 MiB", bytes, held)
	}
}
