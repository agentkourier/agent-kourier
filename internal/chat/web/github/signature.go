package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// signaturePrefix starts the value of HeaderSignature: the algorithm.
const signaturePrefix = "sha256="

// The reasons a signature is refused. None quotes the signature or the body.
var (
	errUnsigned  = errors.New("github: the request is not signed")
	errSignature = errors.New("github: the signature does not match")
)

// Sign returns the value of HeaderSignature for body under key, as GitHub signs a delivery: "sha256=" and the hex HMAC-SHA256 of the
// body. A test's sender signs with it.
func Sign(key, body []byte) string {
	return signaturePrefix + hex.EncodeToString(mac(key, body))
}

// VerifySignature says whether signature, the value of HeaderSignature, is the signature of body under key. The signatures are
// compared in constant time. GitHub's hex is lower case (unverified); upper case is taken too, as hex.
func VerifySignature(key []byte, signature string, body []byte) error {
	if signature == "" {
		return errUnsigned
	}
	hexSig, ok := strings.CutPrefix(signature, signaturePrefix)
	if !ok {
		return errSignature
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil || !hmac.Equal(got, mac(key, body)) {
		return errSignature
	}
	return nil
}

// mac is the HMAC-SHA256 of body under key.
func mac(key, body []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(body)
	return h.Sum(nil)
}
