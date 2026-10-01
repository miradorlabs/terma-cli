package hookmgr

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
)

// huskyLine ends in `|| true` because husky runs a hook file with `sh -e` and its status
// is the last line's: without it, a machine without terma fails every commit.
func huskyLine(hook string) string {
	return fmt.Sprintf(`command -v terma >/dev/null 2>&1 && terma hook %s "$@" || true # %s`, hook, Marker)
}

func planHusky(root string, install bool) (Plan, error) {
	p := Plan{Manager: Husky}
	for _, hook := range GitHooks {
		rel := ".husky/" + hook
		before, err := ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return p, err
		}
		line := huskyLine(hook)
		lines := splitLines(before)
		has := containsMarker(lines)
		switch {
		case install && !has:
			var after []byte
			if before == nil {
				after = []byte(line + "\n")
			} else {
				after = appendLine(before, line)
			}
			p.Changes = append(p.Changes, Change{Path: rel, Before: before, After: after, Mode: 0o755})
		case install && has:
			// A line from an older terma is rewritten where it stands.
			if kept, changed := replaceMarked(lines, line); changed {
				p.Changes = append(p.Changes, Change{Path: rel, Before: before, After: []byte(strings.Join(kept, "\n") + "\n"), Mode: 0o755})
			}
		case !install && has:
			kept := removeMarked(lines)
			if len(bytes.TrimSpace([]byte(strings.Join(kept, "\n")))) == 0 {
				p.Changes = append(p.Changes, Change{Path: rel, Before: before})
			} else {
				p.Changes = append(p.Changes, Change{Path: rel, Before: before, After: []byte(strings.Join(kept, "\n") + "\n"), Mode: 0o755})
			}
		}
	}
	if install {
		p.Notes = append(p.Notes, "Husky installs these on `npm install` (its prepare script); existing clones can run `npx husky`.")
	}
	return p, nil
}

func ownedHuskyLine(line string) bool {
	line = strings.TrimSpace(line)
	for _, hook := range GitHooks {
		bare := `terma hook ` + hook + ` "$@" || true`
		if line == huskyLine(hook) || line == bare || line == bare+" # "+Marker || line == `command -v terma >/dev/null 2>&1 && { `+bare+`; } # `+Marker {
			return true
		}
	}
	return false
}
