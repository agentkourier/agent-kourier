// Package teams is Agent Kourier's Microsoft Teams adapter. This part takes what Teams sends a bot over HTTPS: it verifies the Bot
// Framework's token, refuses an activity of another tenant, drops one that comes again, maps a person's post in a team's
// channel to the neutral model (chat.Inbound), passes an invoke to its own seam (InvokeHandler), and records what each activity says
// about its channel (ConversationStore) for the calls that will post to it and read it through Graph.
//
// The package is no chat.Platform yet: it has no outbound side, and the broker does not run a teams connection. Its Receiver is the
// handler that the connection's route on the chat listener will serve, and Receiver.Serve what the platform's Run will call in http mode.
package teams
