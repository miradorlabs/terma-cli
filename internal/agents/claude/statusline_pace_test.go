package claude

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// notifications records what a hook would have shown on the desktop.
type notifications []string

func (n *notifications) Notify(message string) { *n = append(*n, message) }

// pacePayload is a status line payload with a five-hour and a weekly window.
func pacePayload(fiveUsed float64, fiveResets time.Time, weekUsed float64, weekResets time.Time) string {
	return fmt.Sprintf(`{"session_id":"sess-pace","cwd":"","model":{"id":"m"},"rate_limits":{"five_hour":{"used_percentage":%g,"resets_at":%d},"seven_day":{"used_percentage":%g,"resets_at":%d}}}`,
		fiveUsed, fiveResets.Unix(), weekUsed, weekResets.Unix())
}

// paceNow is a Tuesday morning, UTC, so the notes read the same on every machine.
var paceNow = time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)

// A weekly window used faster than it resets adds a row under the developer's own status
// line, and stands alone, after terma's mark, when there is none.
func TestStatusLinePaceNoteRowUnderTheRendering(t *testing.T) {
	behind := pacePayload(10, paceNow.Add(4*time.Hour), 57.6, time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC))
	note := "weekly limit: runs out ~Thu 14:00, resets Sat 09:00"
	for _, tc := range []struct {
		name, renderer, want string
	}{
		{"under a line", "printf custom", indicatorMark() + "custom\n" + note},
		{"under lines ending in a newline", "printf 'one\\ntwo\\n'", indicatorMark() + "one\ntwo\n" + note},
		{"alone, with nothing rendered", "true", indicatorMark() + note},
		{"alone, with no renderer", "", indicatorMark() + note},
	} {
		env, out, _ := statusEnv(t, behind)
		env.Now = paceNow
		if code := statusLine(context.Background(), env, statusLineOptions{Renderer: tc.renderer, Indicator: true}); code != 0 {
			t.Fatalf("%s: exit %d", tc.name, code)
		}
		if out.String() != tc.want {
			t.Errorf("%s: status line %q, want %q", tc.name, out.String(), tc.want)
		}
	}

	both := pacePayload(75, paceNow.Add(2*time.Hour), 57.6, time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC))
	env, out, _ := statusEnv(t, both)
	env.Now = paceNow
	statusLine(context.Background(), env, statusLineOptions{Renderer: "printf custom", Indicator: true})
	if want := indicatorMark() + "custom\n5-hour limit: runs out ~10:00, resets 11:00 · " + note; out.String() != want {
		t.Errorf("both windows: %q, want %q", out.String(), want)
	}
}

// Without a warning, or with capture off, the status line is the developer's own.
func TestStatusLineWithoutAWarningIsUnchanged(t *testing.T) {
	onPace := pacePayload(10, paceNow.Add(4*time.Hour), 20, paceNow.Add(4*24*time.Hour))
	env, out, _ := statusEnv(t, onPace)
	env.Now = paceNow
	statusLine(context.Background(), env, statusLineOptions{Renderer: "printf custom", Indicator: true})
	if out.String() != indicatorMark()+"custom" {
		t.Errorf("on pace: %q", out.String())
	}
	env, out, _ = statusEnv(t, onPace)
	env.Now = paceNow
	statusLine(context.Background(), env, statusLineOptions{Indicator: true})
	if out.String() != "" {
		t.Errorf("on pace, no renderer: %q", out.String())
	}

	var shown notifications
	behind := pacePayload(10, paceNow.Add(4*time.Hour), 80, paceNow.Add(3*24*time.Hour+12*time.Hour))
	env, out, _ = statusEnv(t, behind)
	env.Now, env.Notifier = paceNow, &shown
	statusLine(context.Background(), env, statusLineOptions{Renderer: "printf custom"})
	if out.String() != "custom" || len(shown) != 0 {
		t.Errorf("capture off: %q, notified %q", out.String(), shown)
	}
}

// A window's first warning is a desktop notification, once however often the status line
// redraws and however many sessions draw it; the window after it warns again.
func TestStatusLinePaceNotifiesOncePerWindow(t *testing.T) {
	var shown notifications
	resets := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	env, _, _ := statusEnv(t, "")
	env.Now, env.Notifier = paceNow, &shown
	draw := func(payload string, at time.Time) {
		env.Stdin, env.Now = strings.NewReader(payload), at
		statusLine(context.Background(), env, statusLineOptions{Renderer: "printf custom", Indicator: true})
	}
	draw(pacePayload(10, paceNow.Add(4*time.Hour), 57.6, resets), paceNow)
	draw(pacePayload(10, paceNow.Add(4*time.Hour), 58, resets), paceNow.Add(time.Minute))
	other := strings.Replace(pacePayload(10, paceNow.Add(4*time.Hour), 58, resets), "sess-pace", "sess-other", 1)
	draw(other, paceNow.Add(2*time.Minute))
	if len(shown) != 1 || shown[0] != "At this pace your Claude Code weekly limit runs out ~Thu 14:00, before it resets Sat 09:00." {
		t.Fatalf("notified %q, want the weekly warning once", shown)
	}

	next := resets.Add(7 * 24 * time.Hour)
	draw(pacePayload(10, paceNow.Add(4*time.Hour), 80, next), next.Add(-3*24*time.Hour-12*time.Hour))
	if len(shown) != 2 {
		t.Fatalf("the next window did not warn: %q", shown)
	}
}
