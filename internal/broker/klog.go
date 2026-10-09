package broker

import (
	"context"
	"log/slog"
	"strconv"

	"k8s.io/klog/v2"
)

// maxKlogVerbosity is the highest klog verbosity Agent Kourier allows. client-go logs request and
// response bodies, Secret lists included, at verbosity 8 and above, so nothing at 8 may ever be
// enabled, whatever is asked for.
const maxKlogVerbosity = 7

// RouteKlog sends klog's output, which is client-go's, to logger, and sets klog's verbosity to v,
// held between 0 and maxKlogVerbosity. It changes klog's process-wide state: call it once at
// startup, before anything logs.
//
// The cap is in two places, because klog has two ways to ask whether a level is on. The
// package-level calls (klog.V) follow the verbosity set here. client-go asks the logger instead,
// klog.FromContext(ctx).V(8).Enabled(), and with an slog logger set that goes to the handler as
// level -8 and ignores the verbosity: so the handler klog gets refuses anything below
// -maxKlogVerbosity, however verbose the caller's own handler is.
func RouteKlog(logger *slog.Logger, v int) error {
	klog.SetSlogLogger(slog.New(cappedHandler{logger.Handler()}))
	var level klog.Level
	// Set changes the process-wide verbosity, whatever the receiver is; the zero Level only gives
	// it a method to be called on.
	return level.Set(strconv.Itoa(min(max(v, 0), maxKlogVerbosity)))
}

// cappedHandler drops every record below slog.Level(-maxKlogVerbosity), which is klog's
// verbosity above maxKlogVerbosity. klog logs V(n) at slog level -n.
type cappedHandler struct{ slog.Handler }

const floor = slog.Level(-maxKlogVerbosity)

func (h cappedHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= floor && h.Handler.Enabled(ctx, l)
}

func (h cappedHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < floor {
		return nil
	}
	return h.Handler.Handle(ctx, r)
}

func (h cappedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return cappedHandler{h.Handler.WithAttrs(attrs)}
}

func (h cappedHandler) WithGroup(name string) slog.Handler {
	return cappedHandler{h.Handler.WithGroup(name)}
}
