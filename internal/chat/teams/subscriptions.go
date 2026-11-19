package teams

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// This file keeps a Microsoft Graph subscription to the messages of each bound channel of a connection with a notificationURL, while
// the connection runs (on the leader alone): one per channel, on /teams/{group}/channels/{channel}/messages, for created and updated,
// with no resource data, whose notification and lifecycle URL are both the notificationURL. push.go takes what Graph sends there.
//
// Measured on a test tenant in November 2026: a create takes about 8 seconds and a renew about 7, since Graph makes its validation
// handshake to both URLs within them; a listing of the app's subscriptions gives each one's resource, URLs and expiry, and its
// clientState as null. Microsoft's documentation, not measured: a subscription to a channel's messages lives at most 4,320 minutes,
// one that lives past an hour must name a lifecycle URL, and a second subscription of the app with the same resource and change types
// is refused with 409.

// The life of a subscription. It is made to expire subscriptionLifetime after it is made or renewed, and renewed once it would expire
// within subscriptionRenewLead, so that a renew that fails is tried again, at each reconcile, for half a day before the subscription
// lapses. Each call to Graph to list, make, renew or delete one is cut at subscriptionCallTimeout: a create and a renew take 7 to 8
// seconds (measured), the time of Graph's validation handshake. The subscriptions are reconciled with the connection's Bindings every
// subscriptionReconcileEvery, and at once when a lifecycle notification or a clientState that is not the connection's asks for it. A
// listing reads at most subscriptionListPages pages.
const (
	subscriptionLifetime       = 24 * time.Hour
	subscriptionRenewLead      = 12 * time.Hour
	subscriptionCallTimeout    = 30 * time.Second
	subscriptionReconcileEvery = time.Minute
	subscriptionListPages      = 10
)

// The states of a bound channel's subscription, the label of agentkourier_teams_graph_channels.
const (
	channelSubscribed   = "subscribed"
	channelWaitingGroup = "waiting_group"
	channelFailing      = "failing"
)

// subscription is a subscription of the connection to the messages of one bound channel, as the subscriber holds it.
type subscription struct {
	id, channel, group string
	expires            time.Time
	renew              bool // a lifecycle notification asked for a renew (reauthorizationRequired)
	recreate           bool // it is to be deleted and made anew: removed by Graph, or its clientState is not the connection's
}

// subFlag is what a notification asks of a subscription.
type subFlag int

const (
	flagRenew subFlag = iota
	flagRecreate
)

// subscriber keeps the connection's subscriptions. run reconciles them on its own goroutine; the route reads them (byID) and flags
// them (flag, mismatch) from the requests' goroutines.
type subscriber struct {
	c                                *Client
	url, state                       string
	lifetime, renewLead, callTimeout time.Duration
	every                            time.Duration
	now                              func() time.Time
	log                              *slog.Logger
	wake                             chan struct{} // a reconcile is wanted now; holds one
	ready                            atomic.Bool   // the subscriptions of this run are known: adopted, or none bound to adopt
	rounds                           atomic.Uint64 // the reconciles done, which a test waits on

	mu   sync.Mutex
	held map[string]*subscription // by channel; guarded by mu
	ids  map[string]*subscription // by ID; guarded by mu

	// What run's goroutine alone reads and writes.
	listed           bool            // the app's subscriptions were listed in this run
	listFailing      bool            // the last listing failed, which was told at WARN
	waiting, failing map[string]bool // by channel: told at WARN
}

func newSubscriber(c *Client, url, state string) *subscriber {
	return &subscriber{c: c, url: url, state: state, lifetime: subscriptionLifetime, renewLead: subscriptionRenewLead,
		callTimeout: subscriptionCallTimeout, every: subscriptionReconcileEvery, now: c.graph.now, log: c.log,
		wake: make(chan struct{}, 1), held: map[string]*subscription{}, ids: map[string]*subscription{}}
}

// byID is the subscription whose ID is id, as a copy, and false if the connection holds none.
func (s *subscriber) byID(id string) (subscription, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.ids[id]
	if !ok {
		return subscription{}, false
	}
	return *sub, true
}

// flag marks the subscription id for a renew or a remake, and wakes the loop.
func (s *subscriber) flag(id string, f subFlag) {
	s.mu.Lock()
	sub, ok := s.ids[id]
	if ok {
		switch f {
		case flagRenew:
			sub.renew = true
		case flagRecreate:
			sub.recreate = true
		}
	}
	s.mu.Unlock()
	if ok {
		s.poke()
	}
}

// mismatch is told of a notification whose clientState is not the connection's, which names the subscription id. If the connection
// holds it, it is made anew, with the clientState of the client secret of now: one adopted from before a change of the secret, or made
// by a broker with another secret, has an old one, which a listing does not show. Anyone may send such a notification, but it can name
// only a subscription ID, which Graph does not publish; a remake costs one delete and one create.
func (s *subscriber) mismatch(id string) {
	if id != "" {
		s.flag(id, flagRecreate)
	}
}

// poke asks the loop for a reconcile now.
func (s *subscriber) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// hold records sub as the subscription of its channel.
func (s *subscriber) hold(sub *subscription) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.held[sub.channel]; ok {
		delete(s.ids, old.id)
	}
	s.held[sub.channel], s.ids[sub.id] = sub, sub
}

// drop forgets the subscription of channel.
func (s *subscriber) drop(channel string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.held[channel]; ok {
		delete(s.ids, old.id)
		delete(s.held, channel)
	}
}

// snapshot is a copy of the subscription of channel, and false if none is held.
func (s *subscriber) snapshot(channel string) (subscription, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.held[channel]
	if !ok {
		return subscription{}, false
	}
	return *sub, true
}

// run reconciles the subscriptions with the channels that lister names until ctx ends, every s.every and when poked. It starts from
// nothing held, and lists the app's subscriptions again first (adopt): another leader may have made or renewed them since this one last
// ran. It leaves the subscriptions in Graph when it stops: the next leader adopts them, and Graph sends to the same URL meanwhile, which
// the leader-labeled Service routes to whichever replica leads. With no lister nothing is bound, and nothing is made.
func (s *subscriber) run(ctx context.Context, lister chat.ChannelLister) {
	s.mu.Lock()
	clear(s.held)
	clear(s.ids)
	s.mu.Unlock()
	s.listed, s.listFailing, s.waiting, s.failing = false, false, map[string]bool{}, map[string]bool{}
	s.ready.Store(false)
	tick := time.NewTicker(s.every)
	defer tick.Stop()
	defer func() {
		s.ready.Store(false)
		s.setGauges(ctx, 0, 0, 0)
	}()
	for {
		s.reconcile(ctx, lister)
		s.rounds.Add(1)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.wake:
		}
	}
}

// reconcile makes the held subscriptions those of the bound channels: it adopts the app's subscriptions to the connection's URL once,
// deletes those of channels no Binding names any longer, makes those of bound channels with none (once the channel's team's Microsoft
// Entra group is known), remakes those flagged for it, and renews those flagged or near their expiry. A call that fails is tried again
// at the next reconcile.
func (s *subscriber) reconcile(ctx context.Context, lister chat.ChannelLister) {
	if ctx.Err() != nil {
		return
	}
	var bound []string
	if lister != nil {
		bound = lister.BoundChannels()
	}
	want := map[string]bool{}
	for _, ch := range bound {
		want[ch] = true
	}
	if !s.listed && len(want) > 0 {
		if err := s.adopt(ctx, want); err != nil {
			if ctx.Err() == nil && !s.listFailing {
				s.log.WarnContext(ctx, "teams: the app's Microsoft Graph subscriptions could not be listed; nothing is subscribed until they are, "+
					"and they are listed again at each reconcile", "error", err.Error())
			}
			s.listFailing = ctx.Err() == nil
			s.setGauges(ctx, 0, 0, len(want))
			return
		}
		if s.listFailing {
			s.listFailing = false
			s.log.InfoContext(ctx, "teams: the app's Microsoft Graph subscriptions were listed again")
		}
		s.listed = true
	}
	s.ready.Store(s.listed || len(want) == 0)
	s.mu.Lock()
	heldChannels := slices.Sorted(maps.Keys(s.held))
	s.mu.Unlock()
	for _, ch := range heldChannels {
		if !want[ch] {
			s.unsubscribe(ctx, ch)
		}
	}
	for ch := range s.waiting {
		if !want[ch] {
			delete(s.waiting, ch)
		}
	}
	for ch := range s.failing {
		if !want[ch] {
			delete(s.failing, ch)
		}
	}
	for _, ch := range bound {
		if ctx.Err() != nil {
			return
		}
		s.keep(ctx, ch)
	}
	subscribed, waiting, failing := 0, 0, 0
	for _, ch := range bound {
		_, held := s.snapshot(ch)
		switch {
		case s.failing[ch]:
			failing++
		case s.waiting[ch]:
			waiting++
		case held:
			subscribed++
		}
	}
	s.setGauges(ctx, subscribed, waiting, failing)
}

// keep makes, remakes or renews the subscription of the bound channel ch, as it needs.
func (s *subscriber) keep(ctx context.Context, ch string) {
	sub, held := s.snapshot(ch)
	if held && sub.recreate {
		if err := s.remove(ctx, sub.id); err != nil {
			s.failed(ctx, ch, "deleted to be made anew", err)
			return
		}
		s.drop(ch)
		held = false
	}
	if !held {
		s.subscribe(ctx, ch)
		return
	}
	if !sub.renew && sub.expires.Sub(s.now()) > s.renewLead {
		s.recovered(ctx, ch)
		return
	}
	expires, err := s.renewCall(ctx, sub.id)
	switch {
	case isNotFound(err): // gone from Graph: made anew
		s.drop(ch)
		s.subscribe(ctx, ch)
	case err != nil:
		s.failed(ctx, ch, "renewed", err)
	default:
		s.renewed(sub.id, expires)
		s.recovered(ctx, ch)
	}
}

// renewed records the new expiry of the subscription id, and clears its renew flag; a remake asked for meanwhile stands.
func (s *subscriber) renewed(id string, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sub, ok := s.ids[id]; ok {
		sub.expires, sub.renew = expires, false
	}
}

// subscribe makes the subscription of ch, once its team's Microsoft Entra group is known. A channel whose group is not known waits, and
// is told once at WARN, with the fix: the group is learned from an activity of the channel, which the bot receives once it is installed
// in the channel's team (a post that mentions it, in http mode or socket mode).
func (s *subscriber) subscribe(ctx context.Context, ch string) {
	group, err := s.c.teamGroup(ctx, ch)
	switch {
	case errors.Is(err, errTeamNotKnown):
		if !s.waiting[ch] {
			s.waiting[ch] = true
			s.log.WarnContext(ctx, "teams: a bound channel's messages cannot be subscribed to yet: the Microsoft Entra group of its team is "+
				"not known. Install the app in the channel's team and mention the bot in the channel, so that an activity of the channel "+
				"names its team; the subscription is made at the next reconcile after", "channel", ch)
		}
		return
	case err != nil:
		s.failed(ctx, ch, "made", err)
		return
	}
	if s.waiting[ch] {
		delete(s.waiting, ch)
	}
	sub, err := s.create(ctx, group, ch)
	var ge *GraphError
	if errors.As(err, &ge) && ge.Status == http.StatusConflict {
		// The app has a subscription to the resource already, which the listing did not show as this connection's: listed again at
		// the next reconcile, which adopts it if it is to the connection's URL.
		s.listed = false
		s.poke()
	}
	if err != nil {
		s.failed(ctx, ch, "made", err)
		return
	}
	s.hold(sub)
	s.recovered(ctx, ch)
	s.log.InfoContext(ctx, "teams: subscribed to a bound channel's messages on Microsoft Graph", "channel", ch, "expires", sub.expires)
}

// unsubscribe deletes the subscription of ch, a channel that no Binding names any longer. One that Graph no longer has is gone already.
func (s *subscriber) unsubscribe(ctx context.Context, ch string) {
	sub, ok := s.snapshot(ch)
	if !ok {
		return
	}
	if err := s.remove(ctx, sub.id); err != nil {
		if ctx.Err() == nil {
			s.log.WarnContext(ctx, "teams: the Microsoft Graph subscription of a channel no Binding names could not be deleted; trying again "+
				"at the next reconcile", "channel", ch, "error", err.Error())
		}
		return
	}
	s.drop(ch)
	s.log.InfoContext(ctx, "teams: deleted the Microsoft Graph subscription of a channel no Binding names", "channel", ch)
}

// failed records that the subscription of ch could not be what (made, renewed, …): told at WARN when the channel was not failing
// already, and at DEBUG after.
func (s *subscriber) failed(ctx context.Context, ch, what string, err error) {
	if ctx.Err() != nil {
		return
	}
	if !s.failing[ch] {
		s.failing[ch] = true
		s.log.WarnContext(ctx, "teams: a bound channel's Microsoft Graph subscription could not be "+what+"; trying again at the next reconcile",
			"channel", ch, "error", err.Error())
		return
	}
	s.log.DebugContext(ctx, "teams: a bound channel's Microsoft Graph subscription could not be "+what+" again", "channel", ch,
		"error", err.Error())
}

// recovered records that the subscription of ch is as it should be, told at INFO if it was failing.
func (s *subscriber) recovered(ctx context.Context, ch string) {
	if s.failing[ch] {
		delete(s.failing, ch)
		s.log.InfoContext(ctx, "teams: a bound channel's Microsoft Graph subscription is kept again", "channel", ch)
	}
}

// setGauges sets agentkourier_teams_graph_channels.
func (s *subscriber) setGauges(ctx context.Context, subscribed, waiting, failing int) {
	telemetry.SetGauge(ctx, "agentkourier_teams_graph_channels", float64(subscribed), channelSubscribed, s.c.conn)
	telemetry.SetGauge(ctx, "agentkourier_teams_graph_channels", float64(waiting), channelWaitingGroup, s.c.conn)
	telemetry.SetGauge(ctx, "agentkourier_teams_graph_channels", float64(failing), channelFailing, s.c.conn)
}

// graphSubscription is a subscription as Graph gives it.
type graphSubscription struct {
	ID                       string    `json:"id"`
	Resource                 string    `json:"resource"`
	ChangeType               string    `json:"changeType"`
	NotificationURL          string    `json:"notificationUrl"`
	LifecycleNotificationURL string    `json:"lifecycleNotificationUrl"`
	ExpirationDateTime       time.Time `json:"expirationDateTime"`
}

// adopt lists the app's subscriptions, and holds those to the connection's URL that it can keep: to the messages of a channel of want
// that it holds none of yet, for the change types it makes, with the lifecycle URL, and not expired. It deletes the others to the
// connection's URL, which nothing reads. A subscription to another URL is another connection's, or another broker's, and is left as it
// is. A listing longer than subscriptionListPages, or one that names a page off the Graph endpoint, is an error, since a subscription it
// did not read would be made again and refused (409).
func (s *subscriber) adopt(ctx context.Context, want map[string]bool) error {
	link := s.c.graph.resource("subscriptions")
	var mine []graphSubscription
	for page := 0; link != ""; page++ {
		if page == subscriptionListPages {
			return fmt.Errorf("teams: the app's Microsoft Graph subscriptions take more than %d pages", subscriptionListPages)
		}
		var p struct {
			Value    []graphSubscription `json:"value"`
			NextLink string              `json:"@odata.nextLink"`
		}
		if err := s.call(ctx, "ListSubscriptions", http.MethodGet, link, nil, &p); err != nil {
			return err
		}
		for _, sub := range p.Value {
			if sub.NotificationURL == s.url {
				mine = append(mine, sub)
			}
		}
		link = p.NextLink
	}
	now := s.now()
	for _, sub := range mine {
		group, ch, ok := parseSubscriptionResource(sub.Resource)
		_, taken := s.snapshot(ch)
		if ok && want[ch] && !taken && sub.LifecycleNotificationURL == s.url && sub.ChangeType == subscriptionChangeTypes &&
			sub.ExpirationDateTime.After(now) {
			s.hold(&subscription{id: sub.ID, channel: ch, group: group, expires: sub.ExpirationDateTime})
			s.log.InfoContext(ctx, "teams: adopted the Microsoft Graph subscription of a bound channel", "channel", ch,
				"expires", sub.ExpirationDateTime)
			continue
		}
		if err := s.remove(ctx, sub.ID); err != nil && ctx.Err() == nil {
			s.log.WarnContext(ctx, "teams: a stale Microsoft Graph subscription to the connection's URL could not be deleted",
				"subscription", sub.ID, "error", err.Error())
		}
	}
	return nil
}

// subscriptionChangeTypes are the change types of a subscription: an edit is notified too, and dropped (push.go), so that one listing
// shape is the connection's.
const subscriptionChangeTypes = "created,updated"

// graphTime is how a time is written to Graph: UTC, with seven digits of a second, as Graph writes it.
func graphTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.0000000Z") }

// create makes the subscription of channel ch of the team of the Microsoft Entra group group.
func (s *subscriber) create(ctx context.Context, group, ch string) (*subscription, error) {
	body := struct {
		ChangeType               string `json:"changeType"`
		NotificationURL          string `json:"notificationUrl"`
		LifecycleNotificationURL string `json:"lifecycleNotificationUrl"`
		Resource                 string `json:"resource"`
		ExpirationDateTime       string `json:"expirationDateTime"`
		ClientState              string `json:"clientState"`
		IncludeResourceData      bool   `json:"includeResourceData"`
	}{subscriptionChangeTypes, s.url, s.url, "/teams/" + group + "/channels/" + ch + "/messages",
		graphTime(s.now().Add(s.lifetime)), s.state, false}
	var got graphSubscription
	if err := s.call(ctx, "Subscribe", http.MethodPost, s.c.graph.resource("subscriptions"), body, &got); err != nil {
		return nil, err
	}
	if got.ID == "" {
		return nil, errors.New("teams: Microsoft Graph named no ID for a subscription it made")
	}
	return &subscription{id: got.ID, channel: ch, group: group, expires: s.expiry(got.ExpirationDateTime)}, nil
}

// renewCall sets the expiry of the subscription id to subscriptionLifetime from now, and returns what Graph set.
func (s *subscriber) renewCall(ctx context.Context, id string) (time.Time, error) {
	body := struct {
		ExpirationDateTime string `json:"expirationDateTime"`
	}{graphTime(s.now().Add(s.lifetime))}
	var got graphSubscription
	if err := s.call(ctx, "Renew", http.MethodPatch, s.c.graph.resource("subscriptions", id), body, &got); err != nil {
		return time.Time{}, err
	}
	return s.expiry(got.ExpirationDateTime), nil
}

// expiry is the expiry Graph answered, or, when it answered none, the one asked for.
func (s *subscriber) expiry(got time.Time) time.Time {
	if got.IsZero() {
		return s.now().Add(s.lifetime)
	}
	return got
}

// remove deletes the subscription id. One that Graph does not have (404) is gone already, and no error.
func (s *subscriber) remove(ctx context.Context, id string) error {
	err := s.call(ctx, "Unsubscribe", http.MethodDelete, s.c.graph.resource("subscriptions", id), nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// call is one call to Graph about the subscriptions, cut at s.callTimeout and counted as operation.
func (s *subscriber) call(ctx context.Context, operation, method, link string, body, out any) (err error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat."+operation)
	defer func() { s.c.finish(ctx, op, operation, err) }()
	ctx, cancel := context.WithTimeout(ctx, s.callTimeout)
	defer cancel()
	return s.c.graph.call(ctx, method, link, body, out)
}

// parseSubscriptionResource reads a subscription's resource, /teams/{group}/channels/{channel}/messages as it is made, or with the
// keys in parentheses, as Graph may give it back (resourceSegments).
func parseSubscriptionResource(res string) (group, channel string, ok bool) {
	segs, ok := resourceSegments(res)
	if !ok || len(segs) != 3 || segs[0].name != "teams" || segs[1].name != "channels" || segs[2].name != "messages" ||
		segs[0].key == "" || segs[1].key == "" || segs[2].key != "" {
		return "", "", false
	}
	return segs[0].key, segs[1].key, true
}
