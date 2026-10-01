package hookmgr

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// EventHook is one entry terma owns in an agent's hooks file: the event it is filed
// under, and the entry as that agent spells it.
type EventHook struct {
	Event string
	Entry json.RawMessage
}

// HooksFile is an agent's hooks file whose "hooks" member maps an event to a list of entries.
type HooksFile struct {
	// Path is relative to the repository root, slash-separated.
	Path string
	// Defaults are top-level members set when the file has none, kept on uninstall since
	// an identical value may predate terma.
	Defaults map[string]json.RawMessage
}

// MergeEventHooks plans terma's entries into, or out of, an agent's hooks file, leaving
// the developer's entries and every other member as they were read.
func MergeEventHooks(root string, file HooksFile, own []EventHook, install bool) (Plan, error) {
	p := Plan{}
	before, err := ReadFile(filepath.Join(root, filepath.FromSlash(file.Path)))
	if err != nil {
		return p, err
	}
	top := map[string]json.RawMessage{}
	if before != nil {
		if err := json.Unmarshal(before, &top); err != nil {
			return p, fmt.Errorf("parse %s: %w", file.Path, err)
		}
	}
	if top == nil {
		return p, fmt.Errorf("parse %s: expected a JSON object", file.Path)
	}
	events := map[string][]json.RawMessage{}
	if raw, ok := top["hooks"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &events); err != nil {
			return p, fmt.Errorf("parse %s hooks: %w", file.Path, err)
		}
	}

	if events == nil {
		return p, fmt.Errorf("parse %s hooks: expected a JSON object", file.Path)
	}
	changed := false
	for _, h := range own {
		entries := events[h.Event]
		// Keep user commands even when they share a matcher group with ours.
		var kept []json.RawMessage
		present := false
		for _, entry := range entries {
			if install && !present && SameJSON(entry, h.Entry) {
				kept = append(kept, entry)
				present = true
				continue
			}
			remaining, removed, err := WithoutTerma(entry)
			if err != nil {
				return p, err
			}
			changed = changed || removed
			if remaining != nil {
				kept = append(kept, remaining)
			}
		}
		if install && !present {
			kept = append(kept, h.Entry)
			changed = true
		}
		if len(kept) == 0 {
			delete(events, h.Event)
		} else {
			events[h.Event] = kept
		}
	}
	if !changed {
		return p, nil
	}

	if len(events) == 0 {
		delete(top, "hooks")
	} else {
		raw, err := MarshalJSON(events, "  ", "  ")
		if err != nil {
			return p, err
		}
		top["hooks"] = raw
	}
	if install {
		for key, value := range file.Defaults {
			if _, ok := top[key]; !ok {
				top[key] = value
			}
		}
	}
	if !install && before != nil && len(top) == 0 {
		p.Changes = append(p.Changes, Change{Path: file.Path, Before: before})
		return p, nil
	}
	out, err := MarshalOrdered(top)
	if err != nil {
		return p, err
	}
	p.Changes = append(p.Changes, Change{Path: file.Path, Before: before, After: append(out, '\n')})
	return p, nil
}

// Group is an event's entry wrapping one handler, {"matcher": …, "hooks": [handler]}, the
// shape of hooks files whose events take matcher groups; an empty matcher is left out.
func Group(event, matcher string, handler any) (EventHook, error) {
	h, err := MarshalJSON(handler, "", "")
	if err != nil {
		return EventHook{}, err
	}
	g, err := MarshalJSON(struct {
		Matcher string            `json:"matcher,omitempty"`
		Hooks   []json.RawMessage `json:"hooks"`
	}{matcher, []json.RawMessage{h}}, "", "")
	return EventHook{Event: event, Entry: g}, err
}
