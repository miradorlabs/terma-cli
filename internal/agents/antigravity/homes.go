package antigravity

import (
	"os"
	"path/filepath"
	"slices"
)

// An Antigravity home is the folder agy calls its gemini dir: config/ holds its hooks.json
// and skills, and an app data folder beside it its conversations. The agy on PATH uses
// ~/.gemini, and nothing moves it there; only `agy acp --gemini-dir` picks another, so a
// client that runs agy that way keeps a home of its own, which nothing on disk names but
// the client's own layout. terma knows one client's: T3 Code's.

// t3AppDataDir is the app data folder T3 Code passes every Antigravity server it starts
// (--app-data-dir), which tells an Antigravity instance's folder from another provider's,
// whatever the developer named the instance.
const t3AppDataDir = "antigravity-acp"

// homes are this machine's Antigravity homes: ~/.gemini first, then each client's, sorted.
func homes() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	clients, err := clientHomes()
	return append([]string{filepath.Join(home, ".gemini")}, clients...), err
}

// clientHomes are the Antigravity homes T3 Code keeps, one per instance and account:
// <T3 home>/userdata/providers/<instance>/<id>/, holding an antigravity-acp folder.
// <T3 home> is $T3CODE_HOME when that is an absolute path, else ~/.t3.
func clientHomes() ([]string, error) {
	t3 := os.Getenv("T3CODE_HOME")
	if !filepath.IsAbs(t3) {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		t3 = filepath.Join(home, ".t3")
	}
	// A folder that cannot be read holds no home terma can write to; it is passed over.
	providers := filepath.Join(t3, "userdata", "providers")
	instances, _ := os.ReadDir(providers)
	var out []string
	for _, instance := range instances {
		ids, _ := os.ReadDir(filepath.Join(providers, instance.Name()))
		for _, id := range ids {
			home := filepath.Join(providers, instance.Name(), id.Name())
			if info, err := os.Stat(filepath.Join(home, t3AppDataDir)); err == nil && info.IsDir() {
				out = append(out, home)
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
