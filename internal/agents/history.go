package agents

import (
	"context"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// HistoryFile describes one discovered local session transcript on disk.
type HistoryFile struct {
	// SourceSystem is the platform's source_system identifier for this agent.
	SourceSystem string
	// SessionID is the agent's native session identifier.
	SessionID string
	// ParentSessionID is the optional parent session identifier for spawned subagent
	// transcripts stored as separate files (so cross-org reach in a child taints its parent),
	// or a fork/continuation ancestor when OneWayParent is true.
	ParentSessionID string
	// OneWayParent reports that ParentSessionID is a one-directional lineage ancestor (such
	// as a forked or continued transcript) whose history flowed into this session, rather
	// than a bidirectional subagent whose activity flows back into the parent.
	OneWayParent bool
	// Path is the main transcript file path on disk.
	Path string
	// ExtraPaths lists companion transcript files belonging to the same session, such as
	// sidechain subagent transcripts.
	ExtraPaths []string
	// WatchDirs lists optional companion directories whose regular .jsonl files are checked
	// when re-verifying that the session has not resumed before upload.
	WatchDirs []string
	// SeenWatchFiles lists regular .jsonl files already present inside WatchDirs at discovery
	// time that were intentionally not included in ExtraPaths (for example, stale copied
	// subagent transcripts inside a fork's directory), so re-verification only flags new files.
	SeenWatchFiles []string
	// ModTime is the latest modification time across Path and ExtraPaths.
	ModTime time.Time
	// Active reports whether the transcript was modified within the active-session window
	// and must be skipped as in progress.
	Active bool
	// OutsideSince reports whether the transcript's latest modification time predates
	// HistoryDiscoverOptions.Since and must be skipped without reading.
	OutsideSince bool
	// CompanionReadError is the path of a companion or sibling transcript (or its
	// directory) that discovery could not read. Non-empty means uploading must fail closed.
	CompanionReadError string
}

// HistoryDiscoverOptions configures discovery of past session transcripts.
type HistoryDiscoverOptions struct {
	// Since excludes transcripts whose latest modification time is before Since.
	// Zero disables the lower bound.
	Since time.Time
	// Now is the reference wall clock for ActiveWindow; defaults to time.Now when zero.
	Now time.Time
	// ActiveWindow is how recently a file must be modified to count as still in progress;
	// defaults to time.Hour when <= 0.
	ActiveWindow time.Duration
}

// HistoryReadOptions configures conversion of one transcript into OTLP log segments.
type HistoryReadOptions struct {
	// IncludeSupplemental asks the source to emit optional supplemental records (such as
	// assistant replies for agents whose native log stream omits them). The uploader still
	// enforces each destination team's content policy on every record.
	IncludeSupplemental bool
}

// HistoryToolReach records paths or shell-reached locations inspected by one tool call
// inside a transcript segment, used to detect cross-organization reach even when no live
// runs ledger exists.
type HistoryToolReach struct {
	At    time.Time
	Cwd   string
	Paths []string
}

// HistorySegment is a contiguous portion of a transcript recorded under one working
// directory, converted into OTLP log batches ready for checkout stamping and content
// filtering.
type HistorySegment struct {
	// SourceSystem is the platform's source_system identifier.
	SourceSystem string
	// SessionID is the agent's native session identifier.
	SessionID string
	// Cwd is the working directory recorded on the segment's entries; empty means the
	// entries had no recorded working directory and must be skipped and reported.
	Cwd string
	// FallbackRepoURL is an optional hosted repository URL recorded inside the transcript
	// itself for Cwd, used only when Cwd no longer exists on disk.
	FallbackRepoURL string
	// FallbackBranch is an optional git branch recorded inside the transcript itself for Cwd.
	FallbackBranch string
	// NativeLogs holds the agent's native OTLP log records, filtered by the relay content
	// filter before upload.
	NativeLogs *logspb.LogsData
	// NativeTraces holds the agent's native OTLP trace spans (for agents whose platform
	// adapter maps /v1/traces rather than /v1/logs), filtered by the relay content filter
	// before upload.
	NativeTraces *tracepb.TracesData
	// SupplementalLogs holds optional terma-scoped log records (such as replies, session
	// titles, subagent parent starts, or file-touch metadata not present in the native
	// stream), filtered by the destination team's content policy before upload.
	SupplementalLogs *logspb.LogsData
	// ToolReaches holds paths reached by tool calls in this segment (with their timestamps)
	// so historical upload enforces the same cross-organization Mixed cutoff as live hooks.
	ToolReaches []HistoryToolReach
	// Subagent reports whether this segment came from a companion subagent transcript
	// sharing the parent session's run (so a subagent crossing into another organization
	// taints the parent session's run at that timestamp).
	Subagent bool
	// RecordCount is the total number of log records and spans across NativeLogs,
	// NativeTraces, and SupplementalLogs.
	RecordCount int
}

// HistorySource is an agent whose past local transcripts can be discovered and converted
// into OTLP log records matching its live telemetry.
type HistorySource interface {
	Agent
	// SourceSystem is the platform's source_system identifier for this agent.
	SourceSystem() string
	// DiscoverHistory lists local transcripts modified at or after opts.Since, marking
	// transcripts modified within opts.ActiveWindow of opts.Now as Active.
	DiscoverHistory(ctx context.Context, opts HistoryDiscoverOptions) ([]HistoryFile, error)
	// ReadHistory reads file and streams segments grouped by recorded working directory to
	// emit. Returning a non-nil error from emit stops reading and returns that error.
	ReadHistory(ctx context.Context, file HistoryFile, opts HistoryReadOptions, emit func(HistorySegment) error) error
}
