package teams

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/secret"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// TestLimitsPage holds the page docs/reference/limits.md to the bounds of the Teams receiver and of what the Client sends.
func TestLimitsPage(t *testing.T) {
	// The waits that a Receiver keeps in fields, which a test may shorten, are read from one NewReceiver built.
	r, err := NewReceiver(ReceiverConfig{Connection: testConn, TenantID: testTenant, AppID: testAppID})
	if err != nil {
		t.Fatal(err)
	}
	limitsdoc.Duration(t, "teamsTokenSkew", tokenSkew)
	limitsdoc.Duration(t, "teamsKeysMaxAge", keysMaxAge)
	limitsdoc.Duration(t, "teamsKeysRefresh", keysRefreshAfter)
	limitsdoc.Duration(t, "teamsKeysCheck", r.keysCheckEvery)
	limitsdoc.Duration(t, "teamsInvokeDeadline", r.HandlerTimeout())
	limitsdoc.Duration(t, "teamsVerifyWait", r.verifyWait)
	limitsdoc.Duration(t, "teamsKeysRetry", keysRetryBase)
	limitsdoc.Duration(t, "teamsKeysRefetch", keysMinRefetch)
	limitsdoc.Duration(t, "teamsKeysFetch", keysFetchTimeout)
	limitsdoc.Bytes(t, "teamsKeysBody", keysMaxBody)
	limitsdoc.Duration(t, "teamsDedupWindow", dedupWindow)
	limitsdoc.Count(t, "teamsDedupMax", dedupMax)
	limitsdoc.Duration(t, "teamsStopDrain", stopDrain)
	limitsdoc.Count(t, "teamsSinkAttempts", sinkAttempts)
	limitsdoc.Duration(t, "teamsSinkRetry", r.sinkRetryBase)
	limitsdoc.Duration(t, "teamsSinkRetryMax", sinkRetryMax)
	limitsdoc.Bytes(t, "teamsActivityID", maxActivityID)
	limitsdoc.Duration(t, "teamsRefusalLog", refusalLogEvery)
	limitsdoc.Bytes(t, "teamsQuote", quoteMax)
	limitsdoc.Count(t, "teamsMinKeyBits", minKeyBits)
	limitsdoc.Text(t, "teamsIDSeparator", idSeparator)
	if id := chat.MessageID(chat.Ref{Platform: "p", Conn: "c", Channel: "h", ID: "i"}); id != strings.Join([]string{"p", "c", "h", "i"}, idSeparator) {
		t.Errorf("chat.MessageID gives %q: idSeparator is not what it joins on", id)
	}

	limitsdoc.Duration(t, "teamsTokenRefresh", tokenRefreshMargin)
	limitsdoc.Duration(t, "teamsTokenStale", tokenStaleMargin)
	limitsdoc.Duration(t, "teamsTokenFetch", tokenFetchTimeout)
	limitsdoc.Duration(t, "teamsTokenRetry", tokenRetryWait)
	limitsdoc.Bytes(t, "teamsTokenBody", tokenMaxBody)
	limitsdoc.Bytes(t, "teamsConnectorBody", connectorMaxBody)
	limitsdoc.Bytes(t, "teamsMessageBytes", maxMessageBytes)
	limitsdoc.Count(t, "teamsTextUnits", maxTextUnits)
	limitsdoc.Count(t, "teamsBodyUnits", maxBodyUnits)
	limitsdoc.Count(t, "teamsMinBodyUnits", minBodyUnits)
	limitsdoc.Count(t, "teamsButtonCards", maxButtonCards)
	limitsdoc.Duration(t, "teamsButtonsWaitMax", buttonsWaitMax)
	limitsdoc.Duration(t, "teamsButtonsRetry", buttonsRetryWait)

	// The reads of people: the bound a Client is built with, the page it asks for, and the names it keeps; and the wait of a press for
	// its decision, which a Client is built with too.
	c, err := New(Config{Connection: testConn, TenantID: testTenant, AppID: testAppID, ClientSecret: secret.New(testClientSecret)})
	if err != nil {
		t.Fatal(err)
	}
	limitsdoc.Duration(t, "teamsRosterTimeout", c.rosterTimeout)
	limitsdoc.Duration(t, "teamsPressWait", c.pressWait)
	limitsdoc.Count(t, "teamsRosterPage", rosterPage)
	limitsdoc.Count(t, "teamsRosterMaxPages", rosterMaxPages)
	limitsdoc.Count(t, "teamsNamesMax", namesMax)
	limitsdoc.Duration(t, "teamsNamesTTL", namesTTL)
	limitsdoc.Count(t, "teamsNameUnits", nameUnits)

	// The reads of a thread through Microsoft Graph, and Find's of a channel: the bound a Client is built with, and the pages, answers and
	// cards it reads.
	limitsdoc.Duration(t, "teamsThreadTimeout", c.threadTimeout)
	limitsdoc.Count(t, "teamsGraphTop", graphPageTop)
	limitsdoc.Count(t, "teamsThreadPages", threadMaxPages)
	limitsdoc.Count(t, "teamsFindRootPages", findRootPages)
	limitsdoc.Bytes(t, "teamsGraphBody", graphMaxBody)
	limitsdoc.Count(t, "teamsCardDepth", cardMaxDepth)
	limitsdoc.Bytes(t, "teamsGraphText", graphTextMax)

	// The Graph intake of a connection with a notificationURL: the bounds a Client is built with, read from one New built, and the
	// constants it reads.
	nc, err := New(Config{Connection: testConn, TenantID: testTenant, AppID: testAppID, ClientSecret: secret.New(testClientSecret),
		NotificationURL: "https://kourier.example.test/chat/teams/" + testConn})
	if err != nil {
		t.Fatal(err)
	}
	limitsdoc.Duration(t, "teamsSubscriptionLifetime", nc.push.subs.lifetime)
	limitsdoc.Duration(t, "teamsSubscriptionRenewLead", nc.push.subs.renewLead)
	limitsdoc.Duration(t, "teamsSubscriptionCall", nc.push.subs.callTimeout)
	limitsdoc.Duration(t, "teamsSubscriptionReconcile", nc.push.subs.every)
	limitsdoc.Count(t, "teamsSubscriptionListPages", subscriptionListPages)
	limitsdoc.Duration(t, "teamsSubscriptionConflictRetry", nc.push.subs.conflictRetry)
	limitsdoc.Bytes(t, "teamsNotificationBody", notificationMaxBody)
	limitsdoc.Count(t, "teamsNotificationItems", notificationMaxItems)
	limitsdoc.Duration(t, "teamsFetchTimeout", nc.push.fetchTimeout)
	limitsdoc.Count(t, "teamsFetchAttempts", fetchAttempts)
	limitsdoc.Duration(t, "teamsFetchRetry", nc.push.fetchRetry)
	limitsdoc.Duration(t, "teamsFetchRetryMax", nc.push.fetchRetryMax)

	// Socket mode: the waits that a socket-mode Client keeps in fields, which a test may shorten, are read from one New built, and the
	// rest from the constants it reads.
	sc, err := New(Config{Connection: testConn, TenantID: testTenant, AppID: testAppID, ClientSecret: secret.New(testClientSecret), Socket: true})
	if err != nil {
		t.Fatal(err)
	}
	s := sc.sockets
	limitsdoc.Text(t, "teamsSocketRegions", strings.Join(s.regions, ", "))
	limitsdoc.Count(t, "teamsSocketRedirects", socketMaxRedirects)
	limitsdoc.Bytes(t, "teamsSocketBody", socketMaxBody)
	limitsdoc.Duration(t, "teamsSocketReadyWait", s.readyWait)
	limitsdoc.Duration(t, "teamsSocketPing", s.pingEvery)
	limitsdoc.Duration(t, "teamsSocketServerTimeout", s.serverTimeout)
	limitsdoc.Duration(t, "teamsSocketWriteWait", socketWriteWait)
	limitsdoc.Bytes(t, "teamsSocketFrame", socketMaxFrame)
	limitsdoc.Duration(t, "teamsSocketReplaceLead", socketReplaceLead)
	limitsdoc.Duration(t, "teamsSocketRetireAfter", s.retireAfter)
	limitsdoc.Duration(t, "teamsSocketRetireMax", s.retireMax)
	limitsdoc.Duration(t, "teamsSocketStopWait", s.stopWait)
	limitsdoc.Duration(t, "teamsSocketRetryBase", s.retryBase)
	limitsdoc.Duration(t, "teamsSocketRetryMax", s.retryMax)
	limitsdoc.Duration(t, "teamsSocketReconnectNow", s.reconnectNow)
	limitsdoc.Duration(t, "teamsSocketRetryAfterMax", socketRetryAfterMax)
	limitsdoc.Duration(t, "teamsSocketRefusedRetry", s.refusedRetry)
	limitsdoc.Count(t, "teamsSocketInFlight", cap(s.slots))
	limitsdoc.Duration(t, "teamsSocketDeadline", socketDefaultDeadline)
	limitsdoc.Duration(t, "teamsSocketReplyMargin", socketReplyMargin)
	limitsdoc.Duration(t, "teamsInvokeDeadline", s.invokeWait) // a press over a socket is answered by the deadline it has over https
}
