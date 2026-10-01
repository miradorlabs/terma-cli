package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/selfupdate"
)

// The heartbeat's facts are settings and versions only, never a hostname, account or home
// path; a random machine id lets the platform count machines without knowing whose they are.

// MachineID is this machine's random id, made once.
func MachineID(dir string) string {
	path := filepath.Join(dir, MachineIDFile)
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

// Heartbeat is what the relay's heartbeat says about this machine's terma.
type Heartbeat struct {
	Dir         string
	Version     string
	InstallKind string
	// Agents says which agents' exporters send to the relay at addr now.
	Agents func(addr string) (pointed, blocked []string)
}

// Info returns what each heartbeat says, reread at most every minute since setup can change it.
func (h Heartbeat) Info() func() map[string]any {
	var mu sync.Mutex
	var at time.Time
	var cached map[string]any
	return func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		if cached != nil && time.Since(at) < time.Minute {
			return cached
		}
		cached, at = h.Facts(), time.Now()
		return cached
	}
}

// Facts are the heartbeat's facts now.
func (h Heartbeat) Facts() map[string]any {
	facts := map[string]any{
		"terma.version":    h.Version,
		"terma.os":         runtime.GOOS,
		"terma.arch":       runtime.GOARCH,
		"terma.machine_id": MachineID(h.Dir),
		"terma.install":    h.InstallKind,
	}
	if _, ok := ServiceInstalled(); ok {
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
	pointed, blocked := h.Agents(Addr(h.Dir))
	facts["terma.relay.agents_pointed"] = pointed
	facts["terma.relay.agents_blocked"] = blocked
	return facts
}

// CheckIn asks the running relay for a setup heartbeat now, proving the relay, credential and
// endpoint work, and briefly waits for a relay that is still starting.
func CheckIn(ctx context.Context) (ok bool, what string) {
	dir, err := Dir()
	if err != nil {
		return false, err.Error()
	}
	token, err := Token()
	if err != nil {
		return false, err.Error()
	}
	addr := Addr(dir)
	client := &http.Client{Timeout: 20 * time.Second}
	var resp *http.Response
	// Waited for only when one is running or its service will start it again.
	wait := 15 * time.Second
	if unlock, err := flock.TryLock(filepath.Join(dir, LockFile)); err == nil {
		unlock()
		if _, service := ServiceInstalled(); !service {
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
