package broker

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/web"
	"github.com/agentkourier/agent-kourier/internal/chat/web/github"
	"github.com/agentkourier/agent-kourier/internal/chat/web/kourierv1"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// newWebPlatform builds the web platform of conn, with the profile of its spec (spec.web.profile) made from the key of its credentials
// Secret that the profile reads (v1alpha1.CredentialKeys): kourier-v1 checks its requests and signs its replies with signingSecret, and
// posts the replies to spec.web.url; github checks GitHub's deliveries with webhookSecret, knows the agent by spec.web.github.login, and
// posts nothing. The replies' client has a transport of its own, so that its idle connections can be closed at shutdown, and a profile with
// no replies makes no request on it. An error is a wiring error, which no retry mends: a profile the broker does not know, or a spec or a
// secret its profile refuses. It names the connection and never holds a secret.
func newWebPlatform(conn Connection, log *slog.Logger, onConnection func(connected bool)) (builtPlatform, error) {
	name := v1alpha1.NamespacedName(conn.Ref.Namespace, conn.Ref.Name)
	if conn.Web == nil {
		return builtPlatform{}, fmt.Errorf("%w: ChatConnection %s is a web connection with no spec.web", ErrWiring, name)
	}
	profile, err := webProfile(conn)
	if err != nil {
		return builtPlatform{}, fmt.Errorf("%w: ChatConnection %s: %w", ErrWiring, name, err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	p, err := web.New(web.Config{Connection: name, Profile: profile, URL: conn.Web.URL, Transport: telemetry.Transport(transport, false),
		OnConnection: onConnection, Logger: log})
	if err != nil {
		return builtPlatform{}, fmt.Errorf("%w: ChatConnection %s: %w", ErrWiring, name, err)
	}
	return builtPlatform{Platform: p, closeIdle: transport.CloseIdleConnections}, nil
}

// webProfile is the profile of the web connection conn, made with its credentials.
func webProfile(conn Connection) (web.Profile, error) {
	switch conn.Web.Profile {
	case v1alpha1.WebProfileKourierV1:
		return kourierv1.New(conn.Credentials[v1alpha1.KourierV1SigningSecretKey])
	case v1alpha1.WebProfileGitHub:
		if conn.Web.GitHub == nil {
			return nil, errors.New("the github profile needs spec.web.github.login")
		}
		return github.New(conn.Credentials[v1alpha1.GitHubWebhookSecretKey], conn.Web.GitHub.Login)
	}
	return nil, fmt.Errorf("the web profile %q has no implementation in this broker", conn.Web.Profile)
}
