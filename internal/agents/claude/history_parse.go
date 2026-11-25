package claude

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"strings"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/session"
)

type claudeAPIRequestMeta struct {
	QuerySource string
	Effort      string
	Model       string
}

type claudePendingTool struct {
	Name      string
	InputJSON string
	Cwd       string
	At        time.Time
	PromptID  string
	AgentID   string
}

type claudePendingCall struct {
	key           string
	cwd           string
	subagent      bool
	sessionID     string
	agentID       string
	promptID      string
	requestID     string
	requestRef    string
	messageID     string
	model         string
	effort        string
	querySource   string
	speed         string
	stopReason    string
	isAPIError    bool
	costUSD       *float64
	durationMs    *int64
	at            time.Time
	hasUsage      bool
	inputTokens   int64
	outputTokens  int64
	cacheRead     int64
	cacheCreation int64
	cache5m       int64
	cache1h       int64
	textUUID      string
	textAt        time.Time
	textParts     []string
}

type claudeHistoryParser struct {
	sessionID      string
	emit           func(agents.HistorySegment) error
	serviceName    string
	serviceVersion string
	seq            int64
	promptID       string

	apiReqs map[string]claudeAPIRequestMeta
	tools   map[string]claudePendingTool
	pending *claudePendingCall

	segStarted  bool
	segCwd      string
	segSubagent bool
	segRecords  []*logspb.LogRecord
	segReaches  []agents.HistoryToolReach
}

func newClaudeHistoryParser(sessionID string, emit func(agents.HistorySegment) error) *claudeHistoryParser {
	return &claudeHistoryParser{
		sessionID:   sessionID,
		emit:        emit,
		serviceName: claudeServiceCLI,
		apiReqs:     map[string]claudeAPIRequestMeta{},
		tools:       map[string]claudePendingTool{},
	}
}

type claudeTranscriptLine struct {
	Type               string          `json:"type"`
	UUID               string          `json:"uuid"`
	SessionID          string          `json:"sessionId"`
	AgentID            string          `json:"agentId"`
	Cwd                string          `json:"cwd"`
	Version            string          `json:"version"`
	Entrypoint         string          `json:"entrypoint"`
	Timestamp          string          `json:"timestamp"`
	PromptID           string          `json:"promptId"`
	RequestID          string          `json:"requestId"`
	RequestRef         string          `json:"requestRef"`
	Effort             string          `json:"effort"`
	PerTurnEffort      string          `json:"perTurnEffort"`
	QuerySource        string          `json:"querySource"`
	IsMeta             bool            `json:"isMeta"`
	IsAPIErrorMessage  bool            `json:"isApiErrorMessage"`
	CostUSD            *float64        `json:"costUSD"`
	DurationMs         *int64          `json:"durationMs"`
	ID                 string          `json:"id"`
	Params             json.RawMessage `json:"params"`
	Message            json.RawMessage `json:"message"`
	PermissionDecision *struct {
		Decision string `json:"decision"`
		Source   string `json:"source"`
	} `json:"permissionDecision"`
}

type claudeMessageEnvelope struct {
	ID         string          `json:"id"`
	Role       string          `json:"role"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Content    json.RawMessage `json:"content"`
	Usage      *claudeUsage    `json:"usage"`
}

type claudeUsage struct {
	InputTokens              int64  `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
	Speed                    string `json:"speed"`
	CacheCreation            *struct {
		Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

type claudeContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

func (p *claudeHistoryParser) readFile(ctx context.Context, path, defaultAgentID string) error {
	if err := p.flushPendingCall(); err != nil {
		return err
	}
	if defaultAgentID != "" {
		if err := p.flushSegment(); err != nil {
			return err
		}
	}
	p.promptID = ""
	return readJSONLLines(ctx, path, func(line []byte) error {
		return p.handleLine(line, defaultAgentID)
	})
}

func (p *claudeHistoryParser) finish() error {
	if err := p.flushPendingCall(); err != nil {
		return err
	}
	return p.flushSegment()
}

func (p *claudeHistoryParser) handleLine(line []byte, defaultAgentID string) error {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return nil
	}
	var rec claudeTranscriptLine
	if json.Unmarshal(line, &rec) != nil {
		return nil
	}
	if rec.SessionID != "" && session.ValidID(rec.SessionID) && p.sessionID == "" {
		p.sessionID = rec.SessionID
	}
	if rec.Version != "" && p.serviceVersion == "" {
		p.serviceVersion = rec.Version
	}
	if rec.Entrypoint == "claude-desktop" || rec.Entrypoint == "desktop" {
		p.serviceName = claudeServiceDesktop
	}

	switch rec.Type {
	case "api-request":
		p.recordAPIRequestMeta(&rec)
	case "user":
		if err := p.flushPendingCall(); err != nil {
			return err
		}
		return p.handleUser(&rec, defaultAgentID)
	case "assistant":
		return p.handleAssistant(&rec, defaultAgentID)
	}
	return nil
}

func (p *claudeHistoryParser) recordAPIRequestMeta(rec *claudeTranscriptLine) {
	if rec.ID == "" {
		return
	}
	var params struct {
		Model        string `json:"model"`
		OutputConfig struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	_ = json.Unmarshal(rec.Params, &params)
	p.apiReqs[rec.ID] = claudeAPIRequestMeta{
		QuerySource: rec.QuerySource,
		Effort:      params.OutputConfig.Effort,
		Model:       params.Model,
	}
}

func (p *claudeHistoryParser) handleUser(rec *claudeTranscriptLine, defaultAgentID string) error {
	if rec.IsMeta || len(rec.Message) == 0 {
		return nil
	}
	var msg claudeMessageEnvelope
	if json.Unmarshal(rec.Message, &msg) != nil {
		return nil
	}
	ts := parseClaudeTime(rec.Timestamp)
	sid := cmp.Or(rec.SessionID, p.sessionID)
	agentID := cmp.Or(rec.AgentID, defaultAgentID)
	if rec.PromptID != "" {
		p.promptID = rec.PromptID
	}

	raw := bytes.TrimSpace(msg.Content)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil || strings.TrimSpace(text) == "" {
			return nil
		}
		return p.appendToSegment(rec.Cwd, defaultAgentID != "", p.makeUserPromptRecord(sid, agentID, rec.UUID, p.promptID, text, ts))
	}
	if raw[0] != '[' {
		return nil
	}
	var blocks []claudeContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var promptTexts []string
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			if strings.TrimSpace(blk.Text) != "" {
				promptTexts = append(promptTexts, blk.Text)
			}
		case "tool_result":
			if blk.ToolUseID == "" {
				continue
			}
			tool, seenTool := p.tools[blk.ToolUseID]
			delete(p.tools, blk.ToolUseID)
			// Never carry a tool call's arguments or delayed output across a working-directory boundary.
			if seenTool && tool.Cwd != rec.Cwd {
				continue
			}
			promptID := cmp.Or(p.promptID, tool.PromptID)
			tAgent := cmp.Or(agentID, tool.AgentID)
			if rec.PermissionDecision != nil && rec.PermissionDecision.Decision != "" {
				decRec := p.makeToolDecisionRecord(sid, tAgent, promptID, blk.ToolUseID, tool.Name, rec.PermissionDecision.Decision, rec.PermissionDecision.Source, ts)
				if err := p.appendToSegment(rec.Cwd, defaultAgentID != "", decRec); err != nil {
					return err
				}
			}
			trRec := p.makeToolResultRecord(sid, tAgent, promptID, blk.ToolUseID, tool, blk, ts)
			if err := p.appendToSegment(rec.Cwd, defaultAgentID != "", trRec); err != nil {
				return err
			}
		}
	}
	if len(promptTexts) > 0 {
		text := strings.Join(promptTexts, "\n")
		return p.appendToSegment(rec.Cwd, defaultAgentID != "", p.makeUserPromptRecord(sid, agentID, rec.UUID, p.promptID, text, ts))
	}
	return nil
}

func (p *claudeHistoryParser) handleAssistant(rec *claudeTranscriptLine, defaultAgentID string) error {
	if len(rec.Message) == 0 {
		return nil
	}
	var msg claudeMessageEnvelope
	if json.Unmarshal(rec.Message, &msg) != nil {
		return nil
	}
	if msg.Model == "<synthetic>" && !rec.IsAPIErrorMessage {
		return nil
	}
	ts := parseClaudeTime(rec.Timestamp)
	sid := cmp.Or(rec.SessionID, p.sessionID)
	agentID := cmp.Or(rec.AgentID, defaultAgentID)
	callKey := cmp.Or(rec.RequestID, rec.RequestRef, msg.ID, rec.UUID)
	if callKey == "" {
		return nil
	}
	if p.pending != nil && (p.pending.key != callKey || p.pending.cwd != rec.Cwd) {
		if err := p.flushPendingCall(); err != nil {
			return err
		}
	}
	meta := p.apiReqs[rec.RequestRef]
	if p.pending == nil {
		p.pending = &claudePendingCall{
			key:         callKey,
			cwd:         rec.Cwd,
			subagent:    defaultAgentID != "",
			sessionID:   sid,
			agentID:     agentID,
			promptID:    p.promptID,
			requestID:   rec.RequestID,
			requestRef:  rec.RequestRef,
			messageID:   msg.ID,
			model:       cmp.Or(msg.Model, meta.Model),
			effort:      cmp.Or(rec.Effort, rec.PerTurnEffort, meta.Effort),
			querySource: cmp.Or(rec.QuerySource, meta.QuerySource),
			isAPIError:  rec.IsAPIErrorMessage,
			costUSD:     rec.CostUSD,
			durationMs:  rec.DurationMs,
			at:          ts,
		}
	}
	return p.mergeAssistantChunk(p.pending, rec, &msg, ts)
}

func (p *claudeHistoryParser) mergeAssistantChunk(c *claudePendingCall, rec *claudeTranscriptLine, msg *claudeMessageEnvelope, ts time.Time) error {
	c.requestID = cmp.Or(c.requestID, rec.RequestID)
	c.requestRef = cmp.Or(c.requestRef, rec.RequestRef)
	c.messageID = cmp.Or(c.messageID, msg.ID)
	c.model = cmp.Or(msg.Model, c.model)
	c.effort = cmp.Or(rec.Effort, rec.PerTurnEffort, c.effort)
	c.querySource = cmp.Or(rec.QuerySource, c.querySource)
	if rec.IsAPIErrorMessage {
		c.isAPIError = true
	}
	if rec.CostUSD != nil {
		c.costUSD = rec.CostUSD
	}
	if rec.DurationMs != nil {
		c.durationMs = rec.DurationMs
	}
	if !ts.IsZero() {
		c.at = ts
	}
	if msg.StopReason != "" {
		c.stopReason = msg.StopReason
	}
	if u := msg.Usage; u != nil {
		if !c.hasUsage || msg.StopReason != "" || u.OutputTokens >= c.outputTokens {
			c.hasUsage = true
			c.inputTokens = u.InputTokens
			c.outputTokens = u.OutputTokens
			c.cacheRead = u.CacheReadInputTokens
			c.cacheCreation = u.CacheCreationInputTokens
			if u.Speed != "" {
				c.speed = u.Speed
			}
			if u.CacheCreation != nil {
				c.cache5m = u.CacheCreation.Ephemeral5mInputTokens
				c.cache1h = u.CacheCreation.Ephemeral1hInputTokens
			}
		}
	}

	raw := bytes.TrimSpace(msg.Content)
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var blocks []claudeContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			if strings.TrimSpace(blk.Text) != "" {
				c.textParts = append(c.textParts, blk.Text)
				c.textUUID = cmp.Or(rec.UUID, c.textUUID)
				c.textAt = ts
			}
		case "tool_use":
			if blk.ID != "" {
				p.tools[blk.ID] = claudePendingTool{
					Name:      blk.Name,
					InputJSON: compactRawJSON(blk.Input),
					Cwd:       c.cwd,
					At:        ts,
					PromptID:  c.promptID,
					AgentID:   c.agentID,
				}
			}
			if paths := claudeToolReachPaths(blk.Name, blk.Input, c.cwd); len(paths) > 0 {
				if err := p.addReach(c.cwd, c.subagent, ts, paths); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
