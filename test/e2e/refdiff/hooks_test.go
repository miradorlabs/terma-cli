package refdiff

import (
	"testing"
	"time"
)

// What changed in the hooks section, said in a line each: events a build knows, the fields and
// variables a hook is handed, and what its answer does.
func TestHookChanges(t *testing.T) {
	snap := func(v string, hooks map[string]any) *Snapshot {
		return &Snapshot{Harness: "claude", Version: v, Hooks: hooks}
	}
	event := func(keys []any, env []any, control map[string]any) map[string]any {
		kinds := map[string]any{}
		for _, k := range keys {
			kinds[k.(string)] = "text"
		}
		return map[string]any{"fired": true, "count": float64(1), "keys": keys, "kinds": kinds, "env": env,
			"env_paths": map[string]any{"CLAUDE_PROJECT_DIR": "<repo>"}, "matcher": true, "control": control}
	}
	a := snap("1.0.0", map[string]any{
		"supported": []any{"Notification", "PreToolUse", "Stop"}, "source": "binary",
		"events": map[string]any{
			"PreToolUse": event([]any{"hook_event_name", "tool_name"}, []any{"CLAUDE_PROJECT_DIR", "CLAUDECODE"},
				map[string]any{"exit_2": "blocks the tool; message to the model"}),
			"Notification": event([]any{"message"}, []any{}, nil),
		},
		"not_fired": map[string]any{"Stop": "none of the probe's runs fired it"},
	})
	b := snap("1.0.1", map[string]any{
		"supported": []any{"PreCompact", "PreToolUse", "Stop"}, "source": "binary",
		"events": map[string]any{
			"PreToolUse": event([]any{"hook_event_name", "tool_input.command"}, []any{"CLAUDE_PROJECT_DIR", "CLAUDE_ENV_FILE"},
				map[string]any{"exit_2": "does not block the tool; message to the model"}),
		},
		"not_fired": map[string]any{"PreCompact": "none of the probe's runs fired it", "Stop": "none of the probe's runs fired it"},
	})
	d := New(a, b, nil, time.Now())
	byID := map[string]Change{}
	for _, c := range d.Changes {
		byID[c.ID] = c
	}
	for id, want := range map[string]struct{ title, impact string }{
		"hooks.supported.PreCompact":                      {"New hook event: PreCompact", "notable"},
		"hooks.supported.Notification":                    {"Hook event no longer supported: Notification", "degrading"},
		"hooks.events.PreToolUse.keys.tool_input.command": {"PreToolUse hook payload field tool_input.command added", "informational"},
		"hooks.events.PreToolUse.keys.tool_name":          {"PreToolUse hook payload field tool_name removed", "notable"},
		"hooks.events.PreToolUse.env.CLAUDE_ENV_FILE":     {"PreToolUse hook environment variable CLAUDE_ENV_FILE added", "informational"},
		"hooks.events.PreToolUse.env.CLAUDECODE":          {"PreToolUse hook environment variable CLAUDECODE removed", "notable"},
		"hooks.events.PreToolUse.control.exit_2": {"PreToolUse hook control (exit_2): blocks the tool; message to the model → does not block the tool; message to the model",
			"notable"},
	} {
		c, ok := byID[id]
		if !ok {
			t.Errorf("no change %s among %v", id, ids(d.Changes))
			continue
		}
		if c.Title != want.title || c.Impact != want.impact {
			t.Errorf("%s = %q (%s), want %q (%s)", id, c.Title, c.Impact, want.title, want.impact)
		}
	}
	// A field's kind and why an event did not fire say again what another change says.
	// Whether an event fired is the run's: what is supported says what the build does.
	for _, id := range []string{"hooks.events.PreToolUse.kinds.tool_input.command", "hooks.events.PreToolUse.kinds.tool_name", "hooks.not_fired.PreCompact",
		"hooks.events.Notification"} {
		if _, ok := byID[id]; ok {
			t.Errorf("%s read as a change of its own", id)
		}
	}
}

func ids(cs []Change) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}
