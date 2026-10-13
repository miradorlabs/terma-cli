package copilot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tailscale/hujson"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// VS Code keeps Copilot's exporters in its user settings, a JSON-with-comments file the
// developer edits by hand too: terma changes only the members below and leaves every other
// byte, comment and comma as it was. VS Code runs Copilot in two places with an exporter
// each: its own agent in the extension host (github.copilot.chat.otel.*), and the Copilot
// runtime in the agent host (chat.agentHost.otel.*), read when VS Code starts.
const (
	chatEnabled  = "github.copilot.chat.otel.enabled"
	chatEndpoint = "github.copilot.chat.otel.otlpEndpoint"
	chatContent  = "github.copilot.chat.otel.captureContent"
	hostEnabled  = "chat.agentHost.otel.enabled"
	hostEndpoint = "chat.agentHost.otel.otlpEndpoint"
	hostContent  = "chat.agentHost.otel.captureContent"
	// syncIgnored lists the settings Settings Sync leaves on this machine.
	syncIgnored = "settingsSync.ignoredSettings"
	// telemetryLevel "off" switches the extension's exporter off with VS Code's own telemetry.
	telemetryLevel = "telemetry.telemetryLevel"
)

// exporterKeys are the exporter settings terma writes, in the order it writes them.
var exporterKeys = []string{chatEnabled, chatEndpoint, chatContent, hostEnabled, hostEndpoint, hostContent}

// vscodeFlavors are the VS Code builds that ship Copilot, by their data directory's name.
var vscodeFlavors = []string{"Code", "Code - Insiders"}

// vscodeUserDirs are the user settings directories of the VS Code builds that have run
// here, under VSCODE_APPDATA where the developer moved VS Code's data with it.
func vscodeUserDirs() []string {
	base := os.Getenv("VSCODE_APPDATA")
	if base == "" {
		var err error
		if base, err = os.UserConfigDir(); err != nil {
			return nil
		}
	}
	var dirs []string
	for _, flavor := range vscodeFlavors {
		dir := filepath.Join(base, flavor, "User")
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// exporterSettings are the members that send both of VS Code's Copilot exporters to the
// relay, each as raw JSON. Neither exporter's headers reach the Copilot runtime VS Code
// starts, so the relay's token rides the endpoint path. All content is exported: the relay
// withholds what the team's policy does not collect.
func exporterSettings(cfg agents.RelayConfig) map[string]string {
	base := cfg.Endpoint + "/" + cfg.Token
	return map[string]string{
		chatEnabled:  "true",
		chatEndpoint: string(hujson.String(base)),
		chatContent:  "true",
		hostEnabled:  "true",
		// The agent host appends the signal path only to an endpoint with none.
		hostEndpoint: string(hujson.String(base + "/v1/traces")),
		hostContent:  "true",
	}
}

// connectVSCode writes the exporter settings into the settings file of the VS Code at dir
// and reports whether it changed the file.
func (a Agent) connectVSCode(dir string, cfg agents.RelayConfig) (path string, changed bool, err error) {
	path = filepath.Join(dir, "settings.json")
	if a.ConfigDir == "" {
		return path, false, errors.New("no config directory to record VS Code's earlier settings in")
	}
	writePath, _, err := config.ResolveWritePath(path)
	if err != nil {
		return path, false, err
	}
	data, err := os.ReadFile(writePath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return path, false, err
	}
	doc, err := parseSettings(data)
	if err != nil {
		return path, false, fmt.Errorf("%s does not parse as JSON with comments, so terma leaves it alone: %w", path, err)
	}
	installing := exporterSettings(cfg)
	// The endpoint names this machine's relay and token: synced, it would switch another
	// machine's export on, pointed at a relay it does not have.
	if ignored, ok := doc.withIgnored(exporterKeys); ok {
		installing[syncIgnored] = ignored
	}
	existing := map[string]string{}
	for key := range installing {
		if raw, ok := doc.raw(key); ok {
			existing[key] = raw
		}
	}
	previous, err := harness.LoadJournal(a.ConfigDir, name, path)
	if err != nil {
		return path, false, err
	}
	for _, key := range append(slices.Clone(exporterKeys), syncIgnored) {
		if raw, ok := installing[key]; ok {
			if err := doc.set(key, raw); err != nil {
				return path, false, err
			}
		}
	}
	// The record first: a write that then fails leaves settings the record does not match,
	// which an undo leaves alone.
	if err := harness.NewJournal(name, path, existing, installing, nil, nil, previous).Save(a.ConfigDir); err != nil {
		return path, false, err
	}
	out := doc.pack()
	// The endpoint carries the relay's token: private to this user.
	return path, !bytes.Equal(out, data), config.WriteFileAtomic(writePath, out, harness.SettingsMode)
}

// disconnectVSCode puts back what the settings file at path held before setup, for each
// member still holding what terma wrote, and names the ones it left because they changed since.
func (a Agent) disconnectVSCode(path, token string) (changed bool, skipped []string, err error) {
	j, err := harness.LoadJournal(a.ConfigDir, name, path)
	if err != nil {
		return false, nil, err
	}
	writePath, _, err := config.ResolveWritePath(path)
	if err != nil {
		return false, nil, err
	}
	data, err := os.ReadFile(writePath)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil, harness.DeleteJournal(a.ConfigDir, name, path)
	}
	if err != nil {
		return false, nil, err
	}
	// Settings terma never pointed at the relay are not terma's to parse.
	if j == nil && (token == "" || !bytes.Contains(data, []byte("/"+token))) {
		return false, nil, nil
	}
	doc, err := parseSettings(data)
	if err != nil {
		return false, nil, fmt.Errorf("%s does not parse as JSON with comments, so terma leaves it alone: %w", path, err)
	}
	if j == nil {
		j = doc.unrecorded(path, token)
	}
	current := map[string]string{}
	for key := range j.Installed {
		if raw, ok := doc.raw(key); ok {
			current[key] = raw
		}
	}
	result, remaining := j.Apply(current)
	for key := range j.Installed {
		if raw, ok := current[key]; ok {
			err = doc.set(key, raw)
		} else {
			doc.remove(key)
		}
		if err != nil {
			return false, nil, err
		}
	}
	slices.Sort(result.Skipped)
	out := doc.pack()
	if changed = !bytes.Equal(out, data); changed {
		mode := fs.FileMode(harness.SettingsMode)
		if info, err := os.Stat(writePath); err == nil {
			mode = info.Mode().Perm()
		}
		if err := config.WriteFileAtomic(writePath, out, mode); err != nil {
			return false, nil, err
		}
	}
	if remaining.Empty() {
		return changed, result.Skipped, harness.DeleteJournal(a.ConfigDir, name, path)
	}
	return changed, result.Skipped, remaining.Save(a.ConfigDir)
}

// vscodePointed reports whether the extension's exporter in the settings file at path is
// on and sends to the relay at addr.
func vscodePointed(path, addr string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	doc, err := parseSettings(data)
	if err != nil {
		return false
	}
	enabled, _ := doc.raw(chatEnabled)
	endpoint, _ := doc.text(chatEndpoint)
	return enabled == "true" && strings.HasPrefix(endpoint, "http://"+addr+"/")
}

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

// withIgnored is the sync-ignore list with keys added, as raw JSON; false when the file's
// own list is not one terma can add to.
func (d *settingsDoc) withIgnored(keys []string) (string, bool) {
	list, ok := d.ignored()
	if !ok {
		return "", false
	}
	for _, key := range keys {
		if !slices.Contains(list, key) {
			list = append(list, key)
		}
	}
	out, err := json.Marshal(list)
	return string(out), err == nil
}

// unrecorded stands in for a lost record of a file still holding the relay's token: the
// exporters pointed at it and their switches are terma's, with nothing earlier to restore.
func (d *settingsDoc) unrecorded(path, token string) *harness.Journal {
	installed := map[string]string{}
	for _, exporter := range [][]string{{chatEndpoint, chatEnabled, chatContent}, {hostEndpoint, hostEnabled, hostContent}} {
		if endpoint, _ := d.text(exporter[0]); !strings.Contains(endpoint, "/"+token) {
			continue
		}
		for _, key := range exporter {
			if raw, ok := d.raw(key); ok && (key == exporter[0] || raw == "true") {
				installed[key] = raw
			}
		}
	}
	j := harness.NewJournal(name, path, nil, installed, nil, nil, nil)
	// The sync-ignore list goes back to what it names besides terma's settings.
	if list, ok := d.ignored(); ok && len(installed) > 0 && slices.ContainsFunc(list, func(k string) bool { return slices.Contains(exporterKeys, k) }) {
		j.Installed[syncIgnored], _ = d.raw(syncIgnored)
		j.Previous[syncIgnored] = nil
		if rest := slices.DeleteFunc(list, func(k string) bool { return slices.Contains(exporterKeys, k) }); len(rest) > 0 {
			if out, err := json.Marshal(rest); err == nil {
				prior := string(out)
				j.Previous[syncIgnored] = &prior
			}
		}
	}
	return j
}
