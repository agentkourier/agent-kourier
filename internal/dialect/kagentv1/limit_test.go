package kagentv1

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// get sends a request through the Dialect's transport to srv and reads the body to its end. It returns how many bytes the
// reader was given and the error of the request or the read.
func get(t *testing.T, srv *httptest.Server, method string, hdr http.Header, creds dialect.Credentials) (int64, error) {
	t.Helper()
	hc := New(0).httpClient(creds, &url.URL{Scheme: "http", Host: srv.Listener.Addr().String()})
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.Copy(io.Discard, resp.Body)
}

// pattern serves total bytes of the repeating unit, in 64 KiB writes, with a Content-Length when declared.
func pattern(contentType string, unit string, total int64, declare bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		if declare {
			w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		}
		chunk := bytes.Repeat([]byte(unit), max(1, (1<<16)/len(unit)))
		for sent := int64(0); sent < total; {
			n := min(int64(len(chunk)), total-sent)
			if _, err := w.Write(chunk[:n]); err != nil {
				return
			}
			sent += n
		}
	}
}

// A response is read up to its bound and not a byte past it, whatever the backend says it is, and one past it is an
// error, not a body cut short: the SDK would parse a truncated card or answer as another one.
func TestResponseBodyIsBoundedByWhatTheRequestWas(t *testing.T) {
	const (
		stream = "text/event-stream"
		accept = "text/event-stream, application/json" // what the SDK asks of a streaming call
	)
	tenant := dialect.Credentials{UserID: "u", Headers: hdr("Accept", stream)} // the AgentBackend's header, on every request
	for _, tc := range []struct {
		name   string
		method string
		accept string
		ctype  string // of the response
		unit   string
		creds  dialect.Credentials
		limit  int64
	}{
		{"the card", http.MethodGet, "", "application/json", " ", dialect.Credentials{UserID: "u"}, dialect.MaxCardBytes},
		{"the card, answered as an event stream", http.MethodGet, accept, stream, "\n\n", dialect.Credentials{UserID: "u"}, dialect.MaxCardBytes},
		{"an answer to a call", http.MethodPost, "application/json", "application/json", " ", dialect.Credentials{UserID: "u"}, dialect.MaxResponseBytes},
		{"an answer to a call, sent as an event stream", http.MethodPost, "application/json", stream, "\n\n", dialect.Credentials{UserID: "u"}, dialect.MaxResponseBytes},
		{"an answer to a call, when the AgentBackend's header asks for a stream", http.MethodPost, "application/json", stream, "\n\n", tenant, dialect.MaxResponseBytes},
		{"a JSON answer to a call that asked for a stream", http.MethodPost, accept, "application/json", " ", dialect.Credentials{UserID: "u"}, dialect.MaxResponseBytes},
	} {
		for _, declare := range []bool{false, true} {
			for _, size := range []int64{tc.limit, tc.limit + 1} {
				t.Run(fmt.Sprintf("%s, %d bytes, length declared %v", tc.name, size, declare), func(t *testing.T) {
					srv := httptest.NewServer(pattern(tc.ctype, tc.unit, size, declare))
					t.Cleanup(srv.Close)
					h := http.Header{}
					if tc.accept != "" {
						h.Set("Accept", tc.accept)
					}
					n, err := get(t, srv, tc.method, h, tc.creds)
					if size <= tc.limit {
						if err != nil || n != size {
							t.Fatalf("read %d bytes, %v; want all %d and no error", n, err, size)
						}
						return
					}
					if !errors.Is(err, dialect.ErrResponseTooLarge) {
						t.Fatalf("err = %v, want dialect.ErrResponseTooLarge", err)
					}
					if n > tc.limit {
						t.Errorf("the reader was given %d bytes, past the bound of %d", n, tc.limit)
					}
				})
			}
		}
	}
}

// A body that says how long it is, past the bound, is refused before a byte of it is read.
func TestDeclaredLengthPastTheBoundFailsBeforeTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(dialect.MaxCardBytes+1))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // the body never comes
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	hc := New(0).httpClient(dialect.Credentials{UserID: "u"}, &url.URL{Scheme: "http", Host: srv.Listener.Addr().String()})
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	resp, err := hc.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, dialect.ErrResponseTooLarge) || ctx.Err() != nil {
		t.Fatalf("err = %v (context: %v), want ErrResponseTooLarge at once", err, ctx.Err())
	}
}

// The bound is on what the SDK would read: a card that is small on the wire and large once decompressed is refused.
func TestCompressedCardIsBoundedWhenDecompressed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		chunk := bytes.Repeat([]byte(" "), 1<<20)
		for range 64 { // 64 MiB of spaces, about 64 KiB on the wire
			_, _ = zw.Write(chunk)
		}
		_ = zw.Close()
	}))
	t.Cleanup(srv.Close)
	n, err := get(t, srv, http.MethodGet, nil, dialect.Credentials{UserID: "u"})
	if !errors.Is(err, dialect.ErrResponseTooLarge) || n > dialect.MaxCardBytes {
		t.Fatalf("read %d bytes, %v; want ErrResponseTooLarge and at most %d bytes", n, err, dialect.MaxCardBytes)
	}
}

// The reproduction of the fault: a card of a gibibyte, which the SDK read into memory whole (2625 MiB of heap, in 1.8 s
// on loopback, before the bound). The test counts what the process allocates, which the reader's buffers keep to a few
// mebibytes now.
func TestHugeCardIsNotReadIntoMemory(t *testing.T) {
	const huge = 1 << 30
	srv := httptest.NewServer(pattern("application/json", " ", huge, false))
	t.Cleanup(srv.Close)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second) // the probe's bound
	defer cancel()
	_, err := New(0).ConnectEndpoint(ctx, srv.URL, dialect.Credentials{UserID: "u"})
	runtime.ReadMemStats(&after)
	if !errors.Is(err, dialect.ErrResponseTooLarge) {
		t.Fatalf("err = %v, want dialect.ErrResponseTooLarge", err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 64<<20 {
		t.Errorf("the fetch allocated %d MiB, want a few: the card is bounded at %d MiB", got>>20, dialect.MaxCardBytes>>20)
	}
}

// eventLines is the lines of an event, each of size bytes with its "\n", up to total bytes, as data lines.
func eventLines(size int, total int64) string {
	line := "data: " + strings.Repeat("x", size-len("data: \n")) + "\n"
	return strings.Repeat(line, int(total)/size)
}

// A stream is bounded by the size of each event and not by its length: the SDK's parser joins the data lines of an
// event, whatever their number, until a blank line, and a2a-go bounds only one line.
func TestStreamEventsAreBoundedOneByOne(t *testing.T) {
	const (
		stream = "text/event-stream"
		accept = "text/event-stream, application/json"
		line   = 1 << 16
		limit  = dialect.MaxEventBytes
	)
	forever := func(unit string) http.HandlerFunc { // an event that never ends: a total far past any bound
		return pattern(stream, unit, 4*limit, false)
	}
	for _, tc := range []struct {
		name    string
		body    http.HandlerFunc
		wantErr bool
		want    int64 // the bytes delivered, when it succeeds
	}{
		{"events of 1 MiB, 3 times the bound in all", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", stream)
			event := eventLines(line, 1<<20) + "\n"
			for range 30 {
				_, _ = io.WriteString(w, event)
			}
		}, false, 30 * (1<<20 + 1)},
		{"events ended by CRLF, 3 times the bound in all", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", stream)
			event := strings.ReplaceAll(eventLines(line, 1<<20), "\n", "\r\n") + "\r\n"
			for range 30 {
				_, _ = io.WriteString(w, event)
			}
		}, false, 30 * (1<<20 + 1<<4 + 2)},
		{"one event of exactly the bound", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", stream)
			_, _ = io.WriteString(w, eventLines(line, limit)+"\n")
		}, false, limit + 1},
		{"one event a byte over the bound", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", stream)
			_, _ = io.WriteString(w, eventLines(line, limit)+"x\n\n")
		}, true, 0},
		{"one event a byte over the bound, that the stream ends in", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", stream)
			_, _ = io.WriteString(w, eventLines(line, limit)+"x")
		}, true, 0},
		{"data lines and no blank line, for ever", forever(strings.TrimSuffix(eventLines(line, line), "\n") + "\n"), true, 0},
		{"data lines between lines of \\r\\r, which the SDK does not see as blank", forever("data: " + strings.Repeat("x", line) + "\n\r\r\n"), true, 0},
		{"data lines between lines of \\r\\r\\r", forever("data: " + strings.Repeat("x", line) + "\n\r\r\r\n"), true, 0},
		{"comment lines and no blank line, for ever", forever(": " + strings.Repeat("c", line) + "\n"), true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.body)
			t.Cleanup(srv.Close)
			n, err := get(t, srv, http.MethodPost, http.Header{"Accept": {accept}}, dialect.Credentials{UserID: "u"})
			if !tc.wantErr {
				if err != nil || n != tc.want {
					t.Fatalf("read %d bytes, %v; want %d and no error", n, err, tc.want)
				}
				return
			}
			if !errors.Is(err, dialect.ErrResponseTooLarge) {
				t.Fatalf("err = %v, want dialect.ErrResponseTooLarge", err)
			}
			if n > limit+limit/4 { // the event, and at most the blank lines of \r\r that were counted with it
				t.Errorf("the reader was given %d bytes, want about the bound of %d", n, limit)
			}
		})
	}
}

// What the SDK makes of an event past the bound is an error with no event: a stream of valid events, and then one that
// is too large for a reader that accumulates data lines, ends with ErrResponseTooLarge and no event of the oversize one.
func TestOversizeEventEndsTheStreamWithAnError(t *testing.T) {
	const working = `data: {"jsonrpc":"2.0","id":"1","result":{"statusUpdate":{"taskId":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_WORKING"}}}}` + "\n\n"
	fd := newFrontDoor(t, true, nil, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, working)
		w.(http.Flusher).Flush()
		// One event that is a valid JSON-RPC response, spread over data lines that the parser joins with "\n".
		_, _ = io.WriteString(w, strings.TrimSuffix(working, "\n\n")+"\n")
		_, _ = io.WriteString(w, eventLines(1<<16, 2*dialect.MaxEventBytes)+"\n")
		_, _ = io.WriteString(w, working)
	})
	client, err := New(0).Connect(t.Context(), fd.backend(), agent, dialect.Credentials{UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	var events int
	var last error
	for ev, err := range client.SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))}) {
		if err != nil {
			last = err
			break
		}
		if _, ok := ev.(*a2a.TaskStatusUpdateEvent); ok {
			events++
		}
	}
	if events != 1 || !errors.Is(last, dialect.ErrResponseTooLarge) {
		t.Errorf("got %d events and error %v, want the first event and then ErrResponseTooLarge", events, last)
	}
}

// A stream that carries far more than any bound in all, in events of a normal size, is read to its end: a long answer
// is not cut.
func TestLongStreamOfSmallEventsIsReadWhole(t *testing.T) {
	const events, size = 1200, 16 << 10 // 19 MiB: more than the bound of an event or of an answer
	chunk := strings.Repeat("a", size)
	fd := newFrontDoor(t, true, nil, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range events {
			_, _ = fmt.Fprintf(w, `data: {"jsonrpc":"2.0","id":"1","result":{"artifactUpdate":{"taskId":"t-1","contextId":"c-1","append":%v,"artifact":{"artifactId":"a-1","parts":[{"text":%q}]}}}}`+"\n\n", i > 0, chunk)
		}
		_, _ = io.WriteString(w, `data: {"jsonrpc":"2.0","id":"1","result":{"statusUpdate":{"taskId":"t-1","contextId":"c-1","status":{"state":"TASK_STATE_COMPLETED"}}}}`+"\n\n")
	})
	client, err := New(0).Connect(t.Context(), fd.backend(), agent, dialect.Credentials{UserID: "u", Token: secret.New(canary)})
	if err != nil {
		t.Fatal(err)
	}
	var got int
	for ev, err := range client.SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))}) {
		if err != nil {
			t.Fatalf("after %d events: %v", got, err)
		}
		if _, ok := ev.(*a2a.TaskArtifactUpdateEvent); ok {
			got++
		}
	}
	if got != events {
		t.Errorf("got %d artifact events, want %d", got, events)
	}
}

// What the bound reports names no host, address, port or header.
func TestTooLargeErrorNamesOnlyTheBound(t *testing.T) {
	srv := httptest.NewServer(pattern("application/json", " ", 2*dialect.MaxCardBytes, false))
	t.Cleanup(srv.Close)
	_, err := New(0).ConnectEndpoint(t.Context(), srv.URL, dialect.Credentials{UserID: "u", Token: secret.New(canary)})
	if err == nil || !strings.Contains(err.Error(), strconv.Itoa(dialect.MaxCardBytes)) {
		t.Fatalf("err = %v, want one that says the bound", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Errorf("the credential reached the error: %v", err)
	}
}

// An A2A 1.0 server of a2a-go, the one the AX agent is built on (examples/ax), streams through the bound: its framing (an
// id line before each event's data line, and the content type with a parameter) is what the bound reads, and an answer
// of well over the bound of an event, in chunks of a normal size, arrives whole.
func TestA2AGoServerStreamsThroughTheBound(t *testing.T) {
	const chunks, size = 24, 128 << 10 // 3 MiB of answer, in artifact chunks
	exec := a2asrv.AgentExecutorFunc(func(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			events := []a2a.Event{a2a.NewSubmittedTask(ec, ec.Message)}
			for range chunks {
				events = append(events, a2a.NewArtifactEvent(ec, a2a.NewTextPart(strings.Repeat("w", size))))
			}
			events = append(events, a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil))
			for _, ev := range events {
				if !yield(ev, nil) {
					return
				}
			}
		}
	})
	fd := newFrontDoor(t, false, nil, a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(exec)).ServeHTTP)
	client, err := New(0).Connect(t.Context(), fd.backend(), agent, dialect.Credentials{UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	var got int
	var final a2a.TaskState
	for ev, err := range client.SendStreamingMessage(t.Context(), &a2a.SendMessageRequest{Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))}) {
		if err != nil {
			t.Fatalf("after %d bytes: %v", got, err)
		}
		switch e := ev.(type) {
		case *a2a.TaskArtifactUpdateEvent:
			got += len(e.Artifact.Parts[0].Text())
		case *a2a.TaskStatusUpdateEvent:
			final = e.Status.State
		}
	}
	if got != chunks*size || final != a2a.TaskStateCompleted {
		t.Errorf("got %d bytes of answer and state %q, want %d and completed", got, final, chunks*size)
	}
}

// The content type of a stream may carry a parameter, which does not make it another type.
func TestEventStreamContentTypeMayCarryAParameter(t *testing.T) {
	const accept = "text/event-stream, application/json"
	srv := httptest.NewServer(pattern("text/event-stream; charset=utf-8", "data: x\n\n", 3*dialect.MaxEventBytes, false))
	t.Cleanup(srv.Close)
	if n, err := get(t, srv, http.MethodPost, http.Header{"Accept": {accept}}, dialect.Credentials{UserID: "u"}); err != nil || n != 3*dialect.MaxEventBytes {
		t.Errorf("read %d bytes, %v; want the whole stream of %d, bounded by event and not in all", n, err, 3*dialect.MaxEventBytes)
	}
}
