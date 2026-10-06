// Package boundary holds the tests that keep terma's architecture: only
// internal/agents/builtin imports an agent's package, nothing else names an agent in
// code, the import graph keeps its directions, and the command line keeps its state in
// its App.
package boundary
