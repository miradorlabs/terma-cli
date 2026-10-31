package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	// Waited for only when one is running or its service will start it again.
	wait := 15 * time.Second
	if unlock, err := flock.TryLock(filepath.Join(dir, LockFile)); err == nil {
		unlock()
		if _, service := ServiceInstalled(stateDir); !service {
			wait = 0
		}
	}
	switch err := Beat(ctx, stateDir, semconv.TermaRelayHeartbeatReasonSetup, wait); {
	case err == nil:
		return true, "this machine reported to your organization"
	case errors.Is(err, relay.ErrNoKey):
		return true, "the relay will report once a repository is connected here"
	case errors.Is(err, ErrNoAnswer):
		return false, err.Error() + "; `terma doctor` says why"
	default:
		return false, err.Error()
	}
}

// ErrNoAnswer is Beat's error when no relay answers on its address.
var ErrNoAnswer = errors.New("the relay did not answer")

// Beat asks the running relay to send a heartbeat for reason now, waiting up to wait for one
// that is still starting. It is nil once the platform accepted the beat, and relay.ErrNoKey
// when this machine holds no team key to send it with.
func Beat(ctx context.Context, stateDir, reason string, wait time.Duration) error {
	dir, err := Dir(stateDir)
	if err != nil {
		return err
	}
	token, err := Token(stateDir)
	if err != nil {
		return err
	}
	addr := Addr(dir)
	client := &http.Client{Timeout: 20 * time.Second}
	var resp *http.Response
	for deadline := time.Now().Add(wait); ; time.Sleep(250 * time.Millisecond) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/heartbeat?reason="+url.QueryEscape(reason), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err = client.Do(req)
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("%w on %s", ErrNoAnswer, addr)
	}
	defer resp.Body.Close()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body)
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case strings.Contains(body.Error, relay.ErrNoKey.Error()):
		return relay.ErrNoKey
	case body.Error != "":
		return errors.New("the relay could not reach your organization: " + body.Error)
	}
	return fmt.Errorf("the relay answered %d", resp.StatusCode)
}
