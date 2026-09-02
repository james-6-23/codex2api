package proxy

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// resetSignalGrace tolerates small differences between the upstream clock and
// the local response-receive time. A reset-after value still must describe the
// current quota window rather than an absolute timestamp or corrupt duration.
const resetSignalGrace = 5 * time.Minute

// maxUnknownUsageWindow bounds reset signals for windows whose duration is not
// present. Codex's longest supported quota window is monthly (28-31 days).
const maxUnknownUsageWindow = 32 * 24 * time.Hour

func parseFiniteUsageHeaderFloat(raw string) (float64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

func usageResetAtFromAfter(observedAt time.Time, resetAfterSeconds, windowSeconds float64) (time.Time, bool) {
	if observedAt.IsZero() || math.IsNaN(resetAfterSeconds) || math.IsInf(resetAfterSeconds, 0) || resetAfterSeconds <= 0 {
		return time.Time{}, false
	}

	maxResetAfter := maxUnknownUsageWindow.Seconds()
	knownWindow := !math.IsNaN(windowSeconds) && !math.IsInf(windowSeconds, 0) &&
		windowSeconds > 0 && windowSeconds <= maxUnknownUsageWindow.Seconds()
	if knownWindow {
		maxResetAfter = windowSeconds + resetSignalGrace.Seconds()
	}
	if resetAfterSeconds > maxResetAfter {
		return time.Time{}, false
	}
	// A small positive difference is normally upstream/local clock skew. Keep
	// accepting it, but do not let it derive a window start in the future: the
	// newest possible start of a full window is the observation instant.
	if knownWindow && resetAfterSeconds > windowSeconds {
		resetAfterSeconds = windowSeconds
	}

	wholeSeconds, fractionalSeconds := math.Modf(resetAfterSeconds)
	resetAt := observedAt.Add(time.Duration(wholeSeconds) * time.Second)
	resetAt = resetAt.Add(time.Duration(fractionalSeconds * float64(time.Second)))
	if !resetAt.After(observedAt) {
		return time.Time{}, false
	}
	return resetAt, true
}

func usageResetAtFromAbsolute(resetAt, observedAt time.Time, windowSeconds int64) (time.Time, bool) {
	if resetAt.IsZero() || observedAt.IsZero() || !resetAt.After(observedAt) {
		return time.Time{}, false
	}
	maxAhead := maxUnknownUsageWindow
	if windowSeconds > 0 && windowSeconds <= int64(maxUnknownUsageWindow/time.Second) {
		window := time.Duration(windowSeconds) * time.Second
		maxAhead = window + resetSignalGrace
		if resetAt.After(observedAt.Add(maxAhead)) {
			return time.Time{}, false
		}
		if resetAt.After(observedAt.Add(window)) {
			return observedAt.Add(window), true
		}
	}
	if resetAt.After(observedAt.Add(maxAhead)) {
		return time.Time{}, false
	}
	return resetAt, true
}
