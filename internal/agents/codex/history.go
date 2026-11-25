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
	// unreadable is the first rollout or directory discovery could not read. A rollout
	// it cannot read may be any session's parent or child, so every session fails closed.
	unreadable := ""
	noteUnreadable := func(path string) {
		if unreadable == "" {
			unreadable = path
		}
	}
	for _, sub := range []string{"sessions", "archived_sessions"} {
		dir := filepath.Join(home, sub)
		if _, err := os.Stat(dir); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				noteUnreadable(dir)
			}
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				if !errors.Is(walkErr, fs.ErrNotExist) {
					noteUnreadable(path)
				}
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
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					noteUnreadable(path)
				}
				return nil
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			modTime := info.ModTime()
			active := now.Sub(modTime) < activeWindow
			outside := !opts.Since.IsZero() && modTime.Before(opts.Since)
			sid := codexSessionIDFromFilename(base)
			metaID, parentID, peekFailed := peekCodexRolloutMeta(path)
			if peekFailed {
				noteUnreadable(path)
			}
			if metaID != "" {
				sid = metaID
			}
			if !session.ValidID(sid) {
				return nil
			}
			hf := agents.HistoryFile{
				SourceSystem:    codexSourceSystem,
				SessionID:       sid,
				ParentSessionID: parentID,
				Path:            path,
				ModTime:         modTime,
				Active:          active,
				OutsideSince:    outside,
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
		hf.CompanionReadError = unreadable
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

// peekCodexRolloutMeta reads the rollout's first lines for session_meta's session id and
// optional spawned subagent parent_thread_id. unreadable is true when opening or reading
// fails with a non-ErrNotExist error so family privacy checks fail closed.
func peekCodexRolloutMeta(path string) (sessionID, parentID string, unreadable bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", !errors.Is(err, fs.ErrNotExist)
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReader(f)
	for range 8 {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var rec struct {
				Type    string `json:"type"`
				Payload struct {
					ID        string          `json:"id"`
					SessionID string          `json:"session_id"`
					Source    json.RawMessage `json:"source"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &rec) == nil && rec.Type == "session_meta" {
				if id := cmp.Or(rec.Payload.ID, rec.Payload.SessionID); session.ValidID(id) {
					sessionID = id
				}
				var source struct {
					Subagent json.RawMessage `json:"subagent"`
				}
				if json.Unmarshal(rec.Payload.Source, &source) == nil && len(source.Subagent) > 0 {
					var sub struct {
						ThreadSpawn *struct {
							ParentThreadID string `json:"parent_thread_id"`
						} `json:"thread_spawn"`
					}
					if json.Unmarshal(source.Subagent, &sub) == nil && sub.ThreadSpawn != nil && session.ValidID(sub.ThreadSpawn.ParentThreadID) {
						parentID = sub.ThreadSpawn.ParentThreadID
					}
				}
				return sessionID, parentID, false
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return "", "", true
			}
			break
		}
	}
	return "", "", false
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

// shellReserved lists the words a shell reads as syntax when they start a command.
const shellReserved = "if then else elif fi for while until do done case esac select function time coproc in"

// shellArgvCommand renders argv as the shell command that would run it. Words a shell
// reads as themselves stay bare, so the scanner still sees command names and flags (it
// reads them as literal words and a quoted word has none); everything else is single-quoted
// so "$foo", spaces and globs keep their argv boundaries and meaning. The first word is
// also quoted when a shell would read it as an assignment or a reserved word.
func shellArgvCommand(argv []string) string {
	words := make([]string, len(argv))
	for i, arg := range argv {
		bare := arg != "" && strings.Trim(arg, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_@%+=:,./-") == ""
		if i == 0 && (strings.Contains(arg, "=") || slices.Contains(strings.Fields(shellReserved), arg)) {
			bare = false
		}
		words[i] = arg
		if !bare {
			words[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
		}
	}
	return strings.Join(words, " ")
}

// codexCommandReachPaths returns what a CommandExecution's argv reaches: its working
// directory dir, already resolved against the session's, when it differs from the
// session's cwd, and the paths the shell scan finds in the command, which unwraps
// `bash -c '<script>'` the same way the live Codex hook does.
func codexCommandReachPaths(cmd []string, dir, sessionCwd string) []string {
	var out []string
	if dir != "" && dir != sessionCwd {
		out = append(out, hookrun.Alone(dir)...)
	}
	if len(cmd) == 0 {
		return out
	}
	return append(out, hookrun.Alone(hookrun.ShellPathsHistorical(shellArgvCommand(cmd), dir)...)...)
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
		patchText := args
		var wrapped struct {
			Input string `json:"input"`
			Patch string `json:"patch"`
		}
		if json.Unmarshal([]byte(args), &wrapped) == nil {
			if p := cmp.Or(wrapped.Input, wrapped.Patch); p != "" {
				patchText = p
			}
		}
		return hookrun.Alone(applypatch.Paths(patchText)...)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &fields) != nil {
		return nil
	}
	strField := func(k string) string {
		var s string
		if raw, ok := fields[k]; ok && json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	var out []string
	dir := cwd
	if raw := cmp.Or(strField("workdir"), strField("cwd")); strings.TrimSpace(raw) != "" {
		dir = codexNormalizeCwd(raw, cwd)
		out = append(out, hookrun.Alone(dir)...)
	}
	scanCmd := func(raw json.RawMessage) {
		if len(raw) == 0 {
			return
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if s != "" {
				out = append(out, hookrun.Alone(hookrun.ShellPathsHistorical(s, dir)...)...)
			}
			return
		}
		var argv []string
		if json.Unmarshal(raw, &argv) == nil && len(argv) > 0 {
			out = append(out, codexCommandReachPaths(argv, dir, cwd)...)
		}
	}
	if raw, ok := fields["cmd"]; ok {
		scanCmd(raw)
	}
	if raw, ok := fields["command"]; ok {
		scanCmd(raw)
	}
	for _, k := range []string{"file_path", "path"} {
		if p := strField(k); p != "" {
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
