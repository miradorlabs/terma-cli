package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/relay/daemon"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

// The facts the relay's heartbeat reports about this machine's terma
// (internal/relay/heartbeat.go): which build runs where and how it is set up. Settings
// and versions only — no hostname, no account, no path under the home directory. The
// machine is named by a random id terma makes on first use (relay/machine-id), so the
// platform can tell machines apart and count them without knowing whose they are.

// relayMachineID is this machine's random id, made once.
func relayMachineID(dir string) string {
	path := filepath.Join(dir, daemon.MachineIDFile)
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
	if _, ok := daemon.ServiceInstalled(); ok {
		facts["terma.relay.service"] = true
	} else {
		facts["terma.relay.service"] = false
	}
	if cfg, err := config.Load(config.Overrides{}); err == nil {
		facts["terma.mode"] = cfg.Policy.Mode
		facts["terma.policy.include_prompts"] = cfg.Policy.IncludePrompts
		facts["terma.policy.include_tool_content"] = cfg.Policy.IncludeToolContent
		facts["terma.policy.revision"] = int(cfg.Policy.Revision)
		if cfg.Policy.Signals != nil {
			facts["terma.policy.signals"] = cfg.Policy.Signals
		}
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
	var pointed, blocked []string
	for _, e := range registered.With[agents.RelayExporter]() {
		if ok, known := e.RelayPointed(daemon.Addr(dir)); known && ok {
			pointed = append(pointed, e.Name())
		}
		if c, ok := e.(agents.RelayChecker); ok {
			if _, _, problem := c.RelayProblem(dir); problem {
				blocked = append(blocked, e.Name())
			}
		}
	}
	facts["terma.relay.agents_pointed"] = pointed
	facts["terma.relay.agents_blocked"] = blocked
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

// relayCheckIn asks the running relay for a heartbeat now (reason "setup") and says
// what came of it: the platform's "installed and working" for this machine, and the
// developer's proof that the relay, their credential and the organization's endpoint
// all work — or which of them does not. A relay that is still starting (a service
// restarted to read a new policy) is waited for, briefly.
func relayCheckIn(ctx context.Context) (ok bool, what string) {
	dir, err := daemon.Dir()
	if err != nil {
		return false, err.Error()
	}
	token, err := daemon.Token()
	if err != nil {
		return false, err.Error()
	}
	addr := daemon.Addr(dir)
	client := &http.Client{Timeout: 20 * time.Second}
	var resp *http.Response
	// Waited for only when one is running (its lock is held) or its service will start
	// it again; with neither, there is nothing to wait for.
	wait := 15 * time.Second
	if unlock, err := flock.TryLock(filepath.Join(dir, daemon.LockFile)); err == nil {
		unlock()
		if _, service := daemon.ServiceInstalled(); !service {
			wait = 0
		}
	}
	for deadline := time.Now().Add(wait); ; time.Sleep(250 * time.Millisecond) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/heartbeat?reason="+relay.HeartbeatSetup, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err = client.Do(req)
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return false, "the relay did not answer on " + addr + "; `terma relay status` says why"
	}
	defer resp.Body.Close()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body)
	switch {
	case resp.StatusCode == http.StatusOK:
		return true, "this machine reported to your organization"
	case strings.Contains(body.Error, "status 404"):
		return true, "your organization does not take check-ins yet; the relay will keep trying every 15 minutes"
	case body.Error != "":
		return false, "the relay could not reach your organization: " + body.Error
	}
	return false, fmt.Sprintf("the relay answered %d", resp.StatusCode)
}
