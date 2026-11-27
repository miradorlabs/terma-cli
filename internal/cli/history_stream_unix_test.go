//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/history"
)

// saveFileBackedSignIn writes the fixture's sign-in into user-credentials.json so a child
// terma binary reads it without touching the host system keychain.
func saveFileBackedSignIn(t *testing.T, fx *historyFixture) {
	t.Helper()
	if err := config.UpdateFile(fx.cfgDir, func(f *config.File) { f.InsecureStorage = true }); err != nil {
		t.Fatal(err)
	}
	cred := storedSession(&fakeAuth{srv: fx.srv})
	if err := saveSignIn(fx.cfgDir, cred); err != nil {
		t.Fatal(err)
	}
}

// Interrupting a --json-stream upload with SIGINT ends with a summary whose "interrupted"
// is true (and no error line) and process exit code 130.
func TestHistoryUploadJSONStreamInterruptExit130(t *testing.T) {
	bin := termaBinary(t)
	fx := newHistoryFixture(t)
	saveFileBackedSignIn(t, fx)

	hitLogs := make(chan struct{}, 1)
	releaseLogs := make(chan struct{})
	fx.answer(func(f *historyFixture) {
		f.onLogs = func(string) {
			select {
			case hitLogs <- struct{}{}:
			default:
			}
			<-releaseLogs
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "history", "upload", "--json-stream")
	cmd.Env = append(os.Environ(),
		"TERMA_CONFIG_DIR="+fx.cfgDir,
		"TERMA_STATE_DIR="+fx.stateDir,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-hitLogs:
	case <-time.After(10 * time.Second):
		close(releaseLogs)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("timed out waiting for upload to reach /v1/logs\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		close(releaseLogs)
		t.Fatalf("signal interrupt: %v", err)
	}
	close(releaseLogs)

	waitErr := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 130 {
		t.Fatalf("exit = %v, want exit code 130\nstdout:\n%s\nstderr:\n%s", waitErr, stdout.String(), stderr.String())
	}

	lines := streamLines(t, stdout.String())
	last := lines[len(lines)-1]
	if last["type"] != "summary" {
		t.Fatalf("last line = %v, want summary (types=%s)", last, streamTypes(lines))
	}
	var sum history.Summary
	if err := json.Unmarshal([]byte(summaryOf(t, stdout.String())), &sum); err != nil {
		t.Fatal(err)
	}
	if !sum.Interrupted {
		t.Errorf("summary.Interrupted = false, want true\nstdout:\n%s", stdout.String())
	}
}

// A reader that never drains stdout must not keep upload.lock held after an interrupt:
// once signalled, the process gives up on blocked stdout writes, releases the lock, and exits.
func TestHistoryUploadJSONStreamBlockedStdoutReleasesLock(t *testing.T) {
	bin := termaBinary(t)
	fx := newHistoryFixture(t)
	saveFileBackedSignIn(t, fx)

	// Create enough sessions so the stream lines (plan + session events) overflow the OS
	// pipe buffer (~64 KiB) and block stdout writes while upload.lock is held.
	oldTime := time.Now().UTC().Add(-4 * time.Hour)
	projectDir := filepath.Dir(fx.ccPath)
	for i := range 250 {
		sid := fmt.Sprintf("33333333-3333-3333-3333-%012d", i)
		p := filepath.Join(projectDir, sid+".jsonl")
		content := strings.Join([]string{
			`{"type":"user","uuid":"u1","promptId":"p1","sessionId":"` + sid + `","cwd":` + jsonQuote(fx.repoA) + `,"version":"2.1.301","timestamp":"` + oldTime.Format(time.RFC3339Nano) + `","message":{"role":"user","content":"blocked pipe test prompt"}}`,
			`{"type":"assistant","uuid":"a1","sessionId":"` + sid + `","cwd":` + jsonQuote(fx.repoA) + `,"version":"2.1.301","timestamp":"` + oldTime.Add(time.Second).Format(time.RFC3339Nano) + `","requestId":"req_1","message":{"id":"msg_1","role":"assistant","model":"claude-opus-5-5","stop_reason":"end_turn","content":[{"type":"text","text":"blocked pipe test reply"}],"usage":{"input_tokens":10,"output_tokens":5}}}`,
		}, "\n") + "\n"
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, oldTime, oldTime)
	}

	// Open an OS pipe whose read end is never read.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "history", "upload", "--json-stream")
	cmd.Env = append(os.Environ(),
		"TERMA_CONFIG_DIR="+fx.cfgDir,
		"TERMA_STATE_DIR="+fx.stateDir,
	)
	cmd.Stdout = pw
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		t.Fatal(err)
	}
	// Close the parent's copy of the write end so only the child holds it.
	_ = pw.Close()

	// Wait until the child holds upload.lock and stalls on the full stdout pipe (uploads
	// stop advancing before all 250 sessions complete).
	deadline := time.Now().Add(10 * time.Second)
	stalled := false
	prevSent := -1
	stableTicks := 0
	for time.Now().Before(deadline) {
		unlock, lockErr := history.LockUpload(fx.stateDir, fx.account, config.DefaultProfile)
		if errors.Is(lockErr, history.ErrUploadRunning) {
			sent := fx.receivedBy("team-a")
			if sent > 0 && sent < 250 {
				if sent == prevSent {
					stableTicks++
					if stableTicks >= 4 {
						stalled = true
						break
					}
				} else {
					prevSent = sent
					stableTicks = 0
				}
			}
		} else if lockErr == nil {
			unlock()
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !stalled {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("child did not stall on unread stdout pipe (sent=%d); stderr=%q", fx.receivedBy("team-a"), stderr.String())
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal interrupt: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case waitErr := <-done:
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 130 {
			t.Fatalf("exit = %v, want 130; stderr=%q", waitErr, stderr.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("process hung on unread stdout pipe after SIGINT; stderr=%q", stderr.String())
	}

	// Lock must be released immediately after exit.
	unlock, err := history.LockUpload(fx.stateDir, fx.account, config.DefaultProfile)
	if err != nil {
		t.Fatalf("upload.lock still held after interrupted exit: %v", err)
	}
	unlock()
}
