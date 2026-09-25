package hookmgr

import (
	"encoding/json"
	"fmt"
	"path/filepath"
)

// eventHook is one entry terma owns in an agent's hooks file: the event it is filed
// under, and the entry as that agent spells it.
type eventHook struct {
	Event string
	Entry json.RawMessage
}

// hooksFile is an agent's hooks file as mergeEventHooks needs to know it: a JSON object
// whose "hooks" member maps an event to a list of entries. Claude Code, Codex and
// Cursor all keep theirs that way and differ only in what an entry looks like.
type hooksFile struct {
	// Path is relative to the repository root, slash-separated.
	Path string
	// Defaults are top-level members terma sets when it writes the file and the file
	// has none of its own — Cursor refuses a hooks file without its schema version.
	// Defaults are preserved on uninstall: an identical value may predate Terma.
	Defaults map[string]json.RawMessage
}

// mergeEventHooks plans terma's entries into, or out of, an agent's hooks file without
// disturbing anything else in it: the developer's own entries in the same event, and
// every other top-level member, survive as they were read.
//
// An entry is terma's when it calls the binary (callsTerma), which is what makes install
// idempotent and lets an entry from an older terma be brought up to date where it
// stands rather than duplicated. Nothing changing is an empty plan.
func mergeEventHooks(root string, file hooksFile, own []eventHook, install bool) (Plan, error) {
	p := Plan{}
	before, err := readFile(filepath.Join(root, filepath.FromSlash(file.Path)))
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
			if install && !present && sameJSON(entry, h.Entry) {
				kept = append(kept, entry)
				present = true
				continue
			}
			remaining, removed, err := withoutTerma(entry)
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
		// The hooks section is terma's to format; every other top-level value is
		// written back exactly as it was read.
		raw, err := marshalJSON(events, "  ", "  ")
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
	// Uninstall from a file that held nothing but what terma wrote: remove it rather
	// than leave an empty shell behind.
	if !install && before != nil && len(top) == 0 {
		p.Changes = append(p.Changes, Change{Path: file.Path, Before: before})
		return p, nil
	}
	out, err := marshalOrdered(top)
	if err != nil {
		return p, err
	}
	p.Changes = append(p.Changes, Change{Path: file.Path, Before: before, After: append(out, '\n')})
	return p, nil
}
