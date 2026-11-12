package machine

import (
	"os/exec"
	"strings"
	"testing"
)

// A HostName the user set wins, as it does for the kernel; else the Sharing name.
func TestFirstSetFollowsMacOSOrder(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		keys map[string]string
		want string
	}{
		{map[string]string{"HostName": "build", "LocalHostName": "Studio"}, "build"},
		{map[string]string{"LocalHostName": "Studio"}, "Studio"},
		{map[string]string{}, ""},
	} {
		if got := firstSet(func(key string) string { return c.keys[key] }); got != c.want {
			t.Errorf("firstSet(%v) = %q, want %q", c.keys, got, c.want)
		}
	}
}

// scutil reads a set key and answers "" for an unset one, which scutil reports by exiting 1
// (HostName, on most Macs).
func TestScutilReadsSystemConfiguration(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"LocalHostName", "HostName"} {
		want := ""
		if out, err := exec.Command("/usr/sbin/scutil", "--get", key).Output(); err == nil {
			want = strings.TrimSpace(string(out))
		}
		if got := scutil(key); got != want {
			t.Errorf("scutil(%s) = %q, want %q", key, got, want)
		}
	}
}
