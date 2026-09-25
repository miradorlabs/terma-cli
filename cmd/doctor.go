package cmd

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/shim"
	"github.com/miradorlabs/terma-cli/internal/spinner"
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
// Each agent's hooks are checked where the install asked for them: a repository nobody
// opens in Cursor is not missing anything, and an install that predates the adapter list
// recorded none and gets the default set. A hooks file that is missing is stale wiring
// whoever uses the agent. Trust is different: some agents refuse to run a committed hook
// until the developer has trusted it, or the repository, once from inside the agent — the
// wiring looks perfect and nothing runs, a silence worth naming — but only for an agent
// this developer uses (mine; empty means "has not said", so all of them). A colleague's
// agent is naturally untrusted here and costs this developer nothing.
func agentHooksCheck(root string, bound *termaproject.File, mine []string) doctor.Check {
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
	for _, name := range installedAdapters(bound) {
		a, ok := adapter.Lookup(name)
		if !ok || a.HooksPath() == "" {
			continue
		}
		used := len(mine) == 0 || slices.Contains(mine, name) || (name == "codex" && slices.Contains(mine, codexDesktopAgent))
		if used {
			of++
		}
		if plan, _ := a.Plan(root, true); !plan.Empty() {
			parts = append(parts, a.DisplayName()+" hooks missing")
			problem("terma install")
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
	want, err := fileDigest(primary)
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
		if got, err := fileDigest(candidate); err != nil || got == want {
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

// shimPathFix is what gets terma's shims ahead of the real binaries from here. `terma
// install` writes the PATH line; when it is already written and still last, the shell
// doctor runs in simply started before it, and no command fixes that.
func shimPathFix() string {
	rc, ok := shim.ShellRC()
	if !ok {
		shimDir, _ := shim.ShimBinDir()
		return `put the shim directory first on PATH, as the last PATH line of your shell's startup file: export PATH="` + shimDir + `:$PATH"`
	}
	switch state, _ := rc.State(); state {
	case shim.RCLast:
		return "open a new terminal — " + tildePath(rc.Path) + " already puts the shims first, and this shell started before it did"
	case shim.RCOvertaken:
		return "terma install (a later line in " + tildePath(rc.Path) + " puts the real binaries back in front; install moves terma's line to the end)"
	}
	return "terma install (it offers to put the shim directory on PATH in " + tildePath(rc.Path) + ")"
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
	d.root, _, d.repoErr = repoHere(ctx, "")
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
	timed(doctor.KeyRouting, "shell routing active", func() doctor.Check {
		return shellRoutingCheck(d.harnesses, d.installed(), selectedForRepo(d.projectID, d.cfg.Harnesses))
	})

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
	repoErr error
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
		return doctor.Check{Status: doctor.Fail, Detail: "hooks call `terma` by name and will not find it", Fix: "add " + filepath.Dir(exe) + " to PATH (or reinstall with the install script)"}
	}
	if current, err := fileDigest(exe); err == nil {
		if installed, err := fileDigest(path); err == nil && current != installed {
			return doctor.Check{Status: doctor.Warn,
				Detail: path + binaryBuildLabel(path) + "; hooks run a different build from " + exe,
				Fix:    "put " + filepath.Dir(exe) + " first on PATH, or replace " + path + " with this build; then run `terma doctor`"}
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
	info, err := buildinfo.ReadFile(path)
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
		return doctor.Check{Status: doctor.Skip, Detail: "not inside a git repository (repo checks skipped)"}
	}
	f, err := termaproject.Load(d.root)
	if err != nil {
		return doctor.Check{Status: doctor.Fail, Detail: "no " + termaproject.FileName + " in " + d.root, Fix: "terma install"}
	}
	d.bound = f
	return doctor.Check{Status: doctor.Pass, Detail: nameOrID(f.Project.Name, f.Project.ID)}
}

func (d *doctorRun) commitHooks() doctor.Check {
	if !d.installed() {
		return doctor.Check{Status: doctor.Skip, Detail: "needs an installed repository"}
	}
	return doctorHooksCheck(judgeHookWiring(d.ctx, d.root, d.bound))
}

func (d *doctorRun) agentHooks() doctor.Check {
	if !d.installed() {
		return doctor.Check{Status: doctor.Skip, Detail: "needs an installed repository"}
	}
	return agentHooksCheck(d.root, d.bound, selectedForRepo(d.projectID, d.cfg.Harnesses))
}

func (d *doctorRun) agentsExporting() doctor.Check {
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
	if res.Err != nil {
		return doctor.Check{Status: doctor.Fail, Detail: "flush failed: " + res.Err.Error(), Fix: "check network / OTLP endpoint " + d.cfg.OTLPURL}
	}
	delivered, undelivered := describeFlush(res)
	flushDetail := "flushed " + delivered + " to " + d.cfg.OTLPURL
	if res.Sent == 0 && len(undelivered) == 0 {
		flushDetail = "nothing queued at verification time (hooks may already have flushed)"
	}
	detail := strings.Join(append([]string{flushDetail}, undelivered...), "; ")
	if d.scratchSHA == "" {
		return doctor.Check{Status: doctor.Skip, Inconclusive: true, Detail: detail + "; no scratch commit event to verify"}
	}
	// Round-trip: the scratch commit's event must be readable back.
	if ok, err := waitForCommitEvent(d.ctx, d.cfg, d.projectID, d.scratchSHA, d.progress); err != nil {
		return doctor.Check{Status: doctor.Warn, Inconclusive: true, Detail: detail + "; could not confirm the round-trip via the API (" + err.Error() + ")", Fix: "terma doctor"}
	} else if !ok {
		return doctor.Check{Status: doctor.Warn, Inconclusive: true, Detail: detail + "; the scratch commit event was not visible via the API within " + roundTripWait.String(), Fix: "terma doctor"}
	}
	return doctor.Check{Status: doctor.Pass, Detail: detail + "; round-trip confirmed"}
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
	tmp, err := os.MkdirTemp("", "terma-doctor-*")
	if err != nil {
		return "", doctor.Check{Status: doctor.Fail, Detail: err.Error()}
	}
	wt := filepath.Join(tmp, "wt")
	if _, err := gitx.Git(ctx, root, "worktree", "add", "--detach", "-q", wt, "HEAD"); err != nil {
		_ = os.RemoveAll(tmp)
		return "", doctor.Check{Status: doctor.Fail, Detail: "could not create a temporary worktree: " + err.Error()}
	}
	defer func() {
		_, _ = gitx.Git(ctx, root, "worktree", "remove", "--force", wt)
		_, _ = gitx.Git(ctx, root, "worktree", "prune")
		_ = os.RemoveAll(tmp)
	}()
	// Seed the binding into the worktree so the post-commit hook attributes the scratch
	// commit to this project — the round-trip needs a routable terma.commit event. The
	// worktree is a checkout of HEAD, so a repository that commits .terma/settings.json
	// already has it; one that gitignores its own binding (like terma-cli) does not, and
	// without this the commit event would carry no project id and go unroutable.
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
	if _, err := gitx.Git(ctx, wt, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "terma doctor scratch commit"); err != nil {
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

// doctorHooksCheck is doctor's wording for the commit-hook verdict. An unreadable plan
// fails here; status does not look at why a plan could not be computed.
func doctorHooksCheck(w hookWiring) doctor.Check {
	switch {
	case w.err != nil:
		return doctor.Check{Status: doctor.Fail, Detail: w.err.Error(), Fix: "terma install"}
	case w.changes > 0:
		return doctor.Check{Status: doctor.Fail, Detail: fmt.Sprintf("%s wiring is missing or stale (%d file change(s))", w.manager, w.changes), Fix: "terma install"}
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
	var connected, installed, pendingShim, repoDecides, silent []string
	for _, v := range verdicts {
		installed = append(installed, v.displayName)
		switch v.route {
		case routeOtherProject:
			return doctor.Check{Status: doctor.Fail, Detail: v.displayName + " reports to project " + v.otherProject + ", not " + projectID, Fix: "terma install"}
		case routeLive:
			connected = append(connected, v.displayName+" (per-repo)")
		case routeGlobal:
			connected = append(connected, v.displayName)
		case routePending:
			pendingShim = append(pendingShim, v.displayName)
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
	// Routing configured but not on PATH is the honest "looks set up, sends nothing
	// yet" case — the exact gap that made doctor report a repo as fine while its
	// sessions used the global config. Surfaced with the fix that actually activates it.
	if len(pendingShim) > 0 {
		detail := "per-repo routing for " + strings.Join(pendingShim, ", ") + " is configured, but terma's shim directory is not ahead of the agent on your PATH — sessions still use the machine-wide config"
		if len(connected) > 0 {
			detail = strings.Join(connected, ", ") + " → " + otlpURL + "; " + detail
		}
		status := doctor.Warn
		fix := shimPathFix()
		if len(connected) == 0 {
			status = doctor.Fail
		}
		if bound && len(silent) > 0 {
			status = doctor.Fail
			detail += "; " + strings.Join(silent, ", ") + " sessions here send nothing because no repository telemetry policy enables their exporters"
			fix += "; terma install to enable the missing repository policy"
		}
		return doctor.Check{Status: status, Detail: detail, Fix: fix}
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

// waitForCommitEvent polls the data API for the scratch commit's event.
func waitForCommitEvent(ctx context.Context, cfg *config.Config, projectID, sha string, progress doctorProgress) (bool, error) {
	// Query the project the scratch event used, independently of command overrides.
	queryConfig := *cfg
	queryConfig.ProjectID = projectID
	client, err := newClient(&queryConfig)
	if err != nil {
		return false, err
	}
	started := time.Now()
	deadline := started.Add(roundTripWait)
	for {
		progress.noting(fmt.Sprintf("backend receives events… waiting for the round-trip (%ds of %ds)",
			int(time.Since(started).Seconds()), int(roundTripWait.Seconds())))
		// The lookup `terma blame` makes, windowed the same way: the scratch commit was
		// made moments before this started, so its record sits inside the window.
		rec, err := client.CommitLog(ctx, sha, started.Add(-blameWindow), started.Add(blameWindow))
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
