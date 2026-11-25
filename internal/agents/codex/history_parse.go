package codex

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

type codexHistoryParser struct {
	sessionID           string
	includeSupplemental bool
	emit                func(agents.HistorySegment) error

	metaTime      time.Time
	metaCwd       string
	metaRepoURL   string
	metaBranch    string
	originator    string
	cliVersion    string
	modelProvider string
	firstModel    string
	firstEffort   string

	cwd             string
	model           string
	effort          string
	turnID          string
	traceID         string
	traceBytes      []byte
	seenUsageRecord bool
	lastTotalTokens int64
	lastPromptTurn  string
	lastPromptText  string
	lastEventNanos  map[string]uint64

	pendingCalls  map[string]codexPendingCall
	priorCwdCalls map[string]bool
	emittedCalls  map[string]bool

	segStarted     bool
	segCwd         string
	segHasStart    bool
	segNative      []*logspb.LogRecord
	segSupplements []*logspb.LogRecord
	segReaches     []agents.HistoryToolReach
}

func newCodexHistoryParser(sessionID string, includeSupplemental bool, emit func(agents.HistorySegment) error) *codexHistoryParser {
	return &codexHistoryParser{
		sessionID:           sessionID,
		includeSupplemental: includeSupplemental,
		emit:                emit,
		originator:          codexDefaultOrigin,
		modelProvider:       codexDefaultProv,
		lastEventNanos:      map[string]uint64{},
		pendingCalls:        map[string]codexPendingCall{},
		priorCwdCalls:       map[string]bool{},
		emittedCalls:        map[string]bool{},
	}
}

type codexRolloutLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

func (p *codexHistoryParser) handleLine(line []byte, offset int64) error {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return nil
	}
	var rec codexRolloutLine
	if json.Unmarshal(line, &rec) != nil {
		return nil
	}
	ts := parseCodexTime(rec.Timestamp)
	switch rec.Type {
	case "session_meta":
		return p.handleSessionMeta(rec.Payload, ts)
	case "turn_context":
		return p.handleTurnContext(rec.Payload)
	case "token_usage_record":
		return p.handleTokenUsageRecord(rec.Payload, ts)
	case "event_msg":
		return p.handleEventMsg(rec.Payload, ts)
	case "response_item":
		return p.handleResponseItem(rec.Payload, ts, offset)
	}
	return nil
}

func (p *codexHistoryParser) finish() error {
	return p.flushSegment()
}

func (p *codexHistoryParser) handleSessionMeta(raw json.RawMessage, ts time.Time) error {
	var pl struct {
		ID            string `json:"id"`
		SessionID     string `json:"session_id"`
		Timestamp     string `json:"timestamp"`
		Cwd           string `json:"cwd"`
		Originator    string `json:"originator"`
		CLIVersion    string `json:"cli_version"`
		ModelProvider string `json:"model_provider"`
		Git           *struct {
			Branch        string `json:"branch"`
			RepositoryURL string `json:"repository_url"`
		} `json:"git"`
	}
	if json.Unmarshal(raw, &pl) != nil {
		return nil
	}
	if id := cmp.Or(pl.ID, pl.SessionID); session.ValidID(id) && p.sessionID == "" {
		p.sessionID = id
	}
	if t := parseCodexTime(pl.Timestamp); !t.IsZero() {
		p.metaTime = t
	} else if !ts.IsZero() {
		p.metaTime = ts
	}
	if pl.Originator != "" {
		p.originator = pl.Originator
	}
	if pl.CLIVersion != "" {
		p.cliVersion = pl.CLIVersion
	}
	if pl.ModelProvider != "" {
		p.modelProvider = pl.ModelProvider
	}
	p.metaCwd = pl.Cwd
	if pl.Git != nil {
		p.metaRepoURL = pl.Git.RepositoryURL
		p.metaBranch = pl.Git.Branch
	}
	return p.setCwd(pl.Cwd)
}

func (p *codexHistoryParser) handleTurnContext(raw json.RawMessage) error {
	var pl struct {
		TurnID            string `json:"turn_id"`
		Cwd               string `json:"cwd"`
		Model             string `json:"model"`
		Effort            string `json:"effort"`
		CollaborationMode struct {
			Settings struct {
				Model           string `json:"model"`
				ReasoningEffort string `json:"reasoning_effort"`
			} `json:"settings"`
		} `json:"collaboration_mode"`
	}
	if json.Unmarshal(raw, &pl) != nil {
		return nil
	}
	if pl.Cwd != "" {
		if err := p.setCwd(pl.Cwd); err != nil {
			return err
		}
	}
	if m := cmp.Or(pl.Model, pl.CollaborationMode.Settings.Model); m != "" {
		p.model = m
		p.firstModel = cmp.Or(p.firstModel, m)
	}
	if e := cmp.Or(pl.Effort, pl.CollaborationMode.Settings.ReasoningEffort); e != "" {
		p.effort = e
		p.firstEffort = cmp.Or(p.firstEffort, e)
	}
	if pl.TurnID != "" && pl.TurnID != p.turnID {
		p.turnID, p.traceID, p.traceBytes = "", "", nil
		if hookrun.EvidenceLabel.MatchString(pl.TurnID) {
			p.turnID = pl.TurnID
		}
	}
	return nil
}

func (p *codexHistoryParser) handleTokenUsageRecord(raw json.RawMessage, ts time.Time) error {
	var pl struct {
		Usage *codexUsageNumbers `json:"usage"`
	}
	if json.Unmarshal(raw, &pl) != nil || pl.Usage == nil {
		return nil
	}
	p.seenUsageRecord = true
	return p.emitModelCall(*pl.Usage, ts)
}

func (p *codexHistoryParser) handleEventMsg(raw json.RawMessage, ts time.Time) error {
	var pl struct {
		Type           string `json:"type"`
		TurnID         string `json:"turn_id"`
		TraceID        string `json:"trace_id"`
		Message        string `json:"message"`
		ThreadSettings struct {
			Cwd             string `json:"cwd"`
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoning_effort"`
		} `json:"thread_settings"`
		Info *struct {
			TotalTokenUsage *codexUsageNumbers `json:"total_token_usage"`
			LastTokenUsage  *codexUsageNumbers `json:"last_token_usage"`
		} `json:"info"`
		Item *struct {
			Type             string   `json:"type"`
			ID               string   `json:"id"`
			Status           string   `json:"status"`
			Cwd              string   `json:"cwd"`
			Path             string   `json:"path"`
			Command          []string `json:"command"`
			ExitCode         *int64   `json:"exit_code"`
			FormattedOutput  string   `json:"formatted_output"`
			AggregatedOutput string   `json:"aggregated_output"`
			Duration         *struct {
				Secs  int64 `json:"secs"`
				Nanos int64 `json:"nanos"`
			} `json:"duration"`
			Changes []struct {
				Path string `json:"path"`
			} `json:"changes"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &pl) != nil {
		return nil
	}
	switch pl.Type {
	case "thread_settings_applied":
		if pl.ThreadSettings.Cwd != "" {
			if err := p.setCwd(pl.ThreadSettings.Cwd); err != nil {
				return err
			}
		}
		if pl.ThreadSettings.Model != "" {
			p.model = pl.ThreadSettings.Model
			p.firstModel = cmp.Or(p.firstModel, p.model)
		}
		if pl.ThreadSettings.ReasoningEffort != "" {
			p.effort = pl.ThreadSettings.ReasoningEffort
			p.firstEffort = cmp.Or(p.firstEffort, p.effort)
		}
	case "task_started", "turn_started":
		if pl.TurnID != p.turnID {
			p.turnID, p.traceID, p.traceBytes = "", "", nil
			if hookrun.EvidenceLabel.MatchString(pl.TurnID) {
				p.turnID = pl.TurnID
			}
		}
		if spool.ValidTraceID(pl.TraceID) {
			p.traceID = pl.TraceID
			p.traceBytes, _ = hex.DecodeString(pl.TraceID)
		}
	case "task_complete", "turn_complete", "turn_aborted":
		p.turnID, p.traceID, p.traceBytes = "", "", nil
		clear(p.pendingCalls)
	case "user_message":
		if strings.TrimSpace(pl.Message) != "" {
			return p.emitUserPrompt(pl.Message, ts)
		}
	case "token_count":
		if p.seenUsageRecord || pl.Info == nil || pl.Info.LastTokenUsage == nil {
			return nil
		}
		if pl.Info.TotalTokenUsage != nil {
			if pl.Info.TotalTokenUsage.TotalTokens <= p.lastTotalTokens {
				return nil
			}
			p.lastTotalTokens = pl.Info.TotalTokenUsage.TotalTokens
		}
		return p.emitModelCall(*pl.Info.LastTokenUsage, ts)
	case "item_completed":
		if pl.Item == nil {
			return nil
		}
		switch pl.Item.Type {
		case "UserMessage":
			var b strings.Builder
			for _, c := range pl.Item.Content {
				if c.Text != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(c.Text)
				}
			}
			if text := b.String(); strings.TrimSpace(text) != "" {
				return p.emitUserPrompt(text, ts)
			}
		case "CommandExecution":
			if pl.Item.ID == "" || p.emittedCalls[pl.Item.ID] {
				return nil
			}
			p.emittedCalls[pl.Item.ID] = true
			cmdCwd := codexNormalizeCwd(pl.Item.Cwd, p.cwd)
			if paths := codexCommandReachPaths(pl.Item.Command, cmdCwd, p.cwd); len(paths) > 0 {
				p.addReach(ts, cmdCwd, paths)
			}
			argsJSON, _ := json.Marshal(map[string]any{"cmd": strings.Join(pl.Item.Command, " ")})
			out := cmp.Or(pl.Item.FormattedOutput, pl.Item.AggregatedOutput)
			success := pl.Item.Status == "completed"
			if pl.Item.ExitCode != nil {
				success = *pl.Item.ExitCode == 0
				if out == "" {
					out = fmt.Sprintf("Process exited with code %d", *pl.Item.ExitCode)
				}
			}
			var durMs int64 = -1
			if pl.Item.Duration != nil {
				durMs = pl.Item.Duration.Secs*1000 + pl.Item.Duration.Nanos/1e6
			}
			return p.emitToolResult(pl.Item.ID, "exec_command", string(argsJSON), out, success, durMs, p.traceBytes, ts)
		case "ImageView":
			// A code-mode script's image read shows up only as this item.
			if pl.Item.Path != "" {
				p.addReach(ts, p.cwd, hookrun.Alone(codexNormalizeCwd(pl.Item.Path, p.cwd)))
			}
		case "FileChange":
			var paths []string
			if pl.Item.Path != "" {
				paths = append(paths, pl.Item.Path)
			}
			for _, ch := range pl.Item.Changes {
				if ch.Path != "" {
					paths = append(paths, ch.Path)
				}
			}
			if len(paths) > 0 {
				p.addReach(ts, codexNormalizeCwd(pl.Item.Cwd, p.cwd), hookrun.Alone(paths...))
			}
		}
	}
	return nil
}

func (p *codexHistoryParser) handleResponseItem(raw json.RawMessage, ts time.Time, offset int64) error {
	var pl struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		ID        string          `json:"id"`
		Phase     string          `json:"phase"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Input     string          `json:"input"`
		Arguments string          `json:"arguments"`
		Output    json.RawMessage `json:"output"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		InternalMeta *struct {
			ContentItemKinds []string `json:"content_item_kinds"`
		} `json:"internal_chat_message_metadata_passthrough"`
	}
	if json.Unmarshal(raw, &pl) != nil {
		return nil
	}
	switch pl.Type {
	case "message":
		switch pl.Role {
		case "user":
			text := joinCodexText(pl.Content, "input_text")
			if strings.TrimSpace(text) == "" || !isCodexUserPrompt(text, pl.InternalMeta != nil, kindsOf(pl.InternalMeta)) {
				return nil
			}
			return p.emitUserPrompt(text, ts)
		case "assistant":
			if !p.includeSupplemental {
				return nil
			}
			text := joinCodexText(pl.Content, "output_text")
			if strings.TrimSpace(text) == "" {
				return nil
			}
			msgID := pl.ID
			if !hookrun.EvidenceLabel.MatchString(msgID) {
				msgID = fundingHash(fmt.Sprintf("%s:%d", p.sessionID, offset))
			}
			phase := ""
			if hookrun.EvidenceLabel.MatchString(pl.Phase) {
				phase = pl.Phase
			}
			return p.emitAssistantReply(msgID, text, phase, ts)
		}
	case "custom_tool_call":
		if pl.CallID != "" {
			p.pendingCalls[pl.CallID] = codexPendingCall{Name: pl.Name, Arguments: pl.Input, Cwd: p.cwd, At: ts, TraceID: p.traceBytes}
		}
		if paths := codexToolReachPaths(pl.Name, pl.Input, p.cwd); len(paths) > 0 {
			p.addReach(ts, p.cwd, paths)
		}
	case "function_call":
		if pl.CallID != "" {
			p.pendingCalls[pl.CallID] = codexPendingCall{Name: pl.Name, Arguments: pl.Arguments, Cwd: p.cwd, At: ts, TraceID: p.traceBytes}
		}
		if paths := codexToolReachPaths(pl.Name, pl.Arguments, p.cwd); len(paths) > 0 {
			p.addReach(ts, p.cwd, paths)
		}
	case "custom_tool_call_output", "function_call_output":
		if pl.CallID == "" || p.emittedCalls[pl.CallID] {
			return nil
		}
		p.emittedCalls[pl.CallID] = true
		call, hadCall := p.pendingCalls[pl.CallID]
		delete(p.pendingCalls, pl.CallID)
		// Drop delayed tool outputs whose originating call started under a different cwd
		// so neither arguments nor returned file contents ever cross organization boundaries.
		if p.priorCwdCalls[pl.CallID] || (hadCall && call.Cwd != p.cwd) {
			delete(p.priorCwdCalls, pl.CallID)
			return nil
		}
		out := decodeCodexOutput(pl.Output)
		success := codexOutputSuccess(pl.Output, out)
		durMs := int64(-1)
		if !call.At.IsZero() && !ts.IsZero() && !ts.Before(call.At) {
			durMs = ts.Sub(call.At).Milliseconds()
		}
		trace := call.TraceID
		if len(trace) == 0 {
			trace = p.traceBytes
		}
		return p.emitToolResult(pl.CallID, call.Name, call.Arguments, out, success, durMs, trace, ts)
	}
	return nil
}
