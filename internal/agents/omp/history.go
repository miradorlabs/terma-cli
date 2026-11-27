package omp

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
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/session"
)

const (
	ompSourceSystem = name
	ompServiceName  = "oh-my-pi"
	ompTermaScope   = "terma-cli"
)

var ompFilenameRe = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d-\d\d-\d\d-\d{3}Z_(.+)\.jsonl$`)

// SourceSystem is the platform's source_system identifier for OMP sessions.
func (Agent) SourceSystem() string { return ompSourceSystem }

// ompSessionsDirs returns the session roots OMP stores per-project transcripts in:
// <ompAgentDir>/sessions (honouring PI_CODING_AGENT_DIR and PI_CONFIG_DIR) plus
// $XDG_DATA_HOME/omp/sessions when PI_CODING_AGENT_DIR is unset and XDG_DATA_HOME is set.
func ompSessionsDirs() ([]string, error) {
	dir, err := ompAgentDir()
	if err != nil {
		return nil, err
	}
	roots := []string{filepath.Join(dir, "sessions")}
	if strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")) == "" {
		if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
			xdgSessions := filepath.Clean(filepath.Join(xdg, "omp", "sessions"))
			if !slices.Contains(roots, xdgSessions) {
				roots = append(roots, xdgSessions)
			}
		}
	}
	return roots, nil
}

// DiscoverHistory lists local OMP transcripts under <ompAgentDir>/sessions (default
// ~/.omp/agent/sessions) and $XDG_DATA_HOME/omp/sessions when XDG_DATA_HOME is set.
// Subagent companion transcripts under <parent-stem>/<agent>.jsonl (and nested subagents
// below them) are attached as ExtraPaths on their root parent session; forks and
// continuations live as sibling top-level files and stay independent sessions.
func (Agent) DiscoverHistory(ctx context.Context, opts agents.HistoryDiscoverOptions) ([]agents.HistoryFile, error) {
	sessionsDirs, err := ompSessionsDirs()
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
	addFile := func(hf agents.HistoryFile) {
		if prev, exists := bySession[hf.SessionID]; !exists || hf.ModTime.After(prev.ModTime) {
			bySession[hf.SessionID] = hf
		}
	}
	for _, sessionsDir := range sessionsDirs {
		projEntries, err := os.ReadDir(sessionsDir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, pe := range projEntries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !pe.IsDir() || pe.Type()&os.ModeSymlink != 0 {
				continue
			}
			projPath := filepath.Join(sessionsDir, pe.Name())
			entries, err := os.ReadDir(projPath)
			if err != nil {
				continue
			}
			topStems := map[string]bool{}
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
				topStems[stem] = true
				modTime := info.ModTime()
				subDir := filepath.Join(projPath, stem)
				extras, watchDirs, seenWatch, extraMod, subErr := findOmpSubagents(ctx, mainPath, subDir)
				if extraMod.After(modTime) {
					modTime = extraMod
				}
				active := now.Sub(modTime) < activeWindow
				outside := !opts.Since.IsZero() && modTime.Before(opts.Since)
				sid := ompSessionIDFromFilename(ent.Name())
				var parentID string
				var oneWayCutoff time.Time
				hdr, peekErr := peekOmpSessionHeader(mainPath)
				if peekErr != nil && !errors.Is(peekErr, fs.ErrNotExist) && subErr == "" {
					subErr = mainPath
				}
				if hdr.Type == "session" {
					if !session.ValidID(sid) && session.ValidID(hdr.ID) {
						sid = hdr.ID
					}
					if ref := cmp.Or(strings.TrimSpace(hdr.ParentSession), strings.TrimSpace(hdr.ContinuationOf)); ref != "" {
						parentID = ompParentSessionID(ref)
						oneWayCutoff = parseOmpTime(hdr.Timestamp)
					}
				}
				if !session.ValidID(sid) {
					continue
				}
				addFile(agents.HistoryFile{
					SourceSystem:       ompSourceSystem,
					SessionID:          sid,
					ParentSessionID:    parentID,
					OneWayParent:       parentID != "",
					OneWayCutoff:       oneWayCutoff,
					Path:               mainPath,
					ExtraPaths:         extras,
					WatchDirs:          watchDirs,
					SeenWatchFiles:     seenWatch,
					ModTime:            modTime,
					Active:             active,
					OutsideSince:       outside,
					CompanionReadError: subErr,
				})
			}
			// Discover orphaned subagent transcripts whose parent .jsonl was deleted so history
			// upload fails closed on them instead of silently dropping or re-parenting them.
			for _, ent := range entries {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if !ent.IsDir() || ent.Type()&os.ModeSymlink != 0 || topStems[ent.Name()] {
					continue
				}
				orphanParentPath := filepath.Join(projPath, ent.Name()+".jsonl")
				orphanDir := filepath.Join(projPath, ent.Name())
				missingParentID := ompSessionIDFromFilename(ent.Name() + ".jsonl")
				if !session.ValidID(missingParentID) {
					missingParentID = "missing-parent-" + ent.Name()
				}
				for _, hf := range findOrphanOmpSubagents(ctx, orphanParentPath, orphanDir, missingParentID, now, activeWindow, opts.Since) {
					addFile(hf)
				}
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

type ompSessionHeader struct {
	Type                  string   `json:"type"`
	Version               int      `json:"version"`
	ID                    string   `json:"id"`
	Title                 string   `json:"title"`
	Timestamp             string   `json:"timestamp"`
	Cwd                   string   `json:"cwd"`
	ParentSession         string   `json:"parentSession"`
	ContinuationOf        string   `json:"continuationOf"`
	AdditionalDirectories []string `json:"additionalDirectories"`
}

// findOmpSubagents recursively discovers companion subagent transcripts inside stemDir
// whose header's parentSession points to parentPath, returning them sorted parent-before-child,
// every watched directory, any non-matching .jsonl files already present in those directories,
// the latest modification time, and the first unreadable path if any.
func findOmpSubagents(ctx context.Context, parentPath, stemDir string) (paths, watchDirs, seenWatch []string, latest time.Time, readErr string) {
	watchDirs = []string{stemDir}
	var walk func(curParent, curDir string, depth int)
	walk = func(curParent, curDir string, depth int) {
		if depth > 8 || ctx.Err() != nil {
			return
		}
		entries, err := os.ReadDir(curDir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) && readErr == "" {
				readErr = curDir
			}
			return
		}
		jsonStems := map[string]bool{}
		for _, ent := range entries {
			if ctx.Err() != nil {
				return
			}
			if ent.IsDir() || ent.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(ent.Name(), ".jsonl") {
				continue
			}
			jsonStems[strings.TrimSuffix(ent.Name(), ".jsonl")] = true
			subPath := filepath.Join(curDir, ent.Name())
			info, err := ent.Info()
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) && readErr == "" {
					readErr = subPath
				}
				continue
			}
			if !info.Mode().IsRegular() {
				continue
			}
			hdr, err := peekOmpSessionHeader(subPath)
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) && readErr == "" {
					readErr = subPath
				}
				continue
			}
			if hdr.Type != "session" || !session.ValidID(hdr.ID) {
				// Incomplete or non-session file: do not exempt in seenWatch so recheck
				// treats a freshly-started subagent transcript as active.
				continue
			}
			if !sameParentSessionPath(subPath, curParent, hdr.ParentSession, runtime.GOOS == "windows") {
				seenWatch = append(seenWatch, subPath)
				continue
			}
			paths = append(paths, subPath)
			if info.ModTime().After(latest) {
				latest = info.ModTime()
			}
			nestedDir := strings.TrimSuffix(subPath, ".jsonl")
			watchDirs = append(watchDirs, nestedDir)
			if depth < 8 {
				walk(subPath, nestedDir, depth+1)
			} else if info, err := os.Stat(nestedDir); (err == nil && info.IsDir() && dirHasJSONLOrErr(ctx, nestedDir, 0)) || (err != nil && !errors.Is(err, fs.ErrNotExist)) {
				if readErr == "" {
					readErr = nestedDir
				}
			}
		}
		// If an intermediate subagent transcript was deleted while a nested subdirectory
		// still holds grandchild transcripts (or cannot be read), fail closed on the parent.
		for _, ent := range entries {
			if ctx.Err() != nil {
				return
			}
			if !ent.IsDir() || ent.Type()&os.ModeSymlink != 0 || jsonStems[ent.Name()] {
				continue
			}
			orphanSubDir := filepath.Join(curDir, ent.Name())
			if dirHasJSONLOrErr(ctx, orphanSubDir, depth+1) && readErr == "" {
				readErr = orphanSubDir
			}
		}
	}
	walk(parentPath, stemDir, 0)
	sortPathsParentFirst(paths)
	slices.Sort(watchDirs)
	watchDirs = slices.Compact(watchDirs)
	slices.Sort(seenWatch)
	seenWatch = slices.Compact(seenWatch)
	return paths, watchDirs, seenWatch, latest, readErr
}

func findOrphanOmpSubagents(ctx context.Context, expectedParent, dir, missingParentID string, now time.Time, activeWindow time.Duration, since time.Time) []agents.HistoryFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []agents.HistoryFile
	for _, ent := range entries {
		if ctx.Err() != nil {
			return out
		}
		if ent.IsDir() || ent.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(ent.Name(), ".jsonl") {
			continue
		}
		subPath := filepath.Join(dir, ent.Name())
		info, err := ent.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		hdr, err := peekOmpSessionHeader(subPath)
		if err != nil || hdr.Type != "session" || !session.ValidID(hdr.ID) {
			continue
		}
		if !sameParentSessionPath(subPath, expectedParent, hdr.ParentSession, runtime.GOOS == "windows") {
			continue
		}
		modTime := info.ModTime()
		out = append(out, agents.HistoryFile{
			SourceSystem:    ompSourceSystem,
			SessionID:       hdr.ID,
			ParentSessionID: missingParentID,
			Path:            subPath,
			ModTime:         modTime,
			Active:          now.Sub(modTime) < activeWindow,
			OutsideSince:    !since.IsZero() && modTime.Before(since),
		})
	}
	return out
}

// peekOmpSessionHeader reads the leading lines of an OMP transcript (skipping any optional
// fixed-width first-line title slot) and returns its session header.
func peekOmpSessionHeader(path string) (ompSessionHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return ompSessionHeader{}, err
	}
	defer func() { _ = f.Close() }()
	lr := &io.LimitedReader{R: f, N: 1 << 20}
	br := bufio.NewReader(lr)
	for range 8 {
		line, rerr := br.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 && trimmed[0] == '{' {
			var hdr ompSessionHeader
			if json.Unmarshal(trimmed, &hdr) == nil && hdr.Type == "session" {
				return hdr, nil
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return ompSessionHeader{}, rerr
		}
	}
	if lr.N == 0 {
		return ompSessionHeader{}, errors.New("omp session header exceeds 1 MiB")
	}
	return ompSessionHeader{}, nil
}

// ReadHistory converts an OMP transcript (and any companion subagent transcripts) into
// OTLP trace segments (matching OMP's live @oh-my-pi/pi-agent-core spans) plus supplemental
// terma-cli log records (session titles, subagent starts, and confirmed file touches).
func (Agent) ReadHistory(ctx context.Context, file agents.HistoryFile, opts agents.HistoryReadOptions, emit func(agents.HistorySegment) error) error {
	p := newOmpHistoryParser(file.SessionID, opts.IncludeSupplemental, emit)
	pathToID := map[string]string{}

	isSubagent := false
	if file.ParentSessionID != "" {
		if hdr, err := peekOmpSessionHeader(file.Path); err == nil && hdr.Type == "session" &&
			sameParentSessionPath(file.Path, filepath.Dir(file.Path)+".jsonl", hdr.ParentSession, runtime.GOOS == "windows") {
			isSubagent = true
		}
	}
	if err := p.readFile(ctx, file.Path, file.SessionID, file.ParentSessionID, "", isSubagent); err != nil {
		return err
	}
	if p.curSessionID != "" {
		pathToID[filepath.Clean(file.Path)] = p.curSessionID
	}

	extras := slices.Clone(file.ExtraPaths)
	sortPathsParentFirst(extras)
	for _, subPath := range extras {
		if err := ctx.Err(); err != nil {
			return err
		}
		parentPath := filepath.Dir(subPath) + ".jsonl"
		parentID := pathToID[filepath.Clean(parentPath)]
		if parentID == "" {
			parentID = ompSessionIDFromFilename(filepath.Base(parentPath))
		}
		if parentID == "" {
			if hdr, err := peekOmpSessionHeader(parentPath); err == nil && session.ValidID(hdr.ID) {
				parentID = hdr.ID
			}
		}
		if parentID == "" {
			parentID = file.SessionID
		}
		agentID := strings.TrimSuffix(filepath.Base(subPath), ".jsonl")
		if err := p.readFile(ctx, subPath, "", parentID, agentID, true); err != nil {
			return err
		}
		if p.curSessionID != "" {
			pathToID[filepath.Clean(subPath)] = p.curSessionID
		}
	}
	return nil
}

func readOmpJSONLLines(ctx context.Context, path string, handle func(line []byte) error) error {
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

var _ agents.HistorySource = Agent{}
