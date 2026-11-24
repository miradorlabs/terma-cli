package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookmgr"
)

// An Antigravity home is the folder agy calls its gemini dir: config/ holds its hooks.json
// and skills, and an app data folder beside it its conversations. The agy on PATH uses
// ~/.gemini, and nothing moves it there; only `agy acp --gemini-dir` picks another, so a
// client that runs agy that way keeps a home of its own, which nothing on disk names but
// the client's own layout. terma knows the layouts in antigravityClients.

// t3AppDataDir is the app data folder T3 Code passes every Antigravity server it starts
// (--app-data-dir), which tells an Antigravity instance's folder from another provider's,
// whatever the developer named the instance.
const t3AppDataDir = "antigravity-acp"

// antigravityClient is where a client that runs its own agy keeps Antigravity homes:
// <client home>/<Providers>/*/*, each holding the AppData folder. <client home> is $HomeEnv
// when that is an absolute path, else Home under the user's home.
type antigravityClient struct {
	HomeEnv, Home, Providers, AppData string
}

// antigravityClients are the clients whose homes terma covers. T3 Code's layout was checked
// against T3 Code 0.1.11 with its bundled antigravity 1.5.14 on 2026-11-23: one home per
// provider instance and account. A second client is one more entry.
var antigravityClients = []antigravityClient{
	{HomeEnv: "T3CODE_HOME", Home: ".t3", Providers: filepath.Join("userdata", "providers"), AppData: t3AppDataDir},
}

// homes are this machine's Antigravity homes: ~/.gemini first, then each client's, sorted.
func homes() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	clients, err := clientHomes()
	return append([]string{filepath.Join(home, ".gemini")}, clients...), err
}

// clientHomes are the Antigravity homes the clients in antigravityClients keep, sorted.
func clientHomes() ([]string, error) {
	var out []string
	for _, c := range antigravityClients {
		root := os.Getenv(c.HomeEnv)
		if !filepath.IsAbs(root) {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			root = filepath.Join(home, c.Home)
		}
		// A folder that cannot be read holds no home terma can write to; it is passed over.
		providers := filepath.Join(root, c.Providers)
		instances, _ := os.ReadDir(providers)
		for _, instance := range instances {
			ids, _ := os.ReadDir(filepath.Join(providers, instance.Name()))
			for _, id := range ids {
				home := filepath.Join(providers, instance.Name(), id.Name())
				if isDir(filepath.Join(home, c.AppData)) {
					out = append(out, home)
				}
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// UserHooksPaths are the hooks.json of every Antigravity home on this machine, the one
// UserHooksPath names first: a hook in one home runs only the sessions begun there.
func (Agent) UserHooksPaths() ([]string, error) {
	all, err := homes()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(all))
	for _, h := range all {
		out = append(out, filepath.Join(h, "config", "hooks.json"))
	}
	return out, nil
}

// defaultHooksDir is the config folder of the home the agy on PATH uses, ~/.gemini/config.
func defaultHooksDir() (string, error) {
	path, err := Agent{}.UserHooksPath()
	return filepath.Dir(path), err
}

// isDefaultHooksDir reports whether dir is defaultHooksDir.
func isDefaultHooksDir(dir string) bool {
	def, err := defaultHooksDir()
	return err == nil && filepath.Clean(dir) == filepath.Clean(def)
}

// tooOldFor is tooOld for the home whose config folder is dir: only ~/.gemini runs the agy
// on PATH; a client's home runs the client's own copy, which no minimum holds back.
func tooOldFor(dir string) (string, bool) {
	if !isDefaultHooksDir(dir) {
		return "", false
	}
	return tooOld(context.Background())
}

// isDir reports whether path is a folder.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// hooksFiles are the homes' hooks files as a refresh finds them.
type hooksFiles struct {
	// carry name terma's hook, on or off; bare are those of homes that are there and name
	// no terma hook, which may be given it: not ~/.gemini's while the agy on PATH is too old.
	carry, bare []string
	// on is whether a file in carry has terma's hook switched on.
	on bool
	// errs are the files that could not be read.
	errs []error
}

// readHooksFiles sorts the hooks files at paths for refreshHooksFile.
func readHooksFiles(paths []string) hooksFiles {
	var f hooksFiles
	for _, path := range paths {
		data, err := hookmgr.ReadFile(path)
		switch {
		case err != nil:
			f.errs = append(f.errs, err)
		case bytes.Contains(data, []byte(" hook --user antigravity-")):
			f.carry = append(f.carry, path)
			f.on = f.on || termaHookOn(data)
		case namesNoTermaHook(data) && isDir(filepath.Dir(filepath.Dir(path))):
			// A home that is not there, ~/.gemini with no agy say, is not made.
			if _, old := tooOldFor(filepath.Dir(path)); !old {
				f.bare = append(f.bare, path)
			}
		}
	}
	return f
}

// termaHookOn reports whether a hooks file, data as read, has terma's hook switched on: a
// top-level terma member, not "enabled": false, that runs at least one of terma's handlers.
// A mention elsewhere, in a comment say, is none.
func termaHookOn(data []byte) bool {
	_, top, err := parseHooks(data)
	if err != nil {
		return false
	}
	var member map[string]json.RawMessage
	if json.Unmarshal(top[antigravityHookName], &member) != nil {
		return false
	}
	if raw, ok := member["enabled"]; ok && string(bytes.TrimSpace(raw)) == "false" {
		return false
	}
	return hookmgr.CallsTerma(top[antigravityHookName])
}
