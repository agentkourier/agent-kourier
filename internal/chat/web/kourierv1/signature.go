package kourierv1

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// signaturePrefix starts the value of HeaderSignature: the version of the scheme.
const signaturePrefix = "v1="

// The reasons a signature is refused. None quotes the signature, the timestamp or the body.
var (
	errUnsigned  = errors.New("kourier-v1: the request is not signed")
	errTimestamp = errors.New("kourier-v1: the timestamp is not Unix seconds")
	errSkew      = errors.New("kourier-v1: the timestamp is too far from now")
	errSignature = errors.New("kourier-v1: the signature does not match")
)

// Sign returns the two header values that sign body at the time at under key: the timestamp, as Unix seconds, and the signature, "v1="
// and the hex HMAC-SHA256 of "v1:", the timestamp, ":" and body. Agent Kourier signs its replies with it; a sender signs its messages the
// same way.
func Sign(key []byte, at time.Time, body []byte) (timestamp, signature string) {
	timestamp = strconv.FormatInt(at.Unix(), 10)
	return timestamp, signaturePrefix + hex.EncodeToString(mac(key, timestamp, body))
}

// VerifySignature says whether signature is the signature of body at timestamp under key, and timestamp is within maxSkew of now, either
// way. The signatures are compared in constant time.
func VerifySignature(key []byte, now time.Time, timestamp, signature string, body []byte) error {
	if timestamp == "" || signature == "" {
		return errUnsigned
	}
	secs, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errTimestamp
	}
	// Compared in seconds, so that no timestamp, however far off, overflows a time.Duration.
	if d := now.Unix() - secs; d > int64(maxSkew/time.Second) || d < -int64(maxSkew/time.Second) {
		return errSkew
	}
	hexSig, ok := strings.CutPrefix(signature, signaturePrefix)
	if !ok {
		return errSignature
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil || !hmac.Equal(got, mac(key, timestamp, body)) {
		return errSignature
	}
	return nil
}

// mac is the HMAC-SHA256 under key of "v1:", timestamp, ":" and body.
func mac(key []byte, timestamp string, body []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte("v1:"))
	h.Write([]byte(timestamp))
	h.Write([]byte(":"))
	h.Write(body)
	return h.Sum(nil)
}
