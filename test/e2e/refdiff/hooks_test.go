package refdiff

import (
	"fmt"
	"strings"
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
		// The values an event was handed differ from run to run: never a change.
		example := map[string]any{"hook_event_name": "PreToolUse", "prompt_id": fmt.Sprint(keys...)}
		values := map[string]any{"CLAUDE_PROJECT_DIR": "<repo>", "CLAUDE_EFFORT": fmt.Sprint(env...)}
		return map[string]any{"fired": true, "count": float64(1), "keys": keys, "kinds": kinds, "env": env,
			"env_paths": map[string]any{"CLAUDE_PROJECT_DIR": "<repo>"}, "matcher": true, "control": control,
			"example": example, "env_values": values}
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
		// Whether the probe trusted its hooks is a note of the capture's.
		"trust": "trusted",
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
	for _, c := range d.Changes {
		if strings.Contains(c.ID, ".example") || strings.Contains(c.ID, ".env_values") {
			t.Errorf("an example's value read as a change: %s", c.ID)
		}
	}
	for _, id := range []string{"hooks.events.PreToolUse.kinds.tool_input.command", "hooks.events.PreToolUse.kinds.tool_name", "hooks.not_fired.PreCompact",
		"hooks.events.Notification", "hooks.trust"} {
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

// Builds probed different ways fired different tools in different terminals: what their events
// were handed is not compared, and what each supports is.
func TestHookMethodsNotCompared(t *testing.T) {
	old := &Snapshot{Harness: "claude", Version: "1.0.0", Hooks: map[string]any{
		"supported": []any{"PreToolUse"}, "source": "binary",
		"events": map[string]any{"PreToolUse": map[string]any{"fired": true, "keys": []any{"tool_name"}, "env": []any{"CLAUDECODE"}}},
	}}
	probed := &Snapshot{Harness: "claude", Version: "1.0.1", Hooks: map[string]any{
		"supported": []any{"PreToolUse", "TaskCreated"}, "source": "binary", "method": float64(2),
		"events": map[string]any{"PreToolUse": map[string]any{"fired": true, "keys": []any{"tool_name", "tool_input.taskId"},
			"env": []any{"CLAUDECODE", "COLUMNS"}, "example": map[string]any{"tool_name": "TaskCreate"}}},
	}}
	var ids []string
	for _, c := range New(old, probed, nil, time.Now()).Changes {
		ids = append(ids, c.ID)
	}
	if len(ids) != 1 || ids[0] != "hooks.supported.TaskCreated" {
		t.Errorf("changes = %q, want only the newly supported event", ids)
	}
}
