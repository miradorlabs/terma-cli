// Package contract holds byte snapshots of every file terma writes for a coding agent.
// A changed byte marks every developer's hooks stale, and may require them to trust the
// hooks again, so it is a release decision: accept one with `go test ./internal/contract -update`.
package contract
