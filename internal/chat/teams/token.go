package teams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/secret"
)

// botFrameworkScope is the scope of the token that the Bot Connector takes: the bot's own token for the Bot Framework.
const botFrameworkScope = "https://api.botframework.com/.default"

// tokenRefreshMargin is how long before a token expires the next is fetched, in the background, while callers go on using the one
// they have; tokenStaleMargin is how long before it expires a token is no longer used, and a caller waits for a new one instead.
// Microsoft Entra issued tokens of 3599 s on every fetch measured (16 in November 2026), which took from 125 ms to 2.5 s. A token
// whose life is less than twice tokenRefreshMargin (only a fake's is) is refreshed at half its life, and one whose life is less than
// four times tokenStaleMargin is stale at three quarters of it (fetch).
const (
	tokenRefreshMargin = 5 * time.Minute
	tokenStaleMargin   = time.Minute
)

// tokenFetchTimeout bounds one fetch of a token, whatever the context of the caller that began it: a fetch is shared by every
// caller that waits for it, so none of their contexts can end it.
const tokenFetchTimeout = 15 * time.Second

// tokenRetryWait is how long a fetch that failed holds back the next: a caller with no usable token is given that failure again
// until it has passed, and a refresh of a token that is still usable is not tried again before it.
const tokenRetryWait = 5 * time.Second

// tokenMaxBody is the most of Microsoft Entra's answer that is read. A token answer is under 4 KiB.
const tokenMaxBody = 64 << 10

// DefaultTokenURL is Microsoft Entra's token endpoint for tenant, which issues the bot's token by the client credentials grant.
func DefaultTokenURL(tenant string) string {
	return "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0/token"
}

// TokenRequestError is Microsoft Entra's refusal of a token: its HTTP status, its OAuth error code (invalid_client, say) and the
// first of its AADSTS error codes, if it gave them. It holds neither the secret nor a token, and not Entra's description, which
// may repeat what the request held.
type TokenRequestError struct {
	// Status is the HTTP status of the answer.
	Status int
	// Code is the OAuth 2.0 error code, "other" for one of another form, or "".
	Code string
	// AADSTS is the first of Entra's numeric error codes, or 0.
	AADSTS int
	// RetryAfter is the wait that the answer asked for, or 0.
	RetryAfter time.Duration
}

func (e *TokenRequestError) Error() string {
	s := fmt.Sprintf("teams token: Microsoft Entra answered %d", e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.AADSTS != 0 {
		s += fmt.Sprintf(" (AADSTS%d)", e.AADSTS)
	}
	return s
}

// rejected says whether Entra refused the bot's credentials or its tenant: a 400 or a 401 to the client credentials grant, which
// names an app, a secret, a tenant or a scope that is wrong, and which no retry mends.
func (e *TokenRequestError) rejected() bool {
	return e.Status == http.StatusBadRequest || e.Status == http.StatusUnauthorized
}

// tokenSource gets the bot's token for the Bot Connector from Microsoft Entra, by the client credentials grant, and keeps it until
// it is near its expiry. It is safe for concurrent use: callers that need a token while none is usable wait for one fetch, which a
// caller's context does not end; a caller that has a usable token is never made to wait, and the token is fetched again in the
// background before it is stale.
type tokenSource struct {
	url      string
	clientID string
	secret   secret.Value
	client   *http.Client
	now      func() time.Time

	mu       sync.Mutex
	tok      secret.Value // guarded by mu: the last token fetched, or empty
	refresh  time.Time    // guarded by mu: when the next fetch begins
	stale    time.Time    // guarded by mu: when tok is no longer used
	inflight *tokenFetch  // guarded by mu: the fetch under way, or nil
	failed   error        // guarded by mu: the error of the last fetch, if it failed
	retry    time.Time    // guarded by mu: when a fetch may follow the one that failed
}

// tokenFetch is one fetch, which every caller that waits for a token shares.
type tokenFetch struct {
	done chan struct{}
	tok  secret.Value
	err  error
}

// newTokenSource returns the token source of the app clientID, whose secret is clientSecret, at Entra's endpoint tokenURL. The
// client must not follow redirects (noRedirects), so that the secret goes to tokenURL alone.
func newTokenSource(tokenURL, clientID string, clientSecret secret.Value, client *http.Client, now func() time.Time) *tokenSource {
	return &tokenSource{url: tokenURL, clientID: clientID, secret: clientSecret, client: client, now: now}
}

// token returns a usable token: the one kept, if it is not stale, and otherwise the result of a fetch, which it waits for until ctx
// ends. A kept token past its refresh time starts a fetch in the background and is returned at once.
func (s *tokenSource) token(ctx context.Context) (secret.Value, error) {
	s.mu.Lock()
	now := s.now()
	usable := !s.tok.IsEmpty() && now.Before(s.stale)
	if usable && now.Before(s.refresh) {
		tok := s.tok
		s.mu.Unlock()
		return tok, nil
	}
	if s.inflight == nil && now.Before(s.retry) {
		tok, err := s.tok, s.failed
		s.mu.Unlock()
		if usable {
			return tok, nil
		}
		return secret.Value{}, fmt.Errorf("teams token: the last fetch failed, and the next waits: %w", err)
	}
	f := s.inflight
	if f == nil {
		f = &tokenFetch{done: make(chan struct{})}
		s.inflight = f
		go s.fetch(context.WithoutCancel(ctx), f)
	}
	tok := s.tok
	s.mu.Unlock()
	if usable {
		return tok, nil
	}
	select {
	case <-f.done:
		return f.tok, f.err
	case <-ctx.Done():
		return secret.Value{}, fmt.Errorf("teams token: %w", ctx.Err())
	}
}

// fetch runs f, bounded by tokenFetchTimeout, and keeps what it got.
func (s *tokenSource) fetch(ctx context.Context, f *tokenFetch) {
	ctx, cancel := context.WithTimeout(ctx, tokenFetchTimeout)
	defer cancel()
	start := s.now()
	tok, life, err := s.request(ctx)
	s.mu.Lock()
	s.inflight = nil
	if err != nil {
		s.failed, s.retry = err, s.now().Add(tokenRetryWait)
	} else {
		// The life is counted from before the request, so that the token is never taken for younger than it is.
		refresh, stale := min(tokenRefreshMargin, life/2), min(tokenStaleMargin, life/4)
		s.tok, s.refresh, s.stale = tok, start.Add(life-refresh), start.Add(life-stale)
		s.failed, s.retry = nil, time.Time{}
	}
	s.mu.Unlock()
	f.tok, f.err = tok, err
	close(f.done)
}

// tokenAnswer is the part of Entra's answer that is read: a token and its life, or an error.
type tokenAnswer struct {
	TokenType   string `json:"token_type"`
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
	Error       string `json:"error"`
	ErrorCodes  []int  `json:"error_codes"`
}

// request asks Entra for a token once.
func (s *tokenSource) request(ctx context.Context) (secret.Value, time.Duration, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {s.clientID},
		"client_secret": {s.secret.Reveal()},
		"scope":         {botFrameworkScope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, strings.NewReader(form.Encode()))
	if err != nil {
		return secret.Value{}, 0, fmt.Errorf("teams token: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return secret.Value{}, 0, fmt.Errorf("teams token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, tokenMaxBody+1))
	if err != nil {
		return secret.Value{}, 0, fmt.Errorf("teams token: reading the answer: %w", err)
	}
	if len(body) > tokenMaxBody {
		return secret.Value{}, 0, fmt.Errorf("teams token: the answer is longer than %d bytes", tokenMaxBody)
	}
	var a tokenAnswer
	jsonErr := json.Unmarshal(body, &a)
	if resp.StatusCode != http.StatusOK {
		e := &TokenRequestError{Status: resp.StatusCode, RetryAfter: retryAfter(resp.Header.Get("Retry-After"), s.now())}
		if jsonErr == nil {
			if a.Error != "" {
				e.Code = codeOf(a.Error)
			}
			if len(a.ErrorCodes) > 0 {
				e.AADSTS = a.ErrorCodes[0]
			}
		}
		return secret.Value{}, 0, e
	}
	switch {
	case jsonErr != nil:
		return secret.Value{}, 0, fmt.Errorf("teams token: the answer is not JSON: %w", jsonErr)
	case a.AccessToken == "":
		return secret.Value{}, 0, errors.New("teams token: the answer holds no token")
	case !strings.EqualFold(a.TokenType, "Bearer"):
		return secret.Value{}, 0, fmt.Errorf("teams token: the token's type is %q, want Bearer", codeOf(a.TokenType))
	case a.ExpiresIn <= 0:
		return secret.Value{}, 0, fmt.Errorf("teams token: the token's life is %d seconds", a.ExpiresIn)
	}
	return secret.New(a.AccessToken), time.Duration(a.ExpiresIn) * time.Second, nil
}

// codeOf is s if it has the form of an error code (letters, digits and "_", at most 64 of them), and "other" if not, so that an error
// never carries text the server chose freely.
func codeOf(s string) string {
	if s == "" || len(s) > 64 || strings.IndexFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_'
	}) >= 0 {
		return "other"
	}
	return s
}

// retryAfter reads a Retry-After header, in seconds or as an HTTP date, as a wait from now; 0 if there is none or it says none.
func retryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		return max(time.Duration(n)*time.Second, 0)
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0)
	}
	return 0
}
