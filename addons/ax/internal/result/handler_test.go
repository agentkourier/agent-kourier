package result

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeReceipts struct {
	calls int
}

func (f *fakeReceipts) Claim(context.Context, Claim) (int, error) {
	f.calls++
	return http.StatusNoContent, nil
}
func (f *fakeReceipts) Report(context.Context, Report, [32]byte) (int, error) {
	f.calls++
	return http.StatusNoContent, nil
}

func TestCallbackWireContract(t *testing.T) {
	valid := Report{Claim: Claim{ResultID: NewID(), TaskUID: "uid", InvocationID: NewID()}, OutputSource: "file"}
	report, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	body := string(report)
	wantToken := sha256.Sum256([]byte("fixture-result-bearer"))
	headers := "Bearer fixture-result-bearer"
	for _, test := range []struct {
		name, path, token, content, body string
		status                           int
	}{
		{"valid result", Path, headers, "application/json", body, http.StatusNoContent},
		{"invalid token before parse", Path, "Bearer wrong", "not json", "", http.StatusUnauthorized},
		{"missing token", Path, "", "application/json", body, http.StatusUnauthorized},
		{"A2A token", Path, "Bearer fixture-a2a-bearer", "application/json", body, http.StatusUnauthorized},
		{"unsupported media", Path, headers, "text/plain", body, http.StatusUnsupportedMediaType},
		{"unknown field", Path, headers, "application/json", strings.Replace(body, `"output"`, `"unknown"`, 1), http.StatusBadRequest},
		{"duplicate field", Path, headers, "application/json", strings.Replace(body, `"exit_code":0`, `"exit_code":0,"exit_code":0`, 1), http.StatusBadRequest},
		{"missing field", Path, headers, "application/json", strings.Replace(body, `,"truncated":false`, "", 1), http.StatusBadRequest},
		{"null field", Path, headers, "application/json", strings.Replace(body, `"exit_code":0`, `"exit_code":null`, 1), http.StatusBadRequest},
		{"invalid exit", Path, headers, "application/json", strings.Replace(body, `"exit_code":0`, `"exit_code":256`, 1), http.StatusBadRequest},
		{"trailing document", Path, headers, "application/json", body + " {}", http.StatusBadRequest},
		{"invalid UTF-8", Path, headers, "application/json", strings.Replace(body, `"output":""`, "\"output\":\"\xff\"", 1), http.StatusBadRequest},
		{"decoded output cap", Path, headers, "application/json", strings.Replace(body, `"output":""`, `"output":"`+strings.Repeat("a", OutputLimit+1)+`"`, 1), http.StatusBadRequest},
		{"raw body cap", Path, headers, "application/json", body + strings.Repeat(" ", BodyLimit), http.StatusRequestEntityTooLarge},
		{"start only accepts its fields", StartPath, headers, "application/json", body, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &fakeReceipts{}
			h := Handler{AcceptToken: func(bearer string) bool { return sha256.Sum256([]byte(bearer)) == wantToken }, Receipts: f}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, test.path, strings.NewReader(test.body))
			if test.token != "" {
				r.Header.Set("Authorization", test.token)
			}
			r.Header.Set("Content-Type", test.content)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != test.status || (f.calls == 1) != (test.status == http.StatusNoContent) {
				t.Fatalf("status %d, receipt calls %d; want status %d", w.Code, f.calls, test.status)
			}
		})
	}
}
