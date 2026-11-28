package interact

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/render"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// ErrNotStop is returned by StopHandler.Press for a button whose value is not a stop control value: it is someone
// else's button, for the router to hand on.
var ErrNotStop = errors.New("interact: the pressed button is not a stop button")

// Fixed texts told to the person who pressed Stop alone. None holds anything the agent or a person wrote.
const (
	textStopping      = "Stopping the task."
	textStopGone      = "This task is no longer running."
	textStopBinding   = "This thread's agent is no longer set up, so the task cannot be stopped."
	textStopCardLarge = "The agent's card is still too large to read, so the task could not be stopped. Fix the card and press Stop again."
	textStopRefused   = "The agent refused the request to stop this task."
	textStopGaveUp    = "Couldn't stop the task. Please press Stop again."
	// textStopNotMember is never told alone, since Slack refuses an ephemeral to a person outside the channel: it is
	// the word for a platform that answers the press itself (chat.InlineDecider).
	textStopNotMember = "Only members of this channel can stop this task."
)

// IsStopPress reports whether p is a stop button press, which StopHandler.Process works.
func IsStopPress(p store.Press) bool {
	return strings.HasPrefix(p.InteractionID, render.StopLanePrefix)
}

// Stopper cancels the thread's recorded task: *session.Manager (via lateSession in the broker).
type Stopper interface {
	Stop(ctx context.Context, key store.ConversationKey, taskID a2a.TaskID, by string) (bool, error)
}

// StopConfig configures a StopHandler. Everything but Now, Logger, Limits and Outcomes is required.
type StopConfig struct {
	Store       store.Presses
	Sessions    SessionReader
	Authorizers chat.Resolver[Authorizer]
	Ephemerals  chat.Resolver[Ephemeral]
	Stopper     Stopper
	Now         func() time.Time // time.Now if nil
	Logger      *slog.Logger     // slog.Default() if nil
	// Limits bounds the presses waiting (default 1000 in all, 10 for one person).
	Limits store.PressLimits
	// Notify is called with each press recorded, for the worker to pick it up. Required. It runs in Press, on the
	// platform's acknowledgement path, so it must never block (Worker.Submit does not).
	Notify func(store.Press)
	// Outcomes, if set, takes what Process finds a press came to, for a platform that waits for it in the press's
	// request (DecidePress). Nil: DecidePress waits for nothing.
	Outcomes *Outcomes
}

// StopHandler takes the presses of a thread's Stop button (spec 8.2). Press is the chat sink's side: it records the
// press under the thread's lane and returns at once, so the platform's acknowledgement is never held waiting on
// authorization checks or CancelTask. Process is the press worker's side: it checks that the session still holds the
// task, checks that the presser is a non-bot channel member (D12), calls Stopper.Stop, and tells the presser the
// outcome. It is safe for concurrent use.
type StopHandler struct {
	cfg StopConfig
}

var _ PressProcessor = (*StopHandler)(nil)

// NewStopHandler returns a StopHandler over cfg.
func NewStopHandler(cfg StopConfig) (*StopHandler, error) {
	if cfg.Store == nil || cfg.Sessions == nil || cfg.Authorizers == nil || cfg.Ephemerals == nil || cfg.Stopper == nil {
		return nil, errors.New("interact: Store, Sessions, Authorizers, Ephemerals and Stopper are required")
	}
	if cfg.Notify == nil {
		return nil, errors.New("interact: Notify is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Limits.Total <= 0 || cfg.Limits.PerUser <= 0 {
		cfg.Limits = store.PressLimits{Total: 1000, PerUser: 10}
	}
	return &StopHandler{cfg: cfg}, nil
}

type stopPressInputs struct {
	Conn  string `json:"conn"`
	Value string `json:"value"`
}

// Press records a press of a Stop button and hands it to Notify. It returns ErrNotStop for a button that is not a Stop
// control, and store.ErrLimit when too many presses wait. A press missing its user, channel or message ref, or a
// redelivery of a press already processed (same Delivery), is acknowledged and dropped.
func (h *StopHandler) Press(ctx context.Context, i chat.Interaction) error {
	recorded, ok, err := h.record(ctx, i)
	if err != nil || !ok {
		return err
	}
	h.cfg.Notify(recorded)
	return nil
}

// DecidePress is Press for a platform that waits for the decision in the press's request (chat.InlineDecider).
func (h *StopHandler) DecidePress(ctx context.Context, i chat.Interaction) (chat.PressOutcome, error) {
	recorded, ok, err := h.record(ctx, i)
	if err != nil || !ok {
		return chat.PressOutcome{}, err
	}
	return h.cfg.Outcomes.Wait(ctx, recorded, h.cfg.Notify), nil
}

func (h *StopHandler) record(ctx context.Context, i chat.Interaction) (store.Press, bool, error) {
	v, ok := render.ParseStopValue(i.PromptID)
	if !ok {
		return store.Press{}, false, ErrNotStop
	}
	if i.User == "" || i.At.Channel == "" || i.At.ID == "" {
		h.cfg.Logger.WarnContext(ctx, "interact: a stop press that names no person or message was dropped", "channel", i.At.Channel, "message", i.At.ID)
		return store.Press{}, false, nil
	}
	inputs, err := json.Marshal(stopPressInputs{Conn: i.At.Conn, Value: i.PromptID})
	if err != nil {
		return store.Press{}, false, fmt.Errorf("interact: encode a stop press: %w", err)
	}
	press := store.Press{
		InteractionID: render.StopLane(v.Thread),
		User:          i.User,
		Connection:    i.At.Conn,
		Channel:       i.At.Channel,
		MessageRef:    i.At.ID,
		Inputs:        inputs,
		PressedAt:     h.cfg.Now(),
		Delivery:      i.Delivery,
	}
	press.TraceParent, press.TraceState = telemetry.Capture(ctx)
	recorded, ok, err := store.RecordOnce(ctx, h.cfg.Store, press, h.cfg.Limits)
	switch {
	case err != nil:
		return store.Press{}, false, fmt.Errorf("interact: record a stop press on %s: %w", v.Thread, err)
	case !ok:
		h.cfg.Logger.InfoContext(ctx, "interact: a stop press delivered again was processed before; it is acknowledged and not recorded again", "thread", v.Thread)
	}
	return recorded, ok, nil
}

// Process works one recorded stop press through and publishes the outcome to Outcomes for inline platforms.
func (h *StopHandler) Process(ctx context.Context, press store.Press) (Result, error) {
	res, text, err := h.process(ctx, press)
	h.cfg.Outcomes.Publish(press.Seq, res, text)
	return res, err
}

func (h *StopHandler) process(ctx context.Context, press store.Press) (Result, string, error) {
	var in stopPressInputs
	v, ok := render.StopValue{}, false
	if err := json.Unmarshal(press.Inputs, &in); err == nil {
		v, ok = render.ParseStopValue(in.Value)
	}
	if !ok {
		h.cfg.Logger.WarnContext(ctx, "interact: a recorded stop press cannot be read; it was dropped", "channel", press.Channel)
		return h.finish(ctx, press, chat.Target{}, "")
	}
	conn := cmp.Or(in.Conn, press.Connection)
	key := store.ConversationKey{Connection: conn, Channel: press.Channel, Thread: v.Thread}
	to := chat.Target{Conn: conn, Channel: press.Channel, Thread: v.Thread}

	sess, err := h.cfg.Sessions.GetSession(ctx, key, store.AnyAge, h.cfg.Now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return h.finish(ctx, press, to, textStopGone)
	case err != nil:
		return Retry, "", fmt.Errorf("interact: read the session for stop on %s: %w", v.Thread, err)
	case sess.TaskID != v.TaskID:
		return h.finish(ctx, press, to, textStopGone)
	}

	authz, err := h.cfg.Authorizers.Platform(conn)
	if err != nil {
		return Retry, "", fmt.Errorf("interact: check who pressed stop on %s: %w", v.Thread, err)
	}
	switch member, err := authz.IsMember(ctx, press.Channel, press.User); {
	case err != nil:
		return Retry, "", fmt.Errorf("interact: check who pressed stop on %s: %w", v.Thread, err)
	case !member: // told nothing alone: Slack refuses an ephemeral to a person outside the channel
		h.cfg.Logger.DebugContext(ctx, "interact: a stop press of a person outside the channel was ignored", "channel", press.Channel)
		res, _, err := h.finish(ctx, press, to, "")
		return res, textStopNotMember, err
	}
	if bot, err := authz.IsBot(ctx, press.User); err != nil {
		return Retry, "", fmt.Errorf("interact: check who pressed stop on %s: %w", v.Thread, err)
	} else if bot {
		return h.finish(ctx, press, to, "")
	}

	stopped, err := h.cfg.Stopper.Stop(ctx, key, a2a.TaskID(v.TaskID), press.User)
	switch {
	case err == nil && stopped:
		return h.finish(ctx, press, to, textStopping)
	case err == nil:
		return h.finish(ctx, press, to, textStopGone)
	case errors.Is(err, session.ErrUnknownBinding):
		return h.finish(ctx, press, to, textStopBinding)
	case errors.Is(err, dialect.ErrCardTooLarge):
		return h.finish(ctx, press, to, textStopCardLarge)
	case errors.Is(err, agent.ErrCredentialsRejected), errors.Is(err, agent.ErrForbidden), errors.Is(err, agent.ErrAgentForbidden):
		return h.finish(ctx, press, to, textStopRefused)
	default:
		return Retry, "", fmt.Errorf("interact: stop task %s on %s: %w", v.TaskID, v.Thread, err)
	}
}

// GiveUp ends a stop press that could not be processed in time: the person is told to press Stop again, and the press
// is deleted.
func (h *StopHandler) GiveUp(ctx context.Context, press store.Press) error {
	var in stopPressInputs
	_ = json.Unmarshal(press.Inputs, &in)
	v, _ := render.ParseStopValue(in.Value)
	_, _, err := h.finish(ctx, press, chat.Target{Conn: cmp.Or(in.Conn, press.Connection), Channel: press.Channel, Thread: v.Thread}, textStopGaveUp)
	return err
}

func (h *StopHandler) finish(ctx context.Context, press store.Press, to chat.Target, text string) (Result, string, error) {
	if text != "" {
		e, err := h.cfg.Ephemerals.Platform(to.Conn)
		if err == nil {
			err = e.Ephemeral(ctx, to, press.User, chat.Message{Fallback: text})
		}
		if err != nil {
			level := slog.LevelWarn
			if errors.Is(err, chat.ErrNoEphemeral) {
				level = slog.LevelDebug
			}
			h.cfg.Logger.Log(ctx, level, "interact: a person could not be told the outcome of their stop press", "channel", press.Channel, "err", err)
		}
	}
	if err := h.cfg.Store.DeletePress(ctx, press.Seq); err != nil {
		return Retry, "", fmt.Errorf("interact: delete the stop press %d: %w", press.Seq, err)
	}
	return Done, text, nil
}
