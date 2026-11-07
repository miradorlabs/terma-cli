package e2e

import (
	"cmp"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// What a profile keeps of an app: its bundle's and its agent's facts, and how the app launched
// its agent (its command line and environment, read from the kernel), the run's and the
// machine's names in them placeholders.

// DesktopFacts are what a desktop app's profile keeps of its bundle and of the agent it runs:
// the agent's executable facts, and beside them the app's.
type DesktopFacts struct {
	BinaryFacts
	App   AppFacts   `json:"app"`
	Agent AgentFacts `json:"agent"`
}

// AppFacts are the app bundle's identity and signature.
type AppFacts struct {
	Name      string   `json:"name"`
	BundleID  string   `json:"bundle_id"`
	Version   string   `json:"version"`
	Build     string   `json:"build,omitempty"`
	Download  string   `json:"download"`
	SizeBytes int64    `json:"size_bytes"`
	Signing   *Signing `json:"signing,omitempty"`
}

// AgentFacts are the agent the app runs: which harness and build, where the app keeps it, and
// how it comes by it.
type AgentFacts struct {
	Harness string `json:"harness"`
	Version string `json:"version"`
	// Path is where the app runs it from, its placeholders in place.
	Path string `json:"path"`
	// Source says how the app comes by it: "bundled" in the app, or "downloaded" (Claude
	// Desktop's, from Download, on first use, checked against Checksum).
	Source   string `json:"source"`
	Download string `json:"download,omitempty"`
	Checksum string `json:"checksum,omitempty"`
	// SDK is the library the app drives it with, where it names one ("@anthropic-ai/claude-agent-sdk 0.3.317").
	SDK string `json:"sdk,omitempty"`
}

// Launch is how the app started its agent: the command line and environment the agent's
// process was started with, read from the kernel, and the processes between it and the app.
type Launch struct {
	Argv []string `json:"argv"`
	// Env are the names of the variables the app set for its agent: those the agent was started
	// with that neither the app's own environment nor its login shell's has with the same value.
	// EnvValues are their values, a secret's redacted, a list of folders <path>; Inherited how
	// many more it passed on as it had them.
	Env       []string          `json:"env"`
	EnvValues map[string]string `json:"env_values,omitempty"`
	Inherited int               `json:"inherited"`
	// Parents are the commands between the app and the agent, the agent's parent first.
	Parents []string `json:"parents,omitempty"`
}

// loginEnv is the environment a login session's launchd gives the apps it starts, the Finder's
// and the Dock's: the user's identity and home, their login shell, the system PATH, and the
// session's own sockets. A runner's or a developer's shell exports none of it.
func loginEnv() []string {
	env := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "SHELL=" + loginShell()}
	for _, k := range []string{"HOME", "USER", "LOGNAME", "TMPDIR", "SSH_AUTH_SOCK", "__CF_USER_TEXT_ENCODING"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// loginShell is the user's login shell, as the user database has it.
func loginShell() string {
	out, err := exec.Command("dscl", ".", "-read", "/Users/"+os.Getenv("USER"), "UserShell").Output()
	if _, shell, ok := strings.Cut(strings.TrimSpace(string(out)), "UserShell: "); err == nil && ok && shell != "" {
		return shell
	}
	return "/bin/zsh"
}

// loginNote says what login shell the app's user had, which the app's agent takes its
// environment from: a new Mac user's (zsh, no startup files) or the machine's own.
func loginNote() string {
	shell := loginShell()
	home, _ := os.UserHomeDir()
	var rc []string
	for _, f := range []string{".bash_profile", ".bashrc", ".profile", ".zshrc", ".zprofile", ".zshenv", ".zlogin"} {
		if _, err := os.Stat(filepath.Join(home, f)); err == nil {
			rc = append(rc, f)
		}
	}
	if shell == "/bin/zsh" && len(rc) == 0 {
		return "The machine's user was a new Mac user: zsh, the macOS default, its login shell, and no shell startup files of its own."
	}
	return "The machine's user's login shell was " + shell + ", with its startup files (" + strings.Join(rc, ", ") + "): the app's agent took its environment from them."
}

// loginShellEnv is what the user's login shell exports, started as an app resolves it (Claude
// Desktop gives its agent the login shell's environment): the machine's, not the app's.
func loginShellEnv() map[string]string {
	cmd := desktopCmd(loginShell(), "-l", "-c", "env")
	cmd.Env = loginEnv()
	out, _ := cmd.Output()
	env := map[string]string{}
	for line := range strings.Lines(string(out)) {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\n"), "="); ok {
			env[k] = v
		}
	}
	return env
}

// sessionEnv are the variables any app has from the login session (launchd's), whose values
// are the machine's, not the app's choice.
var sessionEnv = map[string]bool{"HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "TMPDIR": true, "PATH": true,
	"SSH_AUTH_SOCK": true, "__CF_USER_TEXT_ENCODING": true, "XPC_FLAGS": true, "XPC_SERVICE_NAME": true, "LANG": true,
	"__CFBundleIdentifier": true, "COMMAND_MODE": true, "PWD": true, "OLDPWD": true, "SHLVL": true, "_": true,
	"MallocNanoZone": true, "LC_ALL": true, "LC_CTYPE": true, "TERM": true, "TERM_PROGRAM": true}

// machineNames is v with the machine's own names in it placeholders, as an app writes them into
// a value of its own (Claude Desktop's CLAUDE_CODE_DIAGNOSTICS_FILE, OTEL_RESOURCE_ATTRIBUTES'
// process.owner and os.version). The run's places go first, longest first, each its placeholder
// (the app's bundle <app>, the checkout <checkout>, the home <home>), so a path is named by the
// most it says before the user's name inside it is taken for a name; then the user's name
// <user>, the machine's name <host>, the OS's release <os-release>, and last what MachinePaths
// says of the paths left (a home or a program the run's places did not name).
func machineNames(v string, places map[string]string, m machine) string {
	lits := slices.SortedFunc(maps.Keys(places), func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, lit := range lits {
		if len(lit) > 1 {
			v = strings.ReplaceAll(v, lit, places[lit])
		}
	}
	for _, name := range []struct{ lit, with string }{{m.user, "<user>"}, {m.host, "<host>"}, {shortHost(m.host), "<host>"}} {
		// A short name (a user "dev") would match words that are not it.
		if len(name.lit) >= 3 {
			v = regexp.MustCompile(`\b`+regexp.QuoteMeta(name.lit)+`\b`).ReplaceAllString(v, name.with)
		}
	}
	return MachinePaths(osVersionAttr.ReplaceAllString(v, "${1}<os-release>"))
}

// shortHost is a host name without its domain (Mac-1730.local is Mac-1730), "" where it has
// none.
func shortHost(host string) string {
	if short, _, ok := strings.Cut(host, "."); ok {
		return short
	}
	return ""
}

// desktopPlaces are the places of a desktop app's run whose paths it hands its agent, each its
// placeholder: the app's bundle and the folder it sits in, the checkout the suite runs from
// (where it unpacked the app), and the home.
func desktopPlaces(app DesktopApp, home string) map[string]string {
	places := map[string]string{home: "<home>"}
	if app.Bundle != "" {
		places[app.Bundle] = "<app>"
		places[filepath.Dir(app.Bundle)] = "<app-dir>"
	}
	if wd, err := os.Getwd(); err == nil && filepath.Base(wd) == "e2e" && filepath.Base(filepath.Dir(wd)) == "test" {
		places[filepath.Dir(filepath.Dir(wd))] = "<checkout>"
	}
	delete(places, "")
	return places
}

// osVersionAttr is an OS release as a resource attribute names it.
var osVersionAttr = regexp.MustCompile(`((?:^|[,;\s])os\.version=)[0-9][0-9.]*`)

// runKey is a value that is a key made for one run: base64 of 32 bytes or more.
var runKey = regexp.MustCompile(`^[A-Za-z0-9+/]{40,}={0,2}$`)

// launchOf is how pid was started, below the app at top: what the app set of its environment
// (neither the app's own environment, read from the kernel, nor its login shell's), its values
// scrubbed as a hook's variables are (a secret's by its name or shape redacted, a list of folders
// <path>), and the run's places in them (desktopPlaces) and the machine's names placeholders.
func launchOf(pid, top int, places map[string]string) (Launch, error) {
	argv, env, err := procArgs(pid)
	if err != nil {
		return Launch{}, err
	}
	had := loginShellEnv()
	if _, appEnv, err := procArgs(top); err == nil {
		for _, kv := range appEnv {
			k, v, _ := strings.Cut(kv, "=")
			had[k] = v
		}
	}
	l := Launch{Argv: argv, EnvValues: map[string]string{}}
	m := machineOf()
	if chain := ancestry(pid, top); len(chain) > 1 {
		l.Parents = chain[1:]
	}
	for i, a := range l.Argv {
		if secretValueRE.MatchString(a) {
			l.Argv[i] = "<redacted>"
		}
	}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if was, ok := had[k]; ok && was == v || sessionEnv[k] && !ok {
			l.Inherited++
			continue
		}
		l.Env = append(l.Env, k)
		switch {
		case v != "" && (secretName(k) || secretValueRE.MatchString(v)):
			l.EnvValues[k] = "<redacted>"
		case pathList(v):
			l.EnvValues[k] = "<path>"
		case sessionEnv[k]:
			l.EnvValues[k] = "<session>" // the app changed it: to what is the machine's
		case runKey.MatchString(v):
			l.EnvValues[k] = "<key>" // a key the app made for this run (a public key, base64)
		default:
			l.EnvValues[k] = machineNames(v, places, m)
		}
	}
	sort.Strings(l.Env)
	l.Env = slices.Compact(l.Env)
	return l, nil
}

// RecordLaunch records how the app launched its agent, as the "launch" probe.
func RecordLaunch(t *testing.T, b Binary, l Launch) {
	t.Helper()
	RecordProbe(t, b, "launch", l)
}
