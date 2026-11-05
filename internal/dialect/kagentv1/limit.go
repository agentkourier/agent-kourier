package kagentv1

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// The response of a call can be anything the backend sends, and the SDK reads it whole into memory: the card with
// io.ReadAll, an answer with a JSON decoder, an event as the lines up to a blank one. authTransport bounds each read
// here, for every request, since the SDK puts no bound of its own on the card, on an answer, or on the lines of one
// event (a2a-go v2.7.0: a2aclient/agentcard/resolver.go, a2aclient/jsonrpc.go, internal/sse/sse.go).

// bound is how a response's body is bounded: by its total size, or, for a stream, by the size of each event.
type bound struct {
	limit  int64
	stream bool // the limit is of one event, not of the whole body
}

// boundOf says how the response to req is bounded, given what the request asked for before authentication added
// the AgentBackend's headers (asked: the SDK's own Accept header, which a tenant's header cannot set).
//
//   - The card, which is the one GET, is bounded in total whatever it says it is: a card fetch that was handed an
//     event stream must not be read as one.
//   - The answer to a call that asked for a stream and is one (an event-stream content type) is bounded per event.
//     A stream's length is the caller's context's and its text is bounded by what the agent package holds.
//   - Anything else is a single JSON-RPC response, bounded in total, whatever its content type says: a unary call's
//     answer is decoded whole, and a stream call's JSON answer is a single response too.
func boundOf(req *http.Request, asked string, resp *http.Response) bound {
	switch {
	case req.Method == http.MethodGet:
		return bound{limit: dialect.MaxCardBytes}
	case isEventStream(asked, resp.Header.Get("Content-Type")):
		return bound{limit: dialect.MaxEventBytes, stream: true}
	}
	return bound{limit: dialect.MaxResponseBytes}
}

// isEventStream reports whether a call that sent the Accept header asked, and got a response of contentType, asked for a stream
// and got one. The SDK's own check of a streaming answer is on the content type only, which is why the request counts too.
func isEventStream(asked, contentType string) bool {
	got, _, err := mime.ParseMediaType(contentType)
	return err == nil && got == "text/event-stream" && strings.Contains(asked, "text/event-stream")
}

// limited returns resp with its body bounded as b says, or the error of a body that says it is too large before a byte of
// it is read. A body of unknown length is bounded as it is read.
func limited(resp *http.Response, b bound) (*http.Response, error) {
	if !b.stream && resp.ContentLength > b.limit {
		_ = resp.Body.Close()
		return nil, tooLarge(b)
	}
	if b.stream {
		resp.Body = &eventBody{ReadCloser: resp.Body, b: b}
	} else {
		resp.Body = &cappedBody{ReadCloser: resp.Body, b: b, left: b.limit}
	}
	return resp, nil
}

// tooLarge is the error of a body past its bound. It says the bound, and nothing of the response or the backend.
func tooLarge(b bound) error {
	if b.stream {
		return fmt.Errorf("%w: an event of more than %d bytes", dialect.ErrResponseTooLarge, b.limit)
	}
	return fmt.Errorf("%w: more than %d bytes", dialect.ErrResponseTooLarge, b.limit)
}

// cappedBody is a body that fails with dialect.ErrResponseTooLarge once it has more than left bytes to give, and
// returns none of the bytes past that: a body cut short would parse as something else.
type cappedBody struct {
	io.ReadCloser
	b    bound
	left int64
	err  error
}

func (c *cappedBody) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1] // one byte past the bound says there is more, and no more is read
	}
	n, err := c.ReadCloser.Read(p)
	if int64(n) > c.left {
		c.err = tooLarge(c.b)
		return int(c.left), c.err
	}
	c.left -= int64(n)
	return n, err
}

// eventBody is a stream's body that fails with dialect.ErrResponseTooLarge once an event is larger than the bound. It
// finds the end of an event as the SDK's parser does, which splits lines at "\n" and drops one "\r" before it: an event
// ends at a line of nothing, or of one "\r". A line of "\r\r" is not blank to the parser, so it is not here: a
// blank line the parser does not see must not reset the count. It returns none of the bytes past the bound, so
// an event that is too large is never completed.
type eventBody struct {
	io.ReadCloser
	b    bound
	size int64 // the bytes of the event so far
	line int   // the bytes of the line so far, its "\n" not counted
	lead byte  // the first of them
	err  error
}

func (e *eventBody) Read(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.ReadCloser.Read(p)
	for i := 0; i < n; i++ {
		c := p[i]
		if c == '\n' && (e.line == 0 || (e.line == 1 && e.lead == '\r')) {
			e.size, e.line = 0, 0 // a blank line ends the event
			continue
		}
		if e.size++; e.size > e.b.limit {
			e.err = tooLarge(e.b)
			return i, e.err
		}
		switch {
		case c == '\n':
			e.line = 0
		case e.line == 0:
			e.lead, e.line = c, 1
		default:
			e.line++
		}
	}
	return n, err
}
