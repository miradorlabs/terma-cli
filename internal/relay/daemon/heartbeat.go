package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/flock"
	"github.com/miradorlabs/terma-cli/internal/relay"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// CheckIn asks the running relay for a setup heartbeat now, proving the relay, credential and
// endpoint work, and briefly waits for a relay that is still starting.
func CheckIn(ctx context.Context, stateDir string) (ok bool, what string) {
	dir, err := Dir(stateDir)
	if err != nil {
		return false, err.Error()
	}
	token, err := Token(stateDir)
	if err != nil {
		return false, err.Error()
	}
	client := &http.Client{Timeout: 20 * time.Second}
	var resp *http.Response
	// Waited for only when one is running or its service will start it again.
	wait := 15 * time.Second
	if unlock, err := flock.TryLock(filepath.Join(dir, LockFile)); err == nil {
		unlock()
		if _, service := ServiceInstalled(stateDir); !service {
			wait = 0
		}
	}
	addr := Addr(dir)
	for deadline := time.Now().Add(wait); ; time.Sleep(250 * time.Millisecond) {
		// The token goes only to this user's relay: whatever else listens on its address
		// would learn it. The relay may move as it starts, so its address is read again.
		if Serving(dir) {
			addr = Addr(dir)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/heartbeat?reason="+semconv.TermaRelayHeartbeatReasonSetup, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			if resp, err = client.Do(req); err == nil {
				break
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
	}
	if resp == nil {
		if !Serving(dir) && Squatted(addr) {
			return false, "another process listens on " + addr + ", where the relay should; `terma doctor` says what to do"
		}
		return false, "the relay did not answer on " + addr + "; `terma doctor` says why"
	}
	defer resp.Body.Close()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body)
	switch {
	case resp.StatusCode == http.StatusOK:
		return true, "this machine reported to your organization"
	case strings.Contains(body.Error, relay.ErrNoKey.Error()):
		return true, "the relay will report once a repository is connected here"
	case body.Error != "":
		return false, "the relay could not reach your organization: " + body.Error
	}
	return false, fmt.Sprintf("the relay answered %d", resp.StatusCode)
}
