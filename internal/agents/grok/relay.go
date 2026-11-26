package grok

import (
	"cmp"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

const (
	// sandboxEnv and the config's [sandbox] profile run grok's tools, and its hooks, in a
	// sandbox, where a hook cannot write terma's state or cannot run terma at all.
	sandboxEnv      = "GROK_SANDBOX"
	sandboxTable    = "sandbox"
	sandboxOff      = "off"
	sandboxKey      = "profile"
	setupFix        = "terma setup"
	trustPromptNote = "Grok asks \"Do you trust the contents of this directory?\" the first time it opens a folder; answer Yes, as No quits grok."
)

// ConfigureRelay refuses a grok too old for terma's hooks, and one whose shell or config
// would send its logs elsewhere, switch them off or send its metrics to the relay, before it
// writes anything: written first, content would be switched on while the stream still went
// to the other collector. Otherwise it points grok's log export at the local relay through
// [telemetry]; the hooks step then writes terma's hooks file, which claims the sessions.
func (a Agent) ConfigureRelay(ctx context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	if why, old := tooOld(ctx); old {
		return agents.RelayResult{}, errors.New(why)
	}
	conflicts, err := a.Harness().ConflictsWith(harness.Exporter{Endpoint: cfg.Endpoint, APIKey: cfg.Token})
	if err != nil {
		return agents.RelayResult{}, err
	}
	for _, c := range conflicts {
		if !c.Advisory {
			return agents.RelayResult{}, errors.New(said(c) + "; " + remedy(c) + " again")
		}
	}
	result, err := relayexport.Native(a.Harness(), cfg)
	if err != nil {
		return result, err
	}
	if path, err := a.Harness().ConfigPath(); err == nil {
		result.Paths = append(result.Paths, path)
	}
	result.Notes = append(result.Notes, trustPromptNote)
	for _, c := range conflicts {
		result.Notes = append(result.Notes, displayName+": "+said(c)+".")
	}
	return result, nil
}

// said is a conflict as setup and doctor word it.
func said(c harness.Conflict) string { return c.Key + "=" + c.Value + " is " + c.Reason }

// remedy is what to do about a conflict, by where it was found.
func remedy(c harness.Conflict) string {
	if c.Scope == harness.ScopeUserSettings {
		return "remove it from grok's config.toml, then run terma setup"
	}
	return "unset it, then run terma setup"
}

// UnconfigureRelay restores the [telemetry] keys terma wrote.
func (a Agent) UnconfigureRelay(context.Context, string) (agents.RelayResult, error) {
	return relayexport.Unnative(a.Harness())
}

// RelayPointed reports whether grok's config points its log export at the relay at addr, with
// all eight keys as terma writes them, terma's current hooks file in place to claim its
// sessions, a grok new enough for both, and nothing in the shell or config that defeats the
// export. A sandbox does not make it false: it is a warning (RelayProblem). It is unknown
// where grok is not on the PATH.
func (a Agent) RelayPointed(addr string) (bool, bool) {
	if !(exporter{}).Detect(context.Background()).Found {
		return false, false
	}
	if !relayexport.NativePointed(a.Harness(), addr) {
		return false, true
	}
	_, _, problem := a.relayProblem(addr)
	return !problem, true
}

// RelayProblem reports why grok's export misses the relay at addr, or, where nothing does, a
// warning: that grok runs sandboxed, where its hooks cannot claim a session (the export still
// flows, and the relay holds it and drops it), or that a shell export turns off content the
// team's policy may collect.
func (a Agent) RelayProblem(addr string) (detail, fix string, problem bool) {
	if !(exporter{}).Detect(context.Background()).Found {
		return "", "", false
	}
	if detail, fix, problem := a.relayProblem(addr); problem {
		return detail, fix, true
	}
	// A warning is no reason an export misses the relay: none where it does for another cause.
	if !relayexport.NativePointed(a.Harness(), addr) {
		return "", "", false
	}
	if detail, fix, problem := sandboxed(); problem {
		return detail, fix, true
	}
	conflicts, _ := a.conflictsAt(addr)
	if i := slices.IndexFunc(conflicts, func(c harness.Conflict) bool { return c.Advisory }); i >= 0 {
		return displayName + ": " + said(conflicts[i]), "unset it", true
	}
	return "", "", false
}

// conflictsAt is what would defeat grok's export to the relay at addr: to the endpoint its
// config names, or, where it names none yet, the relay's own.
func (a Agent) conflictsAt(addr string) ([]harness.Conflict, error) {
	st, err := a.Harness().Status()
	if err != nil {
		return nil, err
	}
	return a.Harness().ConflictsWith(harness.Exporter{Endpoint: cmp.Or(st.Endpoint, "http://"+addr)})
}

// relayProblem is what keeps grok's export from the relay: a grok too old, terma's hooks
// file missing or not this terma's, or a blocking conflict.
func (a Agent) relayProblem(addr string) (detail, fix string, problem bool) {
	if why, old := tooOld(context.Background()); old {
		return why, "upgrade grok, then run terma setup", true
	}
	if path, current := hooksCurrent(); !current {
		return "Grok Build's terma hooks " + output.TildePath(path) + " are missing or not this terma's, so no hook claims grok's sessions and the relay drops their telemetry", setupFix, true
	}
	conflicts, err := a.conflictsAt(addr)
	if err != nil {
		return "could not read " + displayName + "'s config: " + err.Error(), "repair it, then run terma setup", true
	}
	if i := slices.IndexFunc(conflicts, func(c harness.Conflict) bool { return !c.Advisory }); i >= 0 {
		return displayName + ": " + said(conflicts[i]), remedy(conflicts[i]), true
	}
	return "", "", false
}

// hooksCurrent reports whether terma's hooks file in grok's hooks directory is the one this
// terma writes, and names the file.
func hooksCurrent() (string, bool) {
	path, err := Agent{}.UserHooksPath()
	if err != nil {
		return path, false
	}
	terma, err := procinfo.AbsExecutable()
	if err != nil {
		return path, false
	}
	plan, err := planUserHooks(filepath.Dir(path), hookmgr.UserHookCommand(terma), true)
	return path, err == nil && plan.Empty()
}

// sandboxed warns of a sandbox profile other than off, the shell's ahead of the config's.
func sandboxed() (detail, fix string, problem bool) {
	profile, where := os.Getenv(sandboxEnv), sandboxEnv+" in your shell"
	if profile == "" {
		_, f, err := (exporter{}).loadConfig()
		if err != nil {
			return "", "", false
		}
		table, _ := f.Doc[sandboxTable].(map[string]any)
		profile, _ = table[sandboxKey].(string)
		where = "[" + sandboxTable + "] " + sandboxKey + " in grok's config.toml"
	}
	if profile == "" || strings.EqualFold(strings.TrimSpace(profile), sandboxOff) {
		return "", "", false
	}
	return where + " runs grok sandboxed (" + profile + "), where terma's hooks cannot record a session: its telemetry is held and dropped",
		"run grok with the sandbox off to have its sessions collected", true
}

// RelayTokenCurrent reports whether grok's logs endpoint presents token, the relay's, as the
// first segment of its path; known is false where the config names no relay endpoint.
func (a Agent) RelayTokenCurrent(token string) (tokenMatches, known bool) {
	st, err := a.Harness().Status()
	if err != nil || !st.Connected {
		return false, false
	}
	u, err := url.Parse(st.Endpoint)
	if err != nil {
		return false, false
	}
	first, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	return first == token, true
}

var (
	_ agents.RelayExporter     = Agent{}
	_ agents.RelayChecker      = Agent{}
	_ agents.RelayTokenChecker = Agent{}
)
