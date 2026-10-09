package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// digestStore is what the digest loop needs of the store of trigger starts: *sqlite.Store.
type digestStore interface {
	ListTriggerDigests(ctx context.Context, now time.Time) ([]store.TriggerDigest, error)
	MarkTriggerDigested(ctx context.Context, binding string, trigger int, start time.Time) error
	PurgeTriggerRuns(ctx context.Context, before time.Time) (int, error)
}

// digestQueue is what the digest loop needs of the outbox: *outbox.Outbox. Enqueue is a local write; the outbox's
// own loop posts the row.
type digestQueue interface {
	Enqueue(ctx context.Context, p outbox.Post) (string, error)
}

// triggerDigestConfig configures a triggerDigest. Store and Queue are required.
type triggerDigestConfig struct {
	Store digestStore
	Queue digestQueue
	// Interval is how often windows are looked at (default 15 seconds). A line is posted within one Interval of
	// its window ending.
	Interval time.Duration
	Now      func() time.Time // time.Now if nil
	Logger   *slog.Logger     // slog.Default() if nil
}

// purgeEvery is how often the loop deletes the runs that are older than any limit's window.
const purgeEvery = time.Hour

// triggerDigest posts the digest line of the starts that a trigger's limits stopped (spec 8.1 step 4, D26): once for each
// window of each trigger, one line at the top level of the Binding's channel, through the outbox. A storm is not a
// message for each alert; the alerts have reached people through their own receiver already.
//
// A window opens with the first stopped start after the last digest and lasts the trigger's rate window, and the
// line is posted when it has ended, so that it can say how many were not investigated. It is idempotent: the row it
// asks the outbox for is named after the window, so asking again, as a pass does when the mark that the window was
// digested failed or a crash came between the two, is the same row and one post. The line names no alert.
type triggerDigest struct {
	cfg       triggerDigestConfig
	lastPurge time.Time
}

// newTriggerDigest returns a triggerDigest over cfg.
func newTriggerDigest(cfg triggerDigestConfig) (*triggerDigest, error) {
	if cfg.Store == nil || cfg.Queue == nil {
		return nil, errors.New("broker: a Store and a Queue are required to post trigger digests")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 15 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &triggerDigest{cfg: cfg}, nil
}

// Run passes every Interval, and not before the first, until ctx ends, and returns its error. Call it once.
func (d *triggerDigest) Run(ctx context.Context) error {
	tick := time.NewTicker(d.cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		d.pass(ctx)
	}
}

// pass queues the digest of every window that has ended and marks it, then deletes the runs older than a day, at most
// once an hour. The digests come first, so a window that ended unseen is posted in the pass that purges its rows.
// Whatever fails is logged and tried again at the next pass.
func (d *triggerDigest) pass(ctx context.Context) {
	now := d.cfg.Now()
	digests, err := d.cfg.Store.ListTriggerDigests(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			d.cfg.Logger.WarnContext(ctx, "triggers: the digests due could not be listed; it will be tried again", "err", err)
		}
		return
	}
	for _, dg := range digests {
		d.post(ctx, dg)
	}
	if now.Sub(d.lastPurge) >= purgeEvery {
		switch n, err := d.cfg.Store.PurgeTriggerRuns(ctx, now.Add(-v1alpha1.MaxLimitWindow)); {
		case err != nil:
			if ctx.Err() == nil {
				d.cfg.Logger.WarnContext(ctx, "triggers: old runs could not be purged; it will be tried again", "err", err)
			}
		default:
			d.lastPurge = now
			if n > 0 {
				d.cfg.Logger.InfoContext(ctx, "triggers: purged runs older than any limit's window", "purged", n)
			}
		}
	}
}

// post queues the line of one window and marks the window digested. The source of the row names the connection,
// Binding, trigger and the start of the window, and nothing of any alert.
func (d *triggerDigest) post(ctx context.Context, dg store.TriggerDigest) {
	source := strings.Join([]string{"trigger-digest", dg.Connection, dg.Binding, strconv.Itoa(dg.Trigger), strconv.FormatInt(dg.Start.UnixNano(), 10)}, "/")
	_, err := d.cfg.Queue.Enqueue(ctx, outbox.Post{
		Source: source, Role: store.OutboxDigest, Channel: dg.Channel,
		Out: chat.Say(chat.Message{Fallback: digestText(dg)}),
	})
	if err != nil {
		d.cfg.Logger.WarnContext(ctx, "triggers: a digest line could not be queued; it will be tried again", "binding", dg.Binding, "trigger", dg.Trigger, "err", err)
		return
	}
	if err := d.cfg.Store.MarkTriggerDigested(ctx, dg.Binding, dg.Trigger, dg.Start); err != nil {
		d.cfg.Logger.WarnContext(ctx, "triggers: the digest of a window could not be recorded as queued; it will be queued again, as the same row", "binding", dg.Binding, "trigger", dg.Trigger, "err", err)
		return
	}
	d.cfg.Logger.InfoContext(ctx, "triggers: queued the digest line of a window of alerts that limits stopped", "binding", dg.Binding, "trigger", dg.Trigger, "channel", dg.Channel, "window", dg.End.Sub(dg.Start))
}

// digestText is the line: how many alerts were not investigated, in how long, and which limit stopped them
// ("7 alert messages not investigated in the last 10m (rate limit)."). When more than one limit did, each is counted. It is
// Agent Kourier's own wording and numbers, and names no alert.
func digestText(dg store.TriggerDigest) string {
	reasons := []struct {
		limit store.TriggerLimit
		name  string
	}{{store.LimitRate, "rate limit"}, {store.LimitConcurrent, "concurrency limit"}, {store.LimitDaily, "daily cap"}}
	total := 0
	var names, counted []string
	for _, r := range reasons {
		if n := dg.Counts[r.limit]; n > 0 {
			total += n
			names, counted = append(names, r.name), append(counted, fmt.Sprintf("%s: %d", r.name, n))
		}
	}
	why := strings.Join(names, "")
	if len(names) > 1 {
		why = strings.Join(counted, ", ")
	}
	noun := "alert messages"
	if total == 1 {
		noun = "alert message"
	}
	return fmt.Sprintf("%d %s not investigated in the last %s (%s).", total, noun, compactDuration(dg.End.Sub(dg.Start)), why)
}

// compactDuration is d without the zero units a Duration prints: 10m, 1h30m, 24h.
func compactDuration(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
