package claude

import (
	"bufio"
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
	"github.com/miradorlabs/terma-cli/internal/session"
)

const claudeSourceSystem = "claude"

// SourceSystem is the platform's source_system identifier for Claude Code sessions.
func (Agent) SourceSystem() string { return claudeSourceSystem }

// claudeProjectsDir returns the directory holding per-project Claude Code transcripts,
// honouring CLAUDE_CONFIG_DIR just as exporter.ConfigPath does.
func claudeProjectsDir() (string, error) {
	cfgPath, err := (exporter{}).ConfigPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfgPath), "projects"), nil
}

// DiscoverHistory lists local Claude Code transcripts under $CLAUDE_CONFIG_DIR/projects
// (default ~/.claude/projects). Transcripts older than opts.Since are returned with
// OutsideSince set, and those modified within opts.ActiveWindow of opts.Now are returned
// with Active set so the caller can skip and report both without reading them.
func (Agent) DiscoverHistory(ctx context.Context, opts agents.HistoryDiscoverOptions) ([]agents.HistoryFile, error) {
	projectsDir, err := claudeProjectsDir()
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
	projEntries, err := os.ReadDir(projectsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	bySession := map[string]agents.HistoryFile{}
	for _, pe := range projEntries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !pe.IsDir() || pe.Type()&os.ModeSymlink != 0 {
			continue
		}
		projPath := filepath.Join(projectsDir, pe.Name())
		entries, err := os.ReadDir(projPath)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if ent.IsDir() || ent.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(ent.Name(), ".jsonl") {
				continue
			}
			mainPath := filepath.Join(projPath, ent.Name())
			info, err := ent.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			stem := strings.TrimSuffix(ent.Name(), ".jsonl")
			modTime := info.ModTime()
			subDir := filepath.Join(projPath, stem, "subagents")
			extras, extraMod, subErr := findClaudeSubagents(subDir)
			if extraMod.After(modTime) {
				modTime = extraMod
			}
			active := now.Sub(modTime) < activeWindow
			outside := !opts.Since.IsZero() && modTime.Before(opts.Since)
			sid := stem
			if !session.ValidID(sid) && !outside && !active {
				if parsed := peekClaudeSessionID(mainPath); parsed != "" {
					sid = parsed
				}
			}
			if !session.ValidID(sid) {
				continue
			}
			hf := agents.HistoryFile{
				SourceSystem:       claudeSourceSystem,
				SessionID:          sid,
				Path:               mainPath,
				ExtraPaths:         extras,
				WatchDirs:          []string{subDir},
				ModTime:            modTime,
				Active:             active,
				OutsideSince:       outside,
				CompanionReadError: subErr,
			}
			if prev, exists := bySession[sid]; !exists || hf.ModTime.After(prev.ModTime) {
				bySession[sid] = hf
			}
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

// findClaudeSubagents lists regular .jsonl files in a session's subagents directory and
// returns them sorted, their latest modification time, and the directory or entry that
// could not be read for a reason other than not existing, empty when none (so callers fail
// closed instead of treating unreadable children as absent).
func findClaudeSubagents(dir string) ([]string, time.Time, string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, time.Time{}, ""
		}
		return nil, time.Time{}, dir
	}
	var paths []string
	var latest time.Time
	readErr := ""
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".jsonl") {
			continue
		}
		if ent.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) && readErr == "" {
				readErr = filepath.Join(dir, ent.Name())
			}
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		paths = append(paths, filepath.Join(dir, ent.Name()))
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	slices.Sort(paths)
	return paths, latest, readErr
}

// peekClaudeSessionID scans the leading lines of a Claude Code transcript for its recorded
// sessionId without loading the whole file.
func peekClaudeSessionID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReader(f)
	for range 64 {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var rec struct {
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(line, &rec) == nil && session.ValidID(rec.SessionID) {
				return rec.SessionID
			}
		}
		if err != nil {
			break
		}
	}
	return ""
}

// ReadHistory converts a Claude Code transcript (and any companion subagent transcripts)
// into contiguous OTLP log segments grouped by recorded working directory.
func (Agent) ReadHistory(ctx context.Context, file agents.HistoryFile, _ agents.HistoryReadOptions, emit func(agents.HistorySegment) error) error {
	p := newClaudeHistoryParser(file.SessionID, emit)
	if err := p.readFile(ctx, file.Path, ""); err != nil {
		return err
	}
	for _, subPath := range file.ExtraPaths {
		if err := ctx.Err(); err != nil {
			return err
		}
		agentID := claudeSubagentIDFromPath(subPath)
		if err := p.readFile(ctx, subPath, agentID); err != nil {
			return err
		}
	}
	return p.finish()
}

func claudeSubagentIDFromPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	return strings.TrimPrefix(base, "agent-")
}

var _ agents.HistorySource = Agent{}

// readJSONLLines streams lines from path using bufio.Reader so arbitrarily large records
// never hit bufio.Scanner's token limit.
func readJSONLLines(ctx context.Context, path string, handle func(line []byte) error) error {
	f, err := os.Open(path)
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
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, rerr := br.ReadBytes('\n')
		if len(line) > 0 {
			if err := handle(line); err != nil {
				return err
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}
