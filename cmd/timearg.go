package cmd

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseTimeArg reads a point in time the way a person types one on a command line.
// It accepts what the gateway accepts — an RFC 3339 timestamp, or a relative age
// (90s, 15m, 2h, 7d) meaning "that long ago" — plus the shorthands people actually
// reach for: now, today, yesterday, and a bare date. The shorthands resolve in local
// time, so "today" is the caller's today. Whatever the input, the gateway is sent an
// absolute timestamp.
func parseTimeArg(raw string, now time.Time) (time.Time, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "":
		return time.Time{}, fmt.Errorf("empty time")
	case "now":
		return now, nil
	case "today":
		return startOfDay(now), nil
	case "yesterday":
		return startOfDay(now).AddDate(0, 0, -1), nil
	}
	if d, ok := parseRelativeAge(value); ok {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(raw), now.Location()); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is not a time — use RFC 3339 (2026-09-09T00:00:00Z), a date (2026-09-09), a relative age (90s, 15m, 2h, 7d), today, yesterday or now", raw)
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// parseRelativeAge extends time.ParseDuration with the day unit the gateway also
// takes. Ages are positive; the flag name carries the direction.
func parseRelativeAge(raw string) (time.Duration, bool) {
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 || n > 3650 {
			return 0, false
		}
		return time.Duration(n) * 24 * time.Hour, true
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// timeWindow is a resolved half-open [since, until) pair. A zero since is unbounded.
type timeWindow struct {
	since, until time.Time
}

// resolveWindow turns --since/--until into absolute bounds. An empty until is now;
// an empty since is defaultSince before until, or unbounded when that is zero.
func resolveWindow(since, until string, defaultSince time.Duration, now time.Time) (timeWindow, error) {
	w := timeWindow{until: now}
	if until != "" {
		t, err := parseTimeArg(until, now)
		if err != nil {
			return w, fmt.Errorf("--until: %w", err)
		}
		w.until = t
	}
	switch {
	case since != "":
		t, err := parseTimeArg(since, now)
		if err != nil {
			return w, fmt.Errorf("--since: %w", err)
		}
		w.since = t
	case defaultSince > 0:
		w.since = w.until.Add(-defaultSince)
	}
	if !w.since.IsZero() && !w.since.Before(w.until) {
		return w, fmt.Errorf("--since (%s) must be before --until (%s)", rfc3339(w.since), rfc3339(w.until))
	}
	return w, nil
}

// rfc3339 renders a time the way the gateway reads it: UTC, second precision.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// localStamp renders a time for a person reading a table: their zone, to the minute.
func localStamp(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04")
}
