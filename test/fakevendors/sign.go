package fakevendors

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// The ways Send signs a delivery (Delivery.Signing).
const (
	SignValid = ""     // with the source's secret
	SignNone  = "none" // not at all: no signature header
	SignBad   = "bad"  // with a secret that is not the source's
)

// wrongSecretSuffix makes the secret of SignBad: the source's, with this after it.
const wrongSecretSuffix = "-not-the-sources"

// signatureOf says where and how a vendor signs a delivery: the header, and the prefix before the hex HMAC-SHA256 of the raw body.
func signatureOf(vendor string) (header, prefix string, ok bool) {
	switch vendor {
	case GitHub:
		return "X-Hub-Signature-256", "sha256=", true
	case Bitbucket, Jira:
		return "X-Hub-Signature", "sha256=", true
	case Linear:
		return "Linear-Signature", "", true
	}
	return "", "", false
}

// Sign returns the header and the value that sign body as vendor does, under key: the prefix, then the hex HMAC-SHA256 of body.
func Sign(vendor string, key, body []byte) (header, value string, err error) {
	h, prefix, ok := signatureOf(vendor)
	if !ok {
		return "", "", fmt.Errorf("fakevendors: no vendor %q", vendor)
	}
	m := hmac.New(sha256.New, key)
	m.Write(body)
	return h, prefix + hex.EncodeToString(m.Sum(nil)), nil
}

// keyFor is the secret to sign with: the source's, or for SignBad one that is not.
func keyFor(secret []byte, signing string) []byte {
	if signing == SignBad {
		return append(bytes.Clone(secret), wrongSecretSuffix...)
	}
	return secret
}

// newGUID is a random GUID, of the shape X-GitHub-Delivery holds.
func newGUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// newID is a random identifier of 16 hex digits.
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return hex.EncodeToString(b)
}
