package codex

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/applypatch"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
)

const codexSourceSystem = "codex"

// SourceSystem is the platform's source_system identifier for Codex sessions.
func (Agent) SourceSystem() string { return codexSourceSystem }

// DiscoverHistory lists local Codex rollouts under $CODEX_HOME/sessions and
// $CODEX_HOME/archived_sessions (default ~/.codex). Rollouts older than opts.Since are
// returned with OutsideSince set, and those modified within opts.ActiveWindow of opts.Now
// are returned with Active set so the caller can skip and report them without reading.
func (Agent) DiscoverHistory(ctx context.Context, opts agents.HistoryDiscoverOptions) ([]agents.HistoryFile, error) {
	home, err := codexHome()
	if err != nil {
		return nil, err
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	activeWindow := opts.ActiveWindow
	if activeWindow <= 0 {
		activeWindow = time.Hour
	}

	bySession := map[string]agents.HistoryFile{}
	for _, sub := range []string{"sessions", "archived_sessions"} {
		dir := filepath.Join(home, sub)
		if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			base := d.Name()
			if !strings.HasPrefix(base, "rollout-") || !strings.HasSuffix(base, ".jsonl") {
				return nil
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			modTime := info.ModTime()
			active := now.Sub(modTime) < activeWindow
			outside := !opts.Since.IsZero() && modTime.Before(opts.Since)
			sid := codexSessionIDFromFilename(base)
			if !outside && !active {
				if metaID := peekCodexSessionID(path); metaID != "" {
					sid = metaID
				}
			}
			if !session.ValidID(sid) {
				return nil
			}
			hf := agents.HistoryFile{
				SourceSystem: codexSourceSystem,
				SessionID:    sid,
				Path:         path,
				ModTime:      modTime,
				Active:       active,
				OutsideSince: outside,
			}
			if prev, exists := bySession[sid]; !exists || hf.ModTime.After(prev.ModTime) {
				bySession[sid] = hf
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	out := make([]agents.HistoryFile, 0, len(bySession))
	for _, hf := range bySession {
		out = append(out, hf)
	}
	slices.SortFunc(out, func(a, b agents.HistoryFile) int {
		return cmp.Or(a.ModTime.Compare(b.ModTime), strings.Compare(a.SessionID, b.SessionID))
	})
	return out, nil
}

// codexSessionIDFromFilename extracts the session UUID suffix from a rollout filename such
// as rollout-2026-10-03T22-01-34-019d2b54-7a10-7000-8000-000000000001.jsonl.
func codexSessionIDFromFilename(base string) string {
	stem := strings.TrimSuffix(strings.TrimPrefix(base, "rollout-"), ".jsonl")
	// Rollout prefix is YYYY-MM-DDTHH-MM-SS- (20 chars) followed by the session id.
	if len(stem) > 20 && stem[4] == '-' && stem[7] == '-' && stem[10] == 'T' && stem[19] == '-' {
		return stem[20:]
	}
	return stem
}

// peekCodexSessionID reads the rollout's first lines for session_meta's session id.
func peekCodexSessionID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReader(f)
	for range 8 {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var rec struct {
				Type    string `json:"type"`
				Payload struct {
					ID        string `json:"id"`
					SessionID string `json:"session_id"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &rec) == nil && rec.Type == "session_meta" {
				if id := cmp.Or(rec.Payload.ID, rec.Payload.SessionID); session.ValidID(id) {
					return id
				}
			}
		}
		if err != nil {
			break
		}
	}
	return ""
}

// ReadHistory converts a Codex rollout file into contiguous OTLP log segments grouped by
// recorded working directory.
func (Agent) ReadHistory(ctx context.Context, file agents.HistoryFile, opts agents.HistoryReadOptions, emit func(agents.HistorySegment) error) error {
	p := newCodexHistoryParser(file.SessionID, opts.IncludeSupplemental, emit)
	f, err := os.Open(file.Path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	br := bufio.NewReaderSize(f, 64<<10)
	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			if err := p.handleLine(line, offset); err != nil {
				return err
			}
			offset += int64(len(line))
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return rerr
		}
	}
	return p.finish()
}

var _ agents.HistorySource = Agent{}

func codexNormalizeCwd(raw, fallback string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fallback
	}
	if strings.HasPrefix(s, "file://") {
		if u, err := url.Parse(s); err == nil && u.Path != "" {
			return filepath.FromSlash(u.Path)
		}
		return strings.TrimPrefix(s, "file://")
	}
	return s
}

func codexCommandReachPaths(cmd []string, itemCwd, sessionCwd string) []string {
	dir := codexNormalizeCwd(itemCwd, sessionCwd)
	var out []string
	if dir != "" && dir != sessionCwd {
		out = append(out, hookrun.Alone(dir)...)
	}
	if len(cmd) == 0 {
		return out
	}
	shellCmd := strings.Join(cmd, " ")
	if len(cmd) >= 3 && (cmd[1] == "-c" || cmd[1] == "-lc" || cmd[1] == "-cl") {
		shellCmd = cmd[2]
	}
	return append(out, hookrun.Alone(hookrun.ShellPaths(shellCmd, dir)...)...)
}

func codexToolReachPaths(name, args, cwd string) []string {
	if args == "" {
		return nil
	}
	if name == "exec" {
		// Code-mode "exec" scripts are JavaScript wrappers whose inner shell calls appear
		// as CommandExecution items; never parse JS as POSIX shell, but do extract any
		// embedded apply_patch file headers so cross-org patch targets are always caught.
		if patched := applypatch.Paths(args); len(patched) > 0 {
			return hookrun.Alone(patched...)
		}
		return nil
	}
	if name == codexPatchTool {
		return hookrun.Alone(applypatch.Paths(args)...)
	}
	var in struct {
		Cmd      string `json:"cmd"`
		Command  string `json:"command"`
		Workdir  string `json:"workdir"`
		Cwd      string `json:"cwd"`
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
	}
	if json.Unmarshal([]byte(args), &in) != nil {
		return nil
	}
	var out []string
	dir := cwd
	if wd := cmp.Or(in.Workdir, in.Cwd); wd != "" {
		if !filepath.IsAbs(wd) && cwd != "" {
			wd = filepath.Join(cwd, wd)
		}
		dir = wd
		out = append(out, hookrun.Alone(wd)...)
	}
	if c := cmp.Or(in.Cmd, in.Command); c != "" {
		out = append(out, hookrun.Alone(hookrun.ShellPaths(c, dir)...)...)
	}
	for _, p := range []string{in.FilePath, in.Path} {
		if p != "" {
			out = append(out, hookrun.Alone(p)...)
		}
	}
	return out
}

func codexOutputSuccess(raw json.RawMessage, decoded string) bool {
	checkJSON := func(s string) (decided, ok bool) {
		var obj struct {
			ExitCode *int64 `json:"exit_code"`
			IsError  bool   `json:"is_error"`
			Success  *bool  `json:"success"`
		}
		if json.Unmarshal([]byte(s), &obj) != nil {
			return false, true
		}
		if obj.ExitCode != nil {
			return true, *obj.ExitCode == 0
		}
		if obj.Success != nil {
			return true, *obj.Success
		}
		if obj.IsError {
			return true, false
		}
		return false, true
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(trimmed, &parts) == nil {
			for _, part := range parts {
				if decided, ok := checkJSON(strings.TrimSpace(part.Text)); decided {
					return ok
				}
			}
		}
	}
	if decided, ok := checkJSON(strings.TrimSpace(decoded)); decided {
		return ok
	}
	for _, prefix := range []string{"Process exited with code ", "Exit code: "} {
		if _, rest, ok := strings.Cut(decoded, prefix); ok {
			rest = strings.TrimSpace(rest)
			if len(rest) > 0 && rest[0] != '0' {
				return false
			}
		}
	}
	return true
}

type codexPendingCall struct {
	Name      string
	Arguments string
	Cwd       string
	At        time.Time
	TraceID   []byte
}

type codexUsageNumbers struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}
