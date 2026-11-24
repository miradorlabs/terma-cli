package hookrun

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// Extension is the hooks a terma-written agent extension calls, in one JSON shape, under
// the session id its telemetry carries too.
type Extension struct {
	Tool string
	// ClaimParent names a subagent's parent in its relay claim, for the relay to stamp on
	// the subagent's telemetry: for an agent whose own telemetry does not name the parent.
	ClaimParent bool
}

// Events is the extension's <prefix>-session-start, -prompt, -session-end, -file-edit and -tool-reach handlers.
func (x Extension) Events(prefix string) map[string]func(context.Context, Env) error {
	return map[string]func(context.Context, Env) error{
		prefix + "-session-start": x.sessionStart,
		prefix + "-prompt":        TurnStart,
		prefix + "-session-end":   x.sessionEnd,
		prefix + "-file-edit":     x.fileEdit,
		prefix + "-tool-reach":    x.toolReach,
	}
}

type extensionHookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`
	File      string `json:"file"`
	// Files are the files one tool call changed, where it changed several.
	Files []string `json:"files"`
	Tool  string   `json:"tool"`
	// ToolCallID is the call's id, as the agent's own telemetry names it.
	ToolCallID string `json:"tool_call_id"`
	// ParentSessionID is set on a session an agent opened for a subagent.
	ParentSessionID string `json:"parent_session_id"`
	// Paths, Globs, Lists, Command and Dir describe what a read/search/shell tool call reached.
	Paths   []string `json:"paths"`
	Globs   []string `json:"globs"`
	Lists   []string `json:"lists"`
	Command string   `json:"command"`
	Dir     string   `json:"dir"`
}

func readExtensionInput(r io.Reader) (*extensionHookInput, error) {
	in, err := ReadInput[extensionHookInput](r)
	if err != nil {
		return nil, err
	}
	if in.SessionID == "" {
		return nil, errors.New("hook input has no session_id")
	}
	return in, nil
}

func (x Extension) sessionStart(ctx context.Context, env Env) error {
	in, err := readExtensionInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	subagent := session.ValidID(in.ParentSessionID) && in.ParentSessionID != in.SessionID
	if subagent {
		env.AliasRun(in.ParentSessionID, in.SessionID)
	} else {
		env.StartRun(in.SessionID)
	}
	r, ok := env.Open(ctx, in.SessionID, in.Cwd)
	if !ok {
		return nil
	}
	sess := env.NewSession(r, in.SessionID, x.Tool, in.Model)
	if !subagent {
		env.Announce(r, sess, nil)
		return nil
	}
	// A subagent's session is announced, never made active: it would claim the
	// developer's next hand-written commit. Its edits still build its own manifest.
	env.PruneManifests(r, sess.UpdatedAt)
	if x.ClaimParent {
		env.parent = in.ParentSessionID
	}
	env.EmitStart(r, sess, map[string]any{semconv.TermaSessionParentIDKey: in.ParentSessionID})
	env.propagateMixed(in.ParentSessionID, in.SessionID, x.Tool)
	return nil
}

func (x Extension) sessionEnd(ctx context.Context, env Env) error {
	in, err := readExtensionInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	r, ok := env.Open(ctx, in.SessionID, in.Cwd)
	if !ok {
		return nil
	}
	env.EndSession(r, in.SessionID, x.Tool)
	return nil
}

func (x Extension) fileEdit(ctx context.Context, env Env) error {
	in, err := readExtensionInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	paths := slices.DeleteFunc(append([]string{in.File}, in.Files...), func(p string) bool { return p == "" })
	if !session.ValidID(in.SessionID) || len(paths) == 0 {
		return nil
	}
	subagent := session.ValidID(in.ParentSessionID) && in.ParentSessionID != in.SessionID
	if subagent {
		env.AliasRun(in.ParentSessionID, in.SessionID)
		if x.ClaimParent {
			env.parent = in.ParentSessionID
		}
	}
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	r, err := env.Repo(ctx)
	if err != nil {
		return nil
	}
	attrs := map[string]any{}
	BoundedAttr(attrs, semconv.GenAIToolCallIDKey, in.ToolCallID)
	env.Touch(r, session.Session{ID: in.SessionID, Tool: x.Tool, Model: in.Model}, in.Tool, paths, attrs)
	if subagent {
		env.propagateMixed(in.ParentSessionID, in.SessionID, x.Tool)
	}
	return nil
}

func (x Extension) toolReach(ctx context.Context, env Env) error {
	in, err := readExtensionInput(env.Stdin)
	if err != nil {
		env.Logf("%v", err)
		return nil
	}
	if !session.ValidID(in.SessionID) {
		return nil
	}
	if env.Routing != nil && !env.Routing.Usable() {
		return ErrFailClosed
	}
	_ = env.Routing.TakeWriteErr()
	env.Cwd = cmp.Or(in.Cwd, env.Cwd)
	subagent := session.ValidID(in.ParentSessionID) && in.ParentSessionID != in.SessionID
	if subagent {
		env.AliasRun(in.ParentSessionID, in.SessionID)
		if x.ClaimParent {
			env.parent = in.ParentSessionID
		}
	}
	if paths := extensionReachPaths(in, env.Cwd); len(paths) > 0 {
		r, _ := env.Repo(ctx)
		env.Classify(ctx, r, session.Session{ID: in.SessionID, Tool: x.Tool, Model: in.Model}, paths, nil)
		if subagent {
			env.propagateMixed(in.ParentSessionID, in.SessionID, x.Tool)
		}
	}
	if err := env.Routing.TakeWriteErr(); err != nil {
		return fmt.Errorf("%w: %w", ErrFailClosed, err)
	}
	return nil
}

func extensionReachPaths(in *extensionHookInput, cwd string) []string {
	paths := slices.DeleteFunc(append([]string{in.File}, in.Paths...), func(p string) bool { return p == "" })
	paths = append(paths, slices.DeleteFunc(slices.Clone(in.Files), func(p string) bool { return p == "" })...)
	globs := slices.DeleteFunc(slices.Clone(in.Globs), func(p string) bool { return p == "" })
	if len(globs) > 0 {
		if len(paths) > 0 {
			for _, root := range slices.Clone(paths) {
				paths = append(paths, Search(root, globs...)[1:]...)
			}
		} else if cwd != "" {
			paths = append(paths, Search(cwd, globs...)...)
		}
	}
	if lists := slices.DeleteFunc(slices.Clone(in.Lists), func(p string) bool { return p == "" }); len(lists) > 0 {
		paths = append(paths, Alone(lists...)...)
	}
	dir := cwd
	if in.Dir != "" {
		dir = in.Dir
		if !filepath.IsAbs(dir) && cwd != "" {
			dir = filepath.Join(cwd, dir)
		}
		paths = append(paths, Alone(dir)...)
	}
	if in.Command != "" {
		paths = append(paths, Alone(ShellPaths(in.Command, dir)...)...)
	}
	return paths
}

// TurnStart records nothing: its caller claims the session and starts the relay before the
// turn exports. It must print nothing, since stdout may reach the model.
func TurnStart(_ context.Context, env Env) error {
	_, err := readExtensionInput(env.Stdin)
	return err
}
