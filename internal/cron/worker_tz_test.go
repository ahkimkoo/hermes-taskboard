package cron

import (
	"testing"
	"time"

	"github.com/ahkimkoo/hermes-taskboard/internal/store"
)

// v0.3.19 coverage: cron specs must be interpreted in the host's
// configured local timezone, not in whatever Location time.Now()
// happens to carry. The Compute() function coerces `from` to
// time.Local to make the rule explicit.
//
// Symptom this guards against: docker container defaults to TZ=UTC,
// time.Now() returns UTC, robfig/cron interprets `0 9 * * *` as 9am
// UTC = 17:00 Beijing. After the fix we coerce to time.Local so a
// `TZ=Asia/Shanghai` env makes that same spec fire at 9am Beijing.

// withLocal swaps time.Local for the duration of the test and
// restores the original on cleanup.
func withLocal(t *testing.T, loc *time.Location) {
	t.Helper()
	orig := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = orig })
}

// TestComputeRespectsLocalTZ — when time.Local is set to UTC+8
// (Asia/Shanghai), `0 9 * * *` fires at 9am Shanghai time, NOT 9am
// UTC. The returned NextRunAt is an absolute instant; check it by
// converting to the same Location and reading the hour.
func TestComputeRespectsLocalTZ(t *testing.T) {
	cst, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("Asia/Shanghai timezone not available: %v", err)
	}
	withLocal(t, cst)

	// Reference moment: 2026-04-25 03:00 UTC = 11:00 Shanghai. Next
	// "0 9 * * *" should be the following day at 09:00 Shanghai
	// (= 01:00 UTC the day after), not "today at 09:00 UTC".
	from := time.Date(2026, 4, 25, 3, 0, 0, 0, time.UTC)
	s := &store.Schedule{Spec: "0 9 * * *", Enabled: true}
	if err := Compute(s, from); err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if s.NextRunAt == nil {
		t.Fatal("NextRunAt nil")
	}
	got := s.NextRunAt.In(cst)
	if got.Hour() != 9 || got.Minute() != 0 {
		t.Fatalf("expected 09:00 Shanghai, got %s", got.Format(time.RFC3339))
	}
	// And the very next firing must be tomorrow Shanghai time, not
	// today UTC. From 03:00 UTC = 11:00 Shanghai, today's 09:00
	// Shanghai is in the past (was 01:00 UTC), so next is tomorrow.
	if got.Day() == 25 {
		t.Fatalf("next firing must be tomorrow (26th) Shanghai, got %s",
			got.Format(time.RFC3339))
	}
}

// TestComputeRespectsLocalTZ_UTC — same Compute() with time.Local set
// to UTC: spec fires at 9am UTC. Confirms the fix doesn't break the
// default-Docker case.
func TestComputeRespectsLocalTZ_UTC(t *testing.T) {
	withLocal(t, time.UTC)

	from := time.Date(2026, 4, 25, 3, 0, 0, 0, time.UTC)
	s := &store.Schedule{Spec: "0 9 * * *", Enabled: true}
	if err := Compute(s, from); err != nil {
		t.Fatalf("Compute: %v", err)
	}
	got := s.NextRunAt.In(time.UTC)
	if got.Hour() != 9 || got.Minute() != 0 {
		t.Fatalf("expected 09:00 UTC, got %s", got.Format(time.RFC3339))
	}
	if got.Day() != 25 {
		t.Fatalf("next firing should be today (25th) UTC at 09:00, got %s",
			got.Format(time.RFC3339))
	}
}

// TestComputeIgnoresFromLocation — even if the caller passes a `from`
// that's anchored to a different Location, Compute() must coerce to
// time.Local so the spec interpretation is consistent regardless of
// how callers happen to construct their time argument.
func TestComputeIgnoresFromLocation(t *testing.T) {
	cst, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Skipf("Asia/Shanghai timezone not available: %v", err)
	}
	withLocal(t, cst)

	// Caller hands us a time anchored to UTC. We should still
	// interpret the cron spec in time.Local (Shanghai).
	fromUTC := time.Date(2026, 4, 25, 3, 0, 0, 0, time.UTC)
	s := &store.Schedule{Spec: "0 9 * * *", Enabled: true}
	if err := Compute(s, fromUTC); err != nil {
		t.Fatalf("Compute: %v", err)
	}
	got := s.NextRunAt.In(cst)
	if got.Hour() != 9 {
		t.Fatalf("Location coercion failed: got hour %d in Shanghai", got.Hour())
	}
}
