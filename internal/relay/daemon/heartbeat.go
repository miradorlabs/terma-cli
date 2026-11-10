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
	switch err := Beat(ctx, stateDir, semconv.TermaRelayHeartbeatReasonSetup, relay.HeartbeatTarget{}, wait); {
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

// ErrUntargeted is Beat's error when the running relay sent a beat without holding it to its
// target: it is older than targets, so the beat proves nothing about the target's delivery.
var ErrUntargeted = errors.New("the running relay is older than this terma and cannot send a beat for one team")

// UndeliveredError is Beat's error when the relay sent the beat and the network or the
// platform failed it; every other error means it could not be tried.
type UndeliveredError struct {
	Detail string
	// Refused says the ingest refused the key.
	Refused bool
}

func (e *UndeliveredError) Error() string {
	return "the relay could not reach your organization: " + e.Detail
}

// KeyRefused reports whether the ingest refused the key rather than the request.
func (e *UndeliveredError) KeyRefused() bool { return e.Refused }

// Beat asks the running relay to send a heartbeat for reason now, waiting up to wait for one
// that is still starting; a target with a team makes it prove that team's delivery
// (relay.HeartbeatTarget). It is nil once the platform accepted the beat; relay.ErrNoKey when
// this machine holds no key to send it with; relay.ErrElsewhere when the relay would send the
// target's beat to another ingest; ErrUntargeted when the relay ignored the target; and an
// *UndeliveredError when the network or the platform failed it.
func Beat(ctx context.Context, stateDir, reason string, target relay.HeartbeatTarget, wait time.Duration) error {
	dir, err := Dir(stateDir)
	if err != nil {
		return err
	}
	token, err := Token(stateDir)
	if err != nil {
		return err
	}
	addr := Addr(dir)
	q := url.Values{"reason": {reason}}
	if target.Team != "" {
		q.Set("team", target.Team)
		q.Set("endpoint", target.Endpoint)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	var resp *http.Response
	for deadline := time.Now().Add(wait); ; time.Sleep(250 * time.Millisecond) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/heartbeat?"+q.Encode(), nil)
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
	var body relay.HeartbeatAnswer
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body)
	relayAnswered := resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusBadGateway
	switch {
	case relayAnswered && body.Team != target.Team:
		// A relay from before targets sent the beat, or failed to, with its own pick of key
		// and ingest.
		return ErrUntargeted
	case resp.StatusCode == http.StatusOK:
		return nil
	case body.Code == relay.HeartbeatNoKey, body.Code == "" && strings.Contains(body.Error, relay.ErrNoKey.Error()):
		return relay.ErrNoKey
	case body.Code == relay.HeartbeatElsewhere:
		return fmt.Errorf("%w%s", relay.ErrElsewhere, strings.TrimPrefix(body.Error, relay.ErrElsewhere.Error()))
	case body.Code == relay.HeartbeatUndelivered, body.Code == "" && body.Error != "":
		// A relay from before codes calls every failure this; only setup's check-in, which
		// names no target, still meets one.
		return &UndeliveredError{Detail: body.Error, Refused: body.KeyRefused}
	case body.Error != "":
		return errors.New("the relay could not send a beat: " + body.Error)
	}
	return fmt.Errorf("the relay answered %d", resp.StatusCode)
}
