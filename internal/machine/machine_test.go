package machine

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestShortCutsTheDomain(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"studio.local":             "studio",
		"studio.lan":               "studio",
		"studio.corp.example.com":  "studio",
		"studio":                   "studio",
		".local":                   ".local",
		"":                         "",
		"192-168-1-23.example.net": "192-168-1-23",
	} {
		if got := short(in); got != want {
			t.Errorf("short(%q) = %q, want %q", in, got, want)
		}
	}
}

// A Mac is named by its LocalHostName, which no network rewrites, never by the kernel's
// hostname, which DHCP and reverse DNS do.
func TestNameIsTheMacsLocalHostName(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	out, err := exec.Command("/usr/sbin/scutil", "--get", "LocalHostName").Output()
	if err != nil {
		t.Skipf("no LocalHostName: %v", err)
	}
	if got, want := Name(), strings.TrimSpace(string(out)); got != want {
		t.Errorf("Name() = %q, want the LocalHostName %q", got, want)
	}
}

func TestNameElsewhereIsTheShortHostname(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "darwin" {
		t.Skip("a Mac answers its LocalHostName")
	}
	host, err := os.Hostname()
	if err != nil {
		t.Skip(err)
	}
	if got := Name(); got != short(host) {
		t.Errorf("Name() = %q, want %q", got, short(host))
	}
}
