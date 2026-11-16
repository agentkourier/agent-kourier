package v1alpha1

// The fixed keys of the credentials Secret of a Slack ChatConnection (spec section 7). Socket Mode reads the bot token and the
// app-level token, and HTTP mode the bot token and the signing secret.
const (
	SlackBotTokenKey      = "botToken"
	SlackAppTokenKey      = "appToken"
	SlackSigningSecretKey = "signingSecret"
)

// The fixed keys of the credentials Secret of a web ChatConnection, one for each profile (spec section 7): the kourier-v1 profile
// checks its requests and signs its replies with the signing secret, and the github profile checks GitHub's webhooks with the
// webhook's secret.
const (
	KourierV1SigningSecretKey = "signingSecret"
	GitHubWebhookSecretKey    = "webhookSecret"
)

// CredentialKeys returns the keys that the credentials Secret of a ChatConnection with spec must hold, in the order they are read and
// reported, or nil for a platform, a Slack mode or a web profile that is not supported. The platform (spec.platform) and, on Slack, the
// mode (spec.slack.mode, socket when unset) or, on web, the profile (spec.web.profile) choose them. It is the one table of them: a
// platform or a profile is one more case here. Each call returns a new slice, which the caller may change. It is a func and not a map,
// so that there is no state to change.
func CredentialKeys(spec ChatConnectionSpec) []string {
	switch spec.Platform {
	case PlatformSlack:
		switch spec.SlackMode() {
		case SlackModeSocket:
			return []string{SlackBotTokenKey, SlackAppTokenKey}
		case SlackModeHTTP:
			return []string{SlackBotTokenKey, SlackSigningSecretKey}
		}
	case PlatformWeb:
		if spec.Web == nil {
			return nil
		}
		switch spec.Web.Profile {
		case WebProfileKourierV1:
			return []string{KourierV1SigningSecretKey}
		case WebProfileGitHub:
			return []string{GitHubWebhookSecretKey}
		}
	}
	return nil
}
