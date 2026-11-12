package machine

import (
	"errors"
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
		"10.0.0.5":                 "10.0.0.5",
		"fe80::1":                  "fe80::1",
	} {
		if got := short(in); got != want {
			t.Errorf("short(%q) = %q, want %q", in, got, want)
		}
	}
}

// The system's own name wins over a hostname the network handed out, whose first label is
// not the machine's; only without one does the hostname name the machine.
func TestPickPrefersTheSystemsOwnName(t *testing.T) {
	t.Parallel()
	host := func(name string) func() (string, error) {
		return func() (string, error) { return name, nil }
	}
	failed := func() (string, error) { return "", errors.New("no hostname") }
	for _, c := range []struct {
		local    string
		hostname func() (string, error)
		want     string
	}{
		{"Studio", host("c-73-1-2-3.hsd1.ca.comcast.net"), "Studio"},
		{"Studio", host("studios-mbp.lan"), "Studio"},
		{"Studio", failed, "Studio"},
		{"", host("studio.lan"), "studio"},
		{"", host("build-01"), "build-01"},
		{"", failed, ""},
	} {
		if got := pick(c.local, c.hostname); got != c.want {
			t.Errorf("pick(%q, …) = %q, want %q", c.local, got, c.want)
		}
	}
}
