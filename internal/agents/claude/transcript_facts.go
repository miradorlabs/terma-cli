package claude

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
)

// Claude Code's telemetry names the model it asked for and the call's token counts, but not
// what the API answered with: the model that served the call, why it stopped, its service tier
// and region, or (from 2.1.231 on) how many output tokens were thinking. Only its transcript
// records those, in each assistant record. Nor does it say when a refusal moved the session to
// another model: only a system record in the transcript does.

const (
	// claudeInferenceGeoUnknown is how the transcript says the API reported no region.
	claudeInferenceGeoUnknown = "not_available"
	// claudeModelFallbackSubtype is the system record of a refusal that moved the session to another model.
	claudeModelFallbackSubtype = "model_refusal_fallback"
	// claudeFallbackScopeSession is the one scope a fallback is read in: the whole session moved.
	claudeFallbackScopeSession = "session"
)

// claudeCallResponseStateDir holds how far each transcript's facts have been read. Its name
// predates the fallbacks, and keeping it keeps every transcript's offset across the upgrade.
var claudeCallResponseStateDir = hookrun.AgentStateDir(name, "call-responses")

// claudeCallResponse is what the API answered for one model call, as its transcript records it.
type claudeCallResponse struct {
	RequestID string
	// ThinkingTokens is nil when the build records no count.
	ThinkingTokens *int64
	Model          string
	StopReason     string
	ServiceTier    string
	InferenceGeo   string
	// Time is when the call's first final record was written, zero when it has none.
	Time time.Time
}

// claudeModelFallback is a refusal that moved the session to another model, as its transcript
// records it: never the record's text or the API's explanation.
type claudeModelFallback struct {
	// RequestID is the refused call's.
	RequestID string
	From      string
	To        string
	Trigger   string
	Category  string
	Time      time.Time
}

// claudeTranscriptRecord is the subset of a transcript record readTranscriptFacts reads: an
// assistant record's response, or a fallback's system record. Only named labels are decoded, so
// no text in the record can reach an event.
type claudeTranscriptRecord struct {
	Type          string    `json:"type"`
	Subtype       string    `json:"subtype"`
	IsSidechain   bool      `json:"isSidechain"`
	RequestID     string    `json:"requestId"`
	Timestamp     time.Time `json:"timestamp"`
	OriginalModel string    `json:"originalModel"`
	FallbackModel string    `json:"fallbackModel"`
	Trigger       string    `json:"trigger"`
	Scope         string    `json:"scope"`
	Category      string    `json:"apiRefusalCategory"`
	Message       struct {
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			ServiceTier         string `json:"service_tier"`
			InferenceGeo        string `json:"inference_geo"`
			OutputTokensDetails *struct {
				ThinkingTokens *int64 `json:"thinking_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	} `json:"message"`
}

// callResponseState is the transcript offset up to which its facts were spooled.
type callResponseState struct {
	Offset int64 `json:"offset"`
}

// captureClaudeTranscriptFacts spools what the transcript recorded since the last read: what the
// API answered for each call, one terma.model_call.response per call, and each refusal that moved
// the session to another model, one terma.model.fallback, when a turn stops or fails, a subagent
// stops, or the session ends. Only labels and counts leave the machine, under the title's consent.
func captureClaudeTranscriptFacts(e hookrun.Env, r *hookrun.Repo, sessionID, transcript string) {
	if transcript == "" || e.Spool == nil || !session.ValidID(sessionID) || !titleConsented(e.Consent()) {
		return
	}
	dir := filepath.Join(e.StateDir, claudeCallResponseStateDir)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	path := filepath.Join(dir, hookrun.EvidenceID(transcript)+".json")
	// A hook already reading this transcript spools what this one would.
	unlock, err := hookrun.LockEvidence(path + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	var last callResponseState
	b, readErr := os.ReadFile(path)
	if readErr == nil && json.Unmarshal(b, &last) != nil {
		last = callResponseState{}
	}
	calls, fallbacks, offset, err := readTranscriptFacts(transcript, last.Offset)
	if err != nil {
		e.Logf("transcript facts: %v", err)
		return
	}
	for _, c := range calls {
		attrs := map[string]any{
			semconv.GenAIMainAgentNameKey:         claudeTool,
			semconv.TermaEvidenceSourceKey:        sourceClaudeTranscript,
			semconv.GenAIResponseFinishReasonsKey: []string{c.StopReason},
		}
		hookrun.BoundedAttr(attrs, semconv.TermaModelCallIDKey, c.RequestID)
		if attrs[semconv.TermaModelCallIDKey] == nil {
			continue
		}
		if c.ThinkingTokens != nil {
			attrs[semconv.GenAIUsageReasoningOutputTokensKey] = *c.ThinkingTokens
		}
		hookrun.BoundedAttr(attrs, semconv.GenAIResponseModelKey, c.Model)
		hookrun.BoundedAttr(attrs, semconv.TermaResponseServiceTierKey, c.ServiceTier)
		hookrun.BoundedAttr(attrs, semconv.TermaResponseInferenceGeoKey, c.InferenceGeo)
		// Unsent calls stay past the stored offset, for the next stop; the platform folds a re-sent one away.
		if !e.EmitFor(r, spool.Event{Time: c.Time, Name: semconv.TermaModelCallResponseEvent, SessionID: sessionID, Attrs: attrs}) {
			return
		}
	}
	for _, f := range fallbacks {
		attrs := map[string]any{semconv.GenAIMainAgentNameKey: claudeTool, semconv.TermaEvidenceSourceKey: sourceClaudeTranscript}
		hookrun.BoundedAttr(attrs, semconv.TermaModelCallIDKey, f.RequestID)
		hookrun.BoundedAttr(attrs, semconv.TermaModelFallbackModelKey, f.To)
		if attrs[semconv.TermaModelCallIDKey] == nil || attrs[semconv.TermaModelFallbackModelKey] == nil {
			continue
		}
		hookrun.BoundedAttr(attrs, semconv.GenAIRequestModelKey, f.From)
		hookrun.BoundedAttr(attrs, semconv.TermaModelFallbackTriggerKey, f.Trigger)
		hookrun.BoundedAttr(attrs, semconv.TermaRefusalCategoryKey, f.Category)
		// The platform keys a fallback by its refused call, so a re-sent one folds away too.
		if !e.EmitFor(r, spool.Event{Time: f.Time, Name: semconv.TermaModelFallbackEvent, SessionID: sessionID, Attrs: attrs}) {
			return
		}
	}
	if b, err := json.Marshal(callResponseState{Offset: offset}); err == nil {
		if err := hookrun.WriteState(path, b); err != nil {
			e.Logf("transcript facts state: %v", err)
		}
	}
	if os.IsNotExist(readErr) {
		hookrun.PruneState(dir, e.Time().Add(-spool.MaxAge))
	}
}

// readTranscriptFacts returns the response of each call recorded in the transcript's whole lines
// from offset on, in the order the calls start, the main thread's session fallbacks in the order
// recorded, and the offset after the last whole line. Claude Code writes a call's record again as
// its response streams, and only a record with the call's stop_reason is final: the fold keeps
// the smallest count it is sent, so a call with no final record yet is left for the read that
// finds one.
// Accepted risk: a fallback a subagent's transcript records, or one scoped to less than the
// session, is not read, as none was ever seen. Upgrade path: send it with terma.agent.id.
func readTranscriptFacts(path string, offset int64) ([]claudeCallResponse, []claudeModelFallback, int64, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, offset, nil
	}
	if err != nil {
		return nil, nil, offset, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil, offset, err
	}
	if offset > info.Size() {
		// A transcript shorter than what was read is a new file at the same path.
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, nil, offset, err
	}
	var calls []claudeCallResponse
	var fallbacks []claudeModelFallback
	index := map[string]int{}
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			// A line without its newline is still being written.
			if errors.Is(err, io.EOF) {
				return calls, fallbacks, offset, nil
			}
			return calls, fallbacks, offset, err
		}
		offset += int64(len(line))
		if !bytes.Contains(line, []byte("stop_reason")) && !bytes.Contains(line, []byte(claudeModelFallbackSubtype)) {
			continue
		}
		var rec claudeTranscriptRecord
		if json.Unmarshal(line, &rec) != nil || rec.RequestID == "" {
			continue
		}
		if rec.Type == "system" && rec.Subtype == claudeModelFallbackSubtype {
			if !rec.IsSidechain && rec.Scope == claudeFallbackScopeSession {
				fallbacks = append(fallbacks, claudeModelFallback{
					RequestID: rec.RequestID, From: rec.OriginalModel, To: rec.FallbackModel,
					Trigger: rec.Trigger, Category: rec.Category, Time: rec.Timestamp,
				})
			}
			continue
		}
		if rec.Type != "assistant" || rec.Message.Model == "<synthetic>" || rec.Message.StopReason == "" {
			continue
		}
		i, seen := index[rec.RequestID]
		if !seen {
			i = len(calls)
			index[rec.RequestID] = i
			calls = append(calls, claudeCallResponse{RequestID: rec.RequestID, Time: rec.Timestamp})
		}
		// A call's final records agree; one that omits a label keeps the one before it.
		c, u := &calls[i], rec.Message.Usage
		c.StopReason = rec.Message.StopReason
		c.Model = cmp.Or(rec.Message.Model, c.Model)
		c.ServiceTier = cmp.Or(u.ServiceTier, c.ServiceTier)
		if u.InferenceGeo != claudeInferenceGeoUnknown {
			c.InferenceGeo = cmp.Or(u.InferenceGeo, c.InferenceGeo)
		}
		if d := u.OutputTokensDetails; d != nil && d.ThinkingTokens != nil && *d.ThinkingTokens >= 0 {
			c.ThinkingTokens = d.ThinkingTokens
		}
	}
}
