// Package boundary holds the tests that keep each coding agent inside its own package.
// An agent's code lives in internal/agents/<name>. internal/agents is the contract they
// implement and the registry type, internal/agents/builtin registers them, and code a
// few of them share is internal/agents/internal. Nothing else imports an agent's package
// or names an agent. What still does is counted package by package in testdata/leaks.txt, a list
// that may only shrink: a new mention fails, and a package that drops below its count
// must have the list rewritten (`go test ./internal/boundary -update`) so the progress
// is recorded.
package boundary
