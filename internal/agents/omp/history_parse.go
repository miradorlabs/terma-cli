package omp

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
)

type ompPendingTool struct {
	Name         string
	ArgsJSON     string
	ArgsRaw      json.RawMessage
	At           time.Time
	TraceID      []byte
	ParentSpanID []byte
	Copied       bool
}

type ompPendingPrompt struct {
	Parts    []ompOtelPart
	Attached bool
}

type ompHistoryParser struct {
	rootSessionID       string
	includeSupplemental bool
	emit                func(agents.HistorySegment) error

	curSessionID    string
	parentSessionID string
	subagent        bool
	agentID         string
	agentName       string
	serviceVersion  string

	cwd                string
	headerTime         time.Time
	lastTS             time.Time
	copiedFromParent   bool
	slotTitle          string
	slotTitleAt        time.Time
	headerTitle        string
	lastEmittedTitle   string
	lastTitleAt        time.Time
	subagentStartSent  bool
	systemPrompt       []string
	systemPromptSent   bool
	availableTools     []string
	reasoningEffort    string
	configuredModel    string
	configuredProvider string

	turnSeq          int
	chatSeq          int
	toolSeq          int
	usageSeq         int
	stepNumber       int
	turnTraceID      []byte
	turnInvokeSpanID []byte
	pendingPrompt    *ompPendingPrompt
	pendingTools     map[string]ompPendingTool

	segSpans   []*tracepb.Span
	segSupp    []*logspb.LogRecord
	segReaches []agents.HistoryToolReach
}

func newOmpHistoryParser(rootSessionID string, includeSupplemental bool, emit func(agents.HistorySegment) error) *ompHistoryParser {
	return &ompHistoryParser{
		rootSessionID:       rootSessionID,
		includeSupplemental: includeSupplemental,
		emit:                emit,
		pendingTools:        map[string]ompPendingTool{},
	}
}

func (p *ompHistoryParser) resetForFile(sessionID, parentID, agentID string, subagent bool) {
	clear(p.pendingTools)
	*p = ompHistoryParser{
		rootSessionID:       p.rootSessionID,
		includeSupplemental: p.includeSupplemental,
		emit:                p.emit,
		pendingTools:        p.pendingTools,
		curSessionID:        sessionID,
		parentSessionID:     parentID,
		subagent:            subagent,
		agentID:             agentID,
	}
}

func (p *ompHistoryParser) readFile(ctx context.Context, path, defaultSessionID, parentID, agentID string, subagent bool) error {
	p.resetForFile(defaultSessionID, parentID, agentID, subagent)
	if err := readOmpJSONLLines(ctx, path, p.handleLine); err != nil {
		return err
	}
	return p.finishFile()
}

func (p *ompHistoryParser) handleLine(line []byte) error {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return nil
	}
	var rec ompLine
	if json.Unmarshal(line, &rec) != nil {
		return nil
	}
	if rec.Runtime != nil && rec.Runtime.Version != "" && p.serviceVersion == "" {
		p.serviceVersion = rec.Runtime.Version
	}
	ts := parseOmpTime(rec.Timestamp)
	p.observeTS(ts)
	switch rec.Type {
	case "title":
		if t := strings.TrimSpace(rec.Title); t != "" {
			p.slotTitle = t
			p.slotTitleAt = parseOmpTime(rec.UpdatedAt)
		}
	case "session":
		return p.handleSessionHeader(&rec, ts)
	case "title_change":
		if !p.subagent && !p.isCopiedPrefix(ts) {
			if t := strings.TrimSpace(rec.Title); t != "" {
				return p.emitTitle(t, ts)
			}
		}
	case "session_init":
		p.handleSessionInit(&rec)
	case "thinking_level_change":
		lvl := strings.TrimSpace(cmp.Or(rec.ThinkingLevel, rec.Configured))
		if strings.EqualFold(lvl, "off") || strings.EqualFold(lvl, "none") {
			lvl = ""
		}
		p.reasoningEffort = lvl
	case "model_change":
		if rec.Role == "" || rec.Role == "default" {
			if prov, mod, ok := strings.Cut(rec.Model, "/"); ok && prov != "" && mod != "" {
				p.configuredProvider, p.configuredModel = prov, mod
			} else if rec.Model != "" {
				p.configuredModel = rec.Model
				if rec.Provider != "" {
					p.configuredProvider = rec.Provider
				}
			}
		}
	case "message":
		return p.handleMessage(&rec, ts)
	case "model_usage":
		return p.handleModelUsage(&rec, ts)
	}
	return nil
}

func (p *ompHistoryParser) observeTS(ts time.Time) {
	if !ts.IsZero() && ts.After(p.lastTS) {
		p.lastTS = ts
	}
}

func (p *ompHistoryParser) isCopiedPrefix(ts time.Time) bool {
	return p.copiedFromParent && !p.headerTime.IsZero() && !ts.IsZero() && ts.Before(p.headerTime)
}

func (p *ompHistoryParser) handleSessionHeader(rec *ompLine, ts time.Time) error {
	if session.ValidID(rec.ID) {
		p.curSessionID = rec.ID
	}
	if p.subagent && p.agentID == "" {
		p.agentID = p.curSessionID
	}
	p.cwd = strings.TrimSpace(rec.Cwd)
	if !ts.IsZero() {
		p.headerTime = ts
	}
	if !p.subagent && (strings.TrimSpace(rec.ParentSession) != "" || strings.TrimSpace(rec.ContinuationOf) != "") {
		p.copiedFromParent = true
	}
	if t := strings.TrimSpace(rec.Title); t != "" {
		p.headerTitle = t
	}
	// OMP's /move rewrites header.cwd to the new checkout while keeping earlier entries and
	// listing prior transcript paths in previousSessionFiles. Because earlier turns may belong
	// to another organization's checkout, fail closed on moved transcripts.
	if len(rec.PreviousSessionFiles) > 0 {
		p.segReaches = append(p.segReaches, agents.HistoryToolReach{
			At:    time.Time{},
			Cwd:   p.cwd,
			Paths: []string{hookrun.UnknownReach + "\x00session moved between working directories"},
		})
	}
	// Record each session's (parent and subagent) checkout placement at header time so the
	// Mixed prepass (IncludeSupplemental: false) knows when each org was entered even before
	// the first span or when a child emits no spans.
	if p.cwd != "" {
		p.segReaches = append(p.segReaches, agents.HistoryToolReach{
			At:    ts,
			Cwd:   p.cwd,
			Paths: hookrun.Alone(p.cwd),
		})
	}
	// Multi-root workspace directories recorded on the session header are reachable from
	// session start, so crossing into another organization fails closed at header time.
	var extraDirs []string
	for _, d := range rec.AdditionalDirectories {
		if d = strings.TrimSpace(d); d != "" && d != p.cwd {
			extraDirs = append(extraDirs, hookrun.Alone(d)...)
		}
	}
	if len(extraDirs) > 0 {
		p.segReaches = append(p.segReaches, agents.HistoryToolReach{At: ts, Cwd: p.cwd, Paths: extraDirs})
	}
	return nil
}

func (p *ompHistoryParser) handleSessionInit(rec *ompLine) {
	if rec.Agent != "" {
		p.agentName = rec.Agent
	}
	if len(rec.Tools) > 0 {
		p.availableTools = rec.Tools
	}
	raw := bytes.TrimSpace(rec.SystemPrompt)
	if len(raw) == 0 {
		return
	}
	var next []string
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			next = []string{s}
		}
	case '[':
		var parts []string
		if json.Unmarshal(raw, &parts) == nil && len(parts) > 0 {
			next = parts
		}
	}
	if len(next) > 0 && !slices.Equal(p.systemPrompt, next) {
		p.systemPrompt = next
		p.systemPromptSent = false
	}
}

func (p *ompHistoryParser) handleMessage(rec *ompLine, entryTS time.Time) error {
	if len(rec.Message) == 0 {
		return nil
	}
	var msg ompMessage
	if json.Unmarshal(rec.Message, &msg) != nil {
		return nil
	}
	p.observeTS(messageTime(msg.Timestamp, entryTS))
	switch msg.Role {
	case "user":
		if msg.Synthetic {
			return nil
		}
		if p.isCopiedPrefix(messageTime(msg.Timestamp, entryTS)) {
			return nil
		}
		parts, hasContent := userContentToOtelParts(msg.Content)
		if !hasContent {
			return nil
		}
		p.openTurn(rec.ID)
		p.pendingPrompt = &ompPendingPrompt{Parts: parts}
	case "assistant":
		return p.handleAssistant(rec, &msg, entryTS)
	case "toolResult":
		return p.handleToolResult(rec, &msg, entryTS)
	case "fileMention":
		var paths []string
		for _, f := range msg.Files {
			resolved, unknown := resolveReachPath(f.Path)
			if unknown {
				paths = append(paths, hookrun.UnknownReach+"\x00"+f.Path)
			} else if resolved != "" {
				paths = append(paths, hookrun.Alone(resolved)...)
			}
		}
		if len(paths) > 0 {
			p.segReaches = append(p.segReaches, agents.HistoryToolReach{
				At:    messageTime(msg.Timestamp, entryTS),
				Cwd:   p.cwd,
				Paths: paths,
			})
		}
	case "bashExecution":
		if strings.TrimSpace(msg.Command) != "" {
			ts := messageTime(msg.Timestamp, entryTS)
			if paths := hookrun.Alone(hookrun.ShellPathsHistorical(msg.Command, p.cwd)...); len(paths) > 0 {
				p.segReaches = append(p.segReaches, agents.HistoryToolReach{At: ts, Cwd: p.cwd, Paths: paths})
			}
		}
	}
	return nil
}

func (p *ompHistoryParser) openTurn(entryID string) {
	p.turnSeq++
	key := cmp.Or(entryID, fmt.Sprintf("turn-%d", p.turnSeq))
	p.turnTraceID = deriveTraceID(p.curSessionID, key)
	p.turnInvokeSpanID = deriveSpanID(p.curSessionID, "invoke:"+key)
	p.stepNumber = 0
}

func (p *ompHistoryParser) handleAssistant(rec *ompLine, msg *ompMessage, entryTS time.Time) error {
	start, end := assistantSpanTimes(msg, entryTS)
	reachAt := end
	if !entryTS.IsZero() && (reachAt.IsZero() || reachAt.After(entryTS)) {
		reachAt = entryTS
	}
	blocks := parseOmpContentBlocks(msg.Content)
	copied := p.isCopiedPrefix(reachAt)

	if !copied {
		if len(p.turnTraceID) == 0 {
			p.openTurn(rec.ID)
			// No preceding user message: start at step 1 so adapter/omp never fabricates a
			// phantom empty user prompt from step==0 without gen_ai.input.messages.
			p.stepNumber = 1
		}
		p.ensureSubagentStart(entryTS)
		step := p.stepNumber
		p.stepNumber++
		p.chatSeq++
		p.segSpans = append(p.segSpans, p.makeChatSpan(rec.ID, msg, blocks, step, start, end))
	}

	for _, blk := range blocks {
		if blk.Type != "toolCall" {
			continue
		}
		if blk.ID != "" {
			p.pendingTools[blk.ID] = ompPendingTool{
				Name:         blk.Name,
				ArgsJSON:     boundOmpToolArgs(blk.Arguments),
				ArgsRaw:      blk.Arguments,
				At:           reachAt,
				TraceID:      p.turnTraceID,
				ParentSpanID: p.turnInvokeSpanID,
				Copied:       copied,
			}
		}
		if paths := ompToolCallReachPaths(blk.Name, blk.Arguments, p.cwd); len(paths) > 0 {
			p.segReaches = append(p.segReaches, agents.HistoryToolReach{At: reachAt, Cwd: p.cwd, Paths: paths})
		}
	}
	return p.maybeFlushSegment()
}

func (p *ompHistoryParser) handleToolResult(rec *ompLine, msg *ompMessage, entryTS time.Time) error {
	if msg.ToolCallID == "" {
		return nil
	}
	tool, seen := p.pendingTools[msg.ToolCallID]
	delete(p.pendingTools, msg.ToolCallID)
	if !seen {
		return nil
	}
	end := messageTime(msg.Timestamp, entryTS)
	start := tool.At
	if start.IsZero() || (!end.IsZero() && start.After(end)) {
		start = end
	}
	if end.IsZero() {
		end = start
	}
	toolName := cmp.Or(msg.ToolName, tool.Name)
	var details *ompToolDetails
	if len(bytes.TrimSpace(msg.Details)) > 0 && !bytes.Equal(bytes.TrimSpace(msg.Details), []byte("null")) {
		var d ompToolDetails
		if json.Unmarshal(msg.Details, &d) == nil {
			details = &d
		}
	}
	if paths := ompToolResultReachPaths(toolName, details); len(paths) > 0 {
		p.segReaches = append(p.segReaches, agents.HistoryToolReach{At: end, Cwd: p.cwd, Paths: paths})
	}
	if tool.Copied || p.isCopiedPrefix(end) {
		return p.maybeFlushSegment()
	}
	p.toolSeq++
	p.segSpans = append(p.segSpans, p.makeExecuteToolSpan(rec.ID, msg, tool, toolName, details, start, end))

	if p.includeSupplemental {
		if edited := ompEditedFiles(toolName, tool.ArgsRaw, details, msg.IsError); len(edited) > 0 {
			p.segSupp = append(p.segSupp, p.makeFilesTouchedRecord(toolName, msg.ToolCallID, edited, end))
		}
	}
	return p.maybeFlushSegment()
}

func (p *ompHistoryParser) handleModelUsage(rec *ompLine, entryTS time.Time) error {
	if rec.Usage == nil || p.isCopiedPrefix(entryTS) {
		return nil
	}
	inputTotal := rec.Usage.Input + rec.Usage.CacheRead + rec.Usage.CacheWrite
	costTotal := usageCostTotal(rec.Usage)
	if inputTotal <= 0 && rec.Usage.Output <= 0 && costTotal <= 0 {
		return nil
	}
	p.ensureSubagentStart(entryTS)
	end := entryTS
	start := end
	if d := millisDuration(rec.Duration); d > 0 && !end.IsZero() {
		start = end.Add(-d)
	}
	p.usageSeq++
	p.segSpans = append(p.segSpans, p.makeAuxiliarySpan(rec, start, end))
	return p.maybeFlushSegment()
}

func (p *ompHistoryParser) finishFile() error {
	if p.includeSupplemental && !p.subagent {
		switch {
		case p.slotTitle != "" && p.slotTitle != p.lastEmittedTitle && !p.slotTitleAt.IsZero() &&
			!p.isCopiedPrefix(p.slotTitleAt) &&
			(p.lastTitleAt.IsZero() || !p.slotTitleAt.Before(p.lastTitleAt)):
			if err := p.emitTitle(p.slotTitle, p.slotTitleAt); err != nil {
				return err
			}
		case p.lastEmittedTitle == "":
			if fallback := cmp.Or(p.slotTitle, p.headerTitle); fallback != "" {
				// OMP rewrites header.title on rename without updating header.timestamp;
				// emit as an undated title (TimeUnixNano=0, ObservedTimeUnixNano=lastTS) so
				// FilterMixedLogs withholds it whenever any Mixed cutoff exists.
				if err := p.emitUndatedTitle(fallback, cmp.Or(p.lastTS, p.headerTime)); err != nil {
					return err
				}
			}
		}
	}
	return p.flushSegment()
}
