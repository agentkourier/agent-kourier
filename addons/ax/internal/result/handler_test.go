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
		{"valid result", Path, headers, "application/json", body, 204},
		{"invalid token before parse", Path, "Bearer wrong", "not json", "", 401},
		{"missing token", Path, "", "application/json", body, 401},
		{"A2A token", Path, "Bearer fixture-a2a-bearer", "application/json", body, 401},
		{"unsupported media", Path, headers, "text/plain", body, 415},
		{"unknown field", Path, headers, "application/json", strings.Replace(body, `"output"`, `"unknown"`, 1), 400},
		{"duplicate field", Path, headers, "application/json", strings.Replace(body, `"exit_code":0`, `"exit_code":0,"exit_code":0`, 1), 400},
		{"missing field", Path, headers, "application/json", strings.Replace(body, `,"truncated":false`, "", 1), 400},
		{"null field", Path, headers, "application/json", strings.Replace(body, `"exit_code":0`, `"exit_code":null`, 1), 400},
		{"invalid exit", Path, headers, "application/json", strings.Replace(body, `"exit_code":0`, `"exit_code":256`, 1), 400},
		{"trailing document", Path, headers, "application/json", body + " {}", 400},
		{"invalid UTF-8", Path, headers, "application/json", strings.Replace(body, `"output":""`, "\"output\":\"\xff\"", 1), 400},
		{"decoded output cap", Path, headers, "application/json", strings.Replace(body, `"output":""`, `"output":"`+strings.Repeat("a", OutputLimit+1)+`"`, 1), 400},
		{"raw body cap", Path, headers, "application/json", body + strings.Repeat(" ", BodyLimit), 413},
		{"start only accepts its fields", StartPath, headers, "application/json", body, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &fakeReceipts{}
			h := Handler{Token: func() [32]byte { return wantToken }, Receipts: f}
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, test.path, strings.NewReader(test.body))
			if test.token != "" {
				r.Header.Set("Authorization", test.token)
			}
			r.Header.Set("Content-Type", test.content)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != test.status || (f.calls == 1) != (test.status == 204) {
				t.Fatalf("status %d, receipt calls %d; want status %d", w.Code, f.calls, test.status)
			}
		})
	}
}
