package cmd

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/adapter"
	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/doctor"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/hookmgr"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	"github.com/miradorlabs/terma-cli/internal/migrate"
	"github.com/miradorlabs/terma-cli/internal/output"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/shim"
	"github.com/miradorlabs/terma-cli/internal/spinner"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

func newDoctorCommand() *cobra.Command {
	var skipCommit bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Verify the whole chain end to end and report setup readiness",
		Long: `Checks every link between a coding agent and the Terma backend: the binary,
your sign-in, the repository binding, the installed hooks and adapters, the
harness export, a scratch commit in a temporary worktree (does the hook actually
stamp a trailer?), the event spool, and the backend round-trip.

Every failure names the command that fixes it, and the report ends with the
remaining steps to complete setup.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if executeDoctor(cmd, skipCommit).Failed() {
				return errors.New("some checks failed")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&skipCommit, "skip-commit", false, "do not make a scratch commit in a temporary worktree")
	return cmd
}

// executeDoctor streams each check as it finishes and prints the remaining setup
// actions. It is the shared body of `terma doctor` and the verification `terma install`
// runs at the end; the caller decides what a failure means.
func executeDoctor(cmd *cobra.Command, skipCommit bool) doctor.Report {
	out := cmd.OutOrStdout()
	// Each check prints the moment it finishes, with the mark spinning beside the one
	// still running: the round-trip wait is long enough that a report printed only at
	// the end looks like a hang.
	sp := spinner.New(cmd.ErrOrStderr())
	report := runDoctor(cmd.Context(), skipCommit, doctorProgress{
		start: func(name string) { sp.Start(name + "…") },
		note:  sp.Update,
		done: func(c doctor.Check) {
			sp.Stop()
			doctor.RenderCheck(out, c, doctor.NameWidth)
		},
	})
	sp.Stop()
	doctor.RenderSummary(out, report)
	return report
}

// agentHooksCheck reports, for the agents wired in this repository, whether their hooks
// are in place and whether the agent will run them. It is shared by doctor and status so
// the two cannot disagree about it.
//
// The repository's hooks files are the record of what is wired (adapter.Wired). An agent
// they wire is checked whoever uses it: wiring an older terma wrote is stale for everyone.
// An agent they do not wire is missing only when this developer named it among their
// agents (mine) — a repository nobody opens in Cursor is not missing anything, and a
// developer who has not said which agents they use is not told about every agent terma
// knows. Trust is different: some agents refuse to run a committed hook until the
// developer has trusted it, or the repository, once from inside the agent — the wiring
// looks perfect and nothing runs, a silence worth naming — but only for an agent this
// developer uses (mine; empty means "has not said", so all of them). A colleague's agent
// is naturally untrusted here and costs this developer nothing.
func agentHooksCheck(root string, mine []string) doctor.Check {
	var parts []string
	var fix string
	ready, of := 0, 0
	status := doctor.Pass
	problem := func(f string) {
		status = doctor.Warn
		if fix == "" {
			fix = f
		}
	}
	for _, a := range adapter.All() {
		if a.HooksPath() == "" {
			continue
		}
		// Codex Desktop is wired through the Codex hooks file.
		named := slices.Contains(mine, a.Name()) || (a.Name() == routing.AgentCodex && slices.Contains(mine, codexDesktopAgent))
		if !adapter.Wired(root, a) {
			if named {
				of++
				parts = append(parts, a.DisplayName()+" hooks missing")
				problem("terma install")
			}
			continue
		}
		used := len(mine) == 0 || named
		if used {
			of++
		}
		plan, err := a.Plan(root, true)
		if err != nil {
			parts = append(parts, a.DisplayName()+" hooks could not be read: "+err.Error())
			problem("repair " + a.HooksPath() + "; then run terma install")
			continue
		}
		if !plan.Empty() {
			// Wired, so the file is there: an earlier terma wrote it, and a refresh
			// rewrites it without re-asking everything install asks.
			parts = append(parts, a.DisplayName()+" hooks out of date")
			problem("terma update --refresh")
			continue
		}
		part := a.DisplayName() + " hooks present"
		trusting, gated := a.(adapter.Trusting)
		if !gated || !used {
			if used {
				ready++
			}
			parts = append(parts, part)
			continue
		}
		trust, err := trusting.Trust(root)
		switch {
		case err != nil:
			// Unreadable is not untrusted: say so, and do not count it against anyone.
			part += " (could not read " + a.DisplayName() + "'s trust record: " + err.Error() + ")"
			ready++
		case !trust.Trusted:
			part += trust.Detail
			problem(trust.Fix)
		default:
			part += trust.Detail
			ready++
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return doctor.Check{Status: doctor.Skip, Detail: "no agent hooks are wired in this repository"}
	}
	return doctor.Check{Status: status, Detail: strings.Join(parts, "; "), Fix: fix, Ready: ready, Of: of}
}

// wellKnownBinDirs are where a terma binary gets installed besides wherever PATH points
// today: the install script's and Homebrew's directories, Go's, and the system one that
// apps started outside a shell search first. A variable so tests do not depend on what
// the machine running them has installed.
var wellKnownBinDirs = func() []string {
	dirs := []string{"/usr/local/bin", "/opt/homebrew/bin", "/usr/bin"}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"), filepath.Join(home, "go", "bin"))
	}
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		dirs = append(dirs, filepath.Join(filepath.SplitList(gopath)[0], "bin"))
	}
	return dirs
}

// otherTermas lists the terma binaries on this machine that are a different build from
// primary (the one `terma` resolves to here), each with when it was installed. It looks
// along PATH and in wellKnownBinDirs, never in terma's own shim directory, and compares
// contents — it does not run what it finds. A copy of the same build, or a link to the
// same file, is not reported: having two is only a problem when they disagree.
func otherTermas(primary string) []string {
	want, err := installedBinaryDigest(primary)
	if err != nil {
		return nil
	}
	primaryInfo, _ := os.Stat(primary)
	shimDir, _ := shim.ShimBinDir()
	name := "terma"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	var out []string
	seen := map[string]bool{}
	for _, dir := range append(filepath.SplitList(os.Getenv("PATH")), wellKnownBinDirs()...) {
		if dir == "" || filepath.Clean(dir) == filepath.Clean(shimDir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate) // follows symlinks: a link to primary is primary
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 || os.SameFile(info, primaryInfo) {
			continue
		}
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil || seen[resolved] {
			continue
		}
		seen[resolved] = true
		if got, err := installedBinaryDigest(candidate); err != nil || got == want {
			continue
		}
		out = append(out, tildePath(candidate)+" (installed "+info.ModTime().Format("2006-01-02 15:04")+")")
	}
	return out
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// npmLauncherDigest pins the published npm launcher. A path named terma in an
// npm layout is not enough to trust it: the launcher is executable JavaScript,
// and doctor must never run a PATH candidate to learn where it points.
const npmLauncherDigest = "a90d18d5df9946c37f39c80fe34f178202a1f65bf58abffaf0eb1f6f4f15cedb"

// installedBinary resolves only the official npm launcher to its vendor binary.
// For every other PATH entry, including an edited npm launcher, compare the file
// itself. npm's bin link and the package-local bin file both lead to this path.
func installedBinary(path string) string {
	if runtime.GOOS == "windows" {
		return path
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !strings.HasSuffix(filepath.ToSlash(resolved), "/node_modules/@miradorlabs/terma/bin/terma.js") {
		return path
	}
	if digest, err := fileDigest(resolved); err != nil || digest != npmLauncherDigest {
		return path
	}
	vendor := filepath.Clean(filepath.Join(filepath.Dir(resolved), "..", "vendor", "terma"))
	info, err := os.Stat(vendor)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return path
	}
	return vendor
}

func installedBinaryDigest(path string) (string, error) {
	return fileDigest(installedBinary(path))
}

// addToPathCommand is the command a developer runs to put dir on PATH for good, in their
// own shell: the line appended to the startup file terma knows for it and read into this
// shell, or for fish, fish_add_path, which keeps the entry itself. A shell terma does not
// know gets the line for this shell alone. terma never runs it: this is the developer's
// own binary directory, not the shim block install manages.
func addToPathCommand(dir string) string {
	rc, ok := shim.ShellRC()
	if !ok {
		return `export PATH="` + dir + `:$PATH"`
	}
	line := rc.PathLine(dir)
	if rc.Shell == "fish" {
		return line
	}
	file := shellPath(rc.Path)
	return "echo '" + strings.ReplaceAll(line, "'", `'\''`) + "' >> " + file + " && " + reloadCommand(file)
}

// shellPath writes a path for a command line: ~/… when that needs no quoting, else the
// full path in single quotes.
func shellPath(path string) string {
	const plain = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-~"
	if short := tildePath(path); !strings.ContainsFunc(short, func(r rune) bool { return !strings.ContainsRune(plain, r) }) {
		return short
	}
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// doctorProgress is how runDoctor reports as it goes: a check starting, a word about
// what a long one is waiting on, and a check finishing. Every field is optional.
type doctorProgress struct {
	start func(name string)
	note  func(text string)
	done  func(c doctor.Check)
}

func (p doctorProgress) starting(name string) {
	if p.start != nil {
		p.start(name)
	}
}

func (p doctorProgress) noting(text string) {
	if p.note != nil {
		p.note(text)
	}
}

func (p doctorProgress) finished(c doctor.Check) {
	if p.done != nil {
		p.done(c)
	}
}

// runDoctor runs every check. The checks are ordered so each later one can assume
// the earlier ones' facts (a repo, a binding, ...), and a missing prerequisite is
// reported as a skip rather than a second failure.
func runDoctor(ctx context.Context, skipCommit bool, progress doctorProgress) doctor.Report {
	var checks []doctor.Check
	add := func(c doctor.Check) {
		checks = append(checks, c)
		progress.finished(c)
	}
	timed := func(key, name string, fn func() doctor.Check) {
		progress.starting(name)
		start := time.Now()
		c := fn()
		c.Key, c.Name, c.Duration = key, name, time.Since(start)
		add(c)
	}

	// 1. Binary.
	var binaryCheck doctor.Check
	timed(doctor.KeyBinary, "terma on PATH", func() doctor.Check {
		binaryCheck = doctorBinaryCheck()
		return binaryCheck
	})

	// 1b. Saved state. Every start migrates it, so this is a line only when an update
	// left a migration pending or failed.
	if c, ok := stateCheck(); ok {
		timed(doctor.KeyState, "saved state migrated", func() doctor.Check { return c })
	}

	// 2. Sign-in.
	cfg, err := loadConfig()
	if err != nil {
		add(doctor.Check{Key: doctor.KeyAuth, Name: "configuration", Status: doctor.Fail, Detail: err.Error()})
		return doctor.Build(checks)
	}
	d := &doctorRun{ctx: ctx, cfg: cfg, skipCommit: skipCommit, progress: progress, binaryCheck: binaryCheck}
	timed(doctor.KeyAuth, "signed in", d.signedIn)

	// 3. Repository binding.
	d.root, d.gitDir, d.repoErr = workspaceHere(ctx)
	d.nonGit = d.repoErr == nil && d.gitDir == ""
	timed(doctor.KeyProject, "repository bound", d.repositoryBound)
	d.projectID = cfg.ProjectID
	if d.bound != nil {
		d.projectID = d.bound.Project.ID
	}

	// 4. Hooks + adapters.
	timed(doctor.KeyHooks, "commit hooks installed", d.commitHooks)

	// 4b. The agents' own hooks. A commit is stamped with the session that touched its
	// files, and a session exists only because its agent's hooks announced it — so this
	// is its own check, and a fraction: one agent that cannot run its hooks yet costs
	// that agent's commits, not the whole repository's.
	timed(doctor.KeyAgentHooks, "agent hooks run", d.agentHooks)

	// 5. Harness export.
	timed(doctor.KeyHarness, "agent exporting to Terma", d.agentsExporting)

	// 5b. Status line: the payload Claude Code hands its status line carries the
	// plan's own rate-limit windows, the strongest funding evidence a machine
	// produces. Only worth a line when Claude Code is here and connected.
	if (harness.Claude{}).Detect(ctx).Found {
		timed(doctor.KeyStatusLine, "Claude Code status line", d.statusLine)
	}

	// 6. Scratch commit.
	timed(doctor.KeyScratch, "scratch commit stamped", d.scratchCommit)

	// 7. Spool + backend round-trip.
	timed(doctor.KeySpool, "event spool", d.eventSpool)
	timed(doctor.KeyBackend, "backend receives events", d.backendReceives)

	// (A future GitHub App will report merge/revert/CI outcomes; until it ships there is
	// nothing to check and nothing to suggest, so it is not listed here.)

	return doctor.Build(checks)
}

// doctorRun carries what one check establishes for the ones after it: runDoctor orders
// the checks so each can assume the earlier ones' facts.
type doctorRun struct {
	ctx        context.Context
	cfg        *config.Config
	skipCommit bool
	progress   doctorProgress

	// The repository the CLI stands in, from before the binding check.
	root    string
	gitDir  string
	repoErr error
	nonGit  bool
	// bound is set by repositoryBound, and projectID follows it.
	bound     *termaproject.File
	projectID string
	// scratchSHA is set by scratchCommit, for backendReceives to read back.
	scratchSHA  string
	harnesses   []harnessVerdict
	binaryCheck doctor.Check
}

// installed reports whether the CLI stands in a repository `terma install` has bound.
func (d *doctorRun) installed() bool { return d.repoErr == nil && d.bound != nil }

func doctorBinaryCheck() doctor.Check {
	exe, _ := os.Executable()
	return doctorBinaryCheckFor(exe)
}

func doctorBinaryCheckFor(exe string) doctor.Check {
	path, err := exec.LookPath("terma")
	if err != nil {
		return doctor.Check{Status: doctor.Fail, Detail: "hooks call `terma` by name and will not find it",
			Fix: "run `" + addToPathCommand(filepath.Dir(exe)) + "` to put " + filepath.Dir(exe) + " on PATH (or reinstall with the install script)"}
	}
	if current, err := fileDigest(exe); err == nil {
		if installed, err := installedBinaryDigest(path); err == nil && current != installed {
			return doctor.Check{Status: doctor.Warn,
				Detail: path + binaryBuildLabel(path) + "; hooks run a different build from " + exe,
				Fix:    "run `" + addToPathCommand(filepath.Dir(exe)) + "` to put " + filepath.Dir(exe) + " first on PATH, or replace " + path + " with this build; then run `terma doctor`"}
		}
	}
	// Hooks call `terma` by name, and the name does not resolve the same way
	// everywhere: an app started from the Dock gets the system's PATH, not the
	// shell's, so a second copy in /usr/local/bin is the one Cursor's hooks run. When
	// that copy is another build, the same repository behaves two ways depending on
	// where the agent was launched — and a build from before .terma/settings.json
	// does not see the binding at all.
	if others := otherTermas(path); len(others) > 0 {
		return doctor.Check{Status: doctor.Warn,
			Detail: path + binaryBuildLabel(path) + "; a different build is also installed: " + strings.Join(others, ", "),
			Fix:    "replace or remove the other copy — an agent started outside this shell (from the Dock, an IDE) can resolve `terma` to it"}
	}
	return doctor.Check{Status: doctor.Pass, Detail: path + binaryBuildLabel(path)}
}

// binaryBuildLabel reads build metadata without running an executable found on PATH.
func binaryBuildLabel(path string) string {
	info, err := buildinfo.ReadFile(installedBinary(path))
	if err != nil {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			return " (build " + setting.Value[:min(7, len(setting.Value))] + ")"
		}
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return " (" + info.Main.Version + ")"
	}
	return ""
}

func (d *doctorRun) signedIn() doctor.Check {
	cfg := d.cfg
	if cfg.APIKey != "" {
		return doctor.Check{Status: doctor.Pass, Detail: "using TERMA_API_KEY"}
	}
	cred, err := auth.LoadCredential(cfg.ProfileName)
	if err != nil {
		return doctor.Check{Status: doctor.Fail, Detail: "no credential for this environment", Fix: "terma setup"}
	}
	if err := cred.CheckEnvironment(cfg.AuthURL); err != nil {
		return doctor.Check{Status: doctor.Fail, Detail: "signed in against a different environment", Fix: "terma setup"}
	}
	who := firstNonEmpty(cred.UserEmail, "your account")
	env := ""
	if cfg.Environment != config.EnvProd {
		env = " [" + cfg.Environment + "]"
	}
	return doctor.Check{Status: doctor.Pass, Detail: who + " in " + nameOrID(cfg.OrganizationName, cred.OrganizationID) + env}
}

func (d *doctorRun) repositoryBound() doctor.Check {
	if d.repoErr != nil {
		return doctor.Check{Status: doctor.Fail, Detail: d.repoErr.Error()}
	}
	f, from, err := termaproject.Resolve(d.root, d.gitDir)
	if err != nil {
		where := d.root
		if _, main, ok := gitx.LinkedWorktreeFS(d.gitDir); ok && main != "" {
			where += " or its main checkout " + main
		}
		return doctor.Check{Status: doctor.Fail, Detail: "no " + termaproject.FileName + " in " + where, Fix: "terma install"}
	}
	d.bound = f
	return doctor.Check{Status: doctor.Pass, Detail: nameOrID(f.Project.Name, f.Project.ID) + throughMain(d.root, from)}
}

// throughMain says, for a linked worktree bound through its main checkout, where the
// binding came from; it is empty when the checkout has its own.
func throughMain(root, from string) string {
	if from == "" || from == root {
		return ""
	}
	return " (through the main checkout " + from + ")"
}

func (d *doctorRun) commitHooks() doctor.Check {
	if d.nonGit {
		return doctor.Check{Status: doctor.Skip, Detail: "not a Git repository"}
	}
	if !d.installed() {
		return doctor.Check{Status: doctor.Skip, Detail: "needs an installed repository"}
	}
	return doctorHooksCheck(judgeHookWiring(d.ctx, d.root, d.bound))
}

func (d *doctorRun) agentHooks() doctor.Check {
	if !d.installed() {
		return doctor.Check{Status: doctor.Skip, Detail: "needs an installed repository"}
	}
	return agentHooksCheck(d.root, selectedForRepo(d.projectID, d.cfg.Harnesses))
}

func (d *doctorRun) agentsExporting() doctor.Check {
	if claim.Enabled() {
		return relayDoctorCheck(d.projectID, d.cfg.Harnesses)
	}
	d.harnesses = judgeSelectedHarnesses(d.ctx, d.cfg.OTLPURL, d.projectID, d.root, d.cfg.Harnesses)
	return doctorHarnessCheck(d.harnesses, d.cfg.OTLPURL, d.projectID, d.installed())
}

func (d *doctorRun) statusLine() doctor.Check {
	repoRoot := ""
	if d.repoErr == nil {
		repoRoot = d.root
	}
	return doctorStatusLineCheck(judgeStatusLine(repoRoot))
}

func (d *doctorRun) scratchCommit() doctor.Check {
	if d.nonGit {
		return doctor.Check{Status: doctor.Skip, Detail: "not a Git repository"}
	}
	if !d.installed() {
		return doctor.Check{Status: doctor.Skip, Detail: "needs an installed repository"}
	}
	if d.skipCommit {
		return doctor.Check{Status: doctor.Skip, Detail: "--skip-commit"}
	}
	sha, c := scratchCommit(d.ctx, d.root, d.bound)
	d.scratchSHA = sha
	return c
}

// stateCheck reports saved state this build has not finished migrating, and has nothing
// to say (false) when every migration it has is applied.
func stateCheck() (doctor.Check, bool) {
	dir, err := config.Dir()
	if err != nil || !migrate.Pending(dir) {
		return doctor.Check{}, false
	}
	s, err := migrate.Load(dir)
	switch {
	case err != nil:
		return doctor.Check{Status: doctor.Warn, Detail: "the migration record cannot be read: " + err.Error(), Fix: "terma update --refresh"}, true
	case s.Failed != nil:
		return doctor.Check{Status: doctor.Fail, Detail: fmt.Sprintf("%s failed: %s", s.Failed.Name, s.Failed.Error), Fix: "terma update --refresh"}, true
	}
	return doctor.Check{Status: doctor.Warn, Detail: fmt.Sprintf("%d migration(s) from this update not applied yet", migrate.Remaining(s)), Fix: "terma update --refresh"}, true
}

func (d *doctorRun) eventSpool() doctor.Check {
	s := openSpool()
	if s == nil {
		return doctor.Check{Status: doctor.Fail, Detail: "cannot open the spool directory"}
	}
	n, _, _ := s.Pending()
	// Reading a queue proves nothing about writing one, and a spool that
	// cannot be appended to is how a commit goes unbilled without anyone
	// hearing about it: the hook swallows the error by design.
	if err := s.Writable(); err != nil {
		return doctor.Check{Status: doctor.Fail, Detail: "events cannot be written to the spool: " + err.Error(), Fix: "check permissions and free space on the config directory"}
	}
	if d.projectID != "" && keystore.Get(d.projectID) == "" {
		name := d.cfg.ProjectName
		if d.bound != nil {
			name = d.bound.Project.Name
		}
		return doctor.Check{Status: doctor.Fail, Detail: fmt.Sprintf("%d queued; no project key stored for %s", n, nameOrID(name, d.projectID)), Fix: "terma install"}
	}
	return doctor.Check{Status: doctor.Pass, Detail: fmt.Sprintf("%d queued, spool writable", n)}
}

func (d *doctorRun) backendReceives() doctor.Check {
	if d.binaryCheck.Status == doctor.Warn || d.binaryCheck.Status == doctor.Fail {
		return doctor.Check{Status: doctor.Warn, Inconclusive: true,
			Detail: "resolve the missing or conflicting Terma executables before verifying hook delivery",
			Fix:    d.binaryCheck.Fix}
	}
	if d.projectID != "" && keystore.Get(d.projectID) == "" {
		return doctor.Check{Status: doctor.Skip, Detail: "no project key on this machine; nothing can be delivered", Fix: "terma install"}
	}
	res, err := flushSpool(d.ctx, true, 0)
	if err != nil {
		return doctor.Check{Status: doctor.Fail, Detail: err.Error(), Fix: "terma install"}
	}
	// The flush delivers every project's queued events, not only this repository's.
	// Another project's refusal says nothing about this repository's chain — it read
	// as this repository's credentials failing — so it is a warning here, named with
	// its own project and host. This project's refusal, or a failure no project owns
	// (a lock, the deadline), fails the check.
	var others []string
	var othersFix string
	if res.Err != nil {
		if f, ok := res.failureFor(d.projectID); ok {
			return doctor.Check{Status: doctor.Fail, Detail: "this project's events were not delivered: " + describeFailure(f), Fix: failureFix(f)}
		}
		if len(res.Failures) == 0 {
			return doctor.Check{Status: doctor.Fail, Detail: "flush failed: " + res.Err.Error(), Fix: "check the network, then run `terma spool flush --force`"}
		}
		for _, f := range res.Failures {
			others = append(others, "another project's events were not delivered: "+f.ProjectID+" "+describeFailure(f))
		}
		othersFix = failureFix(res.Failures[0])
	}
	delivered, undelivered := describeFlush(res)
	hosts := res.Endpoints
	if len(hosts) == 0 {
		hosts = []string{projectEndpoint(d.cfg, d.projectID)}
	}
	flushDetail := "flushed " + delivered + " to " + strings.Join(hosts, ", ")
	if res.Sent == 0 && len(undelivered) == 0 {
		flushDetail = "nothing queued at verification time (hooks may already have flushed)"
	}
	detail := strings.Join(append(append([]string{flushDetail}, undelivered...), others...), "; ")
	if d.scratchSHA == "" {
		return doctor.Check{Status: doctor.Skip, Inconclusive: true, Detail: detail + "; no scratch commit event to verify", Fix: othersFix}
	}
	// Round-trip: the scratch commit's event must be readable back.
	if ok, err := waitForCommitEvent(d.ctx, d.cfg, d.projectID, d.scratchSHA, d.progress); err != nil {
		return doctor.Check{Status: doctor.Warn, Inconclusive: true, Detail: detail + "; could not confirm the round-trip via the API (" + err.Error() + ")", Fix: "terma doctor"}
	} else if !ok {
		return doctor.Check{Status: doctor.Warn, Inconclusive: true, Detail: detail + "; the scratch commit event was not visible via the API within " + roundTripWait.String(), Fix: "terma doctor"}
	}
	if len(others) > 0 {
		return doctor.Check{Status: doctor.Warn, Detail: detail + "; round-trip confirmed for this project", Fix: othersFix}
	}
	return doctor.Check{Status: doctor.Pass, Detail: detail + "; round-trip confirmed"}
}

// describeFailure words one project's failed delivery with the host that refused it:
// the host is the half of the story a developer with projects in two environments
// cannot guess.
func describeFailure(f projectFailure) string {
	if _, ok := errors.AsType[*spool.IngestError](f.Err); ok {
		return "refused by " + f.Endpoint + " (" + f.Err.Error() + ")"
	}
	return "not sent to " + f.Endpoint + " (" + f.Err.Error() + ")"
}

// failureFix is the next step for a failed delivery. A refused key is not a network
// problem, and pointing at the network for one sent a developer to check a
// connection that was working.
func failureFix(f projectFailure) string {
	if ingest, ok := errors.AsType[*spool.IngestError](f.Err); ok && ingest.KeyRefused() {
		return "the key this machine holds for project " + f.ProjectID + " was refused by " + f.Endpoint + " — it may have been revoked, or belong to another environment"
	}
	return "check the network and " + f.Endpoint + ", then run `terma spool flush --force`"
}

// scratchCommit proves the installed hook chain works: a detached temporary
// worktree gets a seeded session manifest and one file, is committed through the
// real hooks, and the resulting message is checked for the trailer. The worktree
// and its unreferenced commit are removed afterwards; nothing touches the user's
// branch.
func scratchCommit(ctx context.Context, root string, bound *termaproject.File) (string, doctor.Check) {
	if gitx.HeadSHA(ctx, root) == "" {
		return "", doctor.Check{Status: doctor.Skip, Detail: "repository has no commits yet"}
	}
	clearStaleScratchWorktrees(ctx, root)
	tmp, err := os.MkdirTemp("", scratchDirPrefix+"*")
	if err != nil {
		return "", doctor.Check{Status: doctor.Fail, Detail: err.Error()}
	}
	wt := filepath.Join(tmp, scratchWorktreeName)
	cleanup := func() {
		removeScratchWorktree(ctx, root, wt)
		_ = os.RemoveAll(tmp)
		_, _ = gitx.Git(context.WithoutCancel(ctx), root, "worktree", "prune")
	}
	if _, err := gitx.GitWithin(ctx, scratchGitTimeout, root, "worktree", "add", "--detach", "-q", wt, "HEAD"); err != nil {
		cleanup()
		return "", doctor.Check{Status: doctor.Fail, Detail: "could not create a temporary worktree: " + err.Error(), Fix: "`terma doctor --skip-commit` runs every other check"}
	}
	defer cleanup()
	// Seed the binding into the worktree so the post-commit hook attributes the scratch
	// commit to this project — the round-trip needs a routable terma.commit event. The
	// worktree is a checkout of HEAD, so a repository that commits .terma/settings.json
	// already has it; one that gitignores its own binding (like terma-cli) does not. This
	// build's hooks would find the main checkout's through the worktree link
	// (project.Resolve), but the hooks run whatever terma is on PATH, and a build from
	// before that would spool a commit event with no project id, dropped as unroutable.
	if bound != nil {
		_ = termaproject.Save(wt, bound)
	}

	_, wtGitDir, err := gitx.Locate(ctx, wt)
	if err != nil {
		return "", doctor.Check{Status: doctor.Fail, Detail: err.Error()}
	}
	sessionID := fmt.Sprintf("doctor-%d", time.Now().UnixNano())
	file := ".terma-doctor"
	if err := os.WriteFile(filepath.Join(wt, file), []byte("terma doctor scratch file\n"), 0o644); err != nil {
		return "", doctor.Check{Status: doctor.Fail, Detail: err.Error()}
	}
	store := session.Open(wtGitDir)
	if err := store.Touch(session.Session{ID: sessionID, Tool: "terma-doctor"}, []string{file}, time.Now()); err != nil {
		return "", doctor.Check{Status: doctor.Fail, Detail: "could not seed a session manifest: " + err.Error()}
	}
	if _, err := gitx.Git(ctx, wt, "add", file); err != nil {
		return "", doctor.Check{Status: doctor.Fail, Detail: err.Error()}
	}
	commitArgs := []string{"-c", "commit.gpgsign=false"}
	// A new worktree has its own config.worktree and checks out HEAD. Immediately
	// after install, neither the per-worktree core.hooksPath nor the uncommitted
	// shim files exist there. Point the scratch commit at the hooks this checkout
	// actually runs, including files the developer has yet to commit.
	if gitx.ConfigGet(ctx, root, "core.hooksPath") != "" {
		hooksPath, err := gitx.Git(ctx, root, "config", "--path", "--get", "core.hooksPath")
		if err != nil {
			return "", doctor.Check{Status: doctor.Fail, Detail: "could not resolve core.hooksPath: " + err.Error()}
		}
		if !filepath.IsAbs(hooksPath) {
			hooksPath = filepath.Join(root, hooksPath)
		}
		commitArgs = append(commitArgs, "-c", "core.hooksPath="+hooksPath)
	}
	commitArgs = append(commitArgs, "commit", "-q", "-m", "terma doctor scratch commit")
	if _, err := gitx.GitWithin(ctx, scratchGitTimeout, wt, commitArgs...); err != nil {
		return "", doctor.Check{Status: doctor.Fail, Detail: "scratch commit failed: " + err.Error(), Fix: "a hook is failing the commit — run it with TERMA_DEBUG=1 to see why"}
	}
	sha := gitx.HeadSHA(ctx, wt)
	message, err := gitx.CommitMessage(ctx, wt, "HEAD")
	if err != nil {
		return sha, doctor.Check{Status: doctor.Fail, Detail: err.Error()}
	}
	for _, t := range trailer.Parse(message, gitx.CommentChar(ctx, wt)) {
		if t.SessionID == sessionID {
			return sha, doctor.Check{Status: doctor.Pass, Detail: "prepare-commit-msg stamped Agent-Session-Id on " + sha[:7]}
		}
	}
	return sha, doctor.Check{Status: doctor.Fail, Detail: "the commit went through but carried no Agent-Session-Id trailer", Fix: "the hook did not run: re-run `terma install`, then the hook manager's install step (see its notes)"}
}

const (
	// scratchGitTimeout bounds the scratch worktree's checkout, commit and removal.
	// They ran under gitx.Timeout, a hook's 2-second budget: a checkout of 9,270
	// files and 2.7 GB takes 11 s, so doctor failed that repository on every run
	// with "git worktree: signal: killed". The commit runs the repository's own
	// hooks too, which are not terma's to budget.
	scratchGitTimeout = 2 * time.Minute
	// scratchDirPrefix and scratchWorktreeName name the scratch worktree
	// (<tmp>/terma-doctor-*/wt), so a later run can recognise one an earlier run
	// could not clean up.
	scratchDirPrefix    = "terma-doctor-"
	scratchWorktreeName = "wt"
)

// removeScratchWorktree unregisters a scratch worktree. --force twice, because an
// add killed mid-checkout leaves its registration locked ("initializing"), and
// `git worktree prune` passes over a locked one for ever. It runs even when doctor
// is interrupted, since that is when a half-made worktree is most likely.
func removeScratchWorktree(ctx context.Context, root, wt string) {
	_, _ = gitx.GitWithin(context.WithoutCancel(ctx), scratchGitTimeout, root, "worktree", "remove", "--force", "--force", wt)
}

// clearStaleScratchWorktrees removes what earlier runs could not: registrations of
// a scratch worktree whose directory is gone. Before scratchGitTimeout, every run
// against a large repository left one behind, locked. Only doctor's own naming is
// touched, and only once the directory no longer exists.
func clearStaleScratchWorktrees(ctx context.Context, root string) {
	out, err := gitx.Git(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(out, "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if !ok || filepath.Base(path) != scratchWorktreeName || !strings.HasPrefix(filepath.Base(filepath.Dir(path)), scratchDirPrefix) {
			continue
		}
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			removeScratchWorktree(ctx, root, path)
		}
	}
}

// doctorHooksCheck is doctor's wording for the commit-hook verdict. An unreadable plan
// fails here; status does not look at why a plan could not be computed.
func doctorHooksCheck(w hookWiring) doctor.Check {
	switch {
	case w.err != nil:
		return doctor.Check{Status: doctor.Fail, Detail: w.err.Error(), Fix: "terma install"}
	case w.changes > 0 && w.stale == w.changes:
		return doctor.Check{Status: doctor.Fail, Detail: fmt.Sprintf("%s wiring was written by an earlier terma (%d file(s) out of date)", w.manager, w.stale), Fix: "terma update --refresh"}
	case w.changes > 0:
		return doctor.Check{Status: doctor.Fail, Detail: fmt.Sprintf("%s wiring is missing (%d file change(s))", w.manager, w.changes), Fix: "terma install"}
	case w.unpointed:
		return doctor.Check{Status: doctor.Fail, Detail: "shims are committed but git is not pointed at them in this clone (core.hooksPath=" + firstNonEmpty(w.hooksPath, "unset") + ")", Fix: "terma install"}
	case w.manager == hookmgr.GitShim:
		return doctor.Check{Status: doctor.Pass, Detail: string(w.manager) + " shims, core.hooksPath set"}
	}
	return doctor.Check{Status: doctor.Pass, Detail: string(w.manager)}
}

// doctorStatusLineCheck is doctor's wording for the status-line verdict.
func doctorStatusLineCheck(v statusLineVerdict) doctor.Check {
	switch v.capture {
	case statusLineUnknown:
		return doctor.Check{Status: doctor.Warn, Detail: v.err.Error()}
	case statusLineOverridden:
		return doctor.Check{Status: doctor.Warn,
			Detail: "overridden by " + strings.Join(v.overrides, ", ") + "; plan usage is not captured in this repository",
			Fix:    "remove statusLine from that file, or accept that this repository does not report plan usage"}
	case statusLineBehind:
		return doctor.Check{Status: doctor.Pass, Detail: "capturing plan usage; " + output.SanitizeTerminal(v.renderer) + " runs behind it"}
	case statusLineDefault:
		return doctor.Check{Status: doctor.Pass, Detail: "capturing plan usage (terma's default line)"}
	case statusLineReplaced:
		return doctor.Check{Status: doctor.Warn, Detail: "replaced by your own status line since terma wrapped it; plan usage is not captured", Fix: "terma install"}
	}
	return doctor.Check{Status: doctor.Warn, Detail: "not wrapped; plan usage is not captured", Fix: "terma install"}
}

// doctorHarnessCheck folds every agent's verdict into doctor's one export check. bound
// says the CLI stands in an installed repository, the only place "this repository does
// not route it" means anything.
func doctorHarnessCheck(verdicts []harnessVerdict, otlpURL, projectID string, bound bool) (check doctor.Check) {
	// Count working agents independently of another agent's failure.
	defer func() {
		check.Of = len(verdicts)
		for _, v := range verdicts {
			if _, ready := statusAgent(v, bound); ready {
				check.Ready++
			}
		}
	}()
	var problems, fixes []string
	for _, v := range verdicts {
		if v.emissionProblem != "" {
			problems = append(problems, v.displayName+": "+v.emissionProblem)
			if !slices.Contains(fixes, v.emissionFix) {
				fixes = append(fixes, v.emissionFix)
			}
		}
	}
	if len(problems) > 0 {
		return doctor.Check{Status: doctor.Fail, Detail: strings.Join(problems, "; "), Fix: strings.Join(fixes, "; ")}
	}
	var connected, installed, repoDecides, silent []string
	for _, v := range verdicts {
		installed = append(installed, v.displayName)
		switch v.route {
		case routeOtherProject:
			return doctor.Check{Status: doctor.Fail, Detail: v.displayName + " reports to project " + v.otherProject + ", not " + projectID, Fix: "terma install"}
		case routeHooks:
			connected = append(connected, v.displayName+" (repository hooks)")
		case routeGlobal:
			connected = append(connected, v.displayName)
		case routeRepoDecides:
			connected = append(connected, v.displayName)
			repoDecides = append(repoDecides, v.displayName)
			// An agent that cannot carry a repository policy at all (Codex) cannot be
			// asked for by one either, so it is as silent here as one whose repository
			// simply has none. doctor used to skip it and report "this repository asks"
			// for a config status said sent nothing.
			if !v.repoAsks {
				silent = append(silent, v.displayName)
			}
		}
	}
	if len(installed) == 0 {
		return doctor.Check{Status: doctor.Fail, Detail: "no coding agent found (Claude Code, Codex, OpenCode)", Fix: "install one, then terma install"}
	}
	if len(connected) == 0 {
		return doctor.Check{Status: doctor.Fail, Detail: strings.Join(installed, ", ") + " installed but not exporting to " + otlpURL, Fix: "terma install"}
	}
	detail := strings.Join(connected, ", ") + " → " + otlpURL
	if len(repoDecides) == 0 {
		return doctor.Check{Status: doctor.Pass, Detail: detail}
	}
	// A globally-connected-but-silent harness that this repository neither routes nor
	// carries a committed policy for is the one case worth a word — and only in the
	// repository the developer is standing in.
	qualifier := "only where a repository asks"
	if len(repoDecides) < len(connected) {
		qualifier = strings.Join(repoDecides, ", ") + ": " + qualifier
	}
	detail += " (" + qualifier + ")"
	if !bound {
		return doctor.Check{Status: doctor.Pass, Detail: detail}
	}
	if len(silent) == 0 {
		return doctor.Check{Status: doctor.Pass, Detail: detail + "; this repository asks"}
	}
	return doctor.Check{
		Status: doctor.Fail,
		Detail: detail + "; this repository does not route " + strings.Join(silent, ", ") + " to its project, so its sessions send nothing",
		Fix:    "terma install",
	}
}

// roundTripWait bounds how long doctor waits for the scratch commit to be readable
// back; roundTripPoll is how often it looks. Polling every second rather than every
// few means the check ends within a second of the event landing, instead of waiting
// out the rest of a long interval.
const (
	roundTripWait = 20 * time.Second
	roundTripPoll = time.Second
)

// commitLogWindow is how far the terma.commit lookup reaches on each side of the time
// it centres on. The log store caps a query's span, so the lookup asks for a tight
// window around the commit rather than scanning back from now.
const commitLogWindow = time.Hour

// waitForCommitEvent polls the data API for the scratch commit's event.
func waitForCommitEvent(ctx context.Context, cfg *config.Config, projectID, sha string, progress doctorProgress) (bool, error) {
	// Query the project the scratch event used, independently of command overrides.
	queryConfig := *cfg
	queryConfig.ProjectID = projectID
	// Read it back where it was delivered. A project in another environment than the
	// active profile's is read from its own data API, with its own key: the signed-in
	// credential is bound to the active profile's auth host, and asking the profile's
	// API for the project's events found nothing on every run.
	if api := projectAPI(cfg, projectID); api != cfg.APIURL {
		if key := keystore.Get(projectID); key != "" {
			queryConfig.APIURL, queryConfig.APIKey = api, key
		}
	}
	client, err := newClient(&queryConfig)
	if err != nil {
		return false, err
	}
	started := time.Now()
	deadline := started.Add(roundTripWait)
	for {
		progress.noting(fmt.Sprintf("backend receives events… waiting for the round-trip (%ds of %ds)",
			int(time.Since(started).Seconds()), int(roundTripWait.Seconds())))
		// The scratch commit was made moments before this started, so its record sits
		// inside the window.
		rec, err := client.CommitLog(ctx, sha, started.Add(-commitLogWindow), started.Add(commitLogWindow))
		if err != nil {
			return false, err
		}
		if rec != nil {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(roundTripPoll):
		}
	}
}

// reloadCommand re-reads a startup file in the running shell: `source` where the shell
// has it (zsh, bash, fish), the POSIX `.` otherwise.
func reloadCommand(file string) string {
	switch filepath.Base(os.Getenv("SHELL")) {
	case "zsh", "bash", "fish":
		return "source " + file
	}
	return ". " + file
}
