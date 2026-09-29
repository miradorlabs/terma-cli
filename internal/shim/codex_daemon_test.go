package shim

import "testing"

func TestCodexEmbeddedLaunchSelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"new", nil, true},
		{"prompt", []string{"explain this code"}, true},
		{"resume", []string{"resume", "--last"}, true},
		{"fork", []string{"fork", "session-id"}, true},
		{"exec", []string{"exec", "hello"}, false},
		{"exec after options", []string{"-C", "repo", "-m", "model", "e", "hello"}, false},
		{"server", []string{"app-server"}, false},
		{"agents", []string{"agents"}, false},
		{"explicit", []string{"--no-daemon"}, false},
		{"resume explicit", []string{"resume", "--no-daemon", "--last"}, false},
		{"remote", []string{"--remote", "ws://localhost:4500"}, false},
		{"resume remote", []string{"resume", "--remote=ws://localhost:4500"}, false},
		{"help", []string{"resume", "--help"}, false},
		{"value resembles command", []string{"--model", "exec"}, true},
		{"value resembles flag", []string{"-c", "--no-daemon"}, true},
		{"literal prompt", []string{"--", "--no-daemon"}, true},
		{"literal command", []string{"--", "exec"}, true},
		{"resume name resembles command", []string{"resume", "exec"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexNeedsEmbeddedFlag(tc.args); got != tc.want {
				t.Fatalf("selection = %v, want %v", got, tc.want)
			}
		})
	}
}
