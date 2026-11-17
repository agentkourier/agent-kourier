package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// Post sends o in t as a new message whose ID is tag, and returns where it is. Its idempotency key is tag, the same on every try, so
// that the receiver can drop a repeat of a post whose answer was lost.
func (p *Platform) Post(ctx context.Context, t chat.Target, o chat.Outgoing, tag string) (chat.Ref, error) {
	if err := p.sendable(o); err != nil {
		return chat.Ref{}, err
	}
	if tag == "" {
		return chat.Ref{}, fmt.Errorf("%w: a post with no tag has no ID", chat.ErrInvalid)
	}
	r := Reply{Kind: ReplyPost, Connection: p.conn, Channel: t.Channel, Thread: t.Thread, ID: tag, Key: tag, Message: o}
	if err := p.send(ctx, r); err != nil {
		return chat.Ref{}, err
	}
	return p.RefAt(t.Channel, t.Thread, tag), nil
}

// Update sends o as the new content of the message at r. Each update has a key of its own: it replaces the content, so a receiver that
// applies one twice shows the same, and a key made of the content would have a receiver drop the third of the updates A, B, A.
func (p *Platform) Update(ctx context.Context, r chat.Ref, o chat.Outgoing) error {
	if err := p.sendable(o); err != nil {
		return err
	}
	return p.send(ctx, Reply{Kind: ReplyUpdate, Connection: p.conn, Channel: r.Channel, Thread: r.Thread, ID: r.ID, Key: "update-" + rand.Text(), Message: o})
}

// Find says not found, and asks nothing: a receiver has no lookup. A post that the outbox sends again after it is not found carries the
// same idempotency key as the first, which the receiver drops.
func (p *Platform) Find(context.Context, chat.Target, string, time.Time) (chat.Ref, bool, error) {
	return chat.Ref{}, false, nil
}

// Fits says whether p can be shown in one message: a valid prompt of questions that the encoder takes. A prompt of tools is invalid,
// since an approval needs the buttons and presses that the platform does not have.
func (p *Platform) Fits(pr chat.Prompt) error {
	o := chat.Ask(pr)
	if err := p.sendable(o); err != nil {
		return err
	}
	_, err := p.enc.Encode(Reply{Kind: ReplyPost, Connection: p.conn, ID: pr.ID, Key: pr.ID, Message: o}, p.now())
	return err
}

// sendable says whether o can be sent: o is valid, and it holds no prompt of tools.
func (p *Platform) sendable(o chat.Outgoing) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if (o.Prompt != nil && len(o.Prompt.Tools) > 0) || (o.Ending != nil && len(o.Ending.Prompt.Tools) > 0) {
		return fmt.Errorf("%w: the web platform shows no tool approvals", chat.ErrInvalid)
	}
	return nil
}

// statusError is a receiver's answer other than 2xx.
type statusError struct {
	// Code is the answer's status code.
	Code int
	// RetryAfter is how long the receiver asked the caller to wait (Retry-After, in seconds or as a date), within maxRetryAfter, or 0
	// when it asked for none.
	RetryAfter time.Duration
}

func (e *statusError) Error() string { return fmt.Sprintf("web: the receiver answered %d", e.Code) }

// send encodes r and sends it to the connection's URL. The error of a request that failed names the URL, which is configuration and no
// secret, and never the body.
func (p *Platform) send(ctx context.Context, r Reply) error {
	now := p.now()
	enc, err := p.enc.Encode(r, now)
	if err != nil {
		return fmt.Errorf("web: encode the %s of %s: %w", r.Kind, r.ID, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(enc.Body))
	if err != nil {
		return fmt.Errorf("web: the request: %w", err)
	}
	for k, vs := range enc.Header {
		req.Header[k] = vs
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("web: send the %s of %s: %w", r.Kind, r.ID, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxAnswerRead))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	return &statusError{Code: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"), now)}
}

// retryAfter reads a Retry-After, in seconds or as an HTTP date, and bounds it to maxRetryAfter. One it cannot read, or one in the past,
// is 0.
func retryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	var d time.Duration
	if s, err := strconv.ParseInt(v, 10, 64); err == nil {
		d = time.Duration(min(s, int64(maxRetryAfter/time.Second))) * time.Second
	} else if at, err := http.ParseTime(v); err == nil {
		d = at.Sub(now)
	}
	return min(max(d, 0), maxRetryAfter)
}

// Classify says what an error of the platform means for a retry. A receiver's 429, 5xx and 408, a request that failed or timed out, and
// any error it does not know may pass on another try; a 429 or a 503 that asked for a wait says how long, a 429 at least a second. Any
// other answer will not pass, nor will a reply the profile cannot carry. A 401 or a 403 is Permanent and not FatalAuth: a receiver that
// refuses the signature fails the replies, and stops nothing else of the connection.
func (p *Platform) Classify(_ chat.Op, err error) chat.Verdict {
	switch {
	case errors.Is(err, chat.ErrInvalid), errors.Is(err, chat.ErrTooLarge):
		return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonInvalidContent, Code: "invalid"}
	case errors.Is(err, chat.ErrChannelNotFound):
		return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone, Code: "channel_not_found"}
	case errors.Is(err, chat.ErrNoReactions), errors.Is(err, chat.ErrNoEphemeral), errors.Is(err, chat.ErrNotStreaming),
		errors.Is(err, chat.ErrGroupsMisconfigured):
		return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonOther}
	}
	se, ok := errors.AsType[*statusError](err)
	if !ok {
		return chat.Verdict{Class: chat.Transient}
	}
	code := strconv.Itoa(se.Code)
	switch {
	case se.Code == http.StatusTooManyRequests:
		return chat.Verdict{Class: chat.Transient, Code: code, RetryAfter: max(se.RetryAfter, time.Second)}
	case se.Code == http.StatusServiceUnavailable:
		return chat.Verdict{Class: chat.Transient, Code: code, RetryAfter: se.RetryAfter}
	case se.Code >= 500, se.Code == http.StatusRequestTimeout:
		return chat.Verdict{Class: chat.Transient, Code: code}
	case se.Code == http.StatusNotFound, se.Code == http.StatusGone:
		return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone, Code: code}
	case se.Code == http.StatusRequestEntityTooLarge:
		return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonTooLong, Code: code}
	case se.Code == http.StatusBadRequest, se.Code == http.StatusUnprocessableEntity:
		return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonInvalidContent, Code: code}
	}
	return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonOther, Code: code} // a 3xx (no redirect is followed), 401, 403, and the rest
}
