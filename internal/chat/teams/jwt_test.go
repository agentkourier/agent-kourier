package teams

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// TestVerifyRefusesEveryBadToken holds each check of the Bot Framework's inbound token to its own reason: a token that fails exactly
// one check is refused for that check, and one that passes them all is taken.
func TestVerifyRefusesEveryBadToken(t *testing.T) {
	key := newSigningKey(t, "teams-key", "msteams", "skype")
	other := newSigningKey(t, "outlook-key", "outlook", "skype")
	impostor := newSigningKey(t, "teams-key", "msteams")
	f := newFakeBotFramework(t, key, other)
	clock := newFakeClock()
	now := clock.now()

	with := func(edit func(c map[string]any)) map[string]any {
		c := claimsAt(now)
		edit(c)
		return c
	}
	header := func(alg string) map[string]any { return map[string]any{"alg": alg, "typ": "JWT", "kid": key.kid} }

	cases := []struct {
		name       string
		authz      string
		serviceURL string // the activity's; testService when ""
		want       Reason
	}{
		{name: "valid", authz: bearer(t, key, claimsAt(now))},
		{name: "aud as an array that holds the app", authz: bearer(t, key, with(func(c map[string]any) { c["aud"] = []string{"other", testAppID} }))},
		{name: "serviceUrl spelled as the docs spell it", authz: bearer(t, key, with(func(c map[string]any) {
			delete(c, "serviceurl")
			c["serviceUrl"] = testService
		}))},
		{name: "expired within the skew", authz: bearer(t, key, with(func(c map[string]any) { c["exp"] = now.Add(-4 * time.Minute).Unix() }))},
		{name: "nbf ahead within the skew", authz: bearer(t, key, with(func(c map[string]any) { c["nbf"] = now.Add(4 * time.Minute).Unix() }))},
		{name: "no nbf", authz: bearer(t, key, with(func(c map[string]any) { delete(c, "nbf") }))},

		{name: "wrong aud", authz: bearer(t, key, with(func(c map[string]any) { c["aud"] = "00000000-0000-4000-8000-00000000000a" })), want: ReasonAudience},
		{name: "aud array without the app", authz: bearer(t, key, with(func(c map[string]any) { c["aud"] = []string{"a", "b"} })), want: ReasonAudience},
		{name: "no aud", authz: bearer(t, key, with(func(c map[string]any) { delete(c, "aud") })), want: ReasonAudience},
		{name: "aud null", authz: bearer(t, key, with(func(c map[string]any) { c["aud"] = nil })), want: ReasonAudience},
		{name: "aud empty", authz: bearer(t, key, with(func(c map[string]any) { c["aud"] = "" })), want: ReasonAudience},
		{name: "wrong iss", authz: bearer(t, key, with(func(c map[string]any) { c["iss"] = "https://sts.windows.net/" + testTenant + "/" })), want: ReasonIssuer},
		{name: "expired beyond the skew", authz: bearer(t, key, with(func(c map[string]any) { c["exp"] = now.Add(-6 * time.Minute).Unix() })), want: ReasonExpired},
		{name: "no exp", authz: bearer(t, key, with(func(c map[string]any) { delete(c, "exp") })), want: ReasonMalformed},
		{name: "nbf in the future beyond the skew", authz: bearer(t, key, with(func(c map[string]any) { c["nbf"] = now.Add(6 * time.Minute).Unix() })), want: ReasonNotYetValid},
		{name: "key not endorsed for msteams", authz: bearer(t, other, claimsAt(now)), want: ReasonNotEndorsed},
		{name: "impostor key under a known kid", authz: bearer(t, impostor, claimsAt(now)), want: ReasonSignature},
		{name: "HS256", authz: "Bearer " + signToken(t, header("HS256"), claimsAt(now), key.priv), want: ReasonAlgorithm},
		{name: "alg none, unsigned", authz: "Bearer " + signToken(t, header("none"), claimsAt(now), nil), want: ReasonAlgorithm},
		{name: "RS256 with an empty signature", authz: "Bearer " + signToken(t, header("RS256"), claimsAt(now), nil), want: ReasonSignature},
		{name: "no kid", authz: "Bearer " + signToken(t, map[string]any{"alg": "RS256"}, claimsAt(now), key.priv), want: ReasonMalformed},
		{name: "missing Bearer", authz: "", want: ReasonNoBearer},
		{name: "another scheme", authz: "Basic dXNlcjpwYXNz", want: ReasonNoBearer},
		{name: "the token without its scheme", authz: bearer(t, key, claimsAt(now))[len("Bearer "):], want: ReasonNoBearer},
		{name: "Bearer and nothing", authz: "Bearer ", want: ReasonNoBearer},
		{name: "two segments", authz: "Bearer abc.def", want: ReasonMalformed},
		{name: "header not base64", authz: "Bearer !!!.e30.c2ln", want: ReasonMalformed},
		{name: "claims not JSON", authz: "Bearer eyJhbGciOiJSUzI1NiIsImtpZCI6InRlYW1zLWtleSJ9.bm90IGpzb24.c2ln", want: ReasonMalformed},
		{name: "serviceUrl of another activity", authz: bearer(t, key, claimsAt(now)), serviceURL: "https://smba.trafficmanager.net/emea/", want: ReasonServiceURL},
		{name: "serviceUrl differing only by its trailing slash", authz: bearer(t, key, claimsAt(now)), serviceURL: testService[:len(testService)-1], want: ReasonServiceURL},
		{name: "no serviceUrl claim", authz: bearer(t, key, with(func(c map[string]any) { delete(c, "serviceurl") })), want: ReasonServiceURL},
		{name: "unknown kid", authz: bearer(t, signingKey{kid: "nobody", priv: key.priv}, claimsAt(now)), want: ReasonUnknownKey},
	}
	v := newTestVerifier(f, clock)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := tc.serviceURL
			if svc == "" {
				svc = testService
			}
			if got := reasonOf(t, verifyActivity(context.Background(), v, tc.authz, svc)); got != tc.want {
				t.Errorf("reason %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerifyFetchesAnUnknownKidOnceThenWaits: a kid that is not in the cached keys makes one fetch, which finds a rotated key; another
// unknown kid within the refetch interval makes none and is refused; after the interval one is made again.
func TestVerifyFetchesAnUnknownKidOnceThenWaits(t *testing.T) {
	first := newSigningKey(t, "first", "msteams")
	rotated := newSigningKey(t, "rotated", "msteams")
	f := newFakeBotFramework(t, first)
	clock := newFakeClock()
	v := newTestVerifier(f, clock)
	ctx := context.Background()

	if err := verifyActivity(ctx, v, bearer(t, first, claimsAt(clock.now())), testService); err != nil {
		t.Fatalf("first key: %v", err)
	}
	if got := f.fetches(); got != 1 {
		t.Fatalf("fetches after the first token = %d, want 1", got)
	}
	f.publish(first, rotated)
	if err := verifyActivity(ctx, v, bearer(t, rotated, claimsAt(clock.now())), testService); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
	if got := f.fetches(); got != 2 {
		t.Fatalf("fetches after the rotated kid = %d, want 2", got)
	}
	forged := signingKey{kid: "forged", priv: first.priv}
	clock.advance(keysMinRefetch - time.Second)
	for range 3 {
		if got := reasonOf(t, verifyActivity(ctx, v, bearer(t, forged, claimsAt(clock.now())), testService)); got != ReasonUnknownKey {
			t.Fatalf("reason %q, want %q", got, ReasonUnknownKey)
		}
	}
	if got := f.fetches(); got != 2 {
		t.Fatalf("fetches for unknown kids within the interval = %d, want still 2", got)
	}
	if err := verifyActivity(ctx, v, bearer(t, first, claimsAt(clock.now())), testService); err != nil {
		t.Fatalf("a known kid during the wait: %v", err)
	}
	clock.advance(time.Second)
	if got := reasonOf(t, verifyActivity(ctx, v, bearer(t, forged, claimsAt(clock.now())), testService)); got != ReasonUnknownKey {
		t.Fatalf("reason %q, want %q", got, ReasonUnknownKey)
	}
	if got := f.fetches(); got != 3 {
		t.Fatalf("fetches once the interval passed = %d, want 3", got)
	}
}

// TestVerifyRefreshesAheadInTheBackground: keys older than keysRefreshAfter verify at once while a fetch runs in the background, and
// what it fetched is used after, so a rotated key needs no fetch of its own.
func TestVerifyRefreshesAheadInTheBackground(t *testing.T) {
	key := newSigningKey(t, "k", "msteams")
	rotated := newSigningKey(t, "rotated", "msteams")
	f := newFakeBotFramework(t, key)
	clock := newFakeClock()
	v := newTestVerifier(f, clock)
	ctx := context.Background()
	if err := verifyActivity(ctx, v, bearer(t, key, claimsAt(clock.now())), testService); err != nil {
		t.Fatal(err)
	}
	clock.advance(keysRefreshAfter - time.Second)
	if err := verifyActivity(ctx, v, bearer(t, key, claimsAt(clock.now())), testService); err != nil {
		t.Fatal(err)
	}
	if got := f.fetches(); got != 1 {
		t.Fatalf("fetches before the refresh age = %d, want 1", got)
	}
	f.publish(key, rotated)
	clock.advance(time.Second)
	if err := verifyActivity(ctx, v, bearer(t, key, claimsAt(clock.now())), testService); err != nil {
		t.Fatalf("at the refresh age, the cached key: %v", err)
	}
	testutil.Eventually(t, "the background refresh has the rotated key", func() bool {
		v.mu.Lock()
		defer v.mu.Unlock()
		_, ok := v.keys["rotated"]
		return ok
	})
	if err := verifyActivity(ctx, v, bearer(t, rotated, claimsAt(clock.now())), testService); err != nil {
		t.Fatalf("the rotated key: %v", err)
	}
	if got := f.fetches(); got != 2 {
		t.Fatalf("fetches = %d, want 2: the refresh, and no fetch for the rotated kid", got)
	}
}

// idle waits until no fetch of v is in flight.
func idle(t *testing.T, v *verifier) {
	t.Helper()
	testutil.Eventually(t, "no fetch is in flight", func() bool {
		v.mu.Lock()
		defer v.mu.Unlock()
		return v.fetching == nil
	})
}

// TestVerifyKeepsTheKeysWhileTheRefreshFails: keys past keysRefreshAfter and under keysMaxAge go on verifying while every refresh
// fails, and are refused, as keys_unavailable, once they are keysMaxAge old.
func TestVerifyKeepsTheKeysWhileTheRefreshFails(t *testing.T) {
	key := newSigningKey(t, "k", "msteams")
	f := newFakeBotFramework(t, key)
	clock := newFakeClock()
	v := newTestVerifier(f, clock)
	ctx := context.Background()
	if err := verifyActivity(ctx, v, bearer(t, key, claimsAt(clock.now())), testService); err != nil {
		t.Fatal(err)
	}
	f.setStatus(http.StatusServiceUnavailable)
	clock.advance(keysRefreshAfter)
	for _, step := range []time.Duration{0, time.Hour, 6 * time.Hour, keysMaxAge - keysRefreshAfter - 7*time.Hour - time.Second} {
		clock.advance(step)
		if err := verifyActivity(ctx, v, bearer(t, key, claimsAt(clock.now())), testService); err != nil {
			t.Fatalf("with the refresh failing, %s after the refresh age: %v", step, err)
		}
		idle(t, v)
	}
	if got := f.fetches(); got != 5 {
		t.Fatalf("fetches = %d, want 5: the first and a refresh at each step", got)
	}
	clock.advance(time.Second)
	if got := reasonOf(t, verifyActivity(ctx, v, bearer(t, key, claimsAt(clock.now())), testService)); got != ReasonKeysUnavailable {
		t.Fatalf("at keysMaxAge: reason %q, want %q", got, ReasonKeysUnavailable)
	}
}

// TestVerifyBacksOffAFailingRefresh: a background refresh that fails is not tried again by every token that comes, but only once its
// backoff has passed.
func TestVerifyBacksOffAFailingRefresh(t *testing.T) {
	key := newSigningKey(t, "k", "msteams")
	f := newFakeBotFramework(t, key)
	clock := newFakeClock()
	v := newTestVerifier(f, clock)
	ctx := context.Background()
	if err := verifyActivity(ctx, v, bearer(t, key, claimsAt(clock.now())), testService); err != nil {
		t.Fatal(err)
	}
	f.setStatus(http.StatusServiceUnavailable)
	clock.advance(keysRefreshAfter)
	for range 5 {
		if err := verifyActivity(ctx, v, bearer(t, key, claimsAt(clock.now())), testService); err != nil {
			t.Fatal(err)
		}
		idle(t, v)
	}
	if got := f.fetches(); got != 2 {
		t.Fatalf("fetches = %d, want 2: the first and one refresh, then none within keysRetryBase", got)
	}
	clock.advance(keysRetryBase)
	if err := verifyActivity(ctx, v, bearer(t, key, claimsAt(clock.now())), testService); err != nil {
		t.Fatal(err)
	}
	idle(t, v)
	if got := f.fetches(); got != 3 {
		t.Fatalf("fetches = %d after keysRetryBase, want 3", got)
	}
}

// TestVerifyRecoversFromAFailedStartWithinTheBackoff: a first fetch that fails refuses the token as keys_unavailable, no fetch is made
// within keysRetryBase, and the one after it succeeds: a blip at a cold start costs about a second, not keysMinRefetch.
func TestVerifyRecoversFromAFailedStartWithinTheBackoff(t *testing.T) {
	key := newSigningKey(t, "k", "msteams")
	f := newFakeBotFramework(t, key)
	f.setStatus(http.StatusInternalServerError)
	clock := newFakeClock()
	v := newTestVerifier(f, clock)
	ctx := context.Background()
	for range 2 {
		if got := reasonOf(t, verifyActivity(ctx, v, bearer(t, key, claimsAt(clock.now())), testService)); got != ReasonKeysUnavailable {
			t.Fatalf("reason %q, want %q", got, ReasonKeysUnavailable)
		}
	}
	if got := f.fetches(); got != 1 {
		t.Fatalf("fetches = %d, want 1", got)
	}
	f.setStatus(http.StatusOK)
	clock.advance(keysRetryBase)
	if err := verifyActivity(ctx, v, bearer(t, key, claimsAt(clock.now())), testService); err != nil {
		t.Fatalf("after keysRetryBase: %v", err)
	}
}

// TestVerifyBacksOffFailedFetches: the wait after failed fetches doubles with each failure in a row, up to keysMinRefetch.
func TestVerifyBacksOffFailedFetches(t *testing.T) {
	key := newSigningKey(t, "k", "msteams")
	f := newFakeBotFramework(t, key)
	f.setStatus(http.StatusInternalServerError)
	clock := newFakeClock()
	v := newTestVerifier(f, clock)
	ctx := context.Background()
	try := func() { _ = verifyActivity(ctx, v, bearer(t, key, claimsAt(clock.now())), testService) }
	try()
	fetches := 1
	for wait := keysRetryBase; ; wait = min(2*wait, keysMinRefetch) {
		clock.advance(wait - time.Millisecond)
		try()
		if got := f.fetches(); got != fetches {
			t.Fatalf("a fetch before the wait of %s after failure %d", wait, fetches)
		}
		clock.advance(time.Millisecond)
		try()
		fetches++
		if got := f.fetches(); got != fetches {
			t.Fatalf("no fetch once the wait of %s after failure %d passed", wait, fetches-1)
		}
		if wait == keysMinRefetch {
			break
		}
	}
}

// TestVerifyRefusesASmallKey: a published key smaller than minKeyBits is left out of the cache, so a token signed with it names an
// unknown kid.
func TestVerifyRefusesASmallKey(t *testing.T) {
	small := newSigningKeyOf(t, 1024, "small", "msteams")
	f := newFakeBotFramework(t, small, newSigningKey(t, "k", "msteams"))
	clock := newFakeClock()
	v := newTestVerifier(f, clock)
	if got := reasonOf(t, verifyActivity(context.Background(), v, bearer(t, small, claimsAt(clock.now())), testService)); got != ReasonUnknownKey {
		t.Fatalf("reason %q, want %q", got, ReasonUnknownKey)
	}
}

// TestVerifyRefusesAKeySetPastItsCap: a JWKS longer than keysMaxBody is not used, and the token is refused as keys_unavailable.
func TestVerifyRefusesAKeySetPastItsCap(t *testing.T) {
	key := newSigningKey(t, "k", "msteams")
	f := newFakeBotFramework(t, key)
	f.padding = keysMaxBody
	clock := newFakeClock()
	v := newTestVerifier(f, clock)
	if got := reasonOf(t, verifyActivity(context.Background(), v, bearer(t, key, claimsAt(clock.now())), testService)); got != ReasonKeysUnavailable {
		t.Fatalf("reason %q, want %q", got, ReasonKeysUnavailable)
	}
}

// TestVerifyMakesOneFetchForConcurrentTokens: tokens that arrive together while no keys are cached wait for one fetch.
func TestVerifyMakesOneFetchForConcurrentTokens(t *testing.T) {
	key := newSigningKey(t, "k", "msteams")
	f := newFakeBotFramework(t, key)
	clock := newFakeClock()
	v := newTestVerifier(f, clock)
	authz := bearer(t, key, claimsAt(clock.now()))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- verifyActivity(context.Background(), v, authz, testService) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent token: %v", err)
		}
	}
	if got := f.fetches(); got != 1 {
		t.Errorf("fetches = %d, want 1", got)
	}
}

// TestVerifyGivesUpWithTheCaller: a caller whose context ends while the keys are being fetched is refused at once, and the fetch goes
// on, so that it fills the cache for the next token.
func TestVerifyGivesUpWithTheCaller(t *testing.T) {
	key := newSigningKey(t, "k", "msteams")
	f := newFakeBotFramework(t, key)
	release, hit := make(chan struct{}), make(chan struct{}, 1)
	f.mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		hit <- struct{}{}
		<-release
		f.serveKeys(w, r)
	})
	f.mu.Lock()
	f.jwksPath = "/slow"
	f.mu.Unlock()
	clock := newFakeClock()
	v := newTestVerifier(f, clock)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- verifyActivity(ctx, v, bearer(t, key, claimsAt(clock.now())), testService) }()
	<-hit
	cancel()
	err := <-done
	if got := reasonOf(t, err); got != ReasonKeysUnavailable || !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v (reason %q), want keys_unavailable wrapping context.Canceled", err, got)
	}
	close(release)
	if err := verifyActivity(context.Background(), v, bearer(t, key, claimsAt(clock.now())), testService); err != nil {
		t.Fatalf("the next token, after the detached fetch: %v", err)
	}
	if got := f.fetches(); got != 1 {
		t.Errorf("fetches = %d, want 1: the next token waits for the detached fetch", got)
	}
}

// TestReasonSignedIsWhatTheSignatureVouchesFor: the reasons found only once the signature checks out are signed, and logged at WARN; the
// others, which anyone can cause, are not. The table holds every reason.
func TestReasonSignedIsWhatTheSignatureVouchesFor(t *testing.T) {
	for reason, want := range map[Reason]bool{
		ReasonNoBearer:        false,
		ReasonMalformed:       false,
		ReasonAlgorithm:       false,
		ReasonUnknownKey:      false,
		ReasonKeysUnavailable: false,
		ReasonSignature:       false,
		ReasonNotEndorsed:     true,
		ReasonIssuer:          true,
		ReasonAudience:        true,
		ReasonExpired:         true,
		ReasonNotYetValid:     true,
		ReasonServiceURL:      true,
	} {
		if got := reason.signed(); got != want {
			t.Errorf("%s: signed() = %t, want %t", reason, got, want)
		}
	}
}

// TestVerifyQuotesAtMostQuoteMax: what a refusal quotes of a token, which its sender chose, is cut to quoteMax bytes.
func TestVerifyQuotesAtMostQuoteMax(t *testing.T) {
	key := newSigningKey(t, "k", "msteams")
	f := newFakeBotFramework(t, key)
	clock := newFakeClock()
	v := newTestVerifier(f, clock)
	long := strings.Repeat("x", 4096)
	for name, authz := range map[string]string{
		"alg": "Bearer " + signToken(t, map[string]any{"alg": long, "kid": "k"}, claimsAt(clock.now()), key.priv),
		"kid": "Bearer " + signToken(t, map[string]any{"alg": "RS256", "kid": long}, claimsAt(clock.now()), key.priv),
	} {
		_, err := v.verify(t.Context(), authz)
		if err == nil {
			t.Fatalf("%s: a token was taken", name)
		}
		if msg := err.Error(); len(msg) > 2*quoteMax+100 || !strings.Contains(msg, strings.Repeat("x", quoteMax)+"...") {
			t.Errorf("%s: the refusal is %d bytes: %.200s", name, len(msg), msg)
		}
	}
	if err := (token{ServiceURL: testService}).checkServiceURL(testService + long); err == nil || len(err.Error()) > 3*quoteMax+100 {
		t.Errorf("service URL: the refusal is %v", err)
	}
	// Real service URLs that differ only at their end, by the trailing slash, are quoted whole, so that the line shows the difference.
	bare := strings.TrimSuffix(testService, "/")
	err := (token{ServiceURL: testService}).checkServiceURL(bare)
	if err == nil || !strings.Contains(err.Error(), strconv.Quote(testService)) || !strings.Contains(err.Error(), strconv.Quote(bare)) {
		t.Errorf("service URLs of %d bytes that differ at the end: the refusal is %v", len(testService), err)
	}
}
