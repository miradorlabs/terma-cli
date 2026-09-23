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
	// has none of its own — Cursor refuses a hooks file without its schema version. A
	// value the developer already has is theirs. On uninstall a file left holding only
	// these is removed: terma put them there.
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
	events := map[string][]json.RawMessage{}
	if raw, ok := top["hooks"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &events); err != nil {
			return p, fmt.Errorf("parse %s hooks: %w", file.Path, err)
		}
	}

	changed := false
	for _, h := range own {
		entries := events[h.Event]
		idx := -1
		for i, e := range entries {
			if callsTerma(e) {
				idx = i
				break
			}
		}
		switch {
		case install && idx == -1:
			events[h.Event] = append(entries, h.Entry)
			changed = true
		case install && !sameJSON(entries[idx], h.Entry):
			entries[idx] = h.Entry
			changed = true
		case !install && idx != -1:
			events[h.Event] = append(entries[:idx], entries[idx+1:]...)
			if len(events[h.Event]) == 0 {
				delete(events, h.Event)
			}
			changed = true
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
	if !install && before != nil && onlyDefaults(top, file.Defaults) {
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

// onlyDefaults reports whether every member of top is one terma would have added.
func onlyDefaults(top, defaults map[string]json.RawMessage) bool {
	for key := range top {
		if _, ok := defaults[key]; !ok {
			return false
		}
	}
	return true
}
