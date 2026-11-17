package fakeweb

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // GitHub's legacy X-Hub-Signature is SHA-1; the fake sends it as GitHub does, and nothing verifies with it
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"regexp"
	"strconv"
)

// The profiles the sender speaks (ChatConnection spec.web.profile).
const (
	ProfileKourierV1 = "kourier-v1"
	ProfileGitHub    = "github"
)

// The ways the sender signs a request (Send.Signing).
const (
	SignValid = ""     // with the connection's secret
	SignNone  = "none" // not at all: no signature header
	SignBad   = "bad"  // with a secret that is not the connection's
)

// The headers of a GitHub delivery, as GitHub's webhook documentation gives them (read 2026-11-16, not checked against a live
// delivery): the event, a GUID for the delivery, the HMAC-SHA256 of the body (and the legacy HMAC-SHA1), and the hook's own.
const (
	HeaderGitHubEvent      = "X-GitHub-Event"
	HeaderGitHubDelivery   = "X-GitHub-Delivery"
	HeaderGitHubSignature  = "X-Hub-Signature-256"
	HeaderGitHubSignature1 = "X-Hub-Signature"
	HeaderGitHubHookID     = "X-GitHub-Hook-ID"
	HeaderGitHubTargetType = "X-GitHub-Hook-Installation-Target-Type"
	HeaderGitHubTargetID   = "X-GitHub-Hook-Installation-Target-ID"
)

// What the fake's GitHub hook says of itself: made up, of the shapes GitHub's documentation shows.
const (
	gitHubUserAgent  = "GitHub-Hookshot/fakeweb"
	gitHubHookID     = "424242"
	gitHubTargetType = "repository"
	gitHubTargetID   = "4242"
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
	// Profile is the profile to send as: ProfileKourierV1 or ProfileGitHub.
	Profile string `json:"profile"`
	// Path is the route of the connection on the chat listener, /chat/web/<namespace>/<name>.
	Path string `json:"path"`
	// Body is the request's body, sent byte for byte.
	Body string `json:"body"`
	// Signing is how it is signed: SignValid (the default), SignNone or SignBad.
	Signing string `json:"signing,omitempty"`
	// Timestamp is, for kourier-v1, the Unix seconds it is signed at; 0 is now. A Send with the Timestamp of an earlier Sent and the same
	// body is that request again, byte for byte.
	Timestamp int64 `json:"timestamp,omitempty"`
	// Event is, for github, the X-GitHub-Event; Delivery is its X-GitHub-Delivery, a new GUID if empty.
	Event    string `json:"event,omitempty"`
	Delivery string `json:"delivery,omitempty"`
}

// Sent is how one request was answered.
type Sent struct {
	// Status is the answer's status, or 0 when no answer came: Error then says why (a refused connection, a timeout).
	Status int    `json:"status"`
	Error  string `json:"error,omitempty"`
	// RetryAfter is the answer's Retry-After.
	RetryAfter string `json:"retryAfter,omitempty"`
	// Timestamp is, for kourier-v1, the timestamp it was signed at, and Delivery, for github, the X-GitHub-Delivery it carried.
	Timestamp int64  `json:"timestamp,omitempty"`
	Delivery  string `json:"delivery,omitempty"`
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
	case ProfileGitHub:
		if in.Event == "" {
			return nil, Sent{}, errors.New("fakeweb: a github send names its event")
		}
		sent.Delivery = in.Delivery
		if sent.Delivery == "" {
			sent.Delivery = newGUID()
		}
		req.Header.Set("User-Agent", gitHubUserAgent)
		req.Header.Set(HeaderGitHubEvent, in.Event)
		req.Header.Set(HeaderGitHubDelivery, sent.Delivery)
		req.Header.Set(HeaderGitHubHookID, gitHubHookID)
		req.Header.Set(HeaderGitHubTargetType, gitHubTargetType)
		req.Header.Set(HeaderGitHubTargetID, gitHubTargetID)
		if in.Signing != SignNone {
			key := s.key(s.github, in.Signing)
			req.Header.Set(HeaderGitHubSignature, "sha256="+hex.EncodeToString(mac(sha256.New, key, body)))
			req.Header.Set(HeaderGitHubSignature1, "sha1="+hex.EncodeToString(mac(sha1.New, key, body)))
		}
	default:
		return nil, Sent{}, fmt.Errorf("fakeweb: the profile %q is not %s or %s", in.Profile, ProfileKourierV1, ProfileGitHub)
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

// mac is the HMAC of body under key with the hash h.
func mac(h func() hash.Hash, key, body []byte) []byte {
	m := hmac.New(h, key)
	m.Write(body)
	return m.Sum(nil)
}

// newGUID is a random GUID, as X-GitHub-Delivery holds one.
func newGUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
