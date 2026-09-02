package proxy

import (
	"testing"
	"time"
)

func TestUsageResetAtFromAfterClampsClockSkewToWindow(t *testing.T) {
	observedAt := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	const windowSeconds = float64(5 * time.Hour / time.Second)

	got, ok := usageResetAtFromAfter(observedAt, windowSeconds+2*60, windowSeconds)
	if !ok {
		t.Fatal("usageResetAtFromAfter() rejected clock skew inside grace")
	}
	if want := observedAt.Add(5 * time.Hour); !got.Equal(want) {
		t.Fatalf("usageResetAtFromAfter() = %v, want clamped reset %v", got, want)
	}
	if start := got.Add(-5 * time.Hour); !start.Equal(observedAt) {
		t.Fatalf("derived start = %v, want observation time %v", start, observedAt)
	}
}

func TestUsageResetAtFromAfterRejectsBeyondClockSkewGrace(t *testing.T) {
	observedAt := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	const windowSeconds = float64(7 * 24 * time.Hour / time.Second)

	if got, ok := usageResetAtFromAfter(observedAt, windowSeconds+resetSignalGrace.Seconds()+1, windowSeconds); ok || !got.IsZero() {
		t.Fatalf("usageResetAtFromAfter() = (%v, %v), want rejected signal", got, ok)
	}
}

func TestUsageResetAtFromAbsoluteClampsClockSkewToWindow(t *testing.T) {
	observedAt := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	const windowSeconds = int64(7 * 24 * time.Hour / time.Second)
	candidate := observedAt.Add(7*24*time.Hour + 2*time.Minute)

	got, ok := usageResetAtFromAbsolute(candidate, observedAt, windowSeconds)
	if !ok {
		t.Fatal("usageResetAtFromAbsolute() rejected clock skew inside grace")
	}
	if want := observedAt.Add(7 * 24 * time.Hour); !got.Equal(want) {
		t.Fatalf("usageResetAtFromAbsolute() = %v, want clamped reset %v", got, want)
	}
	if start := got.Add(-7 * 24 * time.Hour); !start.Equal(observedAt) {
		t.Fatalf("derived start = %v, want observation time %v", start, observedAt)
	}
}

func TestUsageResetAtFromAbsoluteRejectsBeyondClockSkewGrace(t *testing.T) {
	observedAt := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	const windowSeconds = int64(5 * time.Hour / time.Second)
	candidate := observedAt.Add(5*time.Hour + resetSignalGrace + time.Second)

	if got, ok := usageResetAtFromAbsolute(candidate, observedAt, windowSeconds); ok || !got.IsZero() {
		t.Fatalf("usageResetAtFromAbsolute() = (%v, %v), want rejected signal", got, ok)
	}
}
