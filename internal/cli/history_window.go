package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxHistoryWindowDays bounds a window in days, well inside time.Duration's ~292 years.
const maxHistoryWindowDays = 100 * 365

// parseHistoryWindow reads a window as a number of days (90d) or a Go duration (48h, 30m,
// 0). ok is false when spec is neither.
func parseHistoryWindow(spec string) (d time.Duration, ok bool) {
	if days, found := strings.CutSuffix(spec, "d"); found && days != "" {
		n, err := strconv.Atoi(days)
		if err != nil || n > maxHistoryWindowDays || n < -maxHistoryWindowDays {
			return 0, false
		}
		return time.Duration(n) * 24 * time.Hour, true
	}
	d, err := time.ParseDuration(spec)
	return d, err == nil
}

// parseHistorySince parses --since as a positive relative window (e.g. 90d, 24h) or an
// absolute date/timestamp (2006-01-02 or RFC3339).
func parseHistorySince(spec string, now time.Time) (time.Time, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return time.Time{}, errors.New("--since cannot be empty")
	}
	if d, ok := parseHistoryWindow(s); ok {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("invalid --since %q: the window must be positive", spec)
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

// parseHistoryActiveWindow parses --active-window as a window of zero or more (1h, 30m, 0).
func parseHistoryActiveWindow(spec string) (time.Duration, error) {
	d, ok := parseHistoryWindow(strings.TrimSpace(spec))
	switch {
	case !ok:
		return 0, fmt.Errorf("invalid --active-window %q: use a duration like 1h or 30m, or 0 to skip none", spec)
	case d < 0:
		return 0, fmt.Errorf("invalid --active-window %q: the window cannot be negative", spec)
	}
	return d, nil
}
