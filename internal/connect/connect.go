// Package connect writes a coding agent's exporter configuration, machine-wide (Global)
// or as a repository's committed policy (Local). It plans, reports conflicts, asks, and
// writes; signing in, minting a key and storing it come in as Steps.
package connect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// Options are a connect's choices.
type Options struct {
	Signals []harness.Signal
	Reach   harness.Reach
	// Identity is the enduser.id to stamp: "" for git's global email, "none" for none.
	Identity string
	// SuppliedKey is the key the developer passed, empty when one is minted or reused.
	SuppliedKey string
	// InlineKey writes the key into the settings file even where a helper could hold it.
	InlineKey, Force, AssumeYes, NoStatusLine bool
}

// Key is the server key a global connect installs.
type Key struct {
	Value, Prefix string
	// Minted says this connect created it, so a failure after it is the caller's to report.
	Minted, Reused bool
}

// Steps are what a connect does outside the agent's settings.
type Steps struct {
	Confirm func(question string) (bool, error)
	// Key resolves the key a global connect installs: supplied, reused or minted.
	Key func(ctx context.Context) (Key, error)
	// Store keeps the installed key, so the spool can deliver and an install reuses it.
	Store func(key string) error
}

// IO are where a connect reports.
type IO struct{ Out, Err io.Writer }

// errCancelled is a declined confirmation: not a failure, and nothing was written.
var errCancelled = errors.New("cancelled")

// Global points h's machine-wide exporter at cfg's project.
func Global(ctx context.Context, reg *agents.Registry, h harness.Harness, cfg *config.Config, o Options, s Steps, w IO) error {
	signals := o.Signals
	// `--exports repos` switches nothing on globally, but still writes what a committed
	// policy cannot hold: endpoint, key, identity and the master switch.
	if o.Reach == harness.ReachRepos {
		if _, offOnly := h.(harness.LocalOffOnly); offOnly {
			return fmt.Errorf("%s ignores a repository that turns telemetry on, so --exports repos would send nothing; connect without it", h.DisplayName())
		}
		signals = nil
	}
	// ConfigPath first: if the file cannot be located, a key minted below would be stranded.
	configPath, err := h.ConfigPath()
	if err != nil {
		return err
	}
	intended := harness.Exporter{
		Endpoint:           cfg.OTLPURL,
		ProjectID:          cfg.ProjectID,
		Signals:            signals,
		ResourceAttributes: resourceAttributes(ctx, h, cfg, o.Identity),
	}
	// A helper script supplies the header where it can, so the settings file holds a path.
	if !o.InlineKey && h.SupportsHeadersHelper() {
		if intended.HelperPath, err = harness.HelperFilePath(h, cfg.ProjectID); err != nil {
			return err
		}
	}
	// Checked before the key exists: a per-signal endpoint left in place would inherit
	// the Authorization header and hand out a live server key.
	conflicts, err := h.ConflictsWith(intended)
	if err != nil {
		return err
	}
	printConnectPlan(w.Out, h, cfg, h.Detect(ctx), configPath, intended.HelperPath, signals, o.Reach, o)
	printConnectNotes(w.Out, h, intended)
	err = gate(w.Out, h.DisplayName(), conflicts, o, s, "", fmt.Sprintf("Connect %s to Terma?", h.DisplayName()))
	if errors.Is(err, errCancelled) {
		return nil
	}
	if err != nil {
		return err
	}

	key, err := s.Key(ctx)
	if err != nil {
		return err
	}
	if key.Reused {
		fmt.Fprintf(w.Out, "\nReusing the key already configured for this team (%s) — nothing new minted.\n", key.Prefix)
	}
	// The backup is best-effort: a failure is reported, never blocking.
	if backup, err := h.Backup(cfg.OTLPURL); err != nil {
		fmt.Fprintf(w.Err, "Warning: could not back up %s (%v).\n", configPath, err)
	} else if backup != "" {
		fmt.Fprintf(w.Out, "\nBacked up %s\n", backup)
	}
	intended.APIKey = key.Value
	if err := h.Connect(intended, o.Force); err != nil {
		if key.Minted {
			fmt.Fprintf(w.Err, "\nA key (%s) was minted before this failed. Revoke it in the web app if you do not retry.\n", key.Prefix)
		}
		return err
	}
	if err := s.Store(key.Value); err != nil {
		fmt.Fprintf(w.Err, "Warning: could not store the team key for the spool (%v); queued events are not delivered until it is stored.\n", err)
	}
	var notes []string
	if line, ok := reg.Find[agents.StatusLiner](h.Name()); ok && !o.NoStatusLine {
		if note, _ := InstallStatusLine(line, w.Err); note != "" {
			notes = append(notes, note)
		}
	}
	if notifier, ok := reg.Find[agents.Notifier](h.Name()); ok {
		switch changed, err := notifier.InstallNotifier(); {
		case err != nil:
			fmt.Fprintf(w.Err, "Warning: could not install %s's funding notifier (%v).\n", h.DisplayName(), err)
		case changed:
			notes = append(notes, "Notifier: terma will capture plan and quota at the end of each turn; any previous notifier keeps running behind it.")
		default:
			notes = append(notes, "Notifier: terma's plan and quota capture is already installed.")
		}
	}
	fmt.Fprintf(w.Out, "\nConnected. Restart %s, then run a prompt.\n", h.DisplayName())
	for _, n := range notes {
		fmt.Fprintln(w.Out, n)
	}
	return nil
}

// Local writes a repository's policy for global's exporter into local, global's
// repository scope at root. It writes only what the repository ships, never where or
// with which key, so the file stays safe to commit.
func Local(ctx context.Context, global, local harness.Harness, cfg *config.Config, root string, o Options, s Steps, w IO) error {
	configPath, err := local.ConfigPath()
	if err != nil {
		return err
	}
	intended := harness.Exporter{
		// Carried, never written: an outranking per-signal redirect is judged against it.
		Endpoint: cfg.OTLPURL,
		Signals:  o.Signals,
	}
	conflicts, err := local.ConflictsWith(intended)
	if err != nil {
		return err
	}
	printLocalConnectPlan(w.Out, global, global.Detect(ctx), cfg, root, configPath, intended)
	err = gate(w.Out, global.DisplayName(), conflicts, o, s, " in this repository", fmt.Sprintf("Write this repository's telemetry settings for %s?", global.DisplayName()))
	if errors.Is(err, errCancelled) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := local.Connect(intended, o.Force); err != nil {
		return err
	}
	fmt.Fprintf(w.Out, "\nWritten %s.\n", configPath)
	fmt.Fprintf(w.Out, "Commit it so every session in this repository ships the same data. Restart %s for it to take effect.\n", global.DisplayName())
	return nil
}

// gate reports conflicts, refuses what terma cannot or may not clear, and asks. A
// conflict outside the file being written (the shell, a managed file, the developer's
// own opt-out) cannot be cleared by --force: the key would be installed while the
// override decides. Advisory conflicts, which apply only under a selected profile, do not
// gate.
func gate(out io.Writer, agent string, conflicts []harness.Conflict, o Options, s Steps, where, question string) error {
	PrintConflicts(out, conflicts, o.Force)
	conflicts, _ = harness.Partition(conflicts)
	if blocking := unclearable(conflicts); len(blocking) > 0 {
		return fmt.Errorf("%s has settings Terma does not change: %s — remove or adjust them, then retry",
			agent, output.SanitizeTerminal(strings.Join(blocking, ", ")))
	}
	if len(conflicts) > 0 && !o.Force {
		return fmt.Errorf("%s already has OTLP settings%s that would override this connect — remove them, or pass --force to have Terma remove them",
			agent, where)
	}
	if o.AssumeYes {
		return nil
	}
	ok, err := s.Confirm(question)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(out, "Cancelled. Nothing was written.")
		return errCancelled
	}
	return nil
}
