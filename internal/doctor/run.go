package doctor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
)

// Env is what a doctor run reads.
type Env struct {
	Agents *agents.Registry
	// Config is the loaded configuration; ConfigErr says why there is none.
	Config    *config.Config
	ConfigErr error
	// Exe is the running executable; BinDirs is where else a terma may be installed.
	Exe          string
	BinDirs      []string
	Root, GitDir string
	RepoErr      error
	Probes       Probes
}

// Probes reach the event spool and the platform's APIs for a run.
type Probes struct {
	Spool func() SpoolState
	// Deliver flushes every project's queued events, retry windows ignored.
	Deliver  func(ctx context.Context) (Delivery, error)
	Endpoint func(projectID string) string
	// Relay is the local relay as the machine finds it.
	Relay func() Relay
	// Credential is the signed-in developer, or why there is none.
	Credential func() (Credential, error)
	// Keys are the delivery keys stored on this machine.
	Keys Keys
}

// Credential is the signed-in developer as doctor needs them.
type Credential struct {
	Email, OrganizationID string
	// OtherEnvironment is a credential signed in against another environment.
	OtherEnvironment bool
}

// Keys reports, masked, the delivery key stored for agent ("" for the project's own) in
// projectID; "" when there is none.
type Keys func(agent, projectID string) string

// has reports whether agent's key, or else the project's, is stored.
func (k Keys) has(agent, projectID string) bool {
	return k != nil && (agent != "" && k(agent, projectID) != "" || k("", projectID) != "")
}

// Relay is the local relay as a run finds it: its state directory and address, whether
// it runs, and whether something else listens there instead.
type Relay struct {
	Dir, Addr         string
	Running, Squatted bool
	Err               error
	// LastFailure is why the relay last failed to start, when none runs; "" if it did not fail.
	LastFailure string
	// Environment is the backend environment the running relay delivers to; "" when unknown
	// (none runs, or a terma from before relays recorded it).
	Environment string
	// HookStarted means the running relay is not the service's: a hook, or a developer, started it.
	HookStarted bool
	// ServiceInstalled says a relay service is installed; ServiceCurrent that it is the one
	// this terma would install, in the environment install recorded.
	ServiceInstalled, ServiceCurrent bool
}

// SpoolState is the event queue as a run finds it.
type SpoolState struct {
	Open     bool
	Queued   int
	WriteErr error
	// NextAttempt ends the spool-wide retry window; Windows are each project's.
	NextAttempt time.Time
	Windows     map[string]time.Time
}

// Delivery is what one flush of the queue did.
type Delivery struct {
	Sent      int
	Err       error
	Failures  []Failure
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

// Progress is how a run reports as it goes; every field is optional.
type Progress struct {
	Start func(name string)
	Done  func(c Check)
}

func (p Progress) starting(name string) {
	if p.Start != nil {
		p.Start(name)
	}
}

func (p Progress) finished(c Check) {
	if p.Done != nil {
		p.Done(c)
	}
}

// Run runs every check in order, so a missing prerequisite is a skip, not a second failure.
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

	timed(KeyBinary, "terma on PATH", func() Check {
		return BinaryCheck(env.Exe, env.BinDirs, HookCallerFor(env.Root, env.GitDir, env.RepoErr))
	})

	if c, ok := stateCheck(); ok {
		timed(KeyState, "saved state migrated", func() Check { return c })
	}

	if env.ConfigErr != nil {
		add(Check{Key: KeyAuth, Name: "configuration", Status: Fail, Detail: env.ConfigErr.Error()})
		return Build(checks)
	}
	cfg := env.Config
	d := &run{ctx: ctx, env: env, cfg: cfg}
	timed(KeyAuth, "signed in", d.signedIn)

	d.nonGit = env.RepoErr == nil && env.GitDir == ""
	timed(KeyProject, "repository bound", func() Check {
		c, bound := RepositoryCheck(env.Root, env.GitDir, env.RepoErr)
		d.bound = bound
		if bound == nil && env.RepoErr == nil && cfg.Policy.Global() {
			return Check{Status: Pass, Detail: "not bound, which global mode does not need: " + GlobalDestination(cfg)}
		}
		return c
	})
	d.projectID = cfg.ProjectID
	if d.bound != nil {
		d.projectID = d.bound.Project.ID
	}

	timed(KeyHooks, "commit hooks installed", d.commitHooks)

	// A fraction, not a verdict: one agent that cannot run its hooks costs only its own commits.
	timed(KeyAgentHooks, "agent hooks run", d.agentHooks)

	if cfg.Policy.Global() {
		if c, ok := UserHooksCheck(env.Agents, cfg.Harnesses); ok {
			timed(KeyUserHooks, "machine-wide hooks run", func() Check { return c })
		}
	}

	timed(KeyHarness, "agent exporting to Terma", d.agentsExporting)

	if a, ok := StatusLineAgent(env.Agents); ok && a.Installed(ctx) {
		timed(KeyStatusLine, a.DisplayName()+" status line", d.statusLine)
	}

	timed(KeySpool, "event spool", d.eventSpool)
	timed(KeyBackend, "backend receives events", func() Check {
		return BackendCheck(ctx, env.Probes, d.projectID)
	})

	return Build(checks)
}

// run carries what one check establishes for the ones after it.
type run struct {
	ctx       context.Context
	env       Env
	cfg       *config.Config
	nonGit    bool
	bound     *termaproject.File
	projectID string
}

// installed reports whether the CLI stands in a repository `terma install` has bound.
func (d *run) installed() bool { return d.env.RepoErr == nil && d.bound != nil }

func (d *run) signedIn() Check {
	cfg := d.cfg
	if cfg.APIKey != "" {
		return Check{Status: Pass, Detail: "using TERMA_API_KEY"}
	}
	var cred Credential
	err := errors.New("no credential probe")
	if d.env.Probes.Credential != nil {
		cred, err = d.env.Probes.Credential()
	}
	if err != nil {
		return Check{Status: Fail, Detail: "no credential for this environment", Fix: "terma setup"}
	}
	if cred.OtherEnvironment {
		return Check{Status: Fail, Detail: "signed in against a different environment", Fix: "terma setup"}
	}
	who := cmp.Or(cred.Email, "your account")
	env := ""
	if cfg.Environment != config.EnvProd {
		env = " [" + cfg.Environment + "]"
	}
	detail := who + " in " + cmp.Or(cfg.OrganizationName, cred.OrganizationID) + env
	// Doctor's own run sees TERMA_ENV; the hooks and the flushes they start do not.
	if cfg.ProfileEnvironment != "" && cfg.ProfileEnvironment != cfg.Environment {
		return Check{Status: Warn, Detail: detail + ", but the profile records " + cfg.ProfileEnvironment +
			", so hooks, which run without TERMA_ENV, hold this environment's events",
			Fix: "TERMA_ENV=" + cfg.Environment + " terma setup"}
	}
	return Check{Status: Pass, Detail: detail}
}

func (d *run) commitHooks() Check {
	if d.nonGit {
		return Check{Status: Skip, Detail: "not a Git repository"}
	}
	if d.env.RepoErr != nil {
		return Check{Status: Skip, Detail: "needs an installed repository"}
	}
	if !d.installed() {
		return UnboundHooksCheck(JudgeHooksPath(d.ctx, d.env.Root), d.cfg.Policy.Global())
	}
	return HooksCheck(JudgeHookWiring(d.ctx, d.env.Root, d.bound))
}

func (d *run) agentHooks() Check {
	if !d.installed() {
		return Check{Status: Skip, Detail: "needs an installed repository"}
	}
	return AgentHooksCheck(d.env.Agents, d.env.Root, d.cfg.Harnesses)
}

func (d *run) agentsExporting() Check {
	if claim.Enabled() {
		return RelayCheck(d.env.Agents, d.env.Probes.Relay(), d.env.Probes.Keys, d.projectID, d.cfg.Environment, d.cfg.Harnesses)
	}
	verdicts := JudgeHarnesses(d.ctx, d.env.Agents, d.cfg.OTLPURL, d.projectID, d.env.Root)
	return HarnessCheck(d.env.Agents, verdicts, d.cfg.OTLPURL, d.projectID, d.installed())
}

func (d *run) statusLine() Check {
	repoRoot := ""
	if d.env.RepoErr == nil {
		repoRoot = d.env.Root
	}
	return StatusLineCheck(JudgeStatusLine(d.env.Agents, repoRoot))
}

func (d *run) eventSpool() Check {
	s := d.env.Probes.Spool()
	if !s.Open {
		return Check{Status: Fail, Detail: "cannot open the spool directory"}
	}
	n := s.Queued
	// Reading a queue proves nothing about writing one, and the hook swallows append errors.
	if s.WriteErr != nil {
		return Check{Status: Fail, Detail: "events cannot be written to the spool: " + s.WriteErr.Error(), Fix: "check permissions and free space on the config directory"}
	}
	if d.projectID != "" && !d.env.Probes.Keys.has("", d.projectID) {
		name := d.cfg.ProjectName
		if d.bound != nil {
			name = d.bound.Project.Name
		}
		return Check{Status: Fail, Detail: fmt.Sprintf("%d queued; no team key stored for %s", n, cmp.Or(name, d.projectID)), Fix: "terma install"}
	}
	return Check{Status: Pass, Detail: fmt.Sprintf("%d queued, spool writable", n)}
}

// BackendCheck flushes every project's queued events; a flush the backend accepts is the proof.
func BackendCheck(ctx context.Context, p Probes, projectID string) Check {
	if projectID != "" && !p.Keys.has("", projectID) {
		return Check{Status: Skip, Detail: "no team key on this machine; nothing can be delivered", Fix: "terma install"}
	}
	res, err := p.Deliver(ctx)
	if err != nil {
		return Check{Status: Fail, Detail: err.Error(), Fix: "terma install"}
	}
	// Another project's refusal says nothing about this repository, so it only warns; this
	// project's, or a failure no project owns (a lock, the deadline), fails.
	var others []string
	var othersFix string
	if res.Err != nil {
		if i := slices.IndexFunc(res.Failures, func(f Failure) bool { return f.ProjectID == projectID }); i >= 0 {
			f := res.Failures[i]
			return Check{Status: Fail, Detail: "this team's events were not delivered: " + describeFailure(f), Fix: failureFix(f)}
		}
		if len(res.Failures) == 0 {
			return Check{Status: Fail, Detail: "flush failed: " + res.Err.Error(), Fix: "check the network, then run `terma doctor` again (it retries delivery)"}
		}
		for _, f := range res.Failures {
			others = append(others, "another team's events were not delivered: "+f.ProjectID+" "+describeFailure(f))
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
	if len(others) > 0 {
		return Check{Status: Warn, Detail: detail, Fix: othersFix}
	}
	if res.Sent == 0 {
		return Check{Status: Skip, Inconclusive: true, Detail: detail}
	}
	return Check{Status: Pass, Detail: detail}
}

// describeFailure names the host, which a developer with projects in two environments cannot guess.
func describeFailure(f Failure) string {
	if f.Refused {
		return "refused by " + f.Endpoint + " (" + f.Err.Error() + ")"
	}
	return "not sent to " + f.Endpoint + " (" + f.Err.Error() + ")"
}

// failureFix is the next step for a failed delivery; a refused key is not a network problem.
func failureFix(f Failure) string {
	if f.KeyRefused {
		return "the key this machine holds for team " + f.ProjectID + " was refused by " + f.Endpoint + " — it may have been revoked, or belong to another environment"
	}
	return "check the network and " + f.Endpoint + ", then run `terma doctor` again (it retries delivery)"
}
