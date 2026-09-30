// Package boundary holds the tests that keep each coding agent inside its own package.
// An agent's code lives in internal/agents/<name>; the registry that lists them all is
// internal/agents, the contract they implement internal/agents/agent, and code a few of
// them share internal/agents/internal. Nothing else imports an agent's package or names
// an agent. What still does is counted package by package in testdata/leaks.txt, a list
// that may only shrink: a new mention fails, and a package that drops below its count
// must have the list rewritten (`go test ./internal/boundary -update`) so the progress
// is recorded.
package boundary
