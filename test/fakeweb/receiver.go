package fakeweb

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The kourier-v1 wire, as the profile's package comment gives it: both ways, a request carries HeaderTimestamp, its Unix seconds, and
// HeaderSignature, "v1=" and the hex HMAC-SHA256 under the signing secret of "v1:", the timestamp, ":" and the body, which a receiver
// refuses more than MaxSkew from its clock; a reply carries HeaderIdempotencyKey too, the same on every try of a post.
const (
	HeaderTimestamp      = "X-Kourier-Timestamp"
	HeaderSignature      = "X-Kourier-Signature"
	HeaderIdempotencyKey = "Idempotency-Key"
	// MaxSkew is how far from the receiver's clock a signed timestamp may be.
	MaxSkew = 5 * time.Minute
)

// The problems the receiver records for a reply that does not keep the contract. A reply with ProblemUnsigned, ProblemStale or
// ProblemSignature is answered 401; one with another problem 400.
const (
	ProblemUnsigned  = "unsigned"  // no timestamp or no signature
	ProblemStale     = "stale"     // the timestamp is not Unix seconds, or is more than MaxSkew from now
	ProblemSignature = "signature" // the signature is not the body's under the secret
	ProblemNoKey     = "no_key"    // no Idempotency-Key
	ProblemEnvelope  = "envelope"  // the body is not an envelope of version 1 with a kind, a connection, a channel, an ID and a message
)

// Reply is one request the receiver was sent, as it read it. The envelope's fields are read from a reply that does not keep the contract
// too, as far as its body is JSON, so that a test that waits for the replies of a thread sees one that was refused, and why.
type Reply struct {
	At time.Time `json:"at"`
	// Problem is why the reply does not keep the contract, or empty for one that does (the Problem constants).
	Problem string `json:"problem,omitempty"`
	// Status is what the receiver answered: 200 for a reply that keeps the contract, the status a test asked for (Failure) for one it
	// failed on purpose, 401 or 400 for a Problem.
	Status int `json:"status"`
	// Key is its Idempotency-Key. Repeat says a post with that key was recorded before: a receiver that keeps the contract drops it,
	// and this one answers it 200 as it did the first.
	Key    string `json:"key"`
	Repeat bool   `json:"repeat,omitempty"`
	// The envelope: post or update, the connection ("namespace/name"), where the message is, and its ID.
	Kind       string `json:"kind"`
	Connection string `json:"connection"`
	Channel    string `json:"channel"`
	Thread     string `json:"thread,omitempty"`
	ID         string `json:"id"`
	// Fallback is the message's fallback text, and Markdown the markdown of each of its body parts, in order. Message is the message as
	// it was sent.
	Fallback string          `json:"fallback,omitempty"`
	Markdown []string        `json:"markdown,omitempty"`
	Message  json.RawMessage `json:"message,omitempty"`
}

// envelope is a reply's body, as the kourier-v1 contract gives its outbound envelope.
type envelope struct {
	V          int             `json:"v"`
	Kind       string          `json:"kind"`
	Connection string          `json:"connection"`
	Channel    string          `json:"channel"`
	Thread     string          `json:"thread"`
	ID         string          `json:"id"`
	Message    json.RawMessage `json:"message"`
}

// outgoing is what the receiver reads of a reply's message: its version, and a message's fallback and parts.
type outgoing struct {
	V       int `json:"v"`
	Message *struct {
		Fallback string `json:"fallback"`
		Parts    []struct {
			Kind     string `json:"kind"`
			Markdown string `json:"markdown"`
		} `json:"parts"`
	} `json:"message"`
}

// receive records one reply and answers it.
func (s *Server) receive(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "the body is unreadable or too large", http.StatusRequestEntityTooLarge)
		return
	}
	rep := Reply{At: s.now(), Key: r.Header.Get(HeaderIdempotencyKey)}
	envelopeProblem := read(body, &rep) // the envelope is read whatever the signature, for the record; a bad signature is the problem first
	rep.Problem = verify(s.kourier, s.now(), r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderSignature), body)
	if rep.Problem == "" {
		rep.Problem = envelopeProblem
	}
	if rep.Problem == "" && rep.Key == "" {
		rep.Problem = ProblemNoKey
	}
	s.mu.Lock()
	switch {
	case rep.Problem == ProblemUnsigned, rep.Problem == ProblemStale, rep.Problem == ProblemSignature:
		rep.Status = http.StatusUnauthorized
	case rep.Problem != "":
		rep.Status = http.StatusBadRequest
	case s.failures > 0:
		// A reply failed on purpose was not taken: its key is not recorded, so its next try is not a repeat.
		s.failures--
		rep.Status = s.failStatus
	default:
		rep.Status = http.StatusOK
		if rep.Kind == "post" {
			rep.Repeat = s.keys[rep.Key]
			s.keys[rep.Key] = true
		}
	}
	s.replies = append(s.replies, rep)
	s.mu.Unlock()
	switch {
	case rep.Status == http.StatusOK:
		w.WriteHeader(http.StatusOK)
	case rep.Problem == "":
		if rep.Status == http.StatusServiceUnavailable || rep.Status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "1")
		}
		http.Error(w, "failed on purpose", rep.Status)
	case rep.Status == http.StatusUnauthorized:
		http.Error(w, "not signed with the connection's secret", rep.Status)
	default:
		http.Error(w, "not the kourier-v1 reply contract: "+rep.Problem, rep.Status)
	}
}

// read fills rep from a body, and returns ProblemEnvelope for one that is not an envelope. It reads the envelope's fields of a body
// that is not one too, as far as the body is JSON, for the record.
func read(body []byte, rep *Reply) string {
	var env envelope
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		env = envelope{}
		_ = json.Unmarshal(body, &env) // best effort: what routing a body that is not the schema has
		rep.Kind, rep.Connection, rep.Channel, rep.Thread, rep.ID = env.Kind, env.Connection, env.Channel, env.Thread, env.ID
		return ProblemEnvelope
	}
	rep.Kind, rep.Connection, rep.Channel, rep.Thread, rep.ID, rep.Message = env.Kind, env.Connection, env.Channel, env.Thread, env.ID, env.Message
	if env.V != 1 || (env.Kind != "post" && env.Kind != "update") || env.Connection == "" || env.Channel == "" || env.ID == "" || len(env.Message) == 0 {
		return ProblemEnvelope
	}
	var out outgoing
	if json.Unmarshal(env.Message, &out) != nil || out.V != 1 {
		return ProblemEnvelope
	}
	if out.Message != nil {
		rep.Fallback = out.Message.Fallback
		for _, p := range out.Message.Parts {
			if p.Kind == "body" {
				rep.Markdown = append(rep.Markdown, p.Markdown)
			}
		}
	}
	return ""
}

// verify says what is wrong with a kourier-v1 signature of body at now, or "" for none.
func verify(key []byte, now time.Time, timestamp, signature string, body []byte) string {
	if timestamp == "" || signature == "" {
		return ProblemUnsigned
	}
	secs, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || now.Sub(time.Unix(secs, 0)).Abs() > MaxSkew {
		return ProblemStale
	}
	hexSig, ok := strings.CutPrefix(signature, "v1=")
	got, err := hex.DecodeString(hexSig)
	if !ok || err != nil || !hmac.Equal(got, kourierMAC(key, timestamp, body)) {
		return ProblemSignature
	}
	return ""
}

// kourierMAC is the HMAC-SHA256 under key of "v1:", timestamp, ":" and body.
func kourierMAC(key []byte, timestamp string, body []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte("v1:" + timestamp + ":"))
	h.Write(body)
	return h.Sum(nil)
}

// Failure is a test's word that the receiver fail the next N replies that keep the contract with Status, as a receiver that is down or
// overloaded does, so that the test sees the sender try again. A 503 or a 429 carries a Retry-After of 1 second. A later Failure replaces
// what is left of an earlier one.
type Failure struct {
	N      int `json:"n"`
	Status int `json:"status"`
}

// maxFailures bounds Failure.N: a test fails a try or a few, not the sender's whole patience.
const maxFailures = 10

// serveFailNext takes a Failure: 204, or 400 for one with N outside 1 to maxFailures or a Status that is not 4xx or 5xx.
func (s *Server) serveFailNext(w http.ResponseWriter, r *http.Request) {
	var f Failure
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil || f.N < 1 || f.N > maxFailures || f.Status < 400 || f.Status > 599 {
		http.Error(w, "want {\"n\": 1 to 10, \"status\": 4xx or 5xx}", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.failures, s.failStatus = f.N, f.Status
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// serveReplies answers the receiver's record, never null.
func (s *Server) serveReplies(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	out := slices.Clone(s.replies)
	s.mu.Unlock()
	if out == nil {
		out = []Reply{}
	}
	writeJSON(w, out)
}
