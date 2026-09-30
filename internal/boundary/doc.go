// Package boundary holds the tests that keep each coding agent inside its own package. An agent's
// code lives in internal/harness/<name> and the registry that lists them all in
// internal/adapter; nothing else imports an agent package or names an agent. What still
// does is counted package by package in testdata/leaks.txt, a list that may only
// shrink: a new mention fails, and a package that drops below its count must have the
// list rewritten
// (`go test ./internal/boundary -update`) so the progress is recorded.
package boundary
