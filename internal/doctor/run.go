package doctor

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/keystore"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// Env is what a doctor run reads: the registry, the configuration, where the CLI stands,
// and the probes for what this package does not reach itself.
type Env struct {
	Agents *agents.Registry
	// Config is the loaded configuration; ConfigErr says why there is none.
	Config    *config.Config
	ConfigErr error
	// Exe is the running executable, and BinDirs where else a terma may be installed.
	Exe     string
	BinDirs []string
	// Root and GitDir are the workspace the CLI stands in; RepoErr says why there is none.
	Root, GitDir string
	RepoErr      error
	SkipCommit   bool
	Probes       Probes
}

// Probes reach the event spool and the platform's APIs for a run.
type Probes struct {
	// Spool reports the event queue.
	Spool func() SpoolState
	// Deliver flushes every project's queued events, retry windows ignored.
	Deliver func(ctx context.Context) (Delivery, error)
	// Endpoint is the ingest host a project's events go to.
	Endpoint func(projectID string) string
	// CommitRecorded reports whether projectID's data API holds the terma.commit event for
	// sha recorded between from and to.
	CommitRecorded func(ctx context.Context, projectID, sha string, from, to time.Time) (bool, error)
}

// SpoolState is the event queue as a run finds it.
type SpoolState struct {
	Open   bool
	Queued int
	// WriteErr is why one more event could not be written.
	WriteErr error
}

// Delivery is what one flush of the queue did.
type Delivery struct {
	Sent int
	// Err is set when the pass failed; Failures names each project whose send failed.
	Err      error
	Failures []Failure
	// Endpoints are the ingest hosts that accepted events.
	Endpoints []string
	// Delivered words what was sent, and Undelivered what was not.
	Delivered   string
	Undelivered []string
}

// Failure is one project's failed delivery.
type Failure struct {
	ProjectID, Endpoint string
	Err                 error
	// Refused says the host answered and refused; KeyRefused that it refused the key.
	Refused, KeyRefused bool
}

// Progress is how a run reports as it goes: a check starting, a word about what a long
// one waits on, and a check finishing. Every field is optional.
type Progress struct {
	Start func(name string)
	Note  func(text string)
	Done  func(c Check)
}

func (p Progress) starting(name string) {
	if p.Start != nil {
		p.Start(name)
	}
}

func (p Progress) noting(text string) {
	if p.Note != nil {
		p.Note(text)
	}
}

func (p Progress) finished(c Check) {
	if p.Done != nil {
		p.Done(c)
	}
}

// Run runs every check. The checks are ordered so each later one can assume the earlier
// ones' facts (a repo, a binding, ...), and a missing prerequisite is reported as a skip
// rather than a second failure.
func Run(ctx context.Context, env Env, progress Progress) Report {
	var checks []Check
	add := func(c Check) {
		checks = append(checks, c)
		progress.finished(c)
	}
	timed := func(key, name string, fn func() Check) {
		progress.starting(name)
		start := time.Now()
		c := fn()
		c.Key, c.Name, c.Duration = key, name, time.Since(start)
		add(c)
	}

	// 1. Binary.
	var binaryCheck Check
	timed(KeyBinary, "terma on PATH", func() Check {
		binaryCheck = BinaryCheck(env.Exe, env.BinDirs)
		return binaryCheck
	})

	// 1b. Saved state. Every start migrates it, so this is a line only when an update
	// left a migration pending or failed.
	if c, ok := stateCheck(); ok {
		timed(KeyState, "saved state migrated", func() Check { return c })
	}

	// 2. Sign-in.
	if env.ConfigErr != nil {
		add(Check{Key: KeyAuth, Name: "configuration", Status: Fail, Detail: env.ConfigErr.Error()})
		return Build(checks)
	}
	cfg := env.Config
	d := &run{ctx: ctx, env: env, cfg: cfg, progress: progress, binaryCheck: binaryCheck}
	timed(KeyAuth, "signed in", d.signedIn)

	// 3. Repository binding.
	d.nonGit = env.RepoErr == nil && env.GitDir == ""
	timed(KeyProject, "repository bound", func() Check {
		c, bound := RepositoryCheck(env.Root, env.GitDir, env.RepoErr)
		d.bound = bound
		return c
	})
	d.projectID = cfg.ProjectID
	if d.bound != nil {
		d.projectID = d.bound.Project.ID
	}

	// 4. Hooks + adapters.
	timed(KeyHooks, "commit hooks installed", d.commitHooks)

	// 4b. The agents' own hooks. A commit is stamped with the session that touched its
	// files, and a session exists only because its agent's hooks announced it — so this
	// is its own check, and a fraction: one agent that cannot run its hooks yet costs
	// that agent's commits, not the whole repository's.
	timed(KeyAgentHooks, "agent hooks run", d.agentHooks)

	// 5. Harness export.
	timed(KeyHarness, "agent exporting to Terma", d.agentsExporting)

	// 5b. Status line: the payload Claude Code hands its status line carries the
	// plan's own rate-limit windows, the strongest funding evidence a machine
	// produces. Only worth a line when Claude Code is here and connected.
	if a, ok := StatusLineAgent(env.Agents); ok && a.Installed(ctx) {
		timed(KeyStatusLine, a.DisplayName()+" status line", d.statusLine)
	}

	// 6. Scratch commit.
	timed(KeyScratch, "scratch commit stamped", d.scratchCommit)

	// 7. Spool + backend round-trip.
	timed(KeySpool, "event spool", d.eventSpool)
	timed(KeyBackend, "backend receives events", func() Check {
		return BackendCheck(ctx, env.Probes, d.projectID, d.scratchSHA, binaryCheck, progress)
	})

	// (A future GitHub App will report merge/revert/CI outcomes; until it ships there is
	// nothing to check and nothing to suggest, so it is not listed here.)

	return Build(checks)
}

// run carries what one check establishes for the ones after it: Run orders the checks
// so each can assume the earlier ones' facts.
type run struct {
	ctx      context.Context
	env      Env
	cfg      *config.Config
	progress Progress
	nonGit   bool
	// bound is set by the binding check, and projectID follows it.
	bound     *termaproject.File
	projectID string
	// scratchSHA is set by ScratchCommit, for the backend check to read back.
	scratchSHA  string
	binaryCheck Check
}

// installed reports whether the CLI stands in a repository `terma install` has bound.
func (d *run) installed() bool { return d.env.RepoErr == nil && d.bound != nil }

func (d *run) signedIn() Check {
	cfg := d.cfg
	if cfg.APIKey != "" {
		return Check{Status: Pass, Detail: "using TERMA_API_KEY"}
	}
	cred, err := auth.LoadCredential(cfg.ProfileName)
	if err != nil {
		return Check{Status: Fail, Detail: "no credential for this environment", Fix: "terma setup"}
	}
	if err := cred.CheckEnvironment(cfg.AuthURL); err != nil {
		return Check{Status: Fail, Detail: "signed in against a different environment", Fix: "terma setup"}
	}
	who := cmp.Or(cred.UserEmail, "your account")
	env := ""
	if cfg.Environment != config.EnvProd {
		env = " [" + cfg.Environment + "]"
	}
	return Check{Status: Pass, Detail: who + " in " + cmp.Or(cfg.OrganizationName, cred.OrganizationID) + env}
}

// RepositoryCheck finds the binding of the workspace at root, or, in a linked worktree
// without one, its main checkout's.
func RepositoryCheck(root, gitDir string, repoErr error) (Check, *termaproject.File) {
	if repoErr != nil {
		return Check{Status: Fail, Detail: repoErr.Error()}, nil
	}
	f, from, err := termaproject.Resolve(root, gitDir)
	if err != nil {
		where := root
		if _, main, ok := gitx.LinkedWorktreeFS(gitDir); ok && main != "" {
			where += " or its main checkout " + main
		}
		return Check{Status: Fail, Detail: "no " + termaproject.FileName + " in " + where, Fix: "terma install"}, nil
	}
	return Check{Status: Pass, Detail: cmp.Or(f.Project.Name, f.Project.ID) + ThroughMain(root, from)}, f
}

// ThroughMain says, for a linked worktree bound through its main checkout, where the
// binding came from; it is empty when the checkout has its own.
func ThroughMain(root, from string) string {
	if from == "" || from == root {
		return ""
	}
	return " (through the main checkout " + from + ")"
}

func (d *run) commitHooks() Check {
	if d.nonGit {
		return Check{Status: Skip, Detail: "not a Git repository"}
	}
	if !d.installed() {
		return Check{Status: Skip, Detail: "needs an installed repository"}
	}
	return HooksCheck(JudgeHookWiring(d.ctx, d.env.Root, d.bound))
}

func (d *run) agentHooks() Check {
	if !d.installed() {
		return Check{Status: Skip, Detail: "needs an installed repository"}
	}
	return AgentHooksCheck(d.env.Agents, d.env.Root, SelectedForRepo(d.env.Agents, d.projectID, d.cfg.Harnesses))
}

func (d *run) agentsExporting() Check {
	if claim.Enabled() {
		return RelayCheck(d.env.Agents, d.projectID, d.cfg.Harnesses)
	}
	verdicts := JudgeSelectedHarnesses(d.ctx, d.env.Agents, d.cfg.OTLPURL, d.projectID, d.env.Root, d.cfg.Harnesses)
	return HarnessCheck(d.env.Agents, verdicts, d.cfg.OTLPURL, d.projectID, d.installed())
}

func (d *run) statusLine() Check {
	repoRoot := ""
	if d.env.RepoErr == nil {
		repoRoot = d.env.Root
	}
	return StatusLineCheck(JudgeStatusLine(d.env.Agents, repoRoot))
}

func (d *run) scratchCommit() Check {
	if d.nonGit {
		return Check{Status: Skip, Detail: "not a Git repository"}
	}
	if !d.installed() {
		return Check{Status: Skip, Detail: "needs an installed repository"}
	}
	if d.env.SkipCommit {
		return Check{Status: Skip, Detail: "--skip-commit"}
	}
	sha, c := ScratchCommit(d.ctx, d.env.Root, d.bound)
	d.scratchSHA = sha
	return c
}

func (d *run) eventSpool() Check {
	s := d.env.Probes.Spool()
	if !s.Open {
		return Check{Status: Fail, Detail: "cannot open the spool directory"}
	}
	n := s.Queued
	// Reading a queue proves nothing about writing one, and a spool that
	// cannot be appended to is how a commit goes unbilled without anyone
	// hearing about it: the hook swallows the error by design.
	if s.WriteErr != nil {
		return Check{Status: Fail, Detail: "events cannot be written to the spool: " + s.WriteErr.Error(), Fix: "check permissions and free space on the config directory"}
	}
	if d.projectID != "" && keystore.Get(d.projectID) == "" {
		name := d.cfg.ProjectName
		if d.bound != nil {
			name = d.bound.Project.Name
		}
		return Check{Status: Fail, Detail: fmt.Sprintf("%d queued; no project key stored for %s", n, cmp.Or(name, d.projectID)), Fix: "terma install"}
	}
	return Check{Status: Pass, Detail: fmt.Sprintf("%d queued, spool writable", n)}
}

// BackendCheck flushes every project's queued events and reads the scratch commit's
// event back from its project's data API. A binary check that did not pass makes it
// inconclusive: hooks may run another build.
func BackendCheck(ctx context.Context, p Probes, projectID, scratchSHA string, binary Check, progress Progress) Check {
	if binary.Status == Warn || binary.Status == Fail {
		return Check{Status: Warn, Inconclusive: true,
			Detail: "resolve the missing or conflicting Terma executables before verifying hook delivery",
			Fix:    binary.Fix}
	}
	if projectID != "" && keystore.Get(projectID) == "" {
		return Check{Status: Skip, Detail: "no project key on this machine; nothing can be delivered", Fix: "terma install"}
	}
	res, err := p.Deliver(ctx)
	if err != nil {
		return Check{Status: Fail, Detail: err.Error(), Fix: "terma install"}
	}
	// The flush delivers every project's queued events, not only this repository's.
	// Another project's refusal says nothing about this repository's chain — it read
	// as this repository's credentials failing — so it is a warning here, named with
	// its own project and host. This project's refusal, or a failure no project owns
	// (a lock, the deadline), fails the check.
	var others []string
	var othersFix string
	if res.Err != nil {
		if i := slices.IndexFunc(res.Failures, func(f Failure) bool { return f.ProjectID == projectID }); i >= 0 {
			f := res.Failures[i]
			return Check{Status: Fail, Detail: "this project's events were not delivered: " + describeFailure(f), Fix: failureFix(f)}
		}
		if len(res.Failures) == 0 {
			return Check{Status: Fail, Detail: "flush failed: " + res.Err.Error(), Fix: "check the network, then run `terma spool flush --force`"}
		}
		for _, f := range res.Failures {
			others = append(others, "another project's events were not delivered: "+f.ProjectID+" "+describeFailure(f))
		}
		othersFix = failureFix(res.Failures[0])
	}
	delivered, undelivered := res.Delivered, res.Undelivered
	hosts := res.Endpoints
	if len(hosts) == 0 {
		hosts = []string{p.Endpoint(projectID)}
	}
	flushDetail := "flushed " + delivered + " to " + strings.Join(hosts, ", ")
	if res.Sent == 0 && len(undelivered) == 0 {
		flushDetail = "nothing queued at verification time (hooks may already have flushed)"
	}
	detail := strings.Join(append(append([]string{flushDetail}, undelivered...), others...), "; ")
	if scratchSHA == "" {
		return Check{Status: Skip, Inconclusive: true, Detail: detail + "; no scratch commit event to verify", Fix: othersFix}
	}
	// Round-trip: the scratch commit's event must be readable back.
	if ok, err := waitForCommit(ctx, p, projectID, scratchSHA, progress); err != nil {
		return Check{Status: Warn, Inconclusive: true, Detail: detail + "; could not confirm the round-trip via the API (" + err.Error() + ")", Fix: "terma doctor"}
	} else if !ok {
		return Check{Status: Warn, Inconclusive: true, Detail: detail + "; the scratch commit event was not visible via the API within " + roundTripWait.String(), Fix: "terma doctor"}
	}
	if len(others) > 0 {
		return Check{Status: Warn, Detail: detail + "; round-trip confirmed for this project", Fix: othersFix}
	}
	return Check{Status: Pass, Detail: detail + "; round-trip confirmed"}
}

// describeFailure words one project's failed delivery with the host that refused it:
// the host is the half of the story a developer with projects in two environments
// cannot guess.
func describeFailure(f Failure) string {
	if f.Refused {
		return "refused by " + f.Endpoint + " (" + f.Err.Error() + ")"
	}
	return "not sent to " + f.Endpoint + " (" + f.Err.Error() + ")"
}

// failureFix is the next step for a failed delivery. A refused key is not a network
// problem, and pointing at the network for one sent a developer to check a
// connection that was working.
func failureFix(f Failure) string {
	if f.KeyRefused {
		return "the key this machine holds for project " + f.ProjectID + " was refused by " + f.Endpoint + " — it may have been revoked, or belong to another environment"
	}
	return "check the network and " + f.Endpoint + ", then run `terma spool flush --force`"
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

// waitForCommit polls the project's data API for the scratch commit's event.
func waitForCommit(ctx context.Context, p Probes, projectID, sha string, progress Progress) (bool, error) {
	started := time.Now()
	deadline := started.Add(roundTripWait)
	for {
		progress.noting(fmt.Sprintf("backend receives events… waiting for the round-trip (%ds of %ds)",
			int(time.Since(started).Seconds()), int(roundTripWait.Seconds())))
		// The scratch commit was made moments before this started, so its record sits
		// inside the window.
		found, err := p.CommitRecorded(ctx, projectID, sha, started.Add(-commitLogWindow), started.Add(commitLogWindow))
		if err != nil || found {
			return found, err
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
