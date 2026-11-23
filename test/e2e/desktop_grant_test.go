package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// desktopTeam is the sandbox's team as Terma Desktop names it, by id; anotherUser is a
// desktop signed in as someone the account's codes do not sign in.
const (
	desktopTeam = "55555555-5555-4555-8555-555555555555"
	anotherUser = "99999999-9999-4999-8999-999999999999"
)

// TestDesktopGrant is Terma Desktop's onboarding as the CLI sees it: the real binary, its
// stdin and stdout as pipes, and the account fixture's delegate and token endpoints, which
// follow the platform's contract (POST /v1/auth/user/delegate rotates the desktop user
// session's refresh token and binds a single-use code to the CLI's challenge;
// /v1/auth/user/token redeems it with redirect_port 0, the installation's id and the
// verifier, for a user session of the CLI's own in user-credentials.json). The desktop
// writes its identity, reads the challenge, delegates and writes the code; the CLI signs
// itself in and sets up in --org, and doctor says the sign-in came from Terma Desktop. A
// second run reuses it. The same run against dev, with a desktop session's own refresh
// token, waits on the platform's delegate endpoint there.
func TestDesktopGrant(t *testing.T) {
	track(t)
	sb := New(t, Isolated, WithProject(desktopTeam))
	// The CLI starts without a sign-in of its own, or an organization: setup records --org.
	credsPath := filepath.Join(sb.TermaConfig, "user-credentials.json")
	if err := os.Remove(credsPath); err != nil {
		t.Fatal(err)
	}
	sb.editProfile("default", func(p map[string]any) { delete(p, "organization_id") })

	delegate := func(request map[string]any) string {
		challenge, _ := request["challenge"].(string)
		label, _ := request["label"].(string)
		if len(challenge) != 43 || !strings.HasSuffix(label, " (via Terma Desktop)") {
			t.Fatalf("request = %v, want a challenge and the desktop label", request)
		}
		code, err := sb.account.Delegate(challenge, label)
		if err != nil {
			t.Fatal(err)
		}
		return code
	}

	// A desktop whose session may not delegate (NOT_DELEGABLE: sign in on the desktop
	// again) has no code to give and closes stdin: the CLI ends with grant_no_code, as the
	// desktop's fixture pins it.
	sb.account.RefuseDelegation("NOT_DELEGABLE")
	_, rest, err := sb.desktopSetup(accountUser, func(request map[string]any) string {
		challenge, _ := request["challenge"].(string)
		label, _ := request["label"].(string)
		if _, err := sb.account.Delegate(challenge, label); err == nil || !strings.Contains(err.Error(), "403 PERMISSION_DENIED NOT_DELEGABLE") {
			t.Fatalf("delegate for a session that may not delegate: %v", err)
		}
		return ""
	})
	if code, ok := exitCode(err); !ok || code != 1 || !strings.Contains(rest, `{"error":{"code":"grant_no_code"`) {
		t.Fatalf("NOT_DELEGABLE: exit %v, output:\n%s", err, rest)
	}
	sb.account.RefuseDelegation("")

	// A code that signs in someone other than the desktop's user is refused as the desktop
	// reads it, stored nowhere, and that session is signed out.
	first, rest, err := sb.desktopSetup(anotherUser, delegate)
	if code, ok := exitCode(err); !ok || code != 1 || !strings.Contains(rest, `{"error":{"code":"grant_identity_mismatch"`) {
		t.Fatalf("another user's code: first line %v, exit %v, output:\n%s", first, err, rest)
	}
	if _, err := os.Stat(credsPath); !os.IsNotExist(err) {
		t.Fatalf("another user's sign-in was kept: %v", err)
	}
	if delegated, other := sb.account.RevokedDelegated(); delegated != 1 || other != 0 {
		t.Fatalf("another user's session was not signed out: %d delegated sessions and %d others revoked", delegated, other)
	}

	sb.Receiver.Reset()
	first, rest, err = sb.desktopSetup(accountUser, delegate)
	if err != nil {
		t.Fatalf("terma setup --desktop-grant: %v\n%s", err, rest)
	}
	// Setup's check-in goes with the new sign-in to --team, naming this installation.
	beats := sb.Receiver.WaitLogs(10*time.Second, func(l LogRecord) bool { return l.EventName == "terma.relay.heartbeat" })
	if len(beats) == 0 {
		t.Fatal("setup's check-in reached no ingest")
	}
	if b := beats[0]; b.Authorization != personBearer() || b.Project != desktopTeam || b.Resource["terma.installation.id"] != accountInstallation {
		t.Errorf("setup's check-in went with %q naming team %q, resource %v", b.Authorization, b.Project, b.Resource)
	}
	if _, ok := first["challenge"]; !ok || sb.account.delegations.Load() != 2 {
		t.Fatalf("first line = %v after %d delegations", first, sb.account.delegations.Load())
	}
	for _, secret := range []string{userCodePrefix, userRefreshPrefix, userAccessPrefix} {
		if strings.Contains(rest, secret) {
			t.Fatalf("setup printed a code or a token:\n%s", rest)
		}
	}
	creds, err := os.ReadFile(credsPath)
	if err != nil || !strings.Contains(string(creds), "(via Terma Desktop)") || !strings.Contains(string(creds), accountUser) ||
		!strings.Contains(string(creds), desktopSession) || strings.Contains(string(creds), accountOrg) {
		t.Fatalf("user-credentials.json = %s, %v", creds, err)
	}
	if org := sb.profileOrganization("default"); org != accountOrg {
		t.Fatalf("config.json's profile is in organization %q, want --org %s", org, accountOrg)
	}
	if doctor := sb.doctor(); !strings.Contains(doctor, "signed in through Terma Desktop") {
		t.Fatalf("doctor does not name the desktop sign-in:\n%s", doctor)
	}

	// Onboarding again, or a switch back: the CLI's own sign-in serves, nothing is delegated.
	again, rest, err := sb.desktopSetup(accountUser, func(map[string]any) string {
		t.Fatal("a stored sign-in for the same user asked for a grant")
		return ""
	})
	if err != nil || again["reused"] != true || sb.account.delegations.Load() != 2 {
		t.Fatalf("second run: first line %v, %d delegations, %v\n%s", again, sb.account.delegations.Load(), err, rest)
	}
}

// exitCode is a finished command's exit code, ok false when it did not run to an exit.
func exitCode(err error) (int, bool) {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return 0, false
	}
	return exit.ExitCode(), true
}

// desktopSetup runs `terma setup --desktop-grant` as Terma Desktop does: the identity of
// the desktop's user on stdin, stdout read line by line, grant called with a request for
// the code to write back. It returns the first line, everything printed after it (stdout,
// then stderr) and how the command exited.
func (sb *Sandbox) desktopSetup(user string, grant func(request map[string]any) string) (map[string]any, string, error) {
	t := sb.T
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sb.Terma, "setup", "-y", "--desktop-grant", "--org", accountOrg, "--team", sb.ProjectID,
		"--harness", "claude", "--insecure-storage", "--relay-service", "off", "--relay-addr", sb.relayAddr)
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = sb.Repo
	cmd.Env = sb.termaEnv()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, `{"user_id":"`+user+`","org_id":"`+accountOrg+`"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewReader(stdout)
	line, err := lines.ReadString('\n')
	var first map[string]any
	if err != nil || json.Unmarshal([]byte(line), &first) != nil {
		_ = cmd.Wait()
		t.Fatalf("stdout's first line %q is not JSON (%v); stderr:\n%s", line, err, stderr.String())
	}
	// A grant with no code to give writes none: closing stdin says so.
	if _, asked := first["challenge"]; asked {
		if code := grant(first); code != "" {
			if _, err := io.WriteString(stdin, `{"code":"`+code+`"}`+"\n"); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = stdin.Close()
	rest, _ := io.ReadAll(lines)
	err = cmd.Wait()
	if err == nil && sb.waitRelay(5*time.Second) {
		sb.StopRelay()
	}
	return first, string(rest) + stderr.String(), err
}

// doctor is `terma doctor`'s output, whatever its verdict.
func (sb *Sandbox) doctor() string {
	sb.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sb.Terma, "doctor")
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = sb.Repo
	cmd.Env = sb.termaEnv()
	out, _ := cmd.CombinedOutput()
	return string(out)
}
