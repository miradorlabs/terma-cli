package cli

import (
	"cmp"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

// setup asks how terma installs new releases only where terma replaces the binary itself;
// a package manager's installation, a development build and Windows are told how
// updates arrive instead, and with no one to ask the saved choice stands and is shown.
func TestSetupUpdates(t *testing.T) {
	const release, direct = "1.4.0", "/home/dev/.local/bin/terma"
	answer := func(auto bool) updateAsker {
		return func(bool) (bool, error) { return auto, nil }
	}
	failing := errors.New("terminal gone")
	for _, tc := range []struct {
		name      string
		version   string
		exe       string
		goos      string
		saved     bool
		ask       updateAsker
		wantAsked bool
		wantLine  string
		wantAuto  bool
		wantErr   error
	}{
		{name: "asked, turns automatic on", ask: answer(true), wantAsked: true,
			wantLine: "automatic: each new release installs itself after a terma command", wantAuto: true},
		{name: "asked, turns automatic off", saved: true, ask: answer(false), wantAsked: true,
			wantLine: "on request: terma says when one is out, and `terma update` installs it"},
		{name: "asked, cancelled keeps the saved choice", saved: true, wantAsked: true,
			ask:      func(bool) (bool, error) { return false, errCancelled },
			wantLine: "automatic: each new release installs itself after a terma command", wantAuto: true},
		{name: "asked, the picker fails", wantAsked: true, ask: func(bool) (bool, error) { return false, failing }, wantErr: failing},
		{name: "no one to ask keeps automatic", saved: true,
			wantLine: "automatic: each new release installs itself after a terma command", wantAuto: true},
		{name: "no one to ask keeps on request",
			wantLine: "on request: terma says when one is out, and `terma update` installs it"},
		{name: "Homebrew", exe: "/opt/homebrew/Caskroom/terma/1.4.0/terma", ask: answer(true),
			wantLine: "through Homebrew: terma says when one is out, and `brew upgrade terma` installs it"},
		{name: "npm", exe: "/usr/local/lib/node_modules/@miradorlabs/terma/vendor/terma", ask: answer(true),
			wantLine: "through npm: terma says when one is out, and `npm install -g @miradorlabs/terma@latest` installs it"},
		{name: "development build", version: "dev", ask: answer(true),
			wantLine: "none for a development build: `terma update --force` installs the latest release"},
		{name: "Windows", goos: "windows", ask: answer(true),
			wantLine: "on request: terma says when one is out, and you download it from GitHub Releases"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.saved {
				if err := selfupdate.SavePreferences(dir, selfupdate.Preferences{Auto: true}); err != nil {
					t.Fatal(err)
				}
			}
			asked := false
			var ask updateAsker
			if tc.ask != nil {
				ask = func(auto bool) (bool, error) {
					asked = true
					if auto != tc.saved {
						t.Errorf("the picker marked automatic=%v as current, saved %v", auto, tc.saved)
					}
					return tc.ask(auto)
				}
			}
			ui := newSetupUI(io.Discard, false)
			err := setupUpdates(ui, dir, cmp.Or(tc.version, release), cmp.Or(tc.exe, direct), cmp.Or(tc.goos, "linux"), ask)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if asked != tc.wantAsked {
				t.Errorf("asked = %v, want %v", asked, tc.wantAsked)
			}
			if tc.wantErr != nil {
				return
			}
			if len(ui.lines) != 1 || !strings.HasSuffix(ui.lines[0], "Updates       "+tc.wantLine) {
				t.Errorf("Updates line = %q, want %q", ui.lines, tc.wantLine)
			}
			if prefs, _ := selfupdate.LoadPreferences(dir); prefs.Auto != tc.wantAuto {
				t.Errorf("saved automatic = %v, want %v", prefs.Auto, tc.wantAuto)
			}
		})
	}
}
