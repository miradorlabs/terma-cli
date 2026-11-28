package opencode

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestOpenCodeRefreshConnectDisconnectConcurrentLockSafety(t *testing.T) {
	h, path := opencodeIn(t)
	e := opencodeExporter(t, h, true)
	if err := h.Connect(e, false); err != nil {
		t.Fatalf("initial connect: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 60)
	for range 3 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for range 10 {
				if err := h.Connect(e, false); err != nil {
					errs <- err
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 10 {
				if _, _, err := h.RefreshPlugin(); err != nil {
					errs <- err
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 10 {
				if _, err := h.Disconnect(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent opencode connect/refresh/disconnect error: %v", err)
	}
	if _, err := h.Disconnect(); err != nil {
		t.Fatalf("final disconnect: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plugin still present after final disconnect: err=%v", err)
	}
}

func TestOpenCodeHookRunnerKillsTimedOutChildBeforeNextHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell hook test")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "hung.pid")
	statusFile := filepath.Join(dir, "next.status")
	hook := filepath.Join(dir, "hook.sh")
	hookSrc := "#!/bin/sh\n" +
		"if [ \"$1\" = \"opencode-session-start\" ]; then\n" +
		"  echo $$ > " + strconv.Quote(pidFile) + "\n" +
		"  exec sleep 30\n" +
		"fi\n" +
		"if [ \"$1\" = \"opencode-prompt\" ]; then\n" +
		"  pid=$(cat " + strconv.Quote(pidFile) + " 2>/dev/null || true)\n" +
		"  if [ -n \"$pid\" ] && kill -0 \"$pid\" 2>/dev/null; then\n" +
		"    echo alive > " + strconv.Quote(statusFile) + "\n" +
		"  else\n" +
		"    echo dead > " + strconv.Quote(statusFile) + "\n" +
		"  fi\n" +
		"fi\n"
	if err := os.WriteFile(hook, []byte(hookSrc), 0o700); err != nil {
		t.Fatal(err)
	}
	src, err := renderPlugin(opencodeConfig{
		Version:     1,
		Endpoint:    "http://127.0.0.1:43180",
		Signals:     []string{"traces"},
		HookCommand: []string{hook},
	})
	if err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(dir, "terma.mjs")
	if err := os.WriteFile(pluginPath, src, 0o600); err != nil {
		t.Fatal(err)
	}
	driver := `
import { pathToFileURL } from "node:url"
const mod = await import(pathToFileURL(process.argv[2]).href)
const hooks = await mod.default.server({ directory: "/repoA", worktree: "/repoA" })
` + opencodeDriverHelpers + `
hooks.event(created("ses_hang"))
await hooks["chat.message"]({ sessionID: "ses_hang" }, { message: { id: "msg_1" }, parts: [{ type: "text", text: "hi" }] })
await hooks.dispose()
`
	script := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(script, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, script, pluginPath).CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	got, err := os.ReadFile(statusFile)
	if err != nil {
		t.Fatalf("read statusFile: %v", err)
	}
	if strings.TrimSpace(string(got)) != "dead" {
		t.Fatalf("previous hung hook status when next hook started = %q; want dead", strings.TrimSpace(string(got)))
	}
}

// opencodeDriverHelpers builds the OpenCode 1 events the drivers feed the plugin: a
// session's record, a tool call's part once it ended, and a completed model call.
const opencodeDriverHelpers = `
const now = Date.now()
const created = (id, parentID) => ({ event: { type: "session.created", properties: { info: { id, parentID, directory: "/repoA", version: "1.19.6", title: "New session - 2026-01-01T00:00:00.000Z", time: { created: now - 1000 } } } } })
const toolDone = (sessionID, callID, tool, input, output) => ({ event: { type: "message.part.updated", properties: { part: { id: "prt_" + callID, sessionID, messageID: "msg_" + sessionID, callID, type: "tool", tool, state: { status: "completed", input, output, title: tool, metadata: {}, time: { start: now - 20, end: now - 10 } } } } } })
const assistantDone = (sessionID, id, modelID) => ({ event: { type: "message.updated", properties: { info: { id, sessionID, role: "assistant", parentID: "msg_user", modelID, time: { created: now - 10, completed: now } } } } })
`
