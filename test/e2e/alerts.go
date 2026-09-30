package e2e

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Alert is one alert for Alertmanager's API. Unset times are left out, so Alertmanager stamps
// StartsAt; an EndsAt in the past resolves the alert.
type Alert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations,omitempty"`
	StartsAt    time.Time         `json:"startsAt,omitzero"`
	EndsAt      time.Time         `json:"endsAt,omitzero"`
}

// Alertmanager posts alerts to Alertmanager's API, so they go through real grouping, routing,
// repeats and resolves (spec 12.1).
type Alertmanager struct {
	c      *client
	suffix string // per client: Name appends it
}

// NewAlertmanager returns a client for the Alertmanager at baseURL, with a new name suffix. Build
// one per scenario and use it for every alert of that scenario, the resolve included: a second
// client names the same base differently.
func NewAlertmanager(baseURL string) *Alertmanager {
	return &Alertmanager{newClient(baseURL), fmt.Sprintf("%x-%s", time.Now().Unix(), strings.ToLower(rand.Text()[:6]))}
}

// Name returns base made unique to this client (a time and a random string), for an alertname or
// any label. Alertmanager does not notify again for a firing alert it already sent, so a bare name
// would show nothing on a second run against the same cluster. A client always gives the same name
// for a base, so a resolve matches the alert that fired.
func (a *Alertmanager) Name(base string) string { return base + "-" + a.suffix }

// Post sends alerts with POST /api/v2/alerts.
func (a *Alertmanager) Post(ctx context.Context, alerts ...Alert) error {
	if len(alerts) == 0 {
		return errors.New("e2e: no alerts to post") // nil would encode as null, which Alertmanager refuses
	}
	return a.c.do(ctx, http.MethodPost, "/api/v2/alerts", alerts, nil)
}
