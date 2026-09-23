package cmd

import (
	"os"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/shim"
)

// shellRoutingCheck is independent of the export check: global telemetry can work
// while shell integration was never activated. Both doctor and status must say so.
// The check reads configuration only; it never installs a shim or edits the shell.
func shellRoutingCheck(verdicts []harnessVerdict, bound bool, mine []string) doctor.Check {
	if !bound {
		return doctor.Check{Status: doctor.Skip, Detail: "needs an installed repository"}
	}
	var parts []string
	var missingRoute, inactive bool
	for _, v := range verdicts {
		if !shim.Routable(v.name) || (len(mine) > 0 && !slices.Contains(mine, v.name)) {
			continue
		}
		part := v.displayName + ": "
		switch {
		case !v.routed:
			part += "per-repo routing is not configured for this project"
			missingRoute = true
			if !shim.Active(v.name) {
				part += "; shell integration inactive"
				inactive = true
			}
		case !v.live:
			part += "routing configured, shell integration inactive"
			inactive = true
		case os.Getenv(shim.WrapperEnv) == "wrapper":
			part += "shell wrapper active"
		default:
			part += "PATH shim active"
		}
		if !v.live {
			if _, sends := statusAgent(v, bound); sends {
				part += "; telemetry uses global settings"
			} else {
				part += "; telemetry is not configured to reach this project"
			}
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return doctor.Check{Status: doctor.Skip, Detail: "no selected agent needs shell routing"}
	}
	check := doctor.Check{Status: doctor.Pass, Detail: strings.Join(parts, "; ")}
	if missingRoute || inactive {
		check.Status = doctor.Warn
	}
	if missingRoute {
		check.Fix = "terma install"
	}
	if inactive {
		if rc, ok := shim.ShellRC(); ok {
			state, err := rc.State()
			switch {
			case err != nil:
				check.Detail += "; could not read " + tildePath(rc.Path)
			case state == shim.RCAbsent:
				check.Detail += "; Terma's PATH setup is missing from " + tildePath(rc.Path) + " and no shell wrapper is active"
			case state == shim.RCLast:
				check.Detail += "; " + tildePath(rc.Path) + " is configured, but this shell has not activated it"
			case state == shim.RCOvertaken:
				check.Detail += "; a later PATH setting in " + tildePath(rc.Path) + " can bypass Terma's shims"
			}
		}
		activationFix := shimPathFix()
		if check.Fix != "" && !strings.HasPrefix(activationFix, "terma install") {
			check.Fix += "; " + activationFix
		} else {
			check.Fix = activationFix
		}
	}
	return check
}
