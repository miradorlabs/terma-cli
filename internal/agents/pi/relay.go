package pi

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

const (
	setupFix = "terma setup"

	restartNote = "A Pi already running, a T3 Code Pi thread included, loads terma's extension at its next start."
)

// ConfigureRelay refuses a Pi too old for terma's extension before it writes anything.
// Otherwise it writes the extension, pointed at the local relay with the relay's token and
// running this terma for its hooks.
func (Agent) ConfigureRelay(ctx context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	if why, old := tooOld(ctx); old {
		return agents.RelayResult{}, errors.New(why)
	}
	path, err := extensionFile()
	if err != nil {
		return agents.RelayResult{}, err
	}
	if _, err := writeExtension(path, relayExtension(cfg)); err != nil {
		return agents.RelayResult{}, err
	}
	return agents.RelayResult{Paths: []string{path}, Notes: []string{restartNote}}, nil
}

// UnconfigureRelay removes terma's Pi extension.
func (Agent) UnconfigureRelay(context.Context, string) (agents.RelayResult, error) {
	path, err := extensionFile()
	if err != nil {
		return agents.RelayResult{}, err
	}
	return relayexport.RemoveOwn(path)
}

// RelayPointed reports whether terma's extension sends Pi's sessions to the relay at addr:
// the extension this build writes, pointed there, on a Pi new enough to run it, with a terma
// it can run. It is unknown where Pi is not on the PATH.
func (Agent) RelayPointed(addr string) (bool, bool) {
	d := detect(context.Background())
	if !d.Found {
		return false, false
	}
	_, _, problem := relayProblem(d, addr)
	return !problem, true
}

// RelayProblem reports why Pi's sessions miss the relay at addr, the first that applies: a
// Pi too old for the extension; the extension missing, not this build's or pointed elsewhere;
// a terma it cannot run. A Pi not on the PATH has none.
func (Agent) RelayProblem(addr string) (detail, fix string, problem bool) {
	d := detect(context.Background())
	if !d.Found {
		return "", "", false
	}
	return relayProblem(d, addr)
}

// relayProblem is RelayProblem for the pi d found, so one check runs pi --version once.
func relayProblem(d harness.Detection, addr string) (detail, fix string, problem bool) {
	if why, old := tooOldVersion(d.Version); old {
		return why, "upgrade Pi, then run terma setup", true
	}
	path, err := extensionFile()
	if err != nil {
		return "", "", false
	}
	cfg, current := extensionCurrent(path)
	if !current || !pointedAt(cfg, addr) {
		// A Pi started with another agent directory loads another folder.
		return "Pi's terma extension " + output.TildePath(path) + " is missing, not this terma's or not pointed at the local relay (Pi loads it from " +
			output.TildePath(filepath.Dir(filepath.Dir(path))) + ", PI_CODING_AGENT_DIR else ~/.pi/agent), so no hook claims Pi's sessions and nothing of them is exported", setupFix, true
	}
	if terma := cfg.HookCommand[0]; !runnable(terma) {
		return "Pi's terma extension runs " + output.TildePath(terma) + ", which is not there or cannot be run, so Pi sessions are not recorded", setupFix, true
	}
	return "", "", false
}

// RelayTokenCurrent reports whether the extension presents token, the relay's, as its
// bearer; known is false where there is no extension or it sends no such header.
func (Agent) RelayTokenCurrent(token string) (tokenMatches, known bool) {
	path, err := extensionFile()
	if err != nil {
		return false, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, false
	}
	cfg, ok := readExtensionConfig(data)
	if !ok {
		return false, false
	}
	bearer, ok := cfg.Headers["Authorization"]
	if !ok {
		return false, false
	}
	return bearer == "Bearer "+token, true
}

// extensionCurrent reads the extension at path and reports whether it is what this build
// writes for the configuration it holds, which names the terma it runs.
func extensionCurrent(path string) (*extensionConfig, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	cfg, ok := readExtensionConfig(data)
	if !ok || len(cfg.HookCommand) == 0 {
		return nil, false
	}
	want, err := renderExtension(*cfg)
	return cfg, err == nil && want == string(data)
}

// pointedAt reports whether cfg exports to the relay at addr.
func pointedAt(cfg *extensionConfig, addr string) bool {
	ep := strings.TrimRight(cfg.Endpoint, "/")
	return ep == "http://"+addr || strings.HasPrefix(ep, "http://"+addr+"/")
}

// runnable reports whether the extension can start command: an executable regular file, by
// its path or, for a bare name, on the PATH.
func runnable(command string) bool {
	if !strings.ContainsAny(command, `/\`) {
		found, err := exec.LookPath(command)
		if err != nil {
			return false
		}
		command = found
	}
	return executable(command)
}

// executable reports whether path is a regular file someone may run.
func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0)
}

// agentDir is Pi's user configuration directory: PI_CODING_AGENT_DIR, else ~/.pi/agent.
func agentDir() (string, error) {
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".pi", "agent"), err
}

// extensionFile is where Pi loads terma's extension from.
func extensionFile() (string, error) {
	dir, err := agentDir()
	return filepath.Join(dir, "extensions", "terma.ts"), err
}

var (
	_ agents.RelayExporter     = Agent{}
	_ agents.RelayChecker      = Agent{}
	_ agents.RelayTokenChecker = Agent{}
)
