package teams

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
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
// within subscriptionRenewLead, or within half the lifetime that Graph granted when that is shorter (Graph may grant less than it was
// asked, and 24 hours was not measured), so that a renew that fails is tried again, at each reconcile, for half its life before the
// subscription lapses. Each call to Graph to list, make, renew or delete one is cut at subscriptionCallTimeout: a create and a renew take 7 to 8
// seconds (measured), the time of Graph's validation handshake. The subscriptions are reconciled with the connection's Bindings every
// subscriptionReconcileEvery, and at once when a lifecycle notification or a clientState that is not the connection's asks for it, when
// the receiver records the Microsoft Entra group of a channel's team, and when the bound channels change, which the subscriber looks at
// every subscriptionWatchEvery (a read of the config in memory, with no call). A
// listing reads at most subscriptionListPages pages. A bound channel whose resource the app has a subscription to on another URL, which
// Graph lets an app have once (409), is tried again after subscriptionConflictRetry, or once that subscription expires if sooner.
const (
	subscriptionLifetime       = 24 * time.Hour
	subscriptionRenewLead      = 12 * time.Hour
	subscriptionCallTimeout    = 30 * time.Second
	subscriptionReconcileEvery = time.Minute
	subscriptionWatchEvery     = time.Second
	subscriptionListPages      = 10
	subscriptionConflictRetry  = 10 * time.Minute
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
	// granted is the lifetime Graph granted when it was made or renewed; for one adopted, which a listing does not say, the lifetime asked for.
	granted  time.Duration
	renew    bool // a lifecycle notification asked for a renew (reauthorizationRequired)
	recreate bool // it is to be deleted and made anew: removed by Graph, or its clientState is not the connection's
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
	watch                            time.Duration // how often the bound channels are compared with the last reconcile's
	conflictRetry                    time.Duration
	now                              func() time.Time
	log                              *slog.Logger
	wake                             chan struct{} // a reconcile is wanted now; holds one
	ready                            atomic.Bool   // the subscriptions of this run are known: adopted, or none bound to adopt
	rounds                           atomic.Uint64 // the reconciles done, which a test waits on

	mu   sync.Mutex
	held map[string]*subscription // by channel; guarded by mu
	ids  map[string]*subscription // by ID; guarded by mu

	// What run's goroutine alone reads and writes.
	bound            []string             // the bound channels of the last reconcile
	listed           bool                 // the app's subscriptions were listed since the run began or the last 409
	listFailing      bool                 // the last listing failed, which was told at WARN
	waiting, failing map[string]bool      // by channel: told at WARN
	blocked          map[string]time.Time // by channel: the app's subscription on another URL holds its resource; no create before then
	conflicts        map[string]bool      // by channel: the blocked channel was told at WARN
}

func newSubscriber(c *Client, url, state string) *subscriber {
	return &subscriber{c: c, url: url, state: state, lifetime: subscriptionLifetime, renewLead: subscriptionRenewLead,
		callTimeout: subscriptionCallTimeout, every: subscriptionReconcileEvery, watch: subscriptionWatchEvery, conflictRetry: subscriptionConflictRetry, now: c.graph.now, log: c.log,
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

// hold records sub as the subscription of its channel, and its group as that of the channel's team (knowGroup).
func (s *subscriber) hold(sub *subscription) {
	s.c.knowGroup(sub.channel, sub.group)
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

// run reconciles the subscriptions with the channels that lister names until ctx ends, every s.every, when poked, and when the channels
// that lister names are not those of the last reconcile, which it looks at every s.watch. It starts from
// nothing held, and lists the app's subscriptions again first (adopt): another leader may have made or renewed them since this one last
// ran. It leaves the subscriptions in Graph when it stops: the next leader adopts them, and Graph sends to the same URL meanwhile, which
// the leader-labeled Service routes to whichever replica leads. With no lister nothing is bound: the subscriptions to the connection's URL
// are deleted, and nothing is made.
func (s *subscriber) run(ctx context.Context, lister chat.ChannelLister) {
	s.mu.Lock()
	clear(s.held)
	clear(s.ids)
	s.mu.Unlock()
	s.bound, s.listed, s.listFailing, s.waiting, s.failing = nil, false, false, map[string]bool{}, map[string]bool{}
	s.blocked, s.conflicts = map[string]time.Time{}, map[string]bool{}
	s.ready.Store(false)
	tick := time.NewTicker(s.every)
	defer tick.Stop()
	watch := time.NewTicker(s.watch)
	defer watch.Stop()
	defer func() {
		s.ready.Store(false)
		s.setGauges(ctx, 0, 0, 0)
	}()
	for {
		s.reconcile(ctx, lister)
		s.rounds.Add(1)
		for due := false; !due; {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				due = true
			case <-s.wake:
				due = true
			case <-watch.C:
				due = lister != nil && !slices.Equal(lister.BoundChannels(), s.bound)
			}
		}
	}
}

// reconcile makes the held subscriptions those of the bound channels. It lists the app's subscriptions at the start of the run and after
// a 409 (adopt), with no channel bound too, so that a subscription to the connection's URL that nothing reads any longer is deleted. It
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
	s.bound = bound
	want := map[string]bool{}
	for _, ch := range bound {
		want[ch] = true
	}
	creates := true
	if !s.listed {
		if err := s.adopt(ctx, want); err != nil {
			// A listing after a 409 has the run's subscriptions known already (ready): those held are still renewed, and only the makes
			// wait for a listing that succeeds, so that a listing that keeps failing does not let every channel's subscription lapse.
			relisting := s.ready.Load()
			if ctx.Err() == nil && !s.listFailing {
				what := "nothing is subscribed until they are"
				if relisting {
					what = "no subscription is made until they are, and those held are still renewed"
				}
				s.log.WarnContext(ctx, "teams: the app's Microsoft Graph subscriptions could not be listed; "+what+
					", and they are listed again at each reconcile", "error", err.Error())
			}
			s.listFailing = ctx.Err() == nil
			if !relisting {
				s.setGauges(ctx, 0, 0, len(want))
				return
			}
			creates = false
		} else {
			if s.listFailing {
				s.listFailing = false
				s.log.InfoContext(ctx, "teams: the app's Microsoft Graph subscriptions were listed again")
			}
			s.listed = true
		}
	}
	s.ready.Store(true)
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
	for _, m := range []map[string]bool{s.failing, s.conflicts} {
		for ch := range m {
			if !want[ch] {
				delete(m, ch)
			}
		}
	}
	for ch := range s.blocked {
		if !want[ch] {
			delete(s.blocked, ch)
		}
	}
	for _, ch := range bound {
		if ctx.Err() != nil {
			return
		}
		s.keep(ctx, ch, creates)
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

// keep makes, remakes or renews the subscription of the bound channel ch, as it needs; with creates false it only renews one it holds.
func (s *subscriber) keep(ctx context.Context, ch string, creates bool) {
	sub, held := s.snapshot(ch)
	if !creates && (!held || sub.recreate) {
		return // a remake deletes before it makes: left whole until the subscriptions are listed again
	}
	if held && sub.recreate {
		if err := s.remove(ctx, sub.id); err != nil {
			s.failed(ctx, ch, "deleted to be made anew", err)
			return
		}
		s.drop(ch)
		held = false
	}
	if !held {
		if until, ok := s.blocked[ch]; ok && s.now().Before(until) {
			return // failing since the 409; told at WARN once (adopt)
		}
		s.subscribe(ctx, ch)
		return
	}
	if !sub.renew && sub.expires.Sub(s.now()) > min(s.renewLead, sub.granted/2) {
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

// renewed records the new expiry of the subscription id and the lifetime Graph granted, and clears its renew flag; a remake asked for
// meanwhile stands.
func (s *subscriber) renewed(id string, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sub, ok := s.ids[id]; ok {
		sub.expires, sub.granted, sub.renew = expires, expires.Sub(s.now()), false
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
		// The app has a subscription to the resource already, which the last listing did not show: listed again at the next reconcile,
		// and not before, which adopts it if it is to the connection's URL, and holds the channel back if it is to another (adopt).
		s.listed = false
	}
	if err != nil {
		s.failed(ctx, ch, "made", err)
		return
	}
	s.hold(sub)
	delete(s.blocked, ch)
	delete(s.conflicts, ch)
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
// that it holds none of yet, for the change types it makes, with the lifecycle URL, and not expired. One it holds already (a listing
// after a 409) is kept as it is. It deletes the others to the connection's URL, which nothing reads, a second of a held channel among
// them. A subscription to another URL is another connection's, or another broker's, and is left as it is; if it is to the messages of a
// bound channel that holds none, that channel cannot have one until it is gone, since Graph lets an app have one subscription to a
// resource and change types (409): the channel is blocked, failing, until it expires or subscriptionConflictRetry has passed, and told
// at WARN once, with the fix. A listing longer than subscriptionListPages, or one that names a page off the Graph endpoint, is an error,
// since a subscription it did not read would be made again and refused.
func (s *subscriber) adopt(ctx context.Context, want map[string]bool) error {
	link := s.c.graph.resource("subscriptions")
	var mine []graphSubscription
	others := map[string]graphSubscription{} // by bound channel: the app's subscription to its messages on another URL that expires last
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
				continue
			}
			if _, ch, ok := parseSubscriptionResource(sub.Resource); ok && want[ch] &&
				sub.ExpirationDateTime.After(others[ch].ExpirationDateTime) {
				others[ch] = sub
			}
		}
		link = p.NextLink
	}
	now := s.now()
	for _, sub := range mine {
		group, ch, ok := parseSubscriptionResource(sub.Resource)
		held, isHeld := s.snapshot(ch)
		switch {
		case isHeld && held.id == sub.ID:
			continue // this run's own
		case ok && want[ch] && !isHeld && sub.LifecycleNotificationURL == s.url && sub.ChangeType == subscriptionChangeTypes &&
			sub.ExpirationDateTime.After(now):
			s.hold(&subscription{id: sub.ID, channel: ch, group: group, expires: sub.ExpirationDateTime, granted: s.lifetime})
			delete(s.blocked, ch)
			s.log.InfoContext(ctx, "teams: adopted the Microsoft Graph subscription of a bound channel", "channel", ch,
				"expires", sub.ExpirationDateTime)
			continue
		}
		if err := s.remove(ctx, sub.ID); err != nil && ctx.Err() == nil {
			s.log.WarnContext(ctx, "teams: a stale Microsoft Graph subscription to the connection's URL could not be deleted",
				"subscription", sub.ID, "error", err.Error())
		}
	}
	for ch := range want {
		other, found := others[ch]
		if _, isHeld := s.snapshot(ch); isHeld || !found {
			delete(s.blocked, ch)
			continue
		}
		until := now.Add(s.conflictRetry)
		if other.ExpirationDateTime.Before(until) {
			until = other.ExpirationDateTime
		}
		s.blocked[ch] = until
		s.failing[ch] = true
		if !s.conflicts[ch] {
			s.conflicts[ch] = true
			s.log.WarnContext(ctx, "teams: a bound channel cannot be subscribed to: the app has a Microsoft Graph subscription to its messages "+
				"for another notification URL, and Graph lets an app have one. Delete that subscription, or wait for it to expire; the "+
				"channel is tried again every "+s.conflictRetry.String()+" until then", "channel", ch,
				"otherNotificationURL", urlForLog(other.NotificationURL), "otherExpires", other.ExpirationDateTime)
		}
	}
	return nil
}

// urlForLog is link's scheme, host and path, for a log line: a URL that another tool of the app subscribed with may carry a credential
// in its query or its user information, and the broker logs none of that.
func urlForLog(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return "(not a URL)"
	}
	return quote((&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String())
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
	expires := s.expiry(got.ExpirationDateTime)
	return &subscription{id: got.ID, channel: ch, group: group, expires: expires, granted: expires.Sub(s.now())}, nil
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

// call is one call to Graph about the subscriptions, cut at s.callTimeout and counted as operation. A make or a renew is about a
// channel's messages, which the resource-specific permission grants, so its 403 is told that fix; a listing's or a delete's is not.
func (s *subscriber) call(ctx context.Context, operation, method, link string, body, out any) (err error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat."+operation)
	defer func() { s.c.finish(ctx, op, operation, err) }()
	ctx, cancel := context.WithTimeout(ctx, s.callTimeout)
	defer cancel()
	err = s.c.graph.call(ctx, method, link, body, out)
	var ge *GraphError
	if errors.As(err, &ge) && (operation == "Subscribe" || operation == "Renew") {
		ge.channel = true
	}
	return err
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
