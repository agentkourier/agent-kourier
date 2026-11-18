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
}
