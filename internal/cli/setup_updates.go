package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

// updateAsker asks whether terma installs new releases itself, auto marked as the current
// choice and kept by a bare Enter; nil keeps the current choice without asking.
type updateAsker func(auto bool) (bool, error)

// askUpdates is setup's updateAsker: a picker on a terminal a person watches, else nil.
func askUpdates(cmd *cobra.Command, assumeYes bool) updateAsker {
	if assumeYes || !canPrompt() {
		return nil
	}
	return func(auto bool) (bool, error) { return pickUpdates(cmd, auto) }
}

// What each choice does, in the picker and on the Updates line.
const (
	updatesAutomatic = "each new release installs itself after a terma command"
	updatesOnRequest = "terma says when one is out, and `terma update` installs it"
)

// pickUpdates asks between installing new releases automatically and on request.
func pickUpdates(cmd *cobra.Command, auto bool) (bool, error) {
	rows := []pickRow{
		{Label: "Automatically", Note: updatesAutomatic, Current: auto, Default: auto},
		{Label: "On request", Note: updatesOnRequest, Current: !auto, Default: !auto},
	}
	i, err := pick(cmd, "How should terma install new releases?", rows, func(answer string) (int, error) {
		for i, r := range rows {
			if strings.EqualFold(answer, r.Label) {
				return i, nil
			}
		}
		return -1, fmt.Errorf("%q is not a choice: answer 1 or 2", answer)
	})
	return i == 0, err
}

// setupUpdates settles how this installation receives new releases and reports it on
// setup's Updates line. Only a release binary terma installed itself on a platform it
// can replace in place has the choice; ask makes it, and nil keeps the saved one.
func setupUpdates(ui *setupUI, dir, version, exe, goos string, ask updateAsker) error {
	if m, ok := selfupdate.ManagedBy(exe); ok {
		ui.Summary("Updates", fmt.Sprintf("through %s: terma says when one is out, and `%s` installs it", m.Name, m.Command))
		return nil
	}
	if !selfupdate.IsRelease(version) {
		ui.Summary("Updates", "none for a development build: `terma update --force` installs the latest release")
		return nil
	}
	if goos == "windows" {
		ui.Summary("Updates", "on request: terma says when one is out, and you download it from GitHub Releases")
		return nil
	}
	prefs, err := selfupdate.LoadPreferences(dir)
	if err != nil {
		return err
	}
	if ask != nil {
		auto, err := ask(prefs.Auto)
		switch {
		case errors.Is(err, errCancelled):
		case err != nil:
			return err
		case auto != prefs.Auto:
			prefs.Auto = auto
			if err := selfupdate.SavePreferences(dir, prefs); err != nil {
				return err
			}
		}
	}
	if prefs.Auto {
		ui.Summary("Updates", "automatic: "+updatesAutomatic)
	} else {
		ui.Summary("Updates", "on request: "+updatesOnRequest)
	}
	return nil
}
