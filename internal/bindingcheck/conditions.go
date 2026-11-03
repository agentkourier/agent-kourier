// Package bindingcheck computes and keeps the per-Binding conditions that say whether a Binding that loaded can
// also be served: whether its agent backend answers, whether the bot is in its channel, and whether its approver
// groups can be read.
//
// The package does no I/O of its own and knows neither the agent client nor a chat platform. A Prober calls the
// AgentChecker and ChannelChecker it is given, and records what they report in a State. The conditions are shaped
// like metav1.Condition so that a later controller can copy them into a status unchanged.
package bindingcheck

import "time"

// ConditionType names one fact about a Binding.
type ConditionType string

// The condition types. Ready is derived by State from the others and is never set by a caller.
const (
	// BackendReachable is True when a fresh fetch of the agent card succeeded and the card is one the broker can use.
	BackendReachable ConditionType = "BackendReachable"
	// ChannelJoined is True when the bot is a member of the Binding's channel, which exists and is not archived.
	ChannelJoined ConditionType = "ChannelJoined"
	// ApproversReadable is True when every approver group the Binding names can be read. It applies only to a Binding
	// that asks for tool approvals from groups, so a State holds it only for those.
	ApproversReadable ConditionType = "ApproversReadable"
	// Ready is True when every other condition the Binding has is True.
	Ready ConditionType = "Ready"
)

// Status is the value of a condition.
type Status string

// The statuses, spelled as metav1.ConditionStatus spells them.
const (
	// True means the condition holds.
	True Status = "True"
	// False means the condition was checked and does not hold.
	False Status = "False"
	// Unknown means the condition could not be determined: not probed yet, or the probe could not tell.
	Unknown Status = "Unknown"
)

// valid reports whether s is one of the three statuses.
func (s Status) valid() bool { return s == True || s == False || s == Unknown }

// Reason says why a condition has its status. The set is closed: a Reason is CamelCase and a person can act on it
// without reading code. Ready carries the Type and Reason of the condition that holds it back, joined by a colon
// (for example BackendReachable:AgentNotFound), which metav1.Condition's reason pattern allows and a slash it does not.
// The Prober holds a checker to the set (see Outcome.Reason).
type Reason string

// The reasons of the condition table. A reason that applies to more than one condition appears once.
const (
	// ReasonPending is the reason of a condition whose first probe has not finished.
	ReasonPending Reason = "Pending"
	// ReasonOK is the reason of a condition that is True.
	ReasonOK Reason = "OK"

	// ReasonProbeFailed is the reason of an Unknown condition whose checker returned an error, panicked, or reported
	// no usable outcome. Only the Prober reports it. Checkers that can tell what is wrong report it as an Outcome with
	// its own reason instead.
	ReasonProbeFailed Reason = "ProbeFailed"
	// ReasonProbeTimeout is the reason of an Unknown condition whose probe did not finish within the probe timeout. Only
	// the Prober reports it.
	ReasonProbeTimeout Reason = "ProbeTimeout"

	// ReasonCredentialsUnreadable is a BackendReachable reason: the Binding's token Secret or a Secret behind one of
	// the backend's headers cannot be read.
	ReasonCredentialsUnreadable Reason = "CredentialsUnreadable"
	// ReasonUnreachable is a BackendReachable reason: the backend did not answer (dial, TLS or timeout).
	ReasonUnreachable Reason = "Unreachable"
	// ReasonCredentialsRejected is a BackendReachable reason: the backend answered 401, either because it rejected
	// the Binding's token or because the Binding has none and the backend needs one.
	ReasonCredentialsRejected Reason = "CredentialsRejected"
	// ReasonAgentForbidden is a BackendReachable reason: the backend answered 403, so the agent is not found or not
	// permitted for this identity.
	ReasonAgentForbidden Reason = "AgentForbidden"
	// ReasonAgentNotFound is a BackendReachable reason: the backend answered 404.
	ReasonAgentNotFound Reason = "AgentNotFound"
	// ReasonAgentNotReady is a BackendReachable reason: the backend answered 409.
	ReasonAgentNotReady Reason = "AgentNotReady"
	// ReasonBackendError is a BackendReachable reason: the backend answered 5xx, or 2xx other than 200.
	ReasonBackendError Reason = "BackendError"
	// ReasonUnexpectedStatus is a BackendReachable reason: the backend answered a status no other reason names, a
	// 3xx (often a redirect to a login page) or a 4xx other than 401, 403, 404 and 409.
	ReasonUnexpectedStatus Reason = "UnexpectedStatus"
	// ReasonInvalidAgentCard is a BackendReachable reason: the backend answered 200 with a body that is not a usable
	// agent card, such as an SSO page.
	ReasonInvalidAgentCard Reason = "InvalidAgentCard"
	// ReasonStreamingUnsupported is a BackendReachable reason: the agent card does not advertise streaming.
	ReasonStreamingUnsupported Reason = "StreamingUnsupported"
	// ReasonUnsupportedExtension is a BackendReachable reason: the card requires an extension the broker does not speak.
	ReasonUnsupportedExtension Reason = "UnsupportedExtension"
	// ReasonNoJSONRPCInterface is a BackendReachable reason: the card lists no JSON-RPC interface.
	ReasonNoJSONRPCInterface Reason = "NoJSONRPCInterface"

	// ReasonNotInChannel is a ChannelJoined reason: the bot is not a member of the channel.
	ReasonNotInChannel Reason = "NotInChannel"
	// ReasonChannelNotFound is a ChannelJoined reason: the channel does not exist or the bot cannot see it.
	ReasonChannelNotFound Reason = "ChannelNotFound"
	// ReasonChannelArchived is a ChannelJoined reason: the channel is archived.
	ReasonChannelArchived Reason = "ChannelArchived"
	// ReasonMissingScope is a ChannelJoined or ApproversReadable reason: the chat token lacks the scope for the read.
	ReasonMissingScope Reason = "MissingScope"
	// ReasonChatCredentialsRejected is a False ChannelJoined reason: the chat platform rejected the bot's token, so
	// reading the channel cannot work until an operator replaces it.
	ReasonChatCredentialsRejected Reason = "ChatCredentialsRejected"
	// ReasonChatUnavailable is an Unknown ChannelJoined reason: the chat platform rate-limited or failed the read.
	ReasonChatUnavailable Reason = "ChatUnavailable"
	// ReasonChatCredentialsChanged is an Unknown ChannelJoined reason: the chat credentials changed after the
	// connection started, so a restart is needed before they are used. It is the changed-but-not-yet-restarted case,
	// not the rejected one (ReasonChatCredentialsRejected), where the platform has refused the token the connection
	// is using.
	ReasonChatCredentialsChanged Reason = "ChatCredentialsChanged"

	// ReasonGroupNotFound is an ApproversReadable reason: a named approver group does not exist.
	ReasonGroupNotFound Reason = "GroupNotFound"
)

// reportable reports whether a checker may report r with the status s. OK goes only with True. A reason whose comment
// fixes its status goes only with that status, and any other failure reason with False or Unknown, as the checker can
// tell. Pending, ProbeFailed and ProbeTimeout are the Prober's own, and a reason outside the set is nobody's. It does not
// tie a reason to a condition.
func (r Reason) reportable(s Status) bool {
	switch r {
	case ReasonOK:
		return s == True
	case ReasonChatCredentialsRejected:
		return s == False
	case ReasonChatUnavailable, ReasonChatCredentialsChanged:
		return s == Unknown
	case ReasonCredentialsUnreadable, ReasonUnreachable, ReasonCredentialsRejected, ReasonAgentForbidden, ReasonAgentNotFound,
		ReasonAgentNotReady, ReasonBackendError, ReasonUnexpectedStatus, ReasonInvalidAgentCard, ReasonStreamingUnsupported,
		ReasonUnsupportedExtension, ReasonNoJSONRPCInterface,
		ReasonNotInChannel, ReasonChannelNotFound, ReasonChannelArchived, ReasonMissingScope,
		ReasonGroupNotFound:
		return s != True
	}
	return false
}

// Condition is one fact about one Binding. Its fields and JSON names are those of metav1.Condition, less
// ObservedGeneration (file-loaded objects have none) and plus LastProbeTime, which metav1.Condition does not have: a
// controller that copies a Condition into a status drops LastProbeTime.
type Condition struct {
	// Type is which fact this is.
	Type ConditionType `json:"type"`
	// Status is True, False or Unknown.
	Status Status `json:"status"`
	// Reason is why the condition has this status.
	Reason Reason `json:"reason"`
	// Message is a short, redacted detail. It is for people and is not part of the closed set.
	Message string `json:"message"`
	// LastTransitionTime is when Status last changed. A change of Reason or Message alone does not move it.
	LastTransitionTime time.Time `json:"lastTransitionTime"`
	// LastProbeTime is when the condition was last set from a probe, whether or not anything changed. It is the zero
	// time while the condition is Pending, because no probe has run, and Ready's is the latest of the conditions it
	// derives from.
	LastProbeTime time.Time `json:"lastProbeTime"`
}

// Outcome is what a checker reports for one probe.
type Outcome struct {
	// Status is the condition's new status.
	Status Status
	// Reason is why. Empty is read as OK for True. For False or Unknown it is a checker fault, and the condition
	// becomes Unknown with the reason ProbeFailed. So is a reason that is not reportable: one outside the set, OK with
	// False or Unknown, anything but OK with True, a reason with a status its comment rules out, or one of the Prober's
	// own (Pending, ProbeFailed and ProbeTimeout). A checker that gives up on a slow backend reports Unreachable.
	Reason Reason
	// Message is a short detail, already redacted: nothing in it may be a token or a Secret value.
	Message string
}
