package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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

// historyUsageError reports arguments `history upload` does not take. A --json-stream run
// ends with an error line, as every other failure of one does. Flags are read up to the
// one refused, so --json-stream after it is looked for in the process's arguments.
func historyUsageError(cmd *cobra.Command, jsonStream bool, err error) error {
	if !jsonStream && !argsAskForJSONStream(cmd.Flags(), os.Args[1:]) {
		return err
	}
	s := newHistoryStream(cmd.Context(), cmd.OutOrStdout())
	defer s.close()
	return s.fail(cmd.ErrOrStderr(), err)
}

// argsAskForJSONStream reports whether args set --json-stream as a flag: not as the value of
// another of flags' flags that takes one, and not set to false.
func argsAskForJSONStream(flags *pflag.FlagSet, args []string) bool {
	// takesValue reports whether the flag a names reads the next argument as its value.
	takesValue := func(a string) bool {
		var f *pflag.Flag
		switch {
		case strings.Contains(a, "="):
			return false
		case strings.HasPrefix(a, "--"):
			f = flags.Lookup(a[2:])
		case len(a) == 2 && a[0] == '-':
			f = flags.ShorthandLookup(a[1:])
		}
		return f != nil && f.NoOptDefVal == ""
	}
	asked := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return asked
		case a == "--json-stream":
			asked = true
		case strings.HasPrefix(a, "--json-stream="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--json-stream="))
			asked = err == nil && v
		case takesValue(a):
			i++
		}
	}
	return asked
}
