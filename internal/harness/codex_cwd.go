package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"time"
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

// CodexTurnDir is a directory a Codex thread ran in from a moment on: the one it
// started in (At zero), then each turn's.
type CodexTurnDir struct {
	At  time.Time
	Cwd string
}

// maxTurnScan bounds one read of a rollout for turn directories.
const maxTurnScan = 8 << 20

// CodexRolloutDirs reads a Codex thread's directories from its rollout, starting at byte
// offset from (0: from the beginning, the header included), and returns them with the
// offset to continue from. `codex exec resume` in another directory keeps the thread; the
// resumed process records its directory as it attaches (event_msg
// thread_settings_applied, thread_settings.cwd — before the run's turn and prompt, 0.158)
// and again in each turn_context. Only complete lines are read, at most maxTurnScan per
// call, through the same confined open as every rollout reader; a line is decoded only
// when its prefix names it one of those records (or it is the header) — what the thread
// said is never parsed.
func CodexRolloutDirs(ctx context.Context, sessionID string, from int64) ([]CodexTurnDir, int64) {
	f, _ := openCodexRollout(ctx, sessionID, "")
	if f == nil {
		return nil, from
	}
	defer f.Close()
	buf := make([]byte, maxTurnScan)
	n, _ := f.ReadAt(buf, from)
	buf = buf[:n]
	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 {
		return nil, from
	}
	var out []CodexTurnDir
	offset := from
	for _, line := range bytes.Split(buf[:end], []byte{'\n'}) {
		header := offset == 0
		offset += int64(len(line)) + 1
		prefix := line[:min(len(line), 160)]
		turn := bytes.Contains(prefix, []byte(`"type":"turn_context"`))
		attach := bytes.Contains(prefix, []byte(`"type":"thread_settings_applied"`))
		if !header && !turn && !attach {
			continue
		}
		var rec struct {
			Timestamp string `json:"timestamp"`
			Payload   struct {
				Cwd            string `json:"cwd"`
				ThreadSettings struct {
					Cwd string `json:"cwd"`
				} `json:"thread_settings"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		cwd := rec.Payload.Cwd
		if attach {
			cwd = rec.Payload.ThreadSettings.Cwd
		}
		if cwd == "" || !filepath.IsAbs(cwd) {
			continue
		}
		d := CodexTurnDir{Cwd: filepath.Clean(cwd)}
		if !header {
			if t, err := time.Parse(time.RFC3339Nano, rec.Timestamp); err == nil {
				d.At = t
			}
		}
		out = append(out, d)
	}
	return out, from + int64(end) + 1
}
