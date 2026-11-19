// Package testhooks holds the hooks that only a test image of Agent Kourier carries: a crash between a
// Slack post and its record (spec 8.6, S5), armed through AGENTKOURIER_FAULT, an override of the Slack
// API URL so Agent Kourier can talk to the fake Slack, an override of every Microsoft endpoint of a Microsoft
// Teams connection so it can talk to the fake Teams service (test/faketeams), and a before-send gate for
// failover tests.
//
// All exist only when the binary is built with the testhooks tag. In the default build, which is the
// production build, AfterPost and BeforeSend do nothing, SlackAPIURL returns "", Teams returns no fake and Overrides has
// no field, and the binary contains neither the crash nor the names of the environment variables (buildcheck_test.go
// proves it). The overrides move where the API calls go, never which token or secret they carry.
package testhooks

import (
	"crypto/tls"
	"net/http"
)

const (
	// FaultEnv names the environment variable that arms a fault, read once at startup.
	FaultEnv = "AGENTKOURIER_FAULT"
	// CrashAfterPost is the FaultEnv value that makes AfterPost kill the process.
	CrashAfterPost = "crash-after-post"
	// SlackAPIURLEnv names the environment variable that SlackAPIURL returns.
	SlackAPIURLEnv = "AGENTKOURIER_SLACK_API_URL"
	// TeamsURLEnv names the environment variable that holds the https base URL of the fake Teams service, which Teams returns.
	TeamsURLEnv = "AGENTKOURIER_TEAMS_URL"
	// TeamsCAEnv names the environment variable that holds the PEM certificate the fake Teams service's TLS is checked against:
	// the only one a Teams connection trusts while TeamsURLEnv is set.
	TeamsCAEnv = "AGENTKOURIER_TEAMS_CA"
)

// TeamsFake is where a Microsoft Teams connection of a test image sends what it would send to Microsoft: the endpoints of the fake
// Teams service, the TLS config that trusts its certificate alone, and Wrap, which makes a transport refuse every host but the
// fake's, so that no call of the connection, not even one to a Bot Connector that an activity named, leaves for Microsoft.
type TeamsFake struct {
	// TokenURL is the fake's Microsoft Entra token endpoint, MetadataURL its Bot Framework OpenID metadata, and GraphURL its
	// Microsoft Graph base.
	TokenURL, MetadataURL, GraphURL string
	// SocketURL is the fake's connect endpoint of a region in socket mode.
	SocketURL func(region string) string
	// TLS trusts the fake's certificate and nothing else.
	TLS *tls.Config
	// Wrap returns next limited to the fake's host.
	Wrap func(next http.RoundTripper) http.RoundTripper
}
