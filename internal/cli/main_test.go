package cli

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/builtin"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// testApp is the command line every test runs, with this build's agents and no other termas.
var testApp = func() *App {
	app := New(builtin.Agents, "dev")
	app.binDirs = func() []string { return nil }
	return app
}()

// TestMain gives the package a private HOME so no test can rewrite the developer's agent
// configuration, nor, where ZDOTDIR moves zsh's startup files out of HOME, their shell's;
// Go's caches stay put, or every test build would recompile the world.
func TestMain(m *testing.M) {
	os.Exit(runIsolated(m))
}

func runIsolated(m *testing.M) int {
	if out, err := exec.Command("go", "env", "GOCACHE", "GOMODCACHE", "GOPATH").Output(); err == nil {
		vals := strings.Split(strings.TrimSpace(string(out)), "\n")
		for i, k := range []string{"GOCACHE", "GOMODCACHE", "GOPATH"} {
			if i < len(vals) && vals[i] != "" {
				_ = os.Setenv(k, vals[i])
			}
		}
	}
	home, err := os.MkdirTemp("", "terma-cmd-home")
	if err != nil {
		fmt.Fprintln(os.Stderr, "private home:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(home) }()
	for k, v := range map[string]string{
		"HOME":                home,
		"SHELL":               "/bin/zsh",
		"XDG_CONFIG_HOME":     home + "/.config",
		"CLAUDE_CONFIG_DIR":   home + "/.claude",
		"CODEX_HOME":          home + "/.codex",
		"TERMA_CONFIG_DIR":    home + "/.config/terma",
		"TERMA_STATE_DIR":     home + "/.local/state/terma",
		"GEMINI_CLI_HOME":     home,
		"T3CODE_HOME":         home + "/.t3",
		"COPILOT_HOME":        home + "/.copilot",
		"VSCODE_APPDATA":      home + "/.vscode-data",
		"ZDOTDIR":             home,
		"GIT_CONFIG_NOSYSTEM": "1",
		// Offline; policy integration tests clear this and use the real HTTP path.
		"TERMA_POLICY_STUB": `{"mode":"repo","repositories":["github.com/acme/app"],"include_prompts":true,"include_tool_content":true,"default_project_id":"team"}`,
		// Offline: no active remediation campaign; the remediation tests set their own.
		"TERMA_REMEDIATION_STUB": `{"campaign":null}`,
	} {
		_ = os.Setenv(k, v)
	}
	testApp.configure(home+"/.config/terma", home+"/.local/state/terma")
	return m.Run()
}

// newTestRelay is relay.New with the registered agents' telemetry shapes, as relay run has.
// appRepo is the working copy the package's stub policy lists.
var appRepo = config.Repository{Origin: "github.com/acme/app"}

// selectPolicy stores pol as its team's, makes that team the default profile's, and
// commits it to routing/<profile>.json for the profile's active lane, as `terma setup`
// leaves them.
func selectPolicy(t *testing.T, pol config.Policy) {
	t.Helper()
	if err := config.WritePolicy(testApp.stateDir, pol); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateProfile(testApp.dir, config.DefaultProfile, func(p *config.Profile) { p.Team = pol.TeamID }); err != nil {
		t.Fatal(err)
	}
	cfg, err := testApp.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	lane := ""
	switch {
	case cfg.ServerKeySignIn:
		lane = sendas.NoPersonLane
	case cfg.Account != "":
		lane = cfg.Account
	default:
		lane = auth.Account(cfg.Dir, cfg.ProfileName, cfg.AuthURL)
	}
	if !routing.ValidAccount(lane) || pol.TeamID == "" || pol.FetchedAt.IsZero() {
		return
	}
	orgID := cmp.Or(pol.OrganizationID, cfg.OrganizationID, "org-test")
	authURL := cmp.Or(pol.AuthURL, cfg.AuthURL, "https://auth.example.test")
	pol.OrganizationID, pol.AuthURL = orgID, authURL
	tm := routing.Team{
		OrganizationID: orgID,
		AuthURL:        authURL,
		TeamID:         pol.TeamID,
		Personal:       pol.Global(),
		Policy:         pol,
	}
	if _, err := routing.Replace(testApp.stateDir, cfg.ProfileName, nil, lane, func(s *routing.State) error {
		if _, err := s.Select(tm); err != nil {
			return err
		}
		if !tm.Personal {
			for _, raw := range pol.Repositories {
				if e, ok := routing.ParseEntry(raw, pol.Wildcards); ok {
					if err := s.Bind(e.Key(), orgID, false); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// admitHere signs the profile into a validated repository-mode policy for team, listing
// root's origin, github.com/acme/<root's folder>, as `terma setup` would leave it.
func admitHere(t *testing.T, root, team string) {
	t.Helper()
	if cfg, err := testApp.loadConfig(); err == nil && !cfg.ServerKeySignIn && cfg.Account == "" && auth.Account(cfg.Dir, cfg.ProfileName, cfg.AuthURL) == "" {
		signInHereAs(t, "u-test")
	}
	pol := config.Policy{Mode: config.ModeRepo, Repositories: []string{"github.com/acme/" + filepath.Base(root)}, IncludePrompts: true, IncludeToolContent: true, GitHooks: true,
		TeamID: team, Revision: 1, FetchedAt: time.Now()}
	selectPolicy(t, pol)
}

// signInHereAs stores a person's sign-in for the default profile, as `terma login` leaves
// it, so what hooks collect is in user's lane; it returns that lane's account key.
func signInHereAs(t *testing.T, user string) string {
	t.Helper()
	const authURL = "https://auth.example.test"
	if err := saveSignIn(testApp.dir, &auth.Credential{AccessToken: "token-of-" + user, SessionID: "s-" + user, UserID: user,
		AuthURL: authURL, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	acct := auth.AccountKey(authURL, user)
	if st, found, err := routing.Load(testApp.stateDir, config.DefaultProfile); err == nil && found && st.Account != acct {
		_, err := routing.Replace(testApp.stateDir, config.DefaultProfile, nil, acct, func(s *routing.State) error {
			for _, tm := range st.Teams {
				if _, err := s.Select(tm); err != nil {
					return err
				}
			}
			for k, b := range st.Bindings {
				if b.Refused {
					if err := s.Refuse(k); err != nil {
						return err
					}
				} else if err := s.Bind(k, b.Organization, false); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return acct
}

func newTestRelay(o relay.Options) *relay.Relay {
	o.Correlators, o.Capturers = testApp.agents.With[shape.Correlator](), testApp.agents.With[shape.Capturer]()
	return relay.New(o)
}

func harnessOf(t *testing.T, name string) harness.Harness {
	t.Helper()
	e, ok := testApp.agents.Find[agents.Exporting](name)
	if !ok {
		t.Fatalf("no exporting agent %q", name)
	}
	return e.Harness()
}

// claudeHarness is the status-line agent's harness with the capabilities these tests use.
func claudeHarness(t *testing.T) struct {
	harness.Harness
	agents.StatusLiner
} {
	t.Helper()
	line, ok := testApp.agents.Find[agents.StatusLiner]("claude")
	if !ok {
		t.Fatal("claude lost its status line")
	}
	return struct {
		harness.Harness
		agents.StatusLiner
	}{harnessOf(t, "claude"), line}
}

// local is h bound to the repository at root, which it must support.
func local(t *testing.T, h harness.Harness, root string) harness.Harness {
	t.Helper()
	l, ok := h.Local(root)
	if !ok {
		t.Fatalf("%s has no repository scope", h.Name())
	}
	return l
}
