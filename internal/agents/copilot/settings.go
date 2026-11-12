package copilot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/tailscale/hujson"
)

// VS Code's settings file is JSON with comments and trailing commas, which the developer
// edits by hand: it is read as a syntax tree and changed member by member, so whatever
// terma does not change is written back byte for byte.

// settingsDoc is a VS Code settings file as its syntax tree: packed again, whatever was
// not changed is byte for byte what was read.
type settingsDoc struct {
	root hujson.Value
	obj  *hujson.Object
	// added counts the members set has put first, so later ones follow them in order.
	added int
}

func parseSettings(data []byte) (*settingsDoc, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}\n")
	}
	root, err := hujson.Parse(data)
	if err != nil {
		return nil, err
	}
	obj, ok := root.Value.(*hujson.Object)
	if !ok {
		return nil, errors.New("expected a JSON object")
	}
	return &settingsDoc{root: root, obj: obj}, nil
}

func (d *settingsDoc) pack() []byte { return d.root.Pack() }

// find is the member VS Code reads for key: the last of that name.
func (d *settingsDoc) find(key string) int {
	for i := len(d.obj.Members) - 1; i >= 0; i-- {
		if lit, ok := d.obj.Members[i].Name.Value.(hujson.Literal); ok && lit.String() == key {
			return i
		}
	}
	return -1
}

// raw is key's value as written, without the comments and space around it.
func (d *settingsDoc) raw(key string) (string, bool) {
	i := d.find(key)
	if i < 0 {
		return "", false
	}
	return string(hujson.Value{Value: d.obj.Members[i].Value.Value}.Pack()), true
}

// text is key's value when it is a string.
func (d *settingsDoc) text(key string) (string, bool) {
	i := d.find(key)
	if i < 0 {
		return "", false
	}
	lit, ok := d.obj.Members[i].Value.Value.(hujson.Literal)
	if !ok || lit.Kind() != '"' {
		return "", false
	}
	return lit.String(), true
}

// set gives key the raw JSON value, in place where the file has the member, else as a new
// member at the top: there a comma always follows it, whatever comments end the file.
func (d *settingsDoc) set(key, raw string) error {
	value, err := hujson.Parse([]byte(raw))
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if i := d.find(key); i >= 0 {
		d.obj.Members[i].Value.Value = value.Value
		return nil
	}
	indent := "    "
	if len(d.obj.Members) > 0 {
		before := string(d.obj.Members[0].Name.BeforeExtra)
		if i := strings.LastIndexByte(before, '\n'); i >= 0 && strings.Trim(before[i+1:], " \t") == "" && before[i+1:] != "" {
			indent = before[i+1:]
		}
	} else if !bytes.Contains(d.obj.AfterExtra, []byte("\n")) {
		d.obj.AfterExtra = hujson.Extra("\n")
	}
	member := hujson.ObjectMember{
		Name:  hujson.Value{BeforeExtra: hujson.Extra("\n" + indent), Value: hujson.String(key)},
		Value: hujson.Value{BeforeExtra: hujson.Extra(" "), Value: value.Value},
	}
	d.obj.Members = slices.Insert(d.obj.Members, d.added, member)
	d.added++
	return nil
}

// remove takes key's member out; a comment written ahead of it stays, ahead of what follows.
func (d *settingsDoc) remove(key string) {
	i := d.find(key)
	if i < 0 {
		return
	}
	before := d.obj.Members[i].Name.BeforeExtra
	if i+1 < len(d.obj.Members) {
		next := &d.obj.Members[i+1].Name
		next.BeforeExtra = carry(before, next.BeforeExtra)
	} else {
		d.obj.AfterExtra = carry(before, d.obj.AfterExtra)
	}
	d.obj.Members = slices.Delete(d.obj.Members, i, i+1)
	// An object left with nothing in it closes on the line it opens on.
	if len(d.obj.Members) == 0 && len(bytes.TrimSpace(d.obj.AfterExtra)) == 0 {
		d.obj.AfterExtra = nil
	}
}

// ignoreNames adds the names the sync-ignore list lacks, in place where the file has a
// list, so its layout and comments stay, else as a new list. It returns the names it
// added; ok is false when the file's value is not a list, which is left as it is.
func (d *settingsDoc) ignoreNames(names []string) (added []string, ok bool) {
	i := d.find(syncIgnored)
	if i < 0 {
		return names, d.set(syncIgnored, rawList(names)) == nil
	}
	list, ok := d.obj.Members[i].Value.Value.(*hujson.Array)
	if !ok {
		return nil, false
	}
	// A new element sits as the last one does: on a line of its own, or after the comma.
	var before hujson.Extra
	if n := len(list.Elements); n > 0 {
		last := string(list.Elements[n-1].BeforeExtra)
		if at := strings.LastIndexByte(last, '\n'); at >= 0 && strings.Trim(last[at:], " \t\n") == "" {
			before = hujson.Extra(last[at:])
		}
	}
	for _, name := range names {
		if !slices.ContainsFunc(list.Elements, func(e hujson.Value) bool { return named(e, name) }) {
			list.Elements = append(list.Elements, hujson.Value{BeforeExtra: slices.Clone(before), Value: hujson.String(name)})
			added = append(added, name)
		}
	}
	return added, true
}

// unignoreNames takes names out of the sync-ignore list, whatever else the developer has
// put in it since, and the list out with them once it names nothing.
func (d *settingsDoc) unignoreNames(names []string) {
	i := d.find(syncIgnored)
	if i < 0 {
		return
	}
	list, ok := d.obj.Members[i].Value.Value.(*hujson.Array)
	if !ok {
		return
	}
	for at := len(list.Elements) - 1; at >= 0; at-- {
		e := list.Elements[at]
		if !slices.ContainsFunc(names, func(name string) bool { return named(e, name) }) {
			continue
		}
		if at+1 < len(list.Elements) {
			list.Elements[at+1].BeforeExtra = carry(e.BeforeExtra, list.Elements[at+1].BeforeExtra)
		} else {
			list.AfterExtra = carry(e.BeforeExtra, list.AfterExtra)
		}
		list.Elements = slices.Delete(list.Elements, at, at+1)
	}
	if len(list.Elements) == 0 && len(bytes.TrimSpace(list.AfterExtra)) == 0 {
		d.remove(syncIgnored)
	}
}

// carry is after, the space and comments ahead of what follows a value being taken out,
// with the comments ahead of that value put first: a comment the developer wrote there,
// about the line above as often as the one below, stays where they wrote it.
func carry(before, after hujson.Extra) hujson.Extra {
	kept := bytes.TrimRight(before, " \t\r\n")
	if len(kept) == 0 {
		return after
	}
	out := slices.Clone(kept)
	// A line comment runs to the end of its line: what follows must start a new one.
	if !bytes.Contains(after, []byte("\n")) {
		out = append(out, '\n')
	}
	return append(out, after...)
}

// named reports whether the list element e is the string name.
func named(e hujson.Value, name string) bool {
	lit, ok := e.Value.(hujson.Literal)
	return ok && lit.Kind() == '"' && lit.String() == name
}

// rawList is list as raw JSON.
func rawList(list []string) string {
	out, _ := json.Marshal(list)
	return string(out)
}

// listOf is the names in the raw JSON list raw.
func listOf(raw string) []string {
	var list []string
	_ = json.Unmarshal([]byte(raw), &list)
	return list
}
