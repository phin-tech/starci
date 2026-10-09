package hatchet

import (
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestMissed(t *testing.T) {
	const nightly = "0 3 * * *"
	cases := []struct {
		name      string
		last, now string
		want      bool
	}{
		{"fired this morning, it is the afternoon", "2026-10-09 03:00", "2026-10-09 15:00", false},
		{"fired yesterday, today's is long past", "2026-10-08 03:00", "2026-10-09 09:00", true},
		{"several were missed", "2026-10-01 03:00", "2026-10-09 09:00", true},
		// The engine's own firing is given a moment to be noted before it is
		// taken to have been missed.
		{"today's is due this very minute", "2026-10-08 03:00", "2026-10-09 03:00", false},
		{"today's was due two minutes ago", "2026-10-08 03:00", "2026-10-09 03:02", false},
		{"today's was due five minutes ago", "2026-10-08 03:00", "2026-10-09 03:05", true},
		{"today's is still to come", "2026-10-08 03:00", "2026-10-09 02:59", false},
	}
	for _, c := range cases {
		got, err := missed(nightly, at(c.last), at(c.now))
		if err != nil || got != c.want {
			t.Errorf("%s: missed = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	if _, err := missed("not a cron", at("2026-10-08 03:00"), at("2026-10-09 03:00")); err == nil {
		t.Error("a cron that cannot be read was not reported")
	}
}

// Cron is read in UTC, whatever zone the time it is compared with is in.
func TestMissedIsInUTC(t *testing.T) {
	newYork := time.FixedZone("EDT", -4*3600)
	last := at("2026-10-08 03:00")
	// 23:30 in New York on the 8th is 03:30 UTC on the 9th: the 03:00 UTC
	// firing is half an hour past.
	now := time.Date(2026, 10, 8, 23, 30, 0, 0, newYork)
	if got, _ := missed("0 3 * * *", last, now); !got {
		t.Error("a firing due at 03:00 UTC was not seen as missed at 23:30 New York time")
	}
}

func TestFiringsAreRemembered(t *testing.T) {
	dir := t.TempDir()
	nightly := ScheduledInput{Forge: "forgejo", Repo: "sam/app", Schedule: "nightly"}
	other := ScheduledInput{Forge: "forgejo", Repo: "sam/app", Schedule: "backup"}

	if _, seen := lastFired(dir, nightly); seen {
		t.Fatal("a schedule that never fired has a record")
	}
	noteFired(dir, nightly, at("2026-10-09 03:00"))
	got, seen := lastFired(dir, nightly)
	if !seen || !got.Equal(at("2026-10-09 03:00")) {
		t.Errorf("last fired = %v, %v", got, seen)
	}
	// Each schedule has its own record.
	if _, seen := lastFired(dir, other); seen {
		t.Error("one schedule's firing was recorded for another")
	}
	// With nowhere to keep a record, nothing is kept and nothing breaks.
	noteFired("", nightly, at("2026-10-09 03:00"))
}

func TestCronNames(t *testing.T) {
	got := cronName(ScheduledInput{Forge: "forgejo", Repo: "sam/pi gamebook", Schedule: "nightly"})
	if got != "starci-forgejo-sam-pi-gamebook-nightly" {
		t.Errorf("cron name = %q", got)
	}
}
