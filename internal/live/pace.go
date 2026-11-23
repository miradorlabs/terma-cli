package live

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A window warns only between these fractions of its length: earlier, its average is a
// handful of calls; later, the reset is near enough to need no warning.
const (
	paceEarliest = 0.15
	paceLatest   = 0.95
)

// Window is one rate-limit window as an agent reports it.
type Window struct {
	// Name is the agent's name for the window, which tells two of them apart.
	Name string
	// Length is how long the window runs, ending at ResetsAt.
	Length time.Duration
	// UsedPercent is how much of the limit is used, from 0 to 100.
	UsedPercent float64
	ResetsAt    time.Time
}

// Pace is a window used faster than it resets: at its average rate so far, it runs out at
// RunsOut, before ResetsAt.
type Pace struct {
	Window
	RunsOut time.Time
}

// Project warns when, at now, w's use is ahead of its time: between 15% and 95% of the way
// through, used% ÷ elapsed ≥ 100. A window already used up, or past its reset, warns no more.
func Project(w Window, now time.Time) (Pace, bool) {
	if w.Length <= 0 || w.UsedPercent <= 0 || w.UsedPercent >= 100 {
		return Pace{}, false
	}
	elapsed := 1 - float64(w.ResetsAt.Sub(now))/float64(w.Length)
	if elapsed < paceEarliest || elapsed > paceLatest || w.UsedPercent/elapsed < 100 {
		return Pace{}, false
	}
	start := w.ResetsAt.Add(-w.Length)
	runsOut := start.Add(time.Duration(float64(now.Sub(start)) * 100 / w.UsedPercent))
	return Pace{Window: w, RunsOut: runsOut.Round(time.Minute)}, true
}

// Note is the status line's form, in now's time zone: "weekly limit: runs out ~Thu 14:00,
// resets Sat 09:00".
func (p Pace) Note(now time.Time) string {
	return fmt.Sprintf("%s: runs out ~%s, resets %s", p.label(), clock(p.RunsOut, now), clock(p.ResetsAt, now))
}

// Message is the notification's, for the agent's window: "At this pace your <agent> weekly
// limit runs out ~Thu 14:00, before it resets Sat 09:00."
func (p Pace) Message(agent string, now time.Time) string {
	return fmt.Sprintf("At this pace your %s %s runs out ~%s, before it resets %s.", agent, p.label(), clock(p.RunsOut, now), clock(p.ResetsAt, now))
}

// FirstWarning reports whether p is the first warning of its window, recording it in dir: a
// window warns once, however many sessions and redraws see it. A window is keyed by its reset to
// the hour: the reset a status line reports drifts by seconds between redraws of one window.
// Accepted risk: a reset near half past the hour that drifts across it can warn twice in one
// window; the upgrade path is to match a record of the same name within 30 minutes either side.
func FirstWarning(dir string, p Pace, now time.Time) bool {
	if os.MkdirAll(dir, 0o700) != nil {
		return false
	}
	name := strings.ReplaceAll(p.Name, "_", "-") + "-" + strconv.FormatInt(p.ResetsAt.Round(time.Hour).Unix(), 10) + ".json"
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(struct {
		WarnedAt time.Time `json:"warned_at"`
		RunsOut  time.Time `json:"runs_out"`
	}{now, p.RunsOut})
	return true
}

func (p Pace) label() string {
	if p.Length == 7*24*time.Hour {
		return "weekly limit"
	}
	return fmt.Sprintf("%.0f-hour limit", p.Length.Hours())
}

// clock is t in now's zone, with its weekday when it falls on another day.
func clock(t, now time.Time) string {
	t = t.In(now.Location())
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("Mon 15:04")
}
