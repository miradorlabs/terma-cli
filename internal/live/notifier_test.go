package live

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// macOS shows a notification through osascript, Linux through notify-send where it is
// installed; elsewhere there is none, and doctor can say why.
func TestDesktopCommandByPlatform(t *testing.T) {
	t.Parallel()
	found := func(string) (string, error) { return "/usr/bin/notify-send", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }

	argv, why := desktopCommand("darwin", missing)
	if len(argv) == 0 || argv[0] != "osascript" || why != "" {
		t.Fatalf("darwin: %q, %q", argv, why)
	}
	// The title and message are the script's arguments, so neither is read as AppleScript.
	if script := strings.Join(argv, " "); !strings.Contains(script, "item 1 of argv") || !strings.Contains(script, "item 2 of argv") {
		t.Fatalf("darwin script %q does not read its text from argv", script)
	}
	if argv, why := desktopCommand("linux", found); !slices.Equal(argv, []string{"/usr/bin/notify-send", "--app-name=terma"}) || why != "" {
		t.Fatalf("linux with notify-send: %q, %q", argv, why)
	}
	if argv, why := desktopCommand("linux", missing); argv != nil || !strings.Contains(why, "notify-send") {
		t.Fatalf("linux without notify-send: %q, %q", argv, why)
	}
	if argv, why := desktopCommand("windows", found); argv != nil || why == "" {
		t.Fatalf("windows: %q, %q", argv, why)
	}
}
