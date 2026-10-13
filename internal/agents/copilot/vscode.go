package copilot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tailscale/hujson"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// VS Code runs the Copilot runtime in its agent host, a process of its own whose exporter
// VS Code sets as it starts from chat.agentHost.otel.* in the user settings. That file is
// JSON with comments the developer edits by hand too: terma changes only the members below
// and leaves every other byte, comment and comma as it was.
//
// Copilot Chat's own exporter (github.copilot.chat.otel.*) is left as the developer has
// it: once it is on, VS Code copies its endpoint into the extension host's environment and
// into the terminals it opens for Copilot, where every extension's and tool's exporter
// would read it. The agent host's settings are not Copilot's alone either: VS Code hands
// the same exporter to the Claude and Codex agents a developer has it host there.
const (
	hostEnabled  = "chat.agentHost.otel.enabled"
	hostEndpoint = "chat.agentHost.otel.otlpEndpoint"
	hostContent  = "chat.agentHost.otel.captureContent"
	// syncIgnored lists the settings Settings Sync leaves on this machine.
	syncIgnored = "settingsSync.ignoredSettings"
)

// exporterKeys are the exporter settings terma writes, in the order it writes them.
var exporterKeys = []string{hostEnabled, hostEndpoint, hostContent}

// vscodeFlavors are the VS Code builds that ship Copilot, by their data directory's name.
var vscodeFlavors = []string{"Code", "Code - Insiders"}

// copilotStorage is where Copilot keeps its state in a VS Code it has run in.
const copilotStorage = "github.copilot-chat"

// vscodeUserDirs are the user settings directories of the VS Code builds here, under
// VSCODE_APPDATA where the developer moved VS Code's data with it: those Copilot has run
// in, which setup configures, or with all set every one, which an undo looks through.
func vscodeUserDirs(all bool) []string {
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
		probe := filepath.Join(dir, "globalStorage", copilotStorage)
		if all {
			probe = dir
		}
		if info, err := os.Stat(probe); err == nil && info.IsDir() {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// undoPaths are the settings files an undo looks at: every VS Code's here, and any a
// record says setup wrote, wherever Copilot's storage or VS Code's data has gone since.
func (a Agent) undoPaths() []string {
	var paths []string
	for _, dir := range vscodeUserDirs(true) {
		paths = append(paths, filepath.Join(dir, "settings.json"))
	}
	records, _ := filepath.Glob(filepath.Join(a.ConfigDir, config.SetupDir, name+"-*.json"))
	for _, record := range records {
		var j struct {
			ConfigPath string `json:"config_path"`
		}
		if data, err := os.ReadFile(record); err == nil && json.Unmarshal(data, &j) == nil && j.ConfigPath != "" && !slices.Contains(paths, j.ConfigPath) {
			paths = append(paths, j.ConfigPath)
		}
	}
	return paths
}

// shellEndpoints are the variables the Copilot runtime exports to ahead of what VS Code's
// settings give its agent host.
var shellEndpoints = []string{harness.EnvOTLPEndpoint, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "COPILOT_OTEL_ENDPOINT"}

// shellEndpoint names an OTLP endpoint variable exported where terma runs that is not the
// relay at addr. VS Code applies its settings only to the variables its own environment
// leaves unset, and takes that environment from the developer's shell: with one set, the
// agent host exports there already, and terma's settings would only add what was said to
// it. A collector on this machine is no exception, since it is there to forward. The
// relay's own address is: another agent's exporter names it in the environment terma is
// started from. The value may hold a credential, so only the name is returned.
func shellEndpoint(addr string) (string, bool) {
	for _, name := range shellEndpoints {
		if v := os.Getenv(name); v != "" && !relayURL(v, addr) {
			return name, true
		}
	}
	return "", false
}

// relayURL reports whether raw is a URL of the relay at addr.
func relayURL(raw, addr string) bool {
	u, err := url.Parse(strings.Trim(raw, `"'`))
	return err == nil && addr != "" && u.Host == addr
}

// exporterSettings are the members that send the agent host's exporter to the relay, each
// as raw JSON. No setting gives it a header, so the relay's token rides the endpoint path,
// and the path names the signal: the agent host appends one only to an endpoint with none.
// All content is exported: the relay withholds what the team's policy does not collect.
func exporterSettings(cfg agents.RelayConfig) map[string]string {
	return map[string]string{
		hostEnabled:  "true",
		hostEndpoint: string(hujson.String(cfg.Endpoint + "/" + cfg.Token + "/v1/traces")),
		hostContent:  "true",
	}
}

// connectVSCode writes the exporter settings into the settings file of the VS Code at dir.
// It reports whether it changed the file, and what the developer should know: a
// sync-ignore list terma could not add its settings to.
func (a Agent) connectVSCode(dir string, cfg agents.RelayConfig) (path string, changed bool, note string, err error) {
	path = filepath.Join(dir, "settings.json")
	if a.ConfigDir == "" {
		return path, false, "", errors.New("no config directory to record VS Code's earlier settings in")
	}
	writePath, _, err := config.ResolveWritePath(path)
	if err != nil {
		return path, false, "", err
	}
	data, err := os.ReadFile(writePath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return path, false, "", err
	}
	doc, err := parseSettings(data)
	if err != nil {
		return path, false, "", fmt.Errorf("it does not parse as JSON with comments: %w", err)
	}
	installing := exporterSettings(cfg)
	// The endpoint names this machine's relay and token: synced, it would switch another
	// machine's export on, pointed at a relay it does not have.
	if list, ok := doc.ignored(); ok {
		installing[syncIgnored] = rawList(withKeys(list, exporterKeys))
	} else {
		note = "Could not add terma's settings to " + syncIgnored + " in " + path + ", which is not a list of names: with Settings Sync on, add " + strings.Join(exporterKeys, ", ") + " to it yourself, or they follow you to your other machines."
	}
	existing := map[string]string{}
	for key := range installing {
		if raw, ok := doc.raw(key); ok {
			existing[key] = raw
		}
	}
	previous, err := harness.LoadJournal(a.ConfigDir, name, path)
	if err != nil {
		return path, false, "", err
	}
	if previous == nil {
		doc.forgetOwn(existing, cfg)
	}
	for _, key := range append(slices.Clone(exporterKeys), syncIgnored) {
		if raw, ok := installing[key]; ok {
			if err := doc.set(key, raw); err != nil {
				return path, false, "", err
			}
		}
	}
	// The record first, so no write leaves settings terma cannot undo; a write that then
	// fails takes the record back with it.
	if err := harness.NewJournal(name, path, existing, installing, nil, nil, previous).Save(a.ConfigDir); err != nil {
		return path, false, "", err
	}
	out := doc.pack()
	// The endpoint carries the relay's token: private to this user.
	if err := config.WriteFileAtomic(writePath, out, harness.SettingsMode); err != nil {
		if previous != nil {
			_ = previous.Save(a.ConfigDir)
		} else {
			_ = harness.DeleteJournal(a.ConfigDir, name, path)
		}
		return path, false, "", err
	}
	return path, !bytes.Equal(out, data), note, nil
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
		} else {
			// The developer took the member out: there is nothing of terma's left to undo.
			delete(j.Installed, key)
			delete(j.Previous, key)
		}
	}
	doc.keepOwnIgnored(j, current)
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
		mode := harness.SettingsMode
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

// vscodePointed reports whether the agent host's exporter in the settings file at path is
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
	enabled, _ := doc.raw(hostEnabled)
	endpoint, _ := doc.text(hostEndpoint)
	return enabled == "true" && strings.HasPrefix(endpoint, "http://"+addr+"/")
}

// keepOwnIgnored lets an undo take terma's names out of a sync-ignore list the developer
// has edited since setup: left to the record's rule, an edited list would be skipped whole
// and keep them. It records the list as it now stands as what setup wrote, and that list
// without terma's names as what came before.
func (d *settingsDoc) keepOwnIgnored(j *harness.Journal, current map[string]string) {
	raw, ok := current[syncIgnored]
	if !ok || raw == j.Installed[syncIgnored] {
		return
	}
	list, ok := d.ignored()
	if !ok {
		return
	}
	j.Installed[syncIgnored], j.Previous[syncIgnored] = raw, nil
	if rest := withoutKeys(list, exporterKeys); len(rest) == len(list) {
		// None of terma's names are in it any more: it is the developer's as it stands.
		delete(j.Installed, syncIgnored)
		delete(j.Previous, syncIgnored)
	} else if len(rest) > 0 {
		prior := rawList(rest)
		j.Previous[syncIgnored] = &prior
	}
}

// forgetOwn takes out of existing, what the file held before a setup with no record of an
// earlier one, the values that earlier setup wrote: an exporter already pointed at cfg's
// relay is terma's, and so are its names in the sync-ignore list. Kept, they would be
// restored as the developer's own, and teardown would leave the export on.
func (d *settingsDoc) forgetOwn(existing map[string]string, cfg agents.RelayConfig) {
	if endpoint, _ := d.text(hostEndpoint); !strings.HasPrefix(endpoint, cfg.Endpoint+"/") {
		return
	}
	for _, key := range exporterKeys {
		delete(existing, key)
	}
	delete(existing, syncIgnored)
	if list, ok := d.ignored(); ok {
		if rest := withoutKeys(list, exporterKeys); len(rest) > 0 {
			existing[syncIgnored] = rawList(rest)
		}
	}
}

// unrecorded stands in for a lost record of a file still holding the relay's token: the
// exporter pointed at it, its switches and its names in the sync-ignore list are terma's,
// with nothing earlier to restore.
func (d *settingsDoc) unrecorded(path, token string) *harness.Journal {
	installed := map[string]string{}
	if endpoint, _ := d.text(hostEndpoint); strings.Contains(endpoint, "/"+token) {
		for _, key := range exporterKeys {
			if raw, ok := d.raw(key); ok && (key == hostEndpoint || raw == "true") {
				installed[key] = raw
			}
		}
	}
	j := harness.NewJournal(name, path, nil, installed, nil, nil, nil)
	if list, ok := d.ignored(); ok && len(installed) > 0 && len(withoutKeys(list, exporterKeys)) < len(list) {
		j.Installed[syncIgnored], _ = d.raw(syncIgnored)
		j.Previous[syncIgnored] = nil
		if rest := withoutKeys(list, exporterKeys); len(rest) > 0 {
			prior := rawList(rest)
			j.Previous[syncIgnored] = &prior
		}
	}
	return j
}
