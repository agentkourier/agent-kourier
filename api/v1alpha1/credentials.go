package v1alpha1

// The fixed keys of the credentials Secret of a Slack ChatConnection (spec section 7).
const (
	SlackBotTokenKey = "botToken"
	SlackAppTokenKey = "appToken"
)

// CredentialKeys returns the keys that the credentials Secret of a ChatConnection on platform (spec.platform) must hold, in the order
// they are read and reported, or nil for a platform that is not supported. It is the one table of them: a second platform is one more
// case here. Each call returns a new slice, which the caller may change. It is a func and not a map, so that there is no state to change.
func CredentialKeys(platform string) []string {
	if platform == PlatformSlack {
		return []string{SlackBotTokenKey, SlackAppTokenKey}
	}
	return nil
}
