package hookrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// With Personal, which collects everything outside git, ticked beside an organization's
// team, the commands an agent runs all day in the organization's repository do not cross
// into Personal: a redirect to /dev/null, a listing of /tmp, a scratch file. A device or
// the machine's scratch space is no one's workspace (nobodys). What is someone's still
// crosses: another organization's checkout, though it sits in scratch space as every
// checkout here does, and a folder of the developer's home, read or written.
func TestRoutedCommonCommandsDoNotCrossIntoPersonal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell commands and device paths")
	}
	stateDir := t.TempDir()
	repoA := namedRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	repoB := namedRepo(t, "beta", "git@github.com:orgB/beta.git")
	if err := os.WriteFile(filepath.Join(repoB, "secret.go"), []byte("package b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	beta, err := filepath.Rel(repoA, repoB)
	if err != nil {
		t.Fatal(err)
	}
	home := resolved(t.TempDir())
	t.Setenv("HOME", home)
	diary := filepath.Join(home, "notes", "diary.txt")
	if err := os.MkdirAll(filepath.Dir(diary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diary, []byte("private\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Scratch space, as the machine names it: a folder and a file in it, outside any checkout.
	scratch := resolved(t.TempDir())
	scratchLog := filepath.Join(scratch, "build.log")
	if err := os.WriteFile(scratchLog, []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	personal := makeTeam("orgPersonal", "team-personal", nil, false)
	personal.Personal = true
	personal.Policy.Mode = config.ModeGlobal
	orgA, orgB := makeTeam("orgA", "team-a", []string{"github.com/orgA/alpha"}, true), makeTeam("orgB", "team-b", []string{"github.com/orgB/beta"}, true)
	withPersonal := routedState(t, stateDir, testAccountA, []routing.Team{orgA, orgB, personal})
	orgOnly := routedState(t, t.TempDir(), testAccountA, []routing.Team{orgA, orgB})

	ctx, n := context.Background(), 0
	// kind is the session's claim after one tool call in a new run in repoA reached paths.
	kind := func(rt *Routing, reach func(env Env, sess session.Session)) claim.Kind {
		t.Helper()
		n++
		sid := fmt.Sprintf("018f3a2c-00a0-7000-8000-%012d", n)
		env, _, _ := routedEnv(t, stateDir, repoA, testAccountA, rt)
		env.StartRun(sid)
		reach(env, session.Session{ID: sid, Tool: "claude_code"})
		c, _ := claim.Read(stateDir, sid, env.Time())
		return c.Kind
	}
	shell := func(cmd string) func(Env, session.Session) {
		return func(env Env, sess session.Session) {
			env.Classify(ctx, nil, sess, Alone(ShellPaths(cmd, repoA)...), nil)
		}
	}
	read := func(path string) func(Env, session.Session) {
		return func(env Env, sess session.Session) { env.Classify(ctx, nil, sess, []string{path}, nil) }
	}
	edit := func(path string) func(Env, session.Session) {
		return func(env Env, sess session.Session) {
			r, err := env.Repo(ctx)
			if err != nil {
				t.Fatal(err)
			}
			env.Touch(r, sess, "Write", []string{path}, nil)
		}
	}

	stays := map[string]func(Env, session.Session){
		"go build ./... 2>/dev/null":                           nil,
		"git status >/dev/null 2>&1":                           nil,
		"ls /tmp":                                              nil,
		"go test ./...":                                        nil,
		"npm test 2>&1 | tail -20":                             nil,
		"grep -rn TODO . 2>/dev/null | head -5":                nil,
		"make build &>/dev/null":                               nil,
		"cat /dev/null > out.txt":                              nil,
		"go vet ./... > /tmp/vet.log 2>&1":                     nil,
		"cat /tmp/vet.log":                                     nil,
		"find /tmp -maxdepth 1 -name '*.log'":                  nil,
		"head -c 16 /dev/urandom | xxd":                        nil,
		"cat /proc/cpuinfo":                                    nil,
		"ls " + scratch:                                        nil,
		"cat " + scratchLog:                                    nil,
		"go test ./... >> " + scratchLog + " 2>&1":             nil,
		"ls " + os.TempDir():                                   nil,
		"Read tool: a scratch file":                            read(scratchLog),
		"Read tool: /dev/null":                                 read("/dev/null"),
		"Write tool: a scratch file":                           edit(filepath.Join(scratch, "notes.md")),
		"Write tool: a file of the repository":                 edit(filepath.Join(repoA, "a.go")),
		"cat " + filepath.Join(repoA, "a.go") + " 2>/dev/null": nil,
	}
	crosses := map[string]func(Env, session.Session){
		"cat " + beta + "/secret.go":                 nil,
		"cat " + beta + "/secret.go 2>/dev/null":     nil,
		"go build ./... > " + beta + "/build.log":    nil,
		"cat ~/notes/diary.txt":                      nil,
		"cat " + diary + " 2>/dev/null":              nil,
		"ls ~/notes > /dev/null":                     nil,
		"echo done >> ~/notes/diary.txt":             nil,
		"Read tool: a file of the home folder":       read(diary),
		"Write tool: a file of the home folder":      edit(filepath.Join(home, "notes", "new.txt")),
		"Read tool: another organization's checkout": read(filepath.Join(repoB, "secret.go")),
	}
	for name, reach := range stays {
		if reach == nil {
			reach = shell(name)
		}
		if got := kind(withPersonal, reach); got == claim.KindMixed {
			t.Errorf("Personal + org A: %q marked the session %s", name, got)
		}
		if got := kind(orgOnly, reach); got == claim.KindMixed {
			t.Errorf("org A only: %q marked the session %s", name, got)
		}
	}
	for name, reach := range crosses {
		if reach == nil {
			reach = shell(name)
		}
		if got := kind(withPersonal, reach); got != claim.KindMixed {
			t.Errorf("Personal + org A: %q left the session %q, want mixed", name, got)
		}
		// Without Personal nothing collects the home folder, so only another organization crosses.
		if got, want := kind(orgOnly, reach), strings.Contains(name, "beta") || strings.Contains(name, "another organization"); (got == claim.KindMixed) != want {
			t.Errorf("org A only: %q left the session %q, want mixed %v", name, got, want)
		}
	}
}

// An edit of a scratch file is reported to no team: outside git, only a folder that may be
// the developer's own is personal's to collect. An edit in their home still is.
func TestRoutedScratchEditsAreReportedToNoTeam(t *testing.T) {
	stateDir := t.TempDir()
	repoA := namedRepo(t, "alpha", "git@github.com:orgA/alpha.git")
	home, scratch := resolved(t.TempDir()), resolved(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	personal := makeTeam("orgPersonal", "team-personal", nil, false)
	personal.Personal = true
	personal.Policy.Mode = config.ModeGlobal
	rt := routedState(t, stateDir, testAccountA, []routing.Team{makeTeam("orgA", "team-a", []string{"github.com/orgA/alpha"}, true), personal})
	personalEnrollment := rt.state.Teams[1].EnrollmentID
	touchesFor := func(sid, path string) (personals int) {
		t.Helper()
		env, sp, _ := routedEnv(t, stateDir, repoA, testAccountA, rt)
		env.StartRun(sid)
		r, err := env.Repo(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		env.Touch(r, session.Session{ID: sid, Tool: "claude_code"}, "Write", []string{path}, nil)
		events, err := sp.Peek(100)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			if e.SessionID == sid && e.Name == semconv.TermaFilesTouchedEvent && e.Enrollment == personalEnrollment {
				personals++
			}
		}
		return personals
	}
	if n := touchesFor("018f3a2c-00b0-7000-8000-000000000001", filepath.Join(scratch, "notes.md")); n != 0 {
		t.Errorf("an edit of a scratch file was reported to personal %d times", n)
	}
	if n := touchesFor("018f3a2c-00b0-7000-8000-000000000002", filepath.Join(home, "notes.md")); n != 1 {
		t.Errorf("an edit in the developer's home was reported to personal %d times, want 1", n)
	}
}

func TestNobodysFolders(t *testing.T) {
	home := resolved(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	tmp := resolved(os.TempDir())
	for dir, want := range map[string]bool{
		tmp: true, filepath.Join(tmp, "build", "out"): true, os.TempDir(): true,
		home: false, filepath.Join(home, "notes"): false, filepath.Dir(tmp) + "x": false,
	} {
		if got := nobodys(dir); got != want {
			t.Errorf("nobodys(%q) = %v, want %v", dir, got, want)
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	for dir, want := range map[string]bool{
		"/": true, "/dev": true, "/dev/fd": true, "/proc/self": true, "/sys/kernel": true, "/tmp": true, "/tmp/x/y": true, "/var/tmp": true,
		"/etc": true, "/etc/ssl": true, "/usr/local/bin": true, "/bin": true,
		"/devices": false, "/tmpfiles": false, "/etcetera": false, "/srv/work": false, "/opt/work": false, "/home": false, "/Users": false, "/mnt/data": false,
	} {
		if got := nobodys(dir); got != want {
			t.Errorf("nobodys(%q) = %v, want %v", dir, got, want)
		}
	}
	// A home that is the filesystem's root owns no device and no scratch space.
	t.Setenv("HOME", "/")
	if !nobodys("/dev") || !nobodys("/tmp/x") {
		t.Errorf("with HOME=/: nobodys(/dev) = %v, nobodys(/tmp/x) = %v, want both", nobodys("/dev"), nobodys("/tmp/x"))
	}
	// A home under one of the system's folders (FreeBSD's /usr/home) is still the developer's.
	t.Setenv("HOME", "/usr/home/dev")
	if nobodys("/usr/home/dev/notes") || !nobodys("/usr/bin") || !nobodys("/dev/fd") {
		t.Errorf("with HOME=/usr/home/dev: nobodys(home's notes) = %v, nobodys(/usr/bin) = %v, nobodys(/dev/fd) = %v", nobodys("/usr/home/dev/notes"), nobodys("/usr/bin"), nobodys("/dev/fd"))
	}
}
