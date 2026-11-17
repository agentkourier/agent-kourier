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

// Reply is one request the receiver was sent, as it read it.
type Reply struct {
	At time.Time `json:"at"`
	// Problem is why the reply does not keep the contract, or empty for one that does (the Problem constants).
	Problem string `json:"problem,omitempty"`
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
	rep.Problem = verify(s.kourier, s.now(), r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderSignature), body)
	if rep.Problem == "" {
		rep.Problem = read(body, &rep)
	}
	if rep.Problem == "" && rep.Key == "" {
		rep.Problem = ProblemNoKey
	}
	s.mu.Lock()
	if rep.Problem == "" && rep.Kind == "post" {
		rep.Repeat = s.keys[rep.Key]
		s.keys[rep.Key] = true
	}
	s.replies = append(s.replies, rep)
	s.mu.Unlock()
	switch rep.Problem {
	case "":
		w.WriteHeader(http.StatusOK)
	case ProblemUnsigned, ProblemStale, ProblemSignature:
		http.Error(w, "not signed with the connection's secret", http.StatusUnauthorized)
	default:
		http.Error(w, "not the kourier-v1 reply contract: "+rep.Problem, http.StatusBadRequest)
	}
}

// read fills rep from a verified body, and returns ProblemEnvelope for one that is not an envelope.
func read(body []byte, rep *Reply) string {
	var env envelope
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if dec.Decode(&env) != nil || env.V != 1 || (env.Kind != "post" && env.Kind != "update") || env.Connection == "" || env.Channel == "" ||
		env.ID == "" || len(env.Message) == 0 {
		return ProblemEnvelope
	}
	var out outgoing
	if json.Unmarshal(env.Message, &out) != nil || out.V != 1 {
		return ProblemEnvelope
	}
	rep.Kind, rep.Connection, rep.Channel, rep.Thread, rep.ID, rep.Message = env.Kind, env.Connection, env.Channel, env.Thread, env.ID, env.Message
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
