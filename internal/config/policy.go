package config

import (
	"strings"
	"time"
)

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
	Mode               string `json:"mode"`
	IncludePrompts     bool   `json:"include_prompts"`
	IncludeToolContent bool   `json:"include_tool_content"`
	// DefaultProjectID is where global mode files a session no repository binding and
	// no known remote places: work outside a repository, in an unknown one, and what
	// names no session at all (Codex's metrics).
	DefaultProjectID string `json:"default_project_id,omitempty"`
	// Remotes maps a repository's remote (NormalizeRemote) to the project its sessions
	// go to in global mode, for repositories the organization knows but that carry no
	// binding.
	Remotes   map[string]string `json:"remotes,omitempty"`
	FetchedAt time.Time         `json:"fetched_at"`
}

// ProjectFor is the project global mode files a repository with this remote under: the
// organization's mapping, else its default project. "" outside global mode.
func (p Policy) ProjectFor(remote string) string {
	if !p.Global() {
		return ""
	}
	if id := p.Remotes[NormalizeRemote(remote)]; id != "" {
		return id
	}
	return p.DefaultProjectID
}

// NormalizeRemote reduces the ways one repository's remote is spelled — scp-style
// `git@host:org/repo.git`, `ssh://git@host/org/repo`, `https://user@host/org/repo.git/`
// — to `host/org/repo`, host lower-cased. "" for an empty remote.
func NormalizeRemote(remote string) string {
	r := strings.TrimSpace(remote)
	if r == "" {
		return ""
	}
	if i := strings.Index(r, "://"); i >= 0 {
		r = r[i+3:]
	} else if at, colon := strings.Index(r, "@"), strings.Index(r, ":"); colon > 0 && (at < 0 || at < colon) && !strings.Contains(r[:colon], "/") {
		r = r[:colon] + "/" + r[colon+1:] // scp-style host:path
	}
	if at := strings.Index(r, "@"); at >= 0 && at < strings.Index(r+"/", "/") {
		r = r[at+1:]
	}
	r = strings.TrimRight(r, "/")
	r = strings.TrimSuffix(r, ".git")
	host, path, _ := strings.Cut(r, "/")
	if h, _, ok := strings.Cut(host, ":"); ok && !strings.ContainsAny(host[len(h)+1:], "abcdefghijklmnopqrstuvwxyz") {
		host = h // a port
	}
	return strings.ToLower(host) + "/" + path
}

// DefaultPolicy is what applies before `terma setup` has fetched one: repositories opt
// in, and prompts and tool content are collected unless a project turns them off.
func DefaultPolicy() Policy {
	return Policy{Mode: ModeRepo, IncludePrompts: true, IncludeToolContent: true}
}

// Global reports whether the organization collects every session on the machine.
func (p Policy) Global() bool { return p.Mode == ModeGlobal }
