// Package boundary holds the tests that keep each coding agent inside its own package.
// An agent's code lives in internal/agents/<name>. internal/agents is the contract they
// implement and the registry type, internal/agents/builtin registers them, and code a
// few of them share is internal/agents/internal. Nothing else imports an agent's package
// or names an agent, whether by identifier or in a string: `go test ./internal/boundary
// -mentions` lists each one it finds.
package boundary
