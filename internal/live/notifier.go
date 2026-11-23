// Package live finds what is true of the session a developer is in right now, on this
// machine alone, and tells them within seconds; each finding expires on its own once it
// stops being true. It never touches the network.
package live

import (
	"os/exec"
	"runtime"

	"github.com/miradorlabs/terma-cli/internal/procinfo"
)

// title heads every notification terma shows.
const title = "Terma"

// Notifier tells the developer about a finding.
type Notifier interface {
	Notify(message string)
}

// Desktop shows a notification on this machine's desktop, from a detached process so the
// caller never waits on it: osascript on macOS, notify-send on Linux where it is installed,
// and nothing elsewhere.
type Desktop struct{}

// Notify starts the notification and returns at once; it is silent on failure.
func (Desktop) Notify(message string) {
	argv, _ := desktopCommand(runtime.GOOS, exec.LookPath)
	if argv == nil {
		return
	}
	cmd := exec.Command(argv[0], append(argv[1:], title, message)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	procinfo.Detach(cmd)
	if cmd.Start() != nil {
		return
	}
	_ = cmd.Process.Release()
}

// DesktopUnavailable says why this machine shows no desktop notification, "" when it can.
func DesktopUnavailable() string {
	_, why := desktopCommand(runtime.GOOS, exec.LookPath)
	return why
}

// desktopCommand is the command that shows a notification on goos, its title and message
// still to append, or why there is none. The text goes in as arguments, never into the
// script, so nothing in it is read as AppleScript.
func desktopCommand(goos string, lookPath func(string) (string, error)) ([]string, string) {
	switch goos {
	case "darwin":
		return []string{"osascript",
			"-e", "on run argv",
			"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
			"-e", "end run"}, ""
	case "linux":
		path, err := lookPath("notify-send")
		if err != nil {
			return nil, "notify-send is not installed"
		}
		return []string{path, "--app-name=terma"}, ""
	}
	return nil, "terma shows no desktop notifications on " + goos
}
