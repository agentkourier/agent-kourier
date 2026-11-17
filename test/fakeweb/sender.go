package fakeweb

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
)

// ProfileKourierV1 is the profile the sender speaks (ChatConnection spec.web.profile).
const ProfileKourierV1 = "kourier-v1"

// The ways the sender signs a request (Send.Signing).
const (
	SignValid = ""     // with the connection's secret
	SignNone  = "none" // not at all: no signature header
	SignBad   = "bad"  // with a secret that is not the connection's
)

const (
	// maxAnswerRead is the most of the chat listener's answer the sender reads, so that the connection can be used again.
	maxAnswerRead = 4 << 10
	// wrongSecretSuffix makes the secret of SignBad: the connection's, with this after it.
	wrongSecretSuffix = "-not-the-connections"
	contentTypeJSON   = "application/json"
)

// chatWebPath is what Send.Path may be: the chat listener's route of a web connection, /chat/web/<namespace>/<name>.
var chatWebPath = regexp.MustCompile(`^/chat/web/[a-z0-9]([a-z0-9-]*[a-z0-9])?/[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// Send is a test's word to the sender: one request to the broker's chat listener.
type Send struct {
	// Profile is the profile to send as: ProfileKourierV1.
	Profile string `json:"profile"`
	// Path is the route of the connection on the chat listener, /chat/web/<namespace>/<name>.
	Path string `json:"path"`
	// Body is the request's body, sent byte for byte.
	Body string `json:"body"`
	// Signing is how it is signed: SignValid (the default), SignNone or SignBad.
	Signing string `json:"signing,omitempty"`
	// Timestamp is the Unix seconds it is signed at; 0 is now. A Send with the Timestamp of an earlier Sent and the same body is that
	// request again, byte for byte.
	Timestamp int64 `json:"timestamp,omitempty"`
}

// Sent is how one request was answered.
type Sent struct {
	// Status is the answer's status, or 0 when no answer came: Error then says why (a refused connection, a timeout).
	Status int    `json:"status"`
	Error  string `json:"error,omitempty"`
	// RetryAfter is the answer's Retry-After.
	RetryAfter string `json:"retryAfter,omitempty"`
	// Timestamp is the timestamp it was signed at.
	Timestamp int64 `json:"timestamp,omitempty"`
}

// serveSend sends one request as the test says, and answers how it was answered. A Send the sender cannot make is answered 400.
func (s *Server) serveSend(w http.ResponseWriter, r *http.Request) {
	var in Send
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		http.Error(w, "the body is not a Send", http.StatusBadRequest)
		return
	}
	req, sent, err := s.request(in)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	resp, err := s.client.Do(req.WithContext(r.Context()))
	if err != nil {
		sent.Error = err.Error()
		writeJSON(w, sent)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswerRead))
	_ = resp.Body.Close()
	sent.Status, sent.RetryAfter = resp.StatusCode, resp.Header.Get("Retry-After")
	writeJSON(w, sent)
}

// request builds the signed request of in, and what of it the Sent reports.
func (s *Server) request(in Send) (*http.Request, Sent, error) {
	if !chatWebPath.MatchString(in.Path) {
		return nil, Sent{}, errors.New("fakeweb: the path is not /chat/web/<namespace>/<name>")
	}
	switch in.Signing {
	case SignValid, SignNone, SignBad:
	default:
		return nil, Sent{}, fmt.Errorf("fakeweb: the signing %q is not valid, none or bad", in.Signing)
	}
	body := []byte(in.Body)
	u := *s.chat
	u.Path = in.Path
	req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader(body)) //nolint:noctx // the caller sets the context
	if err != nil {
		return nil, Sent{}, fmt.Errorf("fakeweb: %w", err)
	}
	req.Header.Set("Content-Type", contentTypeJSON)
	var sent Sent
	switch in.Profile {
	case ProfileKourierV1:
		key := s.key(s.kourier, in.Signing)
		sent.Timestamp = in.Timestamp
		if sent.Timestamp == 0 {
			sent.Timestamp = s.now().Unix()
		}
		if in.Signing != SignNone {
			ts := strconv.FormatInt(sent.Timestamp, 10)
			req.Header.Set(HeaderTimestamp, ts)
			req.Header.Set(HeaderSignature, "v1="+hex.EncodeToString(kourierMAC(key, ts, body)))
		}
	default:
		return nil, Sent{}, fmt.Errorf("fakeweb: the profile %q is not %s", in.Profile, ProfileKourierV1)
	}
	return req, sent, nil
}

// key is the secret to sign with: the connection's, or for SignBad one that is not.
func (s *Server) key(secret []byte, signing string) []byte {
	if signing == SignBad {
		return append(bytes.Clone(secret), wrongSecretSuffix...)
	}
	return secret
}
