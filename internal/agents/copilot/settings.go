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

// remove takes key's member out; a comment written above it stays, above what follows.
func (d *settingsDoc) remove(key string) {
	i := d.find(key)
	if i < 0 {
		return
	}
	before := d.obj.Members[i].Name.BeforeExtra
	if len(bytes.TrimSpace(before)) > 0 {
		if i+1 < len(d.obj.Members) {
			next := &d.obj.Members[i+1].Name
			next.BeforeExtra = append(slices.Clone(before), bytes.TrimLeft(next.BeforeExtra, " \t")...)
		} else {
			d.obj.AfterExtra = append(slices.Clone(before), bytes.TrimLeft(d.obj.AfterExtra, " \t")...)
		}
	}
	d.obj.Members = slices.Delete(d.obj.Members, i, i+1)
	// An object left with nothing in it closes on the line it opens on.
	if len(d.obj.Members) == 0 && len(bytes.TrimSpace(d.obj.AfterExtra)) == 0 {
		d.obj.AfterExtra = nil
	}
}

// ignored is the file's sync-ignore list; false when it is not a list of names.
func (d *settingsDoc) ignored() ([]string, bool) {
	var list []string
	if raw, ok := d.raw(syncIgnored); ok {
		plain, err := hujson.Standardize([]byte(raw))
		if err != nil || json.Unmarshal(plain, &list) != nil {
			return nil, false
		}
	}
	return list, true
}

// withKeys is list with the keys it lacks added; withoutKeys is list with none of them.
func withKeys(list, keys []string) []string {
	list = slices.Clone(list)
	for _, key := range keys {
		if !slices.Contains(list, key) {
			list = append(list, key)
		}
	}
	return list
}

func withoutKeys(list, keys []string) []string {
	return slices.DeleteFunc(slices.Clone(list), func(k string) bool { return slices.Contains(keys, k) })
}

// rawList is list as raw JSON.
func rawList(list []string) string {
	out, _ := json.Marshal(list)
	return string(out)
}
