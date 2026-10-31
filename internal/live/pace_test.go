package live

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	week     = 7 * 24 * time.Hour
	fiveHour = 5 * time.Hour
)

// at is a window of length that ends a fraction left of the way from now.
func at(name string, length time.Duration, used, left float64, now time.Time) Window {
	return Window{Name: name, Length: length, UsedPercent: used, ResetsAt: now.Add(time.Duration(left * float64(length)))}
}

// A window warns while its use runs ahead of its time, from 15% to 95% of the way in, in
// either window; one already used up, used not at all, or past its reset does not.
func TestProjectWarnsWhileUseRunsAheadOfTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		w       Window
		warn    bool
		runsOut time.Time
	}{
		{"weekly, exactly on pace", at("seven_day", week, 50, 0.5, now), true, now.Add(week / 2)},
		{"weekly, just behind pace", at("seven_day", week, 49.9, 0.5, now), false, time.Time{}},
		{"weekly, well ahead", at("seven_day", week, 80, 0.5, now), true, now.Add(21 * time.Hour)},
		{"weekly, too early to tell", at("seven_day", week, 50, 0.86, now), false, time.Time{}},
		{"weekly, just early enough", at("seven_day", week, 50, 0.84, now), true, now.Add(-time.Duration(0.16 * float64(week))).Add(time.Duration(0.16 * 2 * float64(week))).Round(time.Minute)},
		{"weekly, too near its reset", at("seven_day", week, 99, 0.04, now), false, time.Time{}},
		{"weekly, just far enough from its reset", at("seven_day", week, 99, 0.06, now), true, time.Time{}},
		{"five-hour, ahead", at("five_hour", fiveHour, 75, 0.4, now), true, now.Add(time.Hour)},
		{"five-hour, behind", at("five_hour", fiveHour, 50, 0.4, now), false, time.Time{}},
		{"reset already passed", at("seven_day", week, 90, -0.1, now), false, time.Time{}},
		{"nothing used", at("seven_day", week, 0, 0.5, now), false, time.Time{}},
		{"already used up", at("five_hour", fiveHour, 100, 0.5, now), false, time.Time{}},
		{"no length", Window{Name: "spend_limit", UsedPercent: 90, ResetsAt: now.Add(time.Hour)}, false, time.Time{}},
	} {
		p, warn := Project(tc.w, now)
		if warn != tc.warn {
			t.Errorf("%s: warn %v, want %v (%+v)", tc.name, warn, tc.warn, p)
			continue
		}
		if !warn {
			continue
		}
		if !p.RunsOut.After(now) || p.RunsOut.After(p.ResetsAt) {
			t.Errorf("%s: runs out %v, want after now and by the reset %v", tc.name, p.RunsOut, p.ResetsAt)
		}
		if !tc.runsOut.IsZero() && !p.RunsOut.Equal(tc.runsOut) {
			t.Errorf("%s: runs out %v, want %v", tc.name, p.RunsOut, tc.runsOut)
		}
	}
}

// The status line's note and the notification name the window and both times in local
// time, with the weekday only for another day.
func TestPaceNoteAndMessage(t *testing.T) {
	t.Parallel()
	zone := time.FixedZone("CET", 3600)
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, zone) // a Tuesday
	weekly, ok := Project(Window{Name: "seven_day", Length: week, UsedPercent: 57.6, ResetsAt: time.Date(2026, 10, 31, 9, 0, 0, 0, zone)}, now)
	if !ok {
		t.Fatal("no warning")
	}
	if got, want := weekly.Note(now), "weekly limit: runs out ~Thu 14:00, resets Sat 09:00"; got != want {
		t.Errorf("note %q, want %q", got, want)
	}
	if got, want := weekly.Message("Agent", now), "At this pace your Agent weekly limit runs out ~Thu 14:00, before it resets Sat 09:00."; got != want {
		t.Errorf("message %q, want %q", got, want)
	}
	hourly, ok := Project(at("five_hour", fiveHour, 75, 0.4, now.UTC()), now)
	if !ok {
		t.Fatal("no five-hour warning")
	}
	if got, want := hourly.Note(now), "5-hour limit: runs out ~10:00, resets 11:00"; got != want {
		t.Errorf("note %q, want %q", got, want)
	}
}

// A window warns once, by its name and reset to the hour, however many processes see it at once
// and however its reported reset drifts; the next window warns again.
func TestFirstWarningOncePerWindow(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "pace-warnings")
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC) // a reset 48 h on sits on the hour
	p := Pace{Name: "seven_day", Length: week, UsedPercent: 80, ResetsAt: now.Add(48 * time.Hour), RunsOut: now.Add(24 * time.Hour)}
	var wg sync.WaitGroup
	var first atomic.Int32
	for range 8 {
		wg.Go(func() {
			if FirstWarning(dir, p, now) {
				first.Add(1)
			}
		})
	}
	wg.Wait()
	if first.Load() != 1 {
		t.Fatalf("%d first warnings, want 1", first.Load())
	}
	if FirstWarning(dir, p, now.Add(time.Hour)) {
		t.Fatal("the same window warned again")
	}
	drifted := p
	drifted.ResetsAt = p.ResetsAt.Add(7 * time.Second)
	if FirstWarning(dir, drifted, now.Add(time.Minute)) {
		t.Fatal("the same window warned again when its reported reset drifted by seconds")
	}
	next := p
	next.ResetsAt = p.ResetsAt.Add(week)
	if !FirstWarning(dir, next, now) {
		t.Fatal("the next window did not warn")
	}
	other := p
	other.Name = "five_hour"
	if !FirstWarning(dir, other, now) {
		t.Fatal("another window with the same reset did not warn")
	}
	if _, err := os.Stat(filepath.Join(dir, "seven-day-"+strconv.FormatInt(p.ResetsAt.Round(time.Hour).Unix(), 10)+".json")); err != nil {
		t.Fatalf("record: %v", err)
	}
}
