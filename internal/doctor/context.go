package doctor

import (
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// Context is what `terma doctor` shows above its checks, which do not cover it: this
// machine's setup, the teams and what each collects, and this repository.
func Context(env Env) []Section {
	if env.Config == nil {
		return nil
	}
	now := time.Now()
	machine := Section{Title: "This machine"}
	for _, r := range machineRows(env.Config, env.Routing != nil) {
		machine.Lines = append(machine.Lines, keyLine(r))
	}
	machine.Lines = append(machine.Lines,
		Line{Key: "config", Text: output.TildePath(env.ConfigDir)},
		Line{Key: "state", Text: output.TildePath(env.StateDir)})
	for _, r := range env.Updates {
		machine.Lines = append(machine.Lines, keyLine(r))
	}
	out := []Section{machine}
	if env.Routing != nil {
		out = append(out, env.Routing.teamSections(now)...)
	}
	return append(out, repositorySection(env, now))
}

// keyLine is a context row as a key/value line.
func keyLine(r Row) Line { return Line{Key: strings.ToLower(r.Label), Text: r.Value} }

// repositorySection is where this working copy's sessions go, and its work in progress.
func repositorySection(env Env, now time.Time) Section {
	s := Section{Title: "This repository"}
	if env.RepoErr == nil && env.GitDir == "" {
		s.Title = "This folder"
	}
	c := RepositoryVerdict(env, now)
	s.Lines = append(s.Lines, Line{Mark: markOf(c.Status), Text: c.Detail})
	if env.RepoErr != nil {
		return s
	}
	if work, err := workRows(env.StateDir, env.Root, env.GitDir); err == nil {
		for _, r := range work {
			s.Lines = append(s.Lines, keyLine(r))
		}
	}
	for _, r := range repoPolicyRows(env.Agents, env.Root) {
		s.Lines = append(s.Lines, keyLine(r))
	}
	return s
}

// RepositoryVerdict is the "repository collected" check, which doctor shows in its
// repository section: where routing sends this working copy's sessions, or, without
// routing state, whether the profile's policy collects it.
func RepositoryVerdict(env Env, now time.Time) Check {
	if env.Routing != nil {
		c, _ := RoutedRepository(*env.Routing, env.GitDir, env.RepoErr, now)
		return c
	}
	pol := env.Config.Policy.InForce(env.Config.OrganizationID, env.Config.AuthURL)
	if env.RepoErr == nil && pol.Global() {
		return unlessCaptureOff(pol, Check{Status: Pass, Detail: "every session, in global mode: " + GlobalDestination(env.Config)})
	}
	return unlessCaptureOff(pol, RepositoryCheck(pol, TeamLabel(env.Config), env.GitDir, env.RepoErr))
}

// InContext reports whether doctor's context already shows c, so its check line would
// repeat it: the teams and where this repository goes.
func InContext(c Check) bool { return c.Key == KeyTeams || c.Key == KeyProject }
