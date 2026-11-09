package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// desktopTeam is the sandbox's team as Terma Desktop names it, by id.
const desktopTeam = "55555555-5555-4555-8555-555555555555"

// TestDesktopGrant is Terma Desktop's onboarding as the CLI sees it: the real binary, its
// stdin and stdout as pipes, and the account fixture's delegate and token endpoints, which
// follow the platform's contract (POST /v1/auth/cli/delegate rotates the desktop's refresh
// token and binds a single-use code to the CLI's challenge; /v1/auth/cli/token redeems it
// with redirect_port 0 and the verifier). The desktop writes its identity, reads the
// challenge, delegates and writes the code; the CLI signs itself in and sets up, and
// doctor says the sign-in came from Terma Desktop. A second run reuses it. The same run
// against dev, with a desktop session's own refresh token, waits on the platform's
// delegate endpoint there.
func TestDesktopGrant(t *testing.T) {
	track(t)
	sb := New(t, Isolated, WithProject(desktopTeam))
	// The CLI starts without a sign-in of its own.
	if err := os.Remove(filepath.Join(sb.TermaConfig, "credentials.json")); err != nil {
		t.Fatal(err)
	}

	first, rest := sb.desktopSetup(func(request map[string]any) string {
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
	})
	if _, ok := first["challenge"]; !ok || sb.account.delegations.Load() != 1 {
		t.Fatalf("first line = %v after %d delegations", first, sb.account.delegations.Load())
	}
	if strings.Contains(rest, "ter_cod_") || strings.Contains(rest, "ter_clr_") {
		t.Fatalf("setup printed a code or a token:\n%s", rest)
	}
	creds, err := os.ReadFile(filepath.Join(sb.TermaConfig, "credentials.json"))
	if err != nil || !strings.Contains(string(creds), "(via Terma Desktop)") || !strings.Contains(string(creds), accountUser) {
		t.Fatalf("credentials.json = %s, %v", creds, err)
	}
	if doctor := sb.doctor(); !strings.Contains(doctor, "signed in through Terma Desktop") {
		t.Fatalf("doctor does not name the desktop sign-in:\n%s", doctor)
	}

	// Onboarding again, or a switch back: the CLI's own sign-in serves, nothing is delegated.
	again, _ := sb.desktopSetup(func(map[string]any) string {
		t.Fatal("a stored sign-in for the same user and organization asked for a grant")
		return ""
	})
	if again["reused"] != true || sb.account.delegations.Load() != 1 {
		t.Fatalf("second run: first line %v, %d delegations", again, sb.account.delegations.Load())
	}
}

// desktopSetup runs `terma setup --desktop-grant` as Terma Desktop does: the identity on
// stdin, stdout read line by line, grant called with a request for the code to write
// back. It returns the first line and everything printed after it.
func (sb *Sandbox) desktopSetup(grant func(request map[string]any) string) (map[string]any, string) {
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
	if _, err := io.WriteString(stdin, `{"user_id":"`+accountUser+`","org_id":"`+accountOrg+`"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewReader(stdout)
	line, err := lines.ReadString('\n')
	var first map[string]any
	if err != nil || json.Unmarshal([]byte(line), &first) != nil {
		_ = cmd.Wait()
		t.Fatalf("stdout's first line %q is not JSON (%v); stderr:\n%s", line, err, stderr.String())
	}
	if _, asked := first["challenge"]; asked {
		code := grant(first)
		if _, err := io.WriteString(stdin, `{"code":"`+code+`"}`+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	_ = stdin.Close()
	rest, _ := io.ReadAll(lines)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("terma setup --desktop-grant: %v\n%s%s%s", err, line, rest, stderr.String())
	}
	if sb.waitRelay(5 * time.Second) {
		sb.StopRelay()
	}
	return first, string(rest) + stderr.String()
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
