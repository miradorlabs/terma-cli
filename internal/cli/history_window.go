package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxActiveWindowDays bounds --active-window in days, well inside time.Duration's ~292 years.
const maxActiveWindowDays = 100 * 365

// parseHistorySince parses --since as a positive relative window (e.g. 90d, 24h) or an
// absolute date/timestamp (2006-01-02 or RFC3339).
func parseHistorySince(spec string, now time.Time) (time.Time, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return time.Time{}, errors.New("--since cannot be empty")
	}
	if strings.HasSuffix(s, "d") && len(s) > 1 {
		days, err := strconv.Atoi(s[:len(s)-1])
		if err != nil || days <= 0 {
			return time.Time{}, fmt.Errorf("invalid --since %q: expected a positive number of days (e.g. 90d)", spec)
		}
		return now.Add(-time.Duration(days) * 24 * time.Hour), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("invalid --since %q: duration must be positive", spec)
		}
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid --since %q: use a duration like 90d or 48h, or a date like 2026-08-01", spec)
}

// parseHistoryActiveWindow parses --active-window as a non-negative whole-second window
// (1h, 30m, 30s, 2d, or 0). Sub-second durations are refused so active_window_seconds is
// always exact.
func parseHistoryActiveWindow(spec string) (time.Duration, error) {
	s := strings.TrimSpace(spec)
	var (
		d  time.Duration
		ok bool
	)
	if days, found := strings.CutSuffix(s, "d"); found && days != "" {
		if n, err := strconv.Atoi(days); err == nil && n <= maxActiveWindowDays && n >= -maxActiveWindowDays {
			d, ok = time.Duration(n)*24*time.Hour, true
		}
	} else if parsed, err := time.ParseDuration(s); err == nil {
		d, ok = parsed, true
	}
	switch {
	case !ok:
		return 0, fmt.Errorf("invalid --active-window %q: use a duration like 1h or 30m, or 0 to skip none", spec)
	case d < 0:
		return 0, fmt.Errorf("invalid --active-window %q: the window cannot be negative", spec)
	case d%time.Second != 0:
		return 0, fmt.Errorf("invalid --active-window %q: must be whole seconds (e.g. 1h, 30m, 30s, or 0)", spec)
	}
	return d, nil
}
