//go:build testhooks

package testhooks

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakePEM is the PEM of the certificate of srv, a TLS server of httptest.
func fakePEM(srv *httptest.Server) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}

// selfSigned is a certificate of its own for 127.0.0.1, signed by itself.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "other"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// call POSTs to u with client, and returns the error, if any.
func call(t *testing.T, client *http.Client, u string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, u, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// countingTransport counts the requests that reach it, and sends them on.
type countingTransport struct {
	n    atomic.Int32
	next http.RoundTripper
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.next.RoundTrip(r)
}

// The fake Teams service of AGENTKOURIER_TEAMS_URL gives a Teams connection every endpoint under its base, a TLS config that trusts the
// certificate of AGENTKOURIER_TEAMS_CA, and a transport that reaches the fake and refuses any other host before it is dialled: a Bot
// Connector of Microsoft's, which an activity could name, above all.
func TestTeamsPointsAConnectionAtTheFakeAlone(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "fake") }))
	t.Cleanup(srv.Close)
	t.Setenv(TeamsURLEnv, srv.URL+"/")
	t.Setenv(TeamsCAEnv, fakePEM(srv))

	fake, ok, err := Teams(Overrides{})
	if err != nil || !ok {
		t.Fatalf("Teams = %v, %v; want the fake", ok, err)
	}
	for name, got := range map[string]string{"token": fake.TokenURL, "metadata": fake.MetadataURL, "graph": fake.GraphURL,
		"socket": fake.SocketURL("amer")} {
		want := map[string]string{"token": "/token", "metadata": "/openid", "graph": "/v1.0", "socket": "/amer/v3/websockets/connect"}[name]
		if got != srv.URL+want {
			t.Errorf("the %s URL is %q; want %q", name, got, srv.URL+want)
		}
	}

	inner := &countingTransport{next: &http.Transport{TLSClientConfig: fake.TLS}}
	client := &http.Client{Transport: fake.Wrap(inner)}
	if err := call(t, client, srv.URL+"/v1.0/subscriptions"); err != nil {
		t.Fatalf("a call to the fake: %v; want it trusted by the certificate of %s", err, TeamsCAEnv)
	}
	for _, u := range []string{"https://smba.trafficmanager.net/teams/v3/conversations/x/activities", "https://login.microsoftonline.com/t/oauth2/v2.0/token"} {
		if err := call(t, client, u); !errors.Is(err, errNotTheFake) {
			t.Errorf("a call to %s: %v; want it refused as not the fake", u, err)
		}
	}
	if n := inner.n.Load(); n != 1 {
		t.Errorf("%d calls reached the transport; want the fake's one, and none of Microsoft's", n)
	}

	// The certificate is the only one trusted: a server with a certificate of its own, for the same name, is refused.
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	other.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	other.StartTLS()
	t.Cleanup(other.Close)
	t.Setenv(TeamsURLEnv, other.URL)
	fake, _, err = Teams(Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if err := call(t, &http.Client{Transport: fake.Wrap(&http.Transport{TLSClientConfig: fake.TLS})}, other.URL); err == nil {
		t.Error("a server whose certificate is not the one of AGENTKOURIER_TEAMS_CA was trusted")
	}
}

// Without AGENTKOURIER_TEAMS_URL there is no fake; a unit test's override wins over the environment; a URL that is not an https base, or a
// certificate that is not PEM, is an error that quotes neither.
func TestTeamsRefusesWhatIsNotAFake(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	t.Setenv(TeamsURLEnv, "")
	t.Setenv(TeamsCAEnv, fakePEM(srv))
	if _, ok, err := Teams(Overrides{}); ok || err != nil {
		t.Errorf("Teams with no URL = %v, %v; want no fake and no error", ok, err)
	}
	t.Setenv(TeamsURLEnv, "https://from-the-env.test")
	if fake, ok, err := Teams(Overrides{TeamsURL: srv.URL, TeamsCA: fakePEM(srv)}); !ok || err != nil || fake.GraphURL != srv.URL+"/v1.0" {
		t.Errorf("Teams with an override = %q, %v, %v; want the override's", fake.GraphURL, ok, err)
	}
	for name, o := range map[string]Overrides{
		"http":          {TeamsURL: "http://faketeams.test", TeamsCA: fakePEM(srv)},
		"a path":        {TeamsURL: "https://faketeams.test/v1.0", TeamsCA: fakePEM(srv)},
		"a query":       {TeamsURL: "https://faketeams.test/?x=secretish", TeamsCA: fakePEM(srv)},
		"no host":       {TeamsURL: "https:///", TeamsCA: fakePEM(srv)},
		"credentials":   {TeamsURL: "https://user:pass@faketeams.test", TeamsCA: fakePEM(srv)},
		"no CA":         {TeamsURL: "https://faketeams.test"},
		"a CA, not PEM": {TeamsURL: "https://faketeams.test", TeamsCA: "not-a-certificate"},
	} {
		_, ok, err := Teams(o)
		if ok || err == nil {
			t.Errorf("%s: Teams = %v, %v; want an error", name, ok, err)
			continue
		}
		if strings.Contains(err.Error(), "secretish") || strings.Contains(err.Error(), "pass") || strings.Contains(err.Error(), "not-a-certificate") {
			t.Errorf("%s: the error %q quotes what it was given", name, err)
		}
	}
}
