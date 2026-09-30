package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

// The facts the relay's heartbeat reports about this machine's terma
// (internal/relay/heartbeat.go): which build runs where and how it is set up. Settings
// and versions only — no hostname, no account, no path under the home directory. The
// machine is named by a random id terma makes on first use (relay/machine-id), so the
// platform can tell machines apart and count them without knowing whose they are.

const relayMachineIDFile = "machine-id"

// relayMachineID is this machine's random id, made once.
func relayMachineID(dir string) string {
	path := filepath.Join(dir, relayMachineIDFile)
	if data, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(data)); len(id) == 32 {
			return id
		}
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	id := hex.EncodeToString(b)
	if os.MkdirAll(dir, 0o700) != nil || config.WriteFileAtomic(path, []byte(id+"\n"), 0o600) != nil {
		return ""
	}
	return id
}

// relayHeartbeatInfo returns what each heartbeat says, read again at most every minute:
// setup can change the mode or the agents while the relay runs.
func relayHeartbeatInfo(dir string) func() map[string]any {
	var mu sync.Mutex
	var at time.Time
	var cached map[string]any
	return func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		if cached != nil && time.Since(at) < time.Minute {
			return cached
		}
		cached, at = heartbeatFacts(dir), time.Now()
		return cached
	}
}

func heartbeatFacts(dir string) map[string]any {
	facts := map[string]any{
		"terma.version":    Version,
		"terma.os":         runtime.GOOS,
		"terma.arch":       runtime.GOARCH,
		"terma.machine_id": relayMachineID(dir),
		"terma.install":    installKind(),
	}
	if _, ok := relayServiceInstalled(); ok {
		facts["terma.relay.service"] = true
	} else {
		facts["terma.relay.service"] = false
	}
	if cfg, err := config.Load(config.Overrides{}); err == nil {
		facts["terma.mode"] = cfg.Policy.Mode
		facts["terma.policy.include_prompts"] = cfg.Policy.IncludePrompts
		facts["terma.policy.include_tool_content"] = cfg.Policy.IncludeToolContent
		if !cfg.Policy.FetchedAt.IsZero() {
			facts["terma.policy.fetched_at"] = cfg.Policy.FetchedAt.UTC().Format(time.RFC3339)
		}
		if len(cfg.Harnesses) > 0 {
			facts["terma.agents"] = cfg.Harnesses
		}
		facts["terma.environment"] = cfg.Environment
		org := cfg.OrganizationID
		if cred, err := auth.LoadCredential(cfg.ProfileName); org == "" && err == nil {
			org = cred.OrganizationID // signed in without setup recording it
		}
		if org != "" {
			facts["terma.organization_id"] = org
		}
	}
	if cdir, err := config.Dir(); err == nil {
		if prefs, err := selfupdate.LoadPreferences(cdir); err == nil {
			facts["terma.auto_update"] = prefs.Auto
		}
	}
	// Which agents' exporters point at this relay now: one pointed elsewhere since
	// (a reinstall, a hand edit) sends nothing through it, and says nothing else of it.
	var pointed []string
	for _, a := range relayAgents {
		if h, err := harness.Lookup(a); err == nil && exportsToRelay(h, relayAddr(dir)) {
			pointed = append(pointed, a)
		}
	}
	facts["terma.relay.agents_pointed"] = pointed
	if _, ok := codexDaemonPredates(dir); ok {
		facts["terma.codex.daemon_predates_setup"] = true
	}
	return facts
}

// installKind is how this terma was installed: the package manager that owns it, a
// source build, or the install script's.
func installKind() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	if m, ok := selfupdate.ManagedBy(exe); ok {
		return strings.ToLower(m.Name)
	}
	if !strings.HasPrefix(Version, "v") || strings.Contains(Version, "-g") || Version == "dev" {
		return "source"
	}
	return "script"
}

// exportsToRelay reports whether h's user-level exporter points at the relay on addr
// (Gemini's endpoint carries the token as its first path segment).
func exportsToRelay(h harness.Harness, addr string) bool {
	st, err := h.Status()
	if err != nil || !st.Connected {
		return false
	}
	ep := strings.TrimRight(st.Endpoint, "/")
	return ep == "http://"+addr || strings.HasPrefix(ep, "http://"+addr+"/")
}

// relayHeartbeatSend delivers a heartbeat to the organization the developer signed in
// to, with their credential (api.SendHeartbeat). Not signed in, there is no organization
// to tell, and nothing is sent.
func relayHeartbeatSend(ctx context.Context, beat *logspb.LogsData) error {
	cfg, err := config.Load(config.Overrides{})
	if err != nil {
		return err
	}
	if cfg.APIKey != "" {
		return errors.New("a server key is not a developer's sign-in: heartbeats go with one")
	}
	body, err := protojson.Marshal(beat)
	if err != nil {
		return err
	}
	client, err := newClient(cfg)
	if err != nil {
		return err
	}
	return client.SendHeartbeat(ctx, body)
}
