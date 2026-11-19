package faketeams

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// subscription is a Graph subscription of an app to a channel's messages.
type subscription struct {
	id, app, resource, changeType string
	channel                       string
	notificationURL, lifecycleURL string
	clientState                   string
	expires                       time.Time
}

// SubscriptionView is a live subscription as the control API shows it.
type SubscriptionView struct {
	ID              string    `json:"id"`
	App             string    `json:"app"`
	Channel         string    `json:"channel"`
	NotificationURL string    `json:"notificationUrl"`
	Expires         time.Time `json:"expires"`
}

// subscriptionJSON is a subscription as Graph lists it: its clientState is null, as measured.
func (sub *subscription) json() map[string]any {
	return map[string]any{"id": sub.id, "resource": sub.resource, "changeType": sub.changeType, "notificationUrl": sub.notificationURL,
		"lifecycleNotificationUrl": sub.lifecycleURL, "expirationDateTime": sub.expires.Format(time.RFC3339Nano), "clientState": nil,
		"applicationId": sub.app}
}

// channelOfResource is the channel of a subscription's resource, /teams/{group}/channels/{channel}/messages, if it names the team's
// group.
func (s *Service) channelOfResource(resource string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimPrefix(resource, "/"), "teams/"+s.cfg.GroupID+"/channels/")
	if !ok {
		return "", false
	}
	channel, ok := strings.CutSuffix(rest, "/messages")
	return channel, ok && channel != "" && !strings.Contains(channel, "/")
}

// listSubscriptions is the app's live subscriptions, in one page.
func (s *Service) listSubscriptions(w http.ResponseWriter, _ *http.Request, app string) {
	s.mu.Lock()
	value := []map[string]any{}
	for _, sub := range s.liveLocked(app) {
		value = append(value, sub.json())
	}
	s.mu.Unlock()
	writeJSON(w, map[string]any{"value": value})
}

// liveLocked is the app's subscriptions that have not expired, in the order of their IDs.
func (s *Service) liveLocked(app string) []*subscription {
	now := time.Now()
	var out []*subscription
	for _, sub := range s.subs {
		if sub.app == app && sub.expires.After(now) {
			out = append(out, sub)
		}
	}
	slices.SortFunc(out, func(a, b *subscription) int { return strings.Compare(a.id, b.id) })
	return out
}

// createSubscription makes a subscription to a channel's messages, after the validation handshake with both of its URLs, as Graph
// does. A second live subscription of the app to the same resource and change types is refused with 409.
func (s *Service) createSubscription(w http.ResponseWriter, r *http.Request, app string) {
	var body struct {
		ChangeType               string    `json:"changeType"`
		NotificationURL          string    `json:"notificationUrl"`
		LifecycleNotificationURL string    `json:"lifecycleNotificationUrl"`
		Resource                 string    `json:"resource"`
		ExpirationDateTime       time.Time `json:"expirationDateTime"`
		ClientState              string    `json:"clientState"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&body); err != nil || body.NotificationURL == "" || body.ChangeType == "" {
		apiError(w, http.StatusBadRequest, "InvalidRequest")
		return
	}
	channel, ok := s.channelOfResource(body.Resource)
	s.mu.Lock()
	known := ok && slices.Contains(s.channels, channel)
	conflict := slices.ContainsFunc(s.liveLocked(app), func(sub *subscription) bool {
		return sub.channel == channel && sub.changeType == body.ChangeType
	})
	s.mu.Unlock()
	switch {
	case !known:
		apiError(w, http.StatusNotFound, "NotFound")
		return
	case conflict:
		apiError(w, http.StatusConflict, "Conflict")
		return
	}
	for _, u := range slices.Compact([]string{body.NotificationURL, body.LifecycleNotificationURL}) {
		if u != "" && !s.validate(r.Context(), u) {
			apiError(w, http.StatusBadRequest, "ValidationError")
			return
		}
	}
	s.mu.Lock()
	if slices.ContainsFunc(s.liveLocked(app), func(sub *subscription) bool { return sub.channel == channel && sub.changeType == body.ChangeType }) {
		s.mu.Unlock()
		apiError(w, http.StatusConflict, "Conflict")
		return
	}
	s.subSeq++
	sub := &subscription{id: "fake-sub-" + strconv.Itoa(s.subSeq), app: app, resource: body.Resource, changeType: body.ChangeType,
		channel: channel, notificationURL: body.NotificationURL, lifecycleURL: body.LifecycleNotificationURL, clientState: body.ClientState,
		expires: body.ExpirationDateTime}
	s.subs[sub.id] = sub
	s.countLocked(&s.stats.SubscriptionCreates, channel)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(sub.json())
}

func (s *Service) countLocked(m *map[string]int, key string) {
	if *m == nil {
		*m = map[string]int{}
	}
	(*m)[key]++
}

// renewSubscription moves a subscription's expiry, after the validation handshake with both of its URLs, as Graph does on every renewal
// (the plan's spike; not measured here): a renewal whose URLs no longer answer it is refused with 400, and the expiry stays.
func (s *Service) renewSubscription(w http.ResponseWriter, r *http.Request, app string) {
	var body struct {
		ExpirationDateTime time.Time `json:"expirationDateTime"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "InvalidRequest")
		return
	}
	s.mu.Lock()
	sub := s.subs[r.PathValue("id")]
	if sub == nil || sub.app != app {
		s.mu.Unlock()
		apiError(w, http.StatusNotFound, "ResourceNotFound")
		return
	}
	urls := slices.Compact([]string{sub.notificationURL, sub.lifecycleURL})
	s.mu.Unlock()
	for _, u := range urls {
		if u != "" && !s.validate(r.Context(), u) {
			apiError(w, http.StatusBadRequest, "ValidationError")
			return
		}
	}
	s.mu.Lock()
	if s.subs[sub.id] != sub {
		s.mu.Unlock()
		apiError(w, http.StatusNotFound, "ResourceNotFound")
		return
	}
	sub.expires = body.ExpirationDateTime
	s.countLocked(&s.stats.SubscriptionRenewals, sub.channel)
	out := sub.json()
	s.mu.Unlock()
	writeJSON(w, out)
}

// deleteSubscription ends a subscription.
func (s *Service) deleteSubscription(w http.ResponseWriter, r *http.Request, app string) {
	s.mu.Lock()
	sub := s.subs[r.PathValue("id")]
	if sub == nil || sub.app != app {
		s.mu.Unlock()
		apiError(w, http.StatusNotFound, "ResourceNotFound")
		return
	}
	delete(s.subs, sub.id)
	s.countLocked(&s.stats.SubscriptionDeletes, sub.channel)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// deliveryURL is where the fake sends what Graph sends to u: u itself, or with DeliverHTTP its plain HTTP twin.
func (s *Service) deliveryURL(u string) string {
	if s.cfg.DeliverHTTP {
		if rest, ok := strings.CutPrefix(u, "https://"); ok {
			return "http://" + rest
		}
	}
	return u
}

// validate makes Graph's validation handshake with u: a POST with a validationToken, which must be answered 200 with the token as plain
// text.
func (s *Service) validate(ctx context.Context, u string) bool {
	var b [16]byte
	_, _ = rand.Read(b[:])
	token := "Validation: fake " + hex.EncodeToString(b[:])
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.deliveryURL(u)+"?validationToken="+url.QueryEscape(token), nil)
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return err == nil && resp.StatusCode == http.StatusOK && string(got) == token
}

// notifyLocked sends, to every live subscription to m's channel whose change types hold changeType, a change notification of m, in the
// background, again every Retry until it is answered 2xx or RetryFor has passed.
func (s *Service) notifyLocked(m *message, changeType string) {
	resource := "teams('" + s.cfg.GroupID + "')/channels('" + m.channel + "')/messages('" + m.id + "')"
	if m.root != "" {
		resource = "teams('" + s.cfg.GroupID + "')/channels('" + m.channel + "')/messages('" + m.root + "')/replies('" + m.id + "')"
	}
	now := time.Now()
	for _, sub := range s.subs {
		if sub.channel != m.channel || !sub.expires.After(now) || !slices.Contains(strings.Split(sub.changeType, ","), changeType) {
			continue
		}
		body, err := json.Marshal(map[string]any{"value": []any{map[string]any{
			"subscriptionId": sub.id, "subscriptionExpirationDateTime": sub.expires.Format(time.RFC3339Nano), "changeType": changeType,
			"clientState": sub.clientState, "resource": resource, "tenantId": s.cfg.TenantID,
			"resourceData": map[string]any{"id": m.id, "@odata.type": "#Microsoft.Graph.chatMessage", "@odata.id": resource},
		}}})
		if err != nil {
			continue
		}
		s.stats.Notifications = append(s.stats.Notifications, Notification{Subscription: sub.id, ChangeType: changeType, Message: m.id})
		i := len(s.stats.Notifications) - 1
		s.wg.Add(1)
		go func(u string) {
			defer s.wg.Done()
			s.deliver(u, body, i)
		}(s.deliveryURL(sub.notificationURL))
	}
}

// deliver sends one notification until it is answered 2xx, and records each attempt in the notification i.
func (s *Service) deliver(u string, body []byte, i int) {
	deadline := time.Now().Add(s.cfg.RetryFor)
	for {
		status := 0
		ctx, cancel := context.WithTimeout(s.ctx, deliveryTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			if resp, err := s.client.Do(req); err == nil {
				status = resp.StatusCode
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
				_ = resp.Body.Close()
			}
		}
		cancel()
		ok := status >= 200 && status < 300
		s.mu.Lock()
		n := &s.stats.Notifications[i]
		n.Attempts++
		n.Status, n.Delivered = status, ok
		s.mu.Unlock()
		if ok || time.Now().After(deadline) {
			return
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(s.cfg.Retry):
		}
	}
}
