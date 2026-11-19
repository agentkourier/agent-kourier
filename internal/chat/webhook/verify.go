package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// verifier checks a request on its raw bytes, before anything of it is parsed.
type verifier struct {
	scheme Scheme
	header string
	prefix string
	// key is the HMAC secret, and digest the SHA-256 of the token: the token is compared by its digest, so the comparison takes the
	// same time whatever the length of what was sent.
	key    []byte
	digest [sha256.Size]byte
	ts     *timestamp
}

// timestamp is the check of a time in the body (Linear's webhookTimestamp).
type timestamp struct {
	ptr    pointer
	unit   time.Duration
	window time.Duration
}

// verify returns "" for a request that is its sender's, or the reason it is refused: reasonUnsigned for a request without the header,
// reasonBadSignature for one whose header does not hold the connection's prefix and then the HMAC of the body (in hex of either case)
// or the token, and reasonStale for a signed body whose timestamp is missing, not an integer JSON number, or further than the window
// from now, either way, or that cannot be read for it. The timestamp is read only after the signature passes.
func (v *verifier) verify(h http.Header, body []byte, now time.Time) string {
	got := h.Get(v.header)
	if got == "" {
		return reasonUnsigned
	}
	rest, ok := strings.CutPrefix(got, v.prefix)
	if !ok {
		return reasonBadSignature
	}
	switch v.scheme {
	case SchemeHMACSHA256:
		sig, err := hex.DecodeString(rest)
		mac := hmac.New(sha256.New, v.key)
		mac.Write(body)
		if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
			return reasonBadSignature
		}
	case SchemeToken:
		sum := sha256.Sum256([]byte(rest))
		if subtle.ConstantTimeCompare(sum[:], v.digest[:]) != 1 {
			return reasonBadSignature
		}
	default:
		return reasonBadSignature
	}
	if v.ts == nil {
		return ""
	}
	vals, err := extract(body, []pointer{v.ts.ptr})
	if err != nil {
		return reasonStale
	}
	at, ok := vals[v.ts.ptr.raw]
	if !ok || !at.number {
		return reasonStale
	}
	n, err := strconv.ParseInt(at.text, 10, 64)
	if err != nil {
		return reasonStale
	}
	t := time.Unix(n, 0)
	if v.ts.unit == time.Millisecond {
		t = time.UnixMilli(n)
	}
	if d := now.Sub(t); d > v.ts.window || d < -v.ts.window {
		return reasonStale
	}
	return ""
}
