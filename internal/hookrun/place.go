package hookrun

import (
	"cmp"
	"context"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// Place is Claude Code's user-level SessionStart hook (`terma hook place`, written by
// `terma setup` under the relay): it records where a session runs, in every directory —
// a repository's own hooks run only in that repository, so a session resumed in a
// personal directory (`claude --resume` keeps the session id) is seen only here. It
// records the placement and nothing else: no event, no manifest, no network. In a
// repository its directory is the repository root, as the repository's own hook
// records it, so the two agree.
func Place(_ context.Context, env Env) error {
	in, err := readClaudeInput(env.Stdin)
	if err != nil {
		env.logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) {
		return nil
	}
	dir := cmp.Or(in.Cwd, env.Cwd)
	if dir == "" {
		return nil
	}
	if root, _, ok := gitx.LocateFS(dir); ok {
		dir = root
	}
	if err := relay.RecordSession(in.SessionID, dir); err != nil {
		env.logf("record session for the relay: %v", err)
	}
	return nil
}
