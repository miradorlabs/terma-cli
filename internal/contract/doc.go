// Package contract holds byte snapshots of every file terma writes for a coding agent:
// the hooks files a repository commits, the machine-wide hooks files, the managed
// configuration an organization deploys, and each agent's relay exporter
// configuration. Its tests fail when a byte changes. A committed file that changes marks
// every developer's hooks out of date on their next `terma update --refresh`, and for
// Codex it changes the hash each developer trusted, so they trust the hooks once more.
// A change here is a release decision, never a side effect of moving code; accept one
// with `go test ./internal/contract -update`.
package contract
