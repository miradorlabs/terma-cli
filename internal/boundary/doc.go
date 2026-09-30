// Package boundary holds the tests that keep each coding agent inside its own package. An agent's
// code lives in internal/harness/<name>, the registry that lists them all in
// internal/adapter, and the identities every package may name in internal/agentid;
// nothing else imports an agent package or names an agent. What still does is counted
// file by file in testdata/leaks.txt, a list that may only shrink: a new mention fails,
// and a file that drops below its count must have the list rewritten
// (`go test ./internal/boundary -update`) so the progress is recorded.
package boundary
