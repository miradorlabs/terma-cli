package e2e

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// Machine-wide state the apps read, changed for a scenario and put back after: managed
// preferences, the proxy's CA in the System keychain.
//
// Root on a developer's machine: the suite runs exactly two commands there as root,
// `sudo -n /usr/bin/eslogger` and lsof, which a sudoers rule allows alone (filetrace_eslogger.go),
// and changes nothing of the machine's. Everything else it would run as root (the desktop
// profiles' managed preferences, the files they remove, the proxy's CA in the System keychain)
// changes machine-wide state, and runs only on a throwaway runner: rootCommand is the one way
// to it, and refuses anywhere else.

// envAllowRootChanges opts a machine that is not a GitHub-hosted runner into changes as root:
// a throwaway VM's.
const envAllowRootChanges = "TERMA_E2E_ALLOW_ROOT_CHANGES"

// rootChangesAllowed says whether this machine may have its machine-wide state changed as root,
// or why not: only a GitHub-hosted runner (a fresh VM thrown away after the job), or a machine
// that says it is as throwaway (TERMA_E2E_ALLOW_ROOT_CHANGES=1).
func rootChangesAllowed() (bool, string) {
	switch {
	case os.Getenv(envAllowRootChanges) == "1":
		return true, ""
	case os.Getenv("GITHUB_ACTIONS") == "true" && os.Getenv("RUNNER_ENVIRONMENT") == "github-hosted":
		return true, ""
	}
	return false, "only a GitHub-hosted runner (GITHUB_ACTIONS=true, RUNNER_ENVIRONMENT=github-hosted) or a throwaway machine (" +
		envAllowRootChanges + "=1) has its machine-wide state changed as root; a developer's machine runs eslogger and lsof under sudo, nothing else"
}

// rootCommand is `sudo -n args…`, a change to machine-wide state, where rootChangesAllowed
// allows it: the one way the suite runs anything as root but eslogger and lsof.
func rootCommand(args ...string) (*exec.Cmd, error) {
	if ok, why := rootChangesAllowed(); !ok {
		return nil, fmt.Errorf("refusing sudo %s: %s", strings.Join(args, " "), why)
	}
	return desktopCmd("sudo", append([]string{"-n"}, args...)...), nil
}

// rootCmd is rootCommand for a test: one the machine refuses fails it, saying why.
func rootCmd(t testing.TB, args ...string) *exec.Cmd {
	t.Helper()
	cmd, err := rootCommand(args...)
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

// desktopCmd is a command the desktop profiles run of the machine's (open, osascript, and
// sudo through rootCommand alone), ended after 90 seconds: one that waits on a dialog nobody
// answers would hold the run.
func desktopCmd(name string, args ...string) *exec.Cmd {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	time.AfterFunc(90*time.Second, cancel)
	return exec.CommandContext(ctx, name, args...)
}

// sudoWrite writes data to path as root, and removes it with t.
func sudoWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	cmd := rootCmd(t, "sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1" && chmod 644 "$1"`, "sh", path)
	cmd.Stdin = bytes.NewReader(data)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write %s: %v: %s", path, err, out)
	}
	t.Cleanup(func() { _ = rootCmd(t, "rm", "-f", path).Run() })
}

// trustProxyCA has the machine trust the recording proxy's CA, as a company's TLS-inspecting
// proxy has its own trusted, so an app's own requests are read rather than refused; t takes
// it back.
func trustProxyCA(t *testing.T, p *EgressProxy) {
	t.Helper()
	const keychain = "/Library/Keychains/System.keychain"
	if out, err := rootCmd(t, "security", "add-trusted-cert", "-d", "-r", "trustRoot", "-k", keychain, p.CAFile).CombinedOutput(); err != nil {
		t.Fatalf("trust the proxy's CA: %v: %s", err, out)
	}
	t.Cleanup(func() {
		_ = rootCmd(t, "security", "remove-trusted-cert", "-d", p.CAFile).Run()
		_ = rootCmd(t, "security", "delete-certificate", "-c", egressCAName, keychain).Run()
	})
}

// plistXML is a property list of string and boolean values.
func plistXML(values map[string]any) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n" +
		`<plist version="1.0">` + "\n<dict>\n")
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range values {
			if !yield(k) {
				return
			}
		}
	}) {
		b.WriteString("\t<key>")
		_ = xml.EscapeText(&b, []byte(k))
		b.WriteString("</key>\n")
		switch v := values[k].(type) {
		case bool:
			if v {
				b.WriteString("\t<true/>\n")
			} else {
				b.WriteString("\t<false/>\n")
			}
		default:
			b.WriteString("\t<string>")
			_ = xml.EscapeText(&b, []byte(fmt.Sprint(v)))
			b.WriteString("</string>\n")
		}
	}
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

// egressCAName is the recording proxy's CA's name, as a keychain finds it.
const egressCAName = "terma e2e egress CA"
