package api

import "time"

// The AI read surface lives under /v1/ai on the data plane. These shapes mirror the
// gateway's OpenAPI schemas. Two rules from the platform shape everything here:
//
//   - A session is identified by session_id + source_system. The id is an opaque
//     token the gateway hands out on a list row — the routing key behind it is never
//     exposed — and every session-scoped read takes both halves.
//   - Principals are ids, never names. user_id / api_key_id are stable and non-PII;
//     the readable name (an email, a key label) comes from ListAIPrincipals and is
//     joined at display time.

// AITokenUsage is a settled model call's, or a whole session's, token and cost roll-up.
// Cost is the provider's price in dollars: an analytics figure, not a ledger value.
type AITokenUsage struct {
	InputTokens      uint64  `json:"input_tokens"`
	OutputTokens     uint64  `json:"output_tokens"`
	CacheReadTokens  uint64  `json:"cache_read_tokens"`
	CacheWriteTokens uint64  `json:"cache_write_tokens"`
	ProviderCostUsd  float64 `json:"provider_cost_usd"`
}

// TotalTokens is every token the provider billed for, across all four buckets.
func (u *AITokenUsage) TotalTokens() uint64 {
	if u == nil {
		return 0
	}
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// CostUSD is the cost in dollars. A nil usage is "no usage recorded" and costs nothing.
func (u *AITokenUsage) CostUSD() float64 {
	if u == nil {
		return 0
	}
	return u.ProviderCostUsd
}

// Add folds another usage into this one. A nil src is "no usage recorded" and adds nothing.
func (u *AITokenUsage) Add(src *AITokenUsage) {
	if src == nil {
		return
	}
	u.InputTokens += src.InputTokens
	u.OutputTokens += src.OutputTokens
	u.CacheReadTokens += src.CacheReadTokens
	u.CacheWriteTokens += src.CacheWriteTokens
	u.ProviderCostUsd += src.ProviderCostUsd
}

// AISession is one coding session's roll-up over its events — the gateway's
// AISessionSummary. It carries no events; those are ListAISessionEvents.
type AISession struct {
	SourceSystem          string        `json:"source_system"`
	SessionID             string        `json:"session_id"`
	UserID                string        `json:"user_id,omitempty"`
	APIKeyID              string        `json:"api_key_id,omitempty"`
	SubjectOrganizationID string        `json:"subject_organization_id,omitempty"`
	FirstSessionTime      *time.Time    `json:"first_session_time,omitempty"`
	LastActivityAt        *time.Time    `json:"last_activity_at,omitempty"`
	SessionVersion        uint64        `json:"session_version,omitempty"`
	Turns                 uint64        `json:"turns"`
	ModelCalls            uint64        `json:"model_calls"`
	ToolCalls             uint64        `json:"tool_calls"`
	UserMessages          uint64        `json:"user_messages"`
	Usage                 *AITokenUsage `json:"usage,omitempty"`
	Models                []string      `json:"models,omitempty"`
	Providers             []string      `json:"providers,omitempty"`
	BranchesCreated       uint64        `json:"branches_created"`
	Commits               uint64        `json:"commits"`
	GitPushes             uint64        `json:"git_pushes"`
	PullRequestsCreated   uint64        `json:"pull_requests_created"`
	PullRequestsMerged    uint64        `json:"pull_requests_merged"`
	GitOrgs               []string      `json:"git_orgs,omitempty"`
	Repositories          []string      `json:"repositories,omitempty"`
	FilesTouched          uint64        `json:"files_touched"`
}

// Session rankings, as the gateway spells them. Every one is descending.
const (
	AISortRecency = "recency"
	AISortCost    = "cost"
	AISortTokens  = "tokens"
	AISortTurns   = "turns"
	AISortTools   = "tools"
)

// AISessionSorts lists every ranking, the gateway's default first.
var AISessionSorts = []string{AISortRecency, AISortCost, AISortTokens, AISortTurns, AISortTools}

// AIPagination is offset pagination over the filtered session catalog. Page is
// 1-indexed and Total is exact for the filter as of when the page was read.
type AIPagination struct {
	Page       int   `json:"page"`
	PerPage    int   `json:"per_page"`
	Total      int64 `json:"total"`
	TotalPages int   `json:"total_pages"`
}

// AISessionsPage is one page of the ranked session list.
type AISessionsPage struct {
	Sessions   []AISession  `json:"sessions"`
	Pagination AIPagination `json:"pagination"`
	ProjectID  string       `json:"project_id,omitempty"`
}

// AIContentPart is one piece of an event's verbatim content.
type AIContentPart struct {
	PartIndex   int64  `json:"part_index"`
	ContentType string `json:"content_type"`
	Content     string `json:"content"`
}

// Event kinds, as the gateway spells them.
const (
	AIEventSessionStart     = "session_start"
	AIEventTurnStart        = "turn_start"
	AIEventUserMessage      = "user_message"
	AIEventAssistantMessage = "assistant_message"
	AIEventModelCall        = "model_call"
	AIEventToolCall         = "tool_call"
	AIEventToolResult       = "tool_result"
	AIEventToolDecision     = "tool_decision"
	AIEventCompaction       = "compaction"
	AIEventError            = "error"
)

// AIEventKinds lists every kind, in the order a conversation produces them.
var AIEventKinds = []string{
	AIEventSessionStart, AIEventTurnStart, AIEventUserMessage, AIEventAssistantMessage,
	AIEventModelCall, AIEventToolCall, AIEventToolResult, AIEventToolDecision,
	AIEventCompaction, AIEventError,
}

// AISessionEvent is one logical event of a session in conversational order.
type AISessionEvent struct {
	Kind      string          `json:"kind"`
	Sequence  *int64          `json:"sequence,omitempty"`
	EventTime time.Time       `json:"event_time"`
	TurnID    string          `json:"turn_id,omitempty"`
	Content   []AIContentPart `json:"content,omitempty"`
	// Usage is present only on a settled model call. Its absence means "no usage
	// recorded", which is different from a zero-cost call.
	Usage        *AITokenUsage `json:"usage,omitempty"`
	Model        string        `json:"model,omitempty"`
	Provider     string        `json:"provider,omitempty"`
	ToolName     string        `json:"tool_name,omitempty"`
	Status       string        `json:"status,omitempty"`
	FinishReason string        `json:"finish_reason,omitempty"`
	DurationMs   *int64        `json:"duration_ms,omitempty"`
	TtftMs       *uint64       `json:"ttft_ms,omitempty"`
	ModelCallID  string        `json:"model_call_id,omitempty"`
	ToolCallID   string        `json:"tool_call_id,omitempty"`
	// LogicalEventID is the event's stable identity across pages and corrections; for
	// one id, the copy with the higher Version supersedes.
	LogicalEventID string `json:"logical_event_id,omitempty"`
	Version        int64  `json:"version,omitempty"`
	// Cursor is this event's own keyset position within the session.
	Cursor string `json:"cursor,omitempty"`
}

// AISessionEventsResponse is one keyset page of a session's events, oldest first.
// NextCursor is empty on the last page.
type AISessionEventsResponse struct {
	SessionID    string           `json:"session_id"`
	SourceSystem string           `json:"source_system"`
	Events       []AISessionEvent `json:"events"`
	NextCursor   string           `json:"next_cursor,omitempty"`
}

// Principal kinds, as the gateway spells them.
const (
	AIPrincipalUser   = "user"
	AIPrincipalAPIKey = "api_key"
)

// AIPrincipal maps a stable principal id to its provider-observed name and an
// optional alias set in the web app. Prefer Alias, then Name, then Id when labelling.
// AccountUserID is the account user (a seat) the web app linked the principal to.
type AIPrincipal struct {
	Kind          string `json:"kind"`
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	SourceSystem  string `json:"source_system"`
	Alias         string `json:"alias,omitempty"`
	AccountUserID string `json:"account_user_id,omitempty"`
}

// DisplayName is the label a person would recognise: the alias they chose, else the
// name the provider reported, else the bare id.
func (p AIPrincipal) DisplayName() string {
	switch {
	case p.Alias != "":
		return p.Alias
	case p.Name != "":
		return p.Name
	default:
		return p.ID
	}
}

// AIPrincipalsPage is one page of the principal catalog.
type AIPrincipalsPage struct {
	Principals    []AIPrincipal `json:"principals"`
	ProjectID     string        `json:"project_id,omitempty"`
	NextPageToken string        `json:"next_page_token,omitempty"`
	Total         *int64        `json:"total,omitempty"`
}

// AIGitActivity is one git or GitHub action an agent's tool call performed.
type AIGitActivity struct {
	ActivityID         string    `json:"activity_id"`
	EventTime          time.Time `json:"event_time"`
	SourceSystem       string    `json:"source_system"`
	SessionID          string    `json:"session_id,omitempty"`
	ToolCallID         string    `json:"tool_call_id,omitempty"`
	Action             string    `json:"action"`
	Outcome            string    `json:"outcome,omitempty"`
	OwnerKey           string    `json:"owner_key,omitempty"`
	RepositoryKey      string    `json:"repository_key,omitempty"`
	RemoteName         string    `json:"remote_name,omitempty"`
	RemoteURL          string    `json:"remote_url,omitempty"`
	SourceRef          string    `json:"source_ref,omitempty"`
	DestRef            string    `json:"dest_ref,omitempty"`
	StdoutCommitSha    string    `json:"stdout_commit_sha,omitempty"`
	StructuredCommitID string    `json:"structured_commit_id,omitempty"`
	PushOldSha         string    `json:"push_old_sha,omitempty"`
	PushNewSha         string    `json:"push_new_sha,omitempty"`
	MessageHeadline    string    `json:"message_headline,omitempty"`
	FilesChanged       int64     `json:"files_changed,omitempty"`
	Insertions         int64     `json:"insertions,omitempty"`
	Deletions          int64     `json:"deletions,omitempty"`
	Flags              []string  `json:"flags,omitempty"`
	AICoAuthored       bool      `json:"ai_co_authored,omitempty"`
	PullRequestNumber  int64     `json:"pull_request_number,omitempty"`
	PullRequestURL     string    `json:"pull_request_url,omitempty"`
	PRState            string    `json:"pr_state,omitempty"`
	PRBaseRef          string    `json:"pr_base_ref,omitempty"`
	PRHeadRef          string    `json:"pr_head_ref,omitempty"`
	MergeCommitSha     string    `json:"merge_commit_sha,omitempty"`
}

// CommitRef is the most specific identifier an activity carries: a PR URL, else the
// structured commit id, else the sha scraped from stdout.
func (a AIGitActivity) CommitRef() string {
	switch {
	case a.PullRequestURL != "":
		return a.PullRequestURL
	case a.StructuredCommitID != "":
		return a.StructuredCommitID
	default:
		return a.StdoutCommitSha
	}
}

// AIGitActivitiesResponse is a session's git actions in event-time order.
type AIGitActivitiesResponse struct {
	SessionID    string          `json:"session_id"`
	SourceSystem string          `json:"source_system"`
	Activities   []AIGitActivity `json:"activities"`
}
