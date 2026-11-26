package result

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
)

// Receipts records a claim or result and returns its protocol status after commit.
type Receipts interface {
	Claim(context.Context, Claim) (int, error)
	Report(context.Context, Report, [32]byte) (int, error)
}

// Handler authenticates both callback routes before decoding or looking up a turn.
type Handler struct {
	Token    func() [32]byte
	Receipts Receipts
	Observe  func(start bool, status int)
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := r.URL.Path == StartPath
	status := h.receive(w, r, start)
	if h.Observe != nil {
		h.Observe(start, status)
	}
	if status == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", http.MethodPost)
	}
	w.WriteHeader(status)
}

func (h Handler) receive(w http.ResponseWriter, r *http.Request, start bool) int {
	if r.URL.Path != Path && !start {
		return http.StatusNotFound
	}
	if r.Method != http.MethodPost {
		return http.StatusMethodNotAllowed
	}
	header := r.Header.Values("Authorization")
	if len(header) != 1 {
		return http.StatusUnauthorized
	}
	kind, bearer, ok := strings.Cut(header[0], " ")
	if !ok || !strings.EqualFold(kind, "Bearer") || bearer == "" || strings.ContainsAny(bearer, " \t,") {
		return http.StatusUnauthorized
	}
	expected, received := h.Token(), sha256.Sum256([]byte(bearer))
	if subtle.ConstantTimeCompare(expected[:], received[:]) != 1 {
		return http.StatusUnauthorized
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return http.StatusUnsupportedMediaType
	}
	if r.ContentLength > BodyLimit {
		return http.StatusRequestEntityTooLarge
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, BodyLimit))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	if err != nil {
		return http.StatusBadRequest
	}
	report, err := Decode(body, start)
	if err != nil {
		return http.StatusBadRequest
	}
	var status int
	if start {
		status, err = h.Receipts.Claim(r.Context(), report.Claim)
	} else {
		status, err = h.Receipts.Report(r.Context(), report, sha256.Sum256(body))
	}
	if err != nil {
		return http.StatusServiceUnavailable
	}
	return status
}
