// Package teams is Agent Kourier's Microsoft Teams adapter, a chat.Platform in http mode (Client).
//
// Its Receiver takes what Teams sends a bot over HTTPS, at the connection's route on the chat listener: it verifies the Bot
// Framework's token, refuses an activity of another tenant, drops one that comes again, maps a person's post in a team's channel to
// the neutral model (chat.Inbound), passes an invoke to its own seam (InvokeHandler), and records what each activity says about its
// channel (ConversationStore).
//
// The Client posts, replies and edits through the Bot Connector, at the service URL that the channel's record names, with the bot's
// token from Microsoft Entra (the client credentials grant). It renders the neutral message as Teams' markdown, with every text that
// the agent, a person or an alert wrote shown as text, a message's steps as a list in a quote under the rest and its buttons as an
// Adaptive Card under it; a prompt, and the ending that replaces it, as an Adaptive Card alone (card.go). A press of a card's button
// comes as an invoke, which the Client hands to the sink and answers in the request (press.go). What Teams cannot do yet (streams,
// reactions, ephemeral messages) it says by the chat package's errors.
package teams
