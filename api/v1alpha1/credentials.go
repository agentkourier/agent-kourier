package v1alpha1

// The fixed keys of the credentials Secret of a Slack ChatConnection (spec section 7). Socket Mode reads the bot token and the
// app-level token, and HTTP mode the bot token and the signing secret.
const (
	SlackBotTokenKey      = "botToken"
	SlackAppTokenKey      = "appToken"
	SlackSigningSecretKey = "signingSecret"
)

// TeamsClientSecretKey is the fixed key of the credentials Secret of a teams ChatConnection (spec section 7): the client secret of
// the bot's Microsoft Entra application, whose ID is spec.teams.appID.
const TeamsClientSecretKey = "clientSecret"

// The fixed keys of the credentials Secret of a webhook ChatConnection (spec section 7), one for each scheme of spec.webhook.verify:
// hmac-sha256 reads the secret the sender signs with, and token the token the sender sends.
const (
	WebhookSecretKey = "webhookSecret"
	WebhookTokenKey  = "token"
)

// CredentialKeys returns the keys that the credentials Secret of a ChatConnection with spec must hold, in the order they are read and
// reported, or nil for a platform, a Slack mode or a webhook scheme that is not supported, or a teams connection with no spec.teams.
// The platform (spec.platform) and, on Slack, the mode (spec.slack.mode, socket when unset), and on a webhook connection, the scheme
// (spec.webhook.verify.scheme) choose them. It is the one table of them: another platform is one more case here. Each call returns a
// new slice, which the caller may change. It is a func and not a map, so that there is no state to change.
func CredentialKeys(spec ChatConnectionSpec) []string {
	switch spec.Platform {
	case PlatformSlack:
		switch spec.SlackMode() {
		case SlackModeSocket:
			return []string{SlackBotTokenKey, SlackAppTokenKey}
		case SlackModeHTTP:
			return []string{SlackBotTokenKey, SlackSigningSecretKey}
		}
	case PlatformTeams:
		if spec.Teams != nil {
			return []string{TeamsClientSecretKey}
		}
	case PlatformWebhook:
		if spec.Webhook == nil {
			return nil
		}
		switch spec.Webhook.Verify.Scheme {
		case WebhookHMACSHA256:
			return []string{WebhookSecretKey}
		case WebhookToken:
			return []string{WebhookTokenKey}
		}
	}
	return nil
}
