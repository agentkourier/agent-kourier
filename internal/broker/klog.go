package broker

import (
	"log/slog"
	"strconv"

	"k8s.io/klog/v2"
)

// maxKlogVerbosity is the highest klog verbosity courier allows. client-go logs request and
// response bodies, Secret lists included, at verbosity 8 and above, so nothing at 8 may ever be
// enabled, whatever is asked for.
const maxKlogVerbosity = 7

// RouteKlog sends klog's output, which is client-go's, to logger, and sets klog's verbosity to v,
// held between 0 and maxKlogVerbosity. It changes klog's process-wide state: call it once at
// startup, before anything logs.
func RouteKlog(logger *slog.Logger, v int) error {
	klog.SetSlogLogger(logger)
	var level klog.Level
	return level.Set(strconv.Itoa(min(max(v, 0), maxKlogVerbosity)))
}
