package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
)

// CodexRolloutCwd reads the directory a Codex thread started in from its rollout's first
// line (session_meta.payload.cwd), through the same confined open as every other rollout
// reader. transcript is an optional hint (a path under CODEX_HOME); without one the
// rollout is found by thread id. Status is "present" with a directory, else the open's
// failure ("missing", "unreadable", ...) or "no_cwd". Nothing but the header is read.
func CodexRolloutCwd(ctx context.Context, sessionID, transcript string) (string, string) {
	f, status := openCodexRollout(ctx, sessionID, transcript)
	if f == nil {
		return "", status
	}
	defer f.Close()
	head := make([]byte, 64<<10)
	n, _ := f.ReadAt(head, 0)
	line, _, _ := bytes.Cut(head[:n], []byte{'\n'})
	var meta struct {
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil {
		return "", "unreadable"
	}
	if meta.Payload.Cwd == "" || !filepath.IsAbs(meta.Payload.Cwd) {
		return "", "no_cwd"
	}
	return filepath.Clean(meta.Payload.Cwd), "present"
}
