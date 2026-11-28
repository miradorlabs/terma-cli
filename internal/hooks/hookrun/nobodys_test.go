package hookrun

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
)

// With Personal ticked beside an organization's team, an edit of a scratch file is no
// one's (nobodys) and reported to no team, while one in the developer's home is Personal's.
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
