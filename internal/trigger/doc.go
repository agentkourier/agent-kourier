// Package trigger is Agent Kourier's message intake: the Sink that applies spec 8.2's trigger table to what a chat
// platform delivers, the same for every platform. Intake turns a mention or a reply into a turn for the session manager
// (Submitter), tries a bot's or a person's message against the Binding's chat.triggers, keeps the thread of each trigger
// key in the store, holds each start to the trigger's limits, and hands a start on (IntakeConfig.OnStart). Replay applies
// the same matcher to a channel's history, for the dry run.
//
// It speaks the chat-neutral model (package chat) and names no platform: the adapter has decided what is a mention and
// who is who. Package chat holds the model and the Platform contract and imports none of this package's session,
// config and store dependencies.
package trigger
