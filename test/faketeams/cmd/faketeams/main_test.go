package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// lineWriter sends each line written to it on a channel, dropping it when the buffer is full, so
// that the server's request logs never block it.
type lineWriter chan string

func (l lineWriter) Write(p []byte) (int, error) {
	select {
	case l <- strings.TrimSpace(string(p)):
	default:
	}
	return len(p), nil
}

// announced reads lines until it has the plain and the TLS address.
func announced(t *testing.T, out lineWriter) (plain, secure string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for plain == "" || secure == "" {
		select {
		case line := <-out:
			if a, ok := strings.CutPrefix(line, "faketeams: TLS listening on "); ok {
				secure = a
			} else if a, ok := strings.CutPrefix(line, "faketeams: listening on "); ok {
				plain = a
			}
		case <-deadline:
			t.Fatal("run did not announce both addresses")
		}
	}
	return plain, secure
}

func get(t *testing.T, client *http.Client, url string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

func TestRunServesTheMicrosoftSideOverTLSAndTheControlSurfacePlain(t *testing.T) {
	dir := t.TempDir()
	if err := cert([]string{"-host", "localhost", "-dir", dir}); err != nil {
		t.Fatal(err)
	}
	crt, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(crt) {
		t.Fatal("cert wrote no PEM certificate")
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := make(lineWriter, 16)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-addr", "127.0.0.1:0", "-tls-addr", "127.0.0.1:0",
			"-tls-cert", filepath.Join(dir, "tls.crt"), "-tls-key", filepath.Join(dir, "tls.key"),
			"-public-url", "https://faketeams.test", "-tenant", "tenant-1", "-team", "19:general@thread.tacv2",
			"-group", "group-1", "-client-secret", "fake-client-secret",
			"-bot", "app-1=http://127.0.0.1:1/api/messages", "-channel", "19:alerts@thread.tacv2",
		}, out)
	}()
	plain, secure := announced(t, out)

	if code, body := get(t, http.DefaultClient, "http://"+plain+"/_control/healthz"); code != http.StatusOK || string(body) != "ok" {
		t.Errorf("plain /_control/healthz = %d %q, want 200 ok", code, body)
	}
	trusting := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	code, body := get(t, trusting, "https://"+strings.Replace(secure, "127.0.0.1", "localhost", 1)+"/openid")
	var meta struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &meta); code != http.StatusOK || err != nil || meta.JWKSURI != "https://faketeams.test/keys" {
		t.Errorf("TLS /openid = %d %s, want 200 with the keys under the public URL", code, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run after cancel = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after its context ended")
	}
}

func TestRunRefusesWhatItCannotServe(t *testing.T) {
	dir := t.TempDir()
	if err := cert([]string{"-host", "localhost", "-dir", dir}); err != nil {
		t.Fatal(err)
	}
	tlsFlags := []string{"-tls-cert", filepath.Join(dir, "tls.crt"), "-tls-key", filepath.Join(dir, "tls.key")}
	ids := []string{"-public-url", "https://faketeams.test", "-tenant", "t", "-team", "19:g@thread.tacv2", "-group", "g", "-client-secret", "s"}
	for name, args := range map[string][]string{
		"an unknown flag":    {"-nope"},
		"no certificate":     ids,
		"a bot with no URL":  append(append(slices.Clone(tlsFlags), ids...), "-bot", "app-1"),
		"no bot":             append(slices.Clone(tlsFlags), ids...),
		"an http public URL": append(slices.Clone(tlsFlags), "-public-url", "http://faketeams.test", "-tenant", "t", "-team", "x", "-group", "g", "-client-secret", "s", "-bot", "a=http://b"),
	} {
		if err := run(t.Context(), args, io.Discard); err == nil {
			t.Errorf("run with %s = nil, want an error", name)
		}
	}
	if err := cert([]string{"-host", "localhost"}); err == nil {
		t.Error("cert with no -dir = nil, want an error")
	}
}
