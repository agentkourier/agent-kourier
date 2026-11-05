package session

// Visible is visible, for the external tests that pin it to chat.Clean.
var Visible = visible

// FixedDetails are the fixed sentences that the manager gives the thread as the detail of a notice, by what each says, for the test of
// package session_test that holds every one to what a thread shows (it cannot import render from here: render imports chat, which imports
// this package).
var FixedDetails = map[string]string{
	"what the agent sent was too large":                            tooLargeDetail,
	"what the agent sent in reply to an answer was too large":      tooLargeAnswerDetail,
	"the lookup could not be read in full":                         lookupIncompleteDetail,
	"the card was too large":                                       cardTooLargeDetail,
	"the outcome of the answer is unknown":                         answerUnknownDetail,
	"the agent was busy with the task and did not take the answer": answerBusyDetail,
	"the session was not ready":                                    answerNotReadyDetail,
	"the agent replied with a message":                             answerMessageDetail,
}
