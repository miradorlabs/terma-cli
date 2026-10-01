// Package boundary holds the tests that keep terma's architecture as drawn in
// docs/ARCHITECTURE.md. An agent's code lives in internal/agents/<name>: nothing but
// internal/agents/builtin imports an agent's package, and nothing else names an agent,
// by identifier or in a string (`go test ./internal/boundary -mentions` lists each one
// it finds). The import graph keeps its directions (bans): the relay learns about agents
// only through its options, doctor and install reach the network only through what the
// command line injects, the hook runtime knows no agent or command. And the command line
// is one package whose state lives in its App.
package boundary
