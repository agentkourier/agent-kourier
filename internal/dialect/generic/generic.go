// Package generic implements A2A 1.0 JSON-RPC with plain-text questions, and kagent's HITL extension: it asks for the
// extension when the card lists it, and reads a pause as the extension's when the pause's own message declares it.
package generic

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/kagentv1"
)

// Dialect uses an exact endpoint rather than a backend-specific agent path.
type Dialect struct{ *kagentv1.Dialect }

var _ dialect.Dialect = (*Dialect)(nil)

// New shares kagent's protected HTTP transport and credential handling.
func New(timeout time.Duration) *Dialect { return &Dialect{kagentv1.New(timeout)} }

// Endpoint validates the configured endpoint without interpolating logical agent identifiers. The URL is
// returned as written, trailing slash included: a router that canonicalizes ".../agent" to ".../agent/"
// answers a POST with a redirect (kagent 0.9.x does, with a 307), and this dialect follows none, so the
// operator's spelling is the one sent. The card is fetched under the endpoint with the slash trimmed.
func (*Dialect) Endpoint(backend *v1alpha1.AgentBackend, _ v1alpha1.AgentRef) (string, error) {
	u, err := url.Parse(backend.Spec.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("A2A endpoint must be an http or https URL without credentials, query or fragment")
	}
	return u.String(), nil
}

// Connect fetches the card and pins JSON-RPC to the configured gateway. It offers kagent's HITL extension, which the
// client activates (sends A2A-Extensions for) only when the card lists it, so an agent that does not is sent no header.
func (d *Dialect) Connect(ctx context.Context, backend *v1alpha1.AgentBackend, agent v1alpha1.AgentRef, creds dialect.Credentials) (*a2aclient.Client, error) {
	endpoint, err := d.Endpoint(backend, agent)
	if err != nil {
		return nil, err
	}
	if d.Dialect == nil {
		return nil, errors.New("generic: Connect needs New")
	}
	return d.ConnectEndpoint(ctx, endpoint, creds, kagentv1.HITLExtension)
}

// ParseInteraction reads a pause whose status message declares kagent's HITL extension and carries its payload as
// kagent-v1 does: a question or tool approval that does not take text replies. Any other pause must be a text question;
// structured parts cannot be approved as text, and a payload the message does not name in its extensions is not read.
func (d *Dialect) ParseInteraction(task a2a.Task) (*dialect.Interaction, error) {
	if task.Status.State != a2a.TaskStateInputRequired {
		return nil, nil
	}
	if msg := task.Status.Message; msg != nil && slices.Contains(msg.Extensions, kagentv1.HITLExtension) && msg.Metadata[kagentv1.HITLExtension] != nil {
		return d.Dialect.ParseInteraction(task)
	}
	fail := func() (*dialect.Interaction, error) {
		return nil, fmt.Errorf("text question missing or structured: %w", dialect.ErrUnusablePause)
	}
	if task.ID == "" || task.Status.Message == nil {
		return fail()
	}
	var text []string
	for _, part := range task.Status.Message.Parts {
		if part == nil {
			return fail()
		}
		t, ok := part.Content.(a2a.Text)
		if !ok {
			return fail()
		}
		text = append(text, string(t))
	}
	question := strings.TrimSpace(strings.Join(text, "\n"))
	if question == "" {
		return fail()
	}
	request := string(task.ID)
	switch {
	case task.Status.Message.ID != "":
		request += "/" + task.Status.Message.ID
	case task.Status.Timestamp != nil:
		request += "/" + task.Status.Timestamp.UTC().Format(time.RFC3339Nano)
	default:
		// Repeated questions on one task must have distinct durable identities.
		return fail()
	}
	return &dialect.Interaction{TextReplies: true, Kind: dialect.AskUser, TaskID: task.ID, ContextID: task.ContextID, RequestID: request, Questions: []dialect.Question{{Text: question}}, Hint: "Reply in this thread or submit your answer below."}, nil
}

// BuildResponse answers a pause read through kagent's HITL extension (Interaction.Extension) as kagent-v1 does, and any
// other with one complete text answer to its text question. An interaction stored before Extension existed has none,
// and was a text question.
func (d *Dialect) BuildResponse(in dialect.Interaction, answer dialect.Answer) (a2a.Message, error) {
	if in.Extension == kagentv1.HITLExtension {
		return d.Dialect.BuildResponse(in, answer)
	}
	if in.Kind != dialect.AskUser || in.TaskID == "" || answer.RequestID != in.RequestID || len(in.Questions) != 1 || len(in.Questions[0].Choices) != 0 || len(answer.Answers) != 1 || len(answer.Answers[0]) != 1 || strings.TrimSpace(answer.Answers[0][0]) == "" || len(answer.Decisions) != 0 {
		return a2a.Message{}, errors.New("one complete text answer to the current question is required")
	}
	return a2a.Message{Role: a2a.MessageRoleUser, TaskID: in.TaskID, ContextID: in.ContextID, Parts: []*a2a.Part{a2a.NewTextPart(answer.Answers[0][0])}}, nil
}
