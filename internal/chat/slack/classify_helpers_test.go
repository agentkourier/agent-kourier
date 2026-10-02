package slack_test

import (
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
)

// These read what Slack's own code is behind a verdict of Classify, which is the only way out of the adapter that
// says what an error means: the code of a permanent error of a post, of a reaction, and of credentials Slack rejects,
// or "" if the error is not one.
func codeOf(op chat.Op, class chat.Class, err error) string {
	if v := slack.NewPlatform(nil, "ns/conn").Classify(op, err); v.Class == class {
		return v.Code
	}
	return ""
}

func permanentCode(err error) string { return codeOf(chat.OpPost, chat.Permanent, err) }

func reactionPermanentCode(err error) string { return codeOf(chat.OpReact, chat.Permanent, err) }

func fatalAuthCode(err error) string { return codeOf(chat.OpOther, chat.FatalAuth, err) }
