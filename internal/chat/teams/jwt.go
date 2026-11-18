package teams

import (
	"bytes"
	"cmp"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/backoff"
)

// What the Bot Framework's inbound token is, is Microsoft's (bot-framework-rest-connector-authentication, read 2026-11-16, not
// verified here beyond the live run of 2026-11-18): a JWT signed RS256 with a key of the JWKS that the OpenID metadata names, whose
// endorsements list the channels the key may sign for; issuer https://api.botframework.com; audience the bot's app ID; a service URL
// claim equal to the activity's serviceUrl; a clock skew of 5 minutes; keys fetched again at least once a day; and no switch to turn
// any of it off. The checks below are all of them, in that order, and none can be turned off.
const (
	// DefaultMetadataURL is the Bot Framework's OpenID metadata, which names the JWKS.
	DefaultMetadataURL = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	// tokenIssuer is the iss of every token the Bot Framework sends a bot, single-tenant ones included.
	tokenIssuer = "https://api.botframework.com" //nolint:gosec // G101: an issuer URL, no credential
	// endorsedChannel is the channel a signing key must be endorsed for. Of the 228 keys published on 2026-11-17, 74 were.
	endorsedChannel = "msteams"
	// tokenSkew is how far exp and nbf may be from the broker's clock.
	tokenSkew = 5 * time.Minute
	// keysMaxAge is how long fetched keys are used: past it a token is refused until a fetch succeeds.
	keysMaxAge = 24 * time.Hour
	// keysRefreshAfter is the age at which the keys are fetched again ahead of keysMaxAge, in the background: the cached keys go on
	// verifying while the refresh runs, and while it fails, until they are keysMaxAge old.
	keysRefreshAfter = keysMaxAge / 2
	// keysCheckEvery is how often a serving receiver checks the keys' age (warm), so that a connection with no requests for a day
	// still has young keys when the next comes, and does not fetch them inside the chat listener's handler deadline.
	keysCheckEvery = time.Hour
	// keysRetryBase is the wait after a fetch that failed, doubled after each failure in a row up to keysMinRefetch, so that an outage
	// of the endpoint is not hammered and a blip at a cold start costs about a second.
	keysRetryBase = time.Second
	// keysMinRefetch spaces the fetches for a kid that young keys do not hold, so that a stream of forged kids cannot make the broker
	// hammer the endpoint, and caps the wait after failed fetches.
	keysMinRefetch = 5 * time.Minute
	// keysFetchTimeout bounds one fetch, the metadata and the JWKS together. The fetch does not end with the request that started it,
	// so a slow one still fills the cache for the requests after.
	keysFetchTimeout = 10 * time.Second
	// keysMaxBody caps the metadata and the JWKS that a fetch reads. The JWKS was 796 KB on 2026-11-18.
	keysMaxBody = 4 << 20
	// minKeyBits is the smallest RSA key the verifier takes; the Bot Framework's are 2048 bits.
	minKeyBits = 2048
	// quoteMax cuts what a refusal or a log line quotes of the token or the activity, which whoever sent the request chose (an alg, a
	// kid, an issuer, a service URL, a channel), so that a log line cannot be made long. It holds a real value whole: a service URL
	// was 74 bytes on 2026-11-18, and a channel ID the API takes is at most 140.
	quoteMax = 256
)

// quote is s cut to quoteMax bytes, marked with "..." where it was cut, for a refusal's message.
func quote(s string) string {
	if len(s) <= quoteMax {
		return s
	}
	return s[:quoteMax] + "..."
}

// Reason is why a token was refused: a closed set, so that it can label a metric.
type Reason string

// The reasons a token is refused.
const (
	ReasonNoBearer        Reason = "no_bearer"        // the Authorization header is not "Bearer <token>"
	ReasonMalformed       Reason = "malformed"        // not a compact JWS whose header and claims can be read, or no kid or no exp
	ReasonAlgorithm       Reason = "algorithm"        // alg is not RS256 (HS256 and none are refused)
	ReasonUnknownKey      Reason = "unknown_key"      // the kid is not among the published keys, after the one fetch it may make
	ReasonKeysUnavailable Reason = "keys_unavailable" // the keys could not be fetched, or are older than keysMaxAge
	ReasonSignature       Reason = "signature"        // the signature is not the key's
	ReasonNotEndorsed     Reason = "not_endorsed"     // the key is not endorsed for msteams
	ReasonIssuer          Reason = "issuer"           // iss is not https://api.botframework.com
	ReasonAudience        Reason = "audience"         // aud does not hold the app ID
	ReasonExpired         Reason = "expired"          // exp is further in the past than tokenSkew
	ReasonNotYetValid     Reason = "not_yet_valid"    // nbf is further in the future than tokenSkew
	ReasonServiceURL      Reason = "service_url"      // the service URL claim is missing, or is not the activity's serviceUrl
)

// signed says whether the reason is found only after the signature checks out: the token is the Bot Framework's, so its refusal points
// at the connection's config (the app ID) or the broker's clock rather than at whoever sent the request.
func (r Reason) signed() bool {
	switch r {
	case ReasonNotEndorsed, ReasonIssuer, ReasonAudience, ReasonExpired, ReasonNotYetValid, ReasonServiceURL:
		return true
	}
	return false
}

// TokenError is a refused token: Reason says which check refused it, and the message says what it found. It never quotes the token.
type TokenError struct {
	Reason Reason
	detail string
	err    error
}

// Error says why the token was refused.
func (e *TokenError) Error() string {
	if e.err != nil {
		return fmt.Sprintf("teams token refused (%s): %s: %v", e.Reason, e.detail, e.err)
	}
	return fmt.Sprintf("teams token refused (%s): %s", e.Reason, e.detail)
}

// Unwrap returns the error behind a keys_unavailable refusal, such as the fetch's, or nil.
func (e *TokenError) Unwrap() error { return e.err }

func refuse(reason Reason, format string, a ...any) *TokenError {
	return &TokenError{Reason: reason, detail: fmt.Sprintf(format, a...)}
}

// token is what a verified token says that the route checks against the activity.
type token struct {
	// ServiceURL is the token's service URL claim.
	ServiceURL string
}

// checkServiceURL refuses the token, with ReasonServiceURL, unless its service URL claim is the activity's serviceUrl, byte for byte
// (as the Bot Framework SDK compares them).
func (t token) checkServiceURL(activity string) error {
	if t.ServiceURL == "" {
		return refuse(ReasonServiceURL, "the token has no service URL claim")
	}
	if t.ServiceURL != activity {
		return refuse(ReasonServiceURL, "the token's service URL %q is not the activity's %q", quote(t.ServiceURL), quote(activity))
	}
	return nil
}

// verifier checks the bearer token of a request the Bot Framework sends a bot, with the keys it publishes, which it caches. The
// Receiver makes it from its own config, so that the audience is the bot the receiver serves. It is safe for concurrent use.
type verifier struct {
	appID       string
	metadataURL string
	client      *http.Client
	now         func() time.Time

	mu        sync.Mutex
	keys      map[string]publicKey // by kid; nil until a fetch succeeds
	fetchedAt time.Time            // when keys were fetched
	tried     time.Time            // when the last fetch began
	failures  int                  // the fetches that failed in a row since the last that succeeded
	triedErr  error                // the last fetch's error, nil if it succeeded
	forced    time.Time            // when the last fetch for a kid the young keys lacked began
	fetching  chan struct{}        // closed when the fetch in flight ends; nil when none is
}

// publicKey is one key of the JWKS.
type publicKey struct {
	pub      *rsa.PublicKey
	endorsed bool // its endorsements hold endorsedChannel
}

// newVerifier returns a verifier of the tokens for the bot appID, which reads the keys from the metadata at metadataURL
// (DefaultMetadataURL if empty) with client (http.DefaultClient if nil), on the clock now (time.Now if nil). It fetches nothing until
// warm or the first token.
func newVerifier(appID, metadataURL string, client *http.Client, now func() time.Time) *verifier {
	v := &verifier{appID: appID, metadataURL: cmp.Or(metadataURL, DefaultMetadataURL), client: client, now: now}
	if v.client == nil {
		v.client = http.DefaultClient
	}
	if v.now == nil {
		v.now = time.Now
	}
	return v
}

// header and claims are the parts of a token the verifier reads.
type (
	header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	claims struct {
		Iss             string          `json:"iss"`
		Aud             json.RawMessage `json:"aud"`
		Exp             *json.Number    `json:"exp"`
		Nbf             *json.Number    `json:"nbf"`
		ServiceURL      string          `json:"serviceUrl"`
		ServiceURLLower string          `json:"serviceurl"` // the spelling of the Bot Framework SDK, and of real tokens
	}
)

// verify checks authorization, the value of a request's Authorization header, and returns what the token says, or a *TokenError that
// names the check that refused it. It checks the algorithm, the key (fetching the keys when it must), the signature, the key's
// endorsement, the issuer, the audience and the times; the caller checks the service URL against the activity (token.checkServiceURL)
// once it has read it. It waits for a fetch of the keys only as long as ctx lasts.
func (v *verifier) verify(ctx context.Context, authorization string) (token, error) {
	raw, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok || raw == "" {
		return token{}, refuse(ReasonNoBearer, "the Authorization header holds no bearer token")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return token{}, refuse(ReasonMalformed, "the token has %d parts, not 3", len(parts))
	}
	var h header
	if err := decodeSegment(parts[0], &h); err != nil {
		return token{}, refuse(ReasonMalformed, "the header: %v", err)
	}
	if h.Alg != "RS256" {
		return token{}, refuse(ReasonAlgorithm, "alg %q is not RS256", quote(h.Alg))
	}
	if h.Kid == "" {
		return token{}, refuse(ReasonMalformed, "the header has no kid")
	}
	var c claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return token{}, refuse(ReasonMalformed, "the claims: %v", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return token{}, refuse(ReasonMalformed, "the signature is not base64url")
	}
	key, err := v.key(ctx, h.Kid)
	if err != nil {
		return token{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key.pub, crypto.SHA256, digest[:], sig) != nil {
		return token{}, refuse(ReasonSignature, "the signature is not that of the key %q", quote(h.Kid))
	}
	if !key.endorsed {
		return token{}, refuse(ReasonNotEndorsed, "the key %q is not endorsed for %s", quote(h.Kid), endorsedChannel)
	}
	if c.Iss != tokenIssuer {
		return token{}, refuse(ReasonIssuer, "iss %q is not %s", quote(c.Iss), tokenIssuer)
	}
	if !audienceHolds(c.Aud, v.appID) {
		return token{}, refuse(ReasonAudience, "aud does not name the app %s", v.appID)
	}
	now := v.now()
	exp, ok := unixTime(c.Exp)
	if !ok {
		return token{}, refuse(ReasonMalformed, "the token has no exp that is a number")
	}
	if now.After(exp.Add(tokenSkew)) {
		return token{}, refuse(ReasonExpired, "it expired at %s", exp.UTC().Format(time.RFC3339))
	}
	if c.Nbf != nil {
		nbf, ok := unixTime(c.Nbf)
		if !ok {
			return token{}, refuse(ReasonMalformed, "nbf is not a number")
		}
		if now.Add(tokenSkew).Before(nbf) {
			return token{}, refuse(ReasonNotYetValid, "it is not valid before %s", nbf.UTC().Format(time.RFC3339))
		}
	}
	return token{ServiceURL: cmp.Or(c.ServiceURLLower, c.ServiceURL)}, nil
}

// decodeSegment reads one base64url segment of a token as JSON into into.
func decodeSegment(seg string, into any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return errors.New("not base64url")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(into); err != nil {
		return errors.New("not a JSON object")
	}
	return nil
}

// audienceHolds says whether aud, a string or an array of strings, holds want.
func audienceHolds(aud json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(aud, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(aud, &many) == nil {
		return slices.Contains(many, want)
	}
	return false
}

// unixTime is the time of a NumericDate claim, and false when it is missing or no number.
func unixTime(n *json.Number) (time.Time, bool) {
	if n == nil {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

// key returns the published key kid, from the cached keys while they are younger than keysMaxAge.
//   - Keys older than keysRefreshAfter are fetched again in the background, and keep verifying meanwhile, so that the daily refresh is
//     not made in a request and an outage of the endpoint shorter than half a day refuses nothing.
//   - Keys that are missing (a cold start) or keysMaxAge old are fetched, and the token waits for the fetch as long as ctx lasts.
//   - A fetch that failed is tried again only after keysRetryBase, doubled for each failure in a row up to keysMinRefetch: until then
//     a token that needs keys is refused as keys_unavailable.
//   - A kid the young keys lack makes one fetch, and the next such kid another only keysMinRefetch later: until then it is refused as
//     unknown_key.
//
// A fetch in flight is waited for, not doubled, and runs on to its own bound when the caller gives up, so that it fills the cache for
// the next token.
func (v *verifier) key(ctx context.Context, kid string) (publicKey, error) {
	for waited := false; ; {
		v.mu.Lock()
		now := v.now()
		usable := v.keys != nil && now.Sub(v.fetchedAt) < keysMaxAge
		if k, ok := v.keys[kid]; ok && usable {
			v.refreshAheadLocked(ctx, now)
			v.mu.Unlock()
			return k, nil
		}
		if wait := v.fetching; wait != nil {
			v.mu.Unlock()
			select {
			case <-wait:
				waited = true
				continue
			case <-ctx.Done():
				return publicKey{}, &TokenError{Reason: ReasonKeysUnavailable, detail: "the keys are being fetched", err: ctx.Err()}
			}
		}
		switch {
		case usable && (waited || (!v.forced.IsZero() && now.Sub(v.forced) < keysMinRefetch)):
			v.mu.Unlock()
			return publicKey{}, refuse(ReasonUnknownKey, "the published keys hold no kid %q", quote(kid))
		case !usable && (waited || !v.retryDueLocked(now)):
			err := v.triedErr
			v.mu.Unlock()
			if err == nil {
				err = errors.New("the keys are older than a day")
			}
			return publicKey{}, &TokenError{Reason: ReasonKeysUnavailable, detail: "the keys could not be fetched", err: err}
		case usable:
			v.forced = now
		}
		v.startFetchLocked(ctx, now)
		v.mu.Unlock()
	}
}

// warm fetches the keys in the background if none are cached or they are older than keysRefreshAfter, and a fetch is due: so that
// the first token after a start finds them. It does not wait.
func (v *verifier) warm(ctx context.Context) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if v.keys == nil && v.fetching == nil && v.retryDueLocked(now) {
		v.startFetchLocked(ctx, now)
		return
	}
	v.refreshAheadLocked(ctx, now)
}

// refreshAheadLocked starts a background fetch when the keys are older than keysRefreshAfter, none is in flight, and one is due.
func (v *verifier) refreshAheadLocked(ctx context.Context, now time.Time) {
	if v.keys != nil && now.Sub(v.fetchedAt) >= keysRefreshAfter && v.fetching == nil && v.retryDueLocked(now) {
		v.startFetchLocked(ctx, now)
	}
}

// retryDueLocked says whether a fetch may be made now: always after one that succeeded, and after failures only once the wait
// (keysRetryBase doubled for each failure in a row, up to keysMinRefetch) has passed since the last began.
func (v *verifier) retryDueLocked(now time.Time) bool {
	return v.failures == 0 || now.Sub(v.tried) >= backoff.Doubling(keysRetryBase, keysMinRefetch, v.failures)
}

// startFetchLocked starts a fetch, detached from ctx's end (it keeps its values) and bounded by keysFetchTimeout.
func (v *verifier) startFetchLocked(ctx context.Context, now time.Time) {
	done := make(chan struct{})
	v.tried, v.fetching = now, done
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), keysFetchTimeout)
	go func() {
		defer cancel()
		v.fetch(fctx, done)
	}()
}

// fetch reads the metadata and the JWKS, puts the keys in the cache if it can, counts a failure if not, and closes done.
func (v *verifier) fetch(ctx context.Context, done chan struct{}) {
	keys, err := v.fetchKeys(ctx)
	v.mu.Lock()
	defer v.mu.Unlock()
	if err == nil {
		v.keys, v.fetchedAt, v.failures = keys, v.now(), 0
	} else {
		v.failures++
	}
	v.triedErr, v.fetching = err, nil
	close(done)
}

// jwk is one key of the Bot Framework's JWKS. Endorsements is the Bot Framework's addition: the channels the key may sign for.
type jwk struct {
	Kty          string   `json:"kty"`
	Kid          string   `json:"kid"`
	N            string   `json:"n"`
	E            string   `json:"e"`
	Endorsements []string `json:"endorsements"`
}

func (v *verifier) fetchKeys(ctx context.Context) (map[string]publicKey, error) {
	var meta struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := v.getJSON(ctx, v.metadataURL, &meta); err != nil {
		return nil, fmt.Errorf("teams openid metadata: %w", err)
	}
	if meta.JWKSURI == "" {
		return nil, errors.New("teams openid metadata names no jwks_uri")
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := v.getJSON(ctx, meta.JWKSURI, &set); err != nil {
		return nil, fmt.Errorf("teams signing keys: %w", err)
	}
	keys := make(map[string]publicKey, len(set.Keys))
	for _, k := range set.Keys {
		pub, err := rsaKey(k)
		if err != nil || k.Kid == "" {
			continue // a key the verifier cannot use is one no token can name
		}
		keys[k.Kid] = publicKey{pub: pub, endorsed: slices.Contains(k.Endorsements, endorsedChannel)}
	}
	return keys, nil
}

// rsaKey is the RSA public key of k, or an error for a key that is not RSA, cannot be read, or is smaller than minKeyBits.
func rsaKey(k jwk) (*rsa.PublicKey, error) {
	if k.Kty != "RSA" {
		return nil, fmt.Errorf("kty %q", k.Kty)
	}
	n, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, errors.New("n is not base64url")
	}
	e, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, errors.New("e is not base64url")
	}
	ei := new(big.Int).SetBytes(e)
	if !ei.IsInt64() || ei.Int64() < 3 || ei.Int64() > 1<<31-1 {
		return nil, errors.New("e is out of range")
	}
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(ei.Int64())}
	if pub.N.BitLen() < minKeyBits {
		return nil, fmt.Errorf("a %d-bit key", pub.N.BitLen())
	}
	return pub, nil
}

// getJSON reads the JSON at u into into, refusing a status other than 200 and a body past keysMaxBody.
func (v *verifier) getJSON(ctx context.Context, u string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, keysMaxBody+1))
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if len(b) > keysMaxBody {
		return fmt.Errorf("the body is longer than %d bytes", keysMaxBody)
	}
	if err := json.Unmarshal(b, into); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
