package config

import "time"

// Collection modes: what the organization the developer signed in to collects from
// this machine.
const (
	// ModeRepo collects what repositories opted in to: sessions a hook of a bound
	// repository claimed. Everything else stays on the machine. The default.
	ModeRepo = "repo"
	// ModeGlobal collects every session on the machine — an organization's choice for
	// company laptops where it wants all AI spend. Recorded only, for now.
	ModeGlobal = "global"
)

// Policy is the organization's collection policy, fetched by `terma setup` from the
// account it signs in to and kept on the profile, so hooks and the relay never ask the
// network for it. Its content defaults apply where the developer has made no choice of
// their own for a project (the project's routing record).
type Policy struct {
	Mode               string    `json:"mode"`
	IncludePrompts     bool      `json:"include_prompts"`
	IncludeToolContent bool      `json:"include_tool_content"`
	FetchedAt          time.Time `json:"fetched_at"`
}

// DefaultPolicy is what applies before `terma setup` has fetched one: repositories opt
// in, and prompts and tool content are collected unless a project turns them off.
func DefaultPolicy() Policy {
	return Policy{Mode: ModeRepo, IncludePrompts: true, IncludeToolContent: true}
}

// Global reports whether the organization collects every session on the machine.
func (p Policy) Global() bool { return p.Mode == ModeGlobal }
