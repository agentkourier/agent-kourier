package teams

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// This file reads people on Microsoft Teams: who is in a channel's team (IsMember), whether a user is a bot (IsBot), and a person's
// name (ResolveUser, and the decider an ending names). Every read is of the Bot Connector's roster of a conversation,
// GET /v3/conversations/{id}/pagedmembers, page by page: its query (pageSize, continuationToken), its answer (members, each with id,
// name and aadObjectId, and the continuationToken of the next page), that the roster of a team's standard channel is the team's
// members, and its errors and throttling are Microsoft's documentation ("Get Teams specific context for your bot", read 2026-11-18),
// not measured: no roster was read on a test tenant.

const (
	// rosterTimeout bounds each read of people as a whole, every page and every team of it: the press worker waits on it, off the
	// invoke's path (Teams has been answered by then), as the Slack client's checks get interact.PressAuthzTimeout.
	rosterTimeout = 10 * time.Second
	// rosterPage is the members a roster read asks for in one page, the most that Microsoft documents.
	rosterPage = 500
	// rosterMaxPages is the most pages one read of people asks for, across every team it reads: 25,000 members at rosterPage, the
	// largest team that Microsoft documents (not measured). A read that needs more is an error, so that a roster whose continuation
	// tokens never end costs a bounded number of calls of the tenant's budget at each try of a press.
	rosterMaxPages = 50
	// namesMax is the most accounts the Client keeps, and namesTTL how long it gives one: a person's name and Teams ID, for
	// ResolveUser, IsBot and the decider of an ending. Membership is never cached.
	namesMax = 1000
	namesTTL = time.Hour
	// nameUnits is the most UTF-16 units of a name that a mention shows (mentionName).
	nameUnits = 64
)

// errUnknownPerson is the error of a read that found no account for the ID in the rosters it read.
var errUnknownPerson = errors.New("teams: no roster that the bot can read lists the person")

// errRosterTooLong is the error of a read of people that would ask for more than rosterMaxPages pages.
var errRosterTooLong = fmt.Errorf("teams: the rosters are longer than %d pages", rosterMaxPages)

// rosterPath is the path of a page of the roster of conv; token is the continuation token of the page, "" for the first.
func rosterPath(conv, token string) string {
	q := url.Values{"pageSize": {strconv.Itoa(rosterPage)}}
	if token != "" {
		q.Set("continuationToken", token)
	}
	return "v3/conversations/" + escapeID(conv) + "/pagedmembers?" + q.Encode()
}

// rosterPageOf is one page of a roster as the Bot Connector answers it.
type rosterPageOf struct {
	Members           []account `json:"members"`
	ContinuationToken string    `json:"continuationToken"`
}

// findIn reads the roster of the conversation conv at serviceURL, page by page, until an account whose aadObjectId is aad, which it
// keeps (names) and returns; false when the last page has none. Each page takes one of *pages, which the caller shares across the
// rosters of one read: a page past them is errRosterTooLong, and no call. A page whose next names a page this roster already gave
// (a cycle) is an error, as is any failure of a page: the read fails closed, whatever a later page would have held. ctx bounds it.
func (c *Client) findIn(ctx context.Context, pages *int, serviceURL, conv, aad string) (account, bool, error) {
	token := ""
	seen := map[string]bool{"": true}
	for {
		if *pages <= 0 {
			return account{}, false, errRosterTooLong
		}
		*pages--
		var page rosterPageOf
		if err := c.api.call(ctx, http.MethodGet, serviceURL, rosterPath(conv, token), nil, &page); err != nil {
			return account{}, false, err
		}
		if i := slices.IndexFunc(page.Members, func(a account) bool { return a.AADObjectID == aad }); i >= 0 {
			c.names.put(page.Members[i])
			return page.Members[i], true, nil
		}
		switch {
		case page.ContinuationToken == "":
			return account{}, false, nil
		case seen[page.ContinuationToken]:
			return account{}, false, errors.New("teams: a page of the roster names as its next a page that the roster already gave")
		}
		token = page.ContinuationToken
		seen[token] = true
	}
}

// IsMember implements chat.Platform by the roster of the channel's conversation, the team's members, read at the channel's service URL
// (DefaultServiceURL for a channel no activity has come from) at each call: nothing of it is cached, so a person who left the team is
// refused at the next press. user is the person's Microsoft Entra object ID, the aadObjectId of the roster's accounts; a user ID of
// any other shape is no member, with no call. The read stops at the person, and is bounded by rosterTimeout and rosterMaxPages.
//
// It fails closed: an error is never "yes", and is false with the error. Classify says what the error is: Transient for a rate limit
// (with its wait), a 5xx, a timeout or the bound; Permanent ReasonChannelGone for a conversation that is gone (404) or a bot removed
// from the team (403 BotNotInConversationRoster); FatalAuth for a token the Bot Connector rejects. An empty channel is
// chat.ErrChannelNotFound with no call.
func (c *Client) IsMember(ctx context.Context, channel, user string) (member bool, err error) {
	if channel == "" {
		return false, fmt.Errorf("teams: %w: no channel", chat.ErrChannelNotFound)
	}
	if !entraID.MatchString(user) {
		return false, nil
	}
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.IsMember")
	defer func() { c.finish(ctx, op, "IsMember", err) }()
	ctx, cancel := context.WithTimeout(ctx, c.rosterTimeout)
	defer cancel()
	pages := rosterMaxPages
	_, found, err := c.findIn(ctx, &pages, c.serviceURL(ctx, channel), channel, user)
	if err != nil {
		return false, fmt.Errorf("teams: reading the channel's roster: %w", err)
	}
	return found, nil
}

// IsBot implements chat.Platform. A Teams ID that begins 28: is a bot's, with no call. A Microsoft Entra object ID is the account that
// ResolveUser reads for it: a bot when its Teams ID begins 28:, and a person otherwise (a person's begins 29:). It fails closed: an ID
// that no roster the bot can read lists (a posting application's ID among them, which no roster holds), a roster that cannot be read,
// and an ID of any other shape are a bot, with the error.
func (c *Client) IsBot(ctx context.Context, user string) (bot bool, err error) {
	if strings.HasPrefix(user, botPrefix) {
		return true, nil
	}
	if !entraID.MatchString(user) {
		return true, errors.New("teams: the user ID is neither a bot's nor a Microsoft Entra object ID")
	}
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.IsBot")
	defer func() { c.finish(ctx, op, "IsBot", err) }()
	a, err := c.account(ctx, user)
	if err != nil {
		return true, err
	}
	return strings.HasPrefix(a.ID, botPrefix), nil
}

// ResolveUser implements chat.Platform: the name of the person whose Microsoft Entra object ID is id, the account's name in the roster
// of a team the bot has heard from in this run (the teams of the channels the receiver recorded), each read in turn, in the order of
// their IDs, until one lists the person, within rosterTimeout and rosterMaxPages in all. The account is kept for namesTTL (namesMax of them), and a kept
// one is given with no call. A team whose roster cannot be read does not end the call: the next is read, and the first error is
// returned only if none lists the person. An ID of another shape, a person no roster lists, and a connection that has heard from no
// team yet (a broker that has just started, before any activity) are errors: the caller shows the ID alone.
func (c *Client) ResolveUser(ctx context.Context, id string) (u chat.User, err error) {
	if !entraID.MatchString(id) {
		return chat.User{}, errors.New("teams: the user ID is not a Microsoft Entra object ID")
	}
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.ResolveUser")
	defer func() { c.finish(ctx, op, "ResolveUser", err) }()
	a, err := c.account(ctx, id)
	if err != nil {
		return chat.User{}, err
	}
	return chat.User{ID: id, Name: a.Name}, nil
}

// account is the account of the Microsoft Entra object ID aad: the kept one, or one read from the rosters of the teams the receiver
// recorded (ResolveUser).
func (c *Client) account(ctx context.Context, aad string) (account, error) {
	if a, ok := c.names.get(aad); ok {
		return a, nil
	}
	teams := c.knownTeams()
	if len(teams) == 0 {
		return account{}, errors.New("teams: no team of the connection is known yet: no activity has come to the bot in this run")
	}
	ctx, cancel := context.WithTimeout(ctx, c.rosterTimeout)
	defer cancel()
	var first error
	pages := rosterMaxPages
	for _, t := range teams {
		a, found, err := c.findIn(ctx, &pages, t.ServiceURL, t.TeamID, aad)
		switch {
		case found:
			return a, nil
		case err != nil && first == nil:
			first = fmt.Errorf("teams: reading a team's roster: %w", err)
		}
	}
	return account{}, cmp.Or(first, errUnknownPerson)
}

// knownTeams are the teams of the channels that the receiver recorded in this run, one each, in the order of their IDs, with the
// service URL that its channels' records name, the least of them in byte order when they differ (DefaultServiceURL for none). The
// channels of one team are expected to name one service URL (the tenant's region; not measured), and the choice only has to be the
// same at every call, which the records, a map, do not give by their order.
func (c *Client) knownTeams() []Conversation {
	c.recv.recMu.Lock()
	byTeam := map[string]string{}
	for _, rec := range c.recv.recorded {
		if rec.TeamID == "" {
			continue
		}
		if cur, ok := byTeam[rec.TeamID]; !ok || cur == "" || (rec.ServiceURL != "" && rec.ServiceURL < cur) {
			byTeam[rec.TeamID] = rec.ServiceURL
		}
	}
	c.recv.recMu.Unlock()
	out := make([]Conversation, 0, len(byTeam))
	for team, service := range byTeam {
		out = append(out, Conversation{TeamID: team, ServiceURL: cmp.Or(service, DefaultServiceURL)})
	}
	slices.SortFunc(out, func(a, b Conversation) int { return strings.Compare(a.TeamID, b.TeamID) })
	return out
}

// mentionNames are the names of the people that o mentions, by Microsoft Entra object ID (personName): the decider of an ending, and the
// person of each mention of a message (chat.Mention), each looked up once. A person whose name cannot be had is left out, which the
// mention shows as someone. A user of another shape is no mention, and is not looked up.
func (c *Client) mentionNames(ctx context.Context, channel string, o chat.Outgoing) map[string]string {
	var people []string
	switch {
	case o.Ending != nil:
		people = append(people, o.Ending.By)
	case o.Message != nil:
		for _, p := range o.Message.Parts {
			if m, ok := p.(chat.Mention); ok {
				people = append(people, m.User)
			}
		}
	}
	var names map[string]string
	for _, aad := range people {
		if _, done := names[aad]; done || !entraID.MatchString(aad) {
			continue
		}
		if names == nil {
			names = map[string]string{}
		}
		names[aad] = c.personName(ctx, channel, aad)
	}
	return names
}

// personName is the name of the person aad, whom a message in channel mentions (the decider of a prompt's ending, or a chat.Mention): the
// kept account's, or the one in the channel's roster, read within rosterTimeout and rosterMaxPages; "" when it cannot be had, which the
// mention shows as someone, keeping it. A failed read is logged, and does not fail the post or the edit.
func (c *Client) personName(ctx context.Context, channel, aad string) string {
	if a, ok := c.names.get(aad); ok {
		return a.Name
	}
	var err error
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.ResolveUser")
	defer func() { c.finish(ctx, op, "ResolveUser", err) }()
	rctx, cancel := context.WithTimeout(ctx, c.rosterTimeout)
	defer cancel()
	pages := rosterMaxPages
	a, found, err := c.findIn(rctx, &pages, c.serviceURL(ctx, channel), channel, aad)
	switch {
	case err != nil:
		c.log.WarnContext(ctx, "teams: the name of a person a message mentions could not be read; the mention names them as someone",
			"error", err.Error())
		return ""
	case !found:
		c.log.DebugContext(ctx, "teams: the channel's roster does not list a person a message mentions; the mention names them as someone")
		return ""
	}
	return a.Name
}

// mentionName is name as a mention shows it in a card's TextBlock, which takes markdown: its letters, marks, digits and the
// punctuation of names (. - '), with every run of anything else (markup, controls, symbols, white space) one space between words, cut
// at nameUnits. So no name ends the mention's <at> tag, styles the line or makes a link. How Teams shows markdown or an entity inside
// a mention's tag was not measured, so nothing else is let through; "" when nothing is left.
func mentionName(name string) string {
	var b strings.Builder
	gap := false
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.Is(unicode.M, r) && r != '.' && r != '-' && r != '\'' {
			gap = b.Len() > 0
			continue
		}
		if gap {
			b.WriteByte(' ')
			gap = false
		}
		b.WriteRune(r)
	}
	return chat.Truncate(b.String(), nameUnits)
}

// nameCache keeps the accounts that roster reads found, by Microsoft Entra object ID, for namesTTL, at most namesMax of them. It is
// safe for concurrent use.
type nameCache struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[string]cachedAccount // guarded by mu
}

type cachedAccount struct {
	account
	fetched time.Time
}

func newNameCache(now func() time.Time) *nameCache {
	return &nameCache{now: now, m: map[string]cachedAccount{}}
}

// get is the account kept for aad, if it is younger than namesTTL.
func (n *nameCache) get(aad string) (account, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	e, ok := n.m[aad]
	if !ok || n.now().Sub(e.fetched) >= namesTTL {
		return account{}, false
	}
	return e.account, true
}

// put keeps a, making room first when namesMax are kept: the expired go, then any.
func (n *nameCache) put(a account) {
	if a.AADObjectID == "" {
		return
	}
	now := n.now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.m[a.AADObjectID]; !ok && len(n.m) >= namesMax {
		for k, e := range n.m {
			if now.Sub(e.fetched) >= namesTTL {
				delete(n.m, k)
			}
		}
		for k := range n.m {
			if len(n.m) < namesMax {
				break
			}
			delete(n.m, k)
		}
	}
	n.m[a.AADObjectID] = cachedAccount{a, now}
}

// len is how many accounts are kept, the expired among them.
func (n *nameCache) len() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.m)
}
