package hookrun

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

// A push is reported once git push has exited, so the event can say whether the remote
// took it. pre-push runs before anything is sent and cannot wait, so it writes what it
// was given to a record under PushesDir and hands it to a detached AwaitPush; a record
// no AwaitPush finished is resolved by SweepPushes. pre-push itself starts no git.

// PushesDir holds, under the state directory, the pushes waiting to be reported.
const PushesDir = "pushes"

const (
	// MaxPushCommits bounds terma.push.commits; terma.push.commit.count stays the count.
	MaxPushCommits = 500
	// maxPushWalk bounds the history walk, and so terma.push.commit.count.
	maxPushWalk = 10000
	// maxPushSessions bounds terma.push.sessions and terma.push.session.ids.
	maxPushSessions = 100
	// maxPushInput bounds what pre-push reads: a line is about 150 bytes, one per ref.
	maxPushInput = 4 << 20
	// pushWait bounds how long a push is waited for, from when pre-push ran.
	pushWait = 10 * time.Minute
	// pushPoll is how often AwaitPush looks for git push to have exited.
	pushPoll = 250 * time.Millisecond
	// pushRetention is how long a record whose report failed is kept to retry.
	pushRetention = 24 * time.Hour
	// pushAbandoned is when SweepPushes takes over a record from its AwaitPush. A report
	// that itself outlasts it can be sent twice, which terma.push.id lets the platform count once.
	pushAbandoned = pushWait + 5*time.Minute
)

// PushRef is one line of pre-push's input: a ref git is about to update on the remote.
type PushRef struct {
	LocalRef  string `json:"local_ref"`
	LocalSHA  string `json:"local_sha"`
	RemoteRef string `json:"remote_ref"`
	RemoteSHA string `json:"remote_sha"`
	// Tracking is the remote-tracking ref that shows the push landed, "" when none can, and
	// TrackingBefore its commit id when pre-push ran, "" when it did not exist.
	Tracking       string `json:"tracking,omitempty"`
	TrackingBefore string `json:"tracking_before,omitempty"`
	// Reported marks a ref already spooled, when the record is kept to retry the others.
	Reported bool `json:"reported,omitempty"`
}

// ParsePushInput reads pre-push's input, keeping the branch updates terma reports: a
// deleted ref sends no commits, and a ref outside refs/heads is no branch. A line it
// cannot read is skipped.
func ParsePushInput(input string) []PushRef {
	var refs []PushRef
	for line := range strings.Lines(input) {
		f := strings.Split(strings.TrimRight(line, "\r\n"), " ")
		if len(f) != 4 || !gitx.ValidOID(f[1]) || !gitx.ValidOID(f[3]) || gitx.ZeroOID(f[1]) {
			continue
		}
		if !strings.HasPrefix(f[2], "refs/heads/") {
			continue
		}
		refs = append(refs, PushRef{LocalRef: f[0], LocalSHA: f[1], RemoteRef: f[2], RemoteSHA: f[3]})
	}
	return refs
}

// gitPushPID is git push's process: terma runs under the hook's shell, which git started.
var gitPushPID = func() int {
	if pids := claimPIDs(); len(pids) > 1 {
		return pids[1]
	}
	return 0
}

// pushRecord is a push waiting to be reported.
type pushRecord struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	// GitPID is git push's process, whose exit AwaitPush waits for; 0 when unknown.
	GitPID int    `json:"git_pid,omitempty"`
	Root   string `json:"root"`
	GitDir string `json:"git_dir"`
	// Remote is the configured remote pushed to, "" for a URL.
	Remote string    `json:"remote,omitempty"`
	Refs   []PushRef `json:"refs"`
	// Excluded are the remote's branches as its remote-tracking refs showed them before the
	// push: a new branch's commits are those none of them has.
	Excluded []string `json:"excluded,omitempty"`
	// Event is the stamped event every ref's report starts from.
	Event spool.Event `json:"event"`
}

// PrePush records a push for AwaitPush, which reports it once git push exits. It reads
// files only: the history walk waits for AwaitPush, after the push.
func PrePush(ctx context.Context, env Env) error {
	if len(env.Args) < 2 || env.AwaitPush == nil || env.Stdin == nil {
		return nil
	}
	r, err := env.Repo(ctx)
	if err != nil || r.GitDir == "" {
		return nil
	}
	input, err := io.ReadAll(io.LimitReader(env.Stdin, maxPushInput))
	if err != nil {
		return nil
	}
	refs := ParsePushInput(string(input))
	if len(refs) == 0 {
		return nil
	}
	// git names a push to a URL by the URL twice; a URL never goes out as a remote's name.
	// A remote can be named for its own location, so only one configured is a remote.
	remote, url := env.Args[0], env.Args[1]
	if remote == url && !gitx.RemoteFS(r.GitDir, remote) {
		remote = ""
	}
	rec := pushRecord{ID: rand.Text(), Time: env.Time(), Root: r.Root, GitDir: r.GitDir, Remote: remote, Refs: refs}
	rec.GitPID = gitPushPID()
	// A push to a URL has no remote's branches to go by: every remote's stand in.
	tracked := "refs/remotes/"
	if remote != "" {
		tracked += remote + "/"
	}
	branches, _ := gitx.RefsFS(r.GitDir, tracked)
	rec.Excluded = slices.Compact(slices.Sorted(maps.Values(branches)))
	for i, ref := range rec.Refs {
		tracking, ok := gitx.TrackingRefFS(r.GitDir, remote, ref.RemoteRef)
		if !ok {
			continue
		}
		if before, readable := gitx.RefFS(r.GitDir, tracking); readable {
			rec.Refs[i].Tracking, rec.Refs[i].TrackingBefore = tracking, before
		}
	}
	attrs := map[string]any{}
	if remote != "" {
		attrs[semconv.TermaPushRemoteNameKey] = remote
	}
	if u := gitx.NormalizeRemote(url); u != "" {
		attrs[semconv.TermaPushRemoteURLKey] = u
	}
	if origin := gitx.RemoteURLFS(r.GitDir); origin != "" {
		attrs[semconv.VCSRepositoryURLFullKey] = origin
	}
	if root := r.workTree(); root != "" {
		attrs[semconv.TermaRepositoryRootKey] = root
	}
	vcsAttrs(attrs, r)
	// No relay claim: the sessions stamped into pushed commits did not make the push.
	rec.Event = env.stamp(r, spool.Event{Name: semconv.TermaPushEvent, Time: rec.Time, Attrs: attrs})
	path := filepath.Join(env.StateDir, PushesDir, rec.ID+".json")
	if err := config.WriteJSON(path, rec, 0o600); err != nil {
		env.Logf("record push: %v", err)
		return nil
	}
	env.AwaitPush(path)
	return nil
}

// AwaitPush waits for git push to exit, then reports the push recorded at path and
// starts a flush. Another process that has taken the record first reports it instead.
func AwaitPush(ctx context.Context, env Env, path string) {
	rec, err := readPush(path)
	if err != nil {
		return
	}
	for rec.GitPID > 0 && procinfo.Alive(rec.GitPID) && time.Since(rec.Time) < pushWait {
		select {
		case <-ctx.Done():
			return
		case <-time.After(pushPoll):
		}
	}
	reportPush(ctx, env, path, rec)
}

// SweepPushes reports the pushes whose AwaitPush never finished: one the machine slept
// through, or that was killed.
func SweepPushes(ctx context.Context, env Env) {
	dir := filepath.Join(env.StateDir, PushesDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil || e.IsDir() || time.Since(info.ModTime()) < pushAbandoned {
			continue
		}
		switch {
		case strings.HasSuffix(path, takenSuffix):
			// Taken and never finished: report it again, which terma.push.id makes harmless.
			if err := config.Rename(path, strings.TrimSuffix(path, takenSuffix)); err != nil {
				continue
			}
			path = strings.TrimSuffix(path, takenSuffix)
		case !strings.HasSuffix(path, ".json"):
			continue
		}
		rec, err := readPush(path)
		if err != nil {
			_ = config.Remove(path)
			continue
		}
		reportPush(ctx, env, path, rec)
	}
}

// takenSuffix marks a record a process is reporting: the rename is what makes it its own.
const takenSuffix = ".taken"

func readPush(path string) (pushRecord, error) {
	var rec pushRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, err
	}
	if rec.ID == "" || rec.Root == "" || rec.GitDir == "" {
		return rec, errors.New("incomplete push record")
	}
	return rec, nil
}

// reportPush spools one event per ref of the record at path, then removes it.
func reportPush(ctx context.Context, env Env, path string, rec pushRecord) {
	taken := path + takenSuffix
	if err := config.Rename(path, taken); err != nil {
		return // another process took it
	}
	// The sweep's clock starts at the take: a report still under way is not abandoned.
	now := time.Now()
	_ = os.Chtimes(taken, now, now)
	// git push has exited once its process has, or the wait ran out: only the first shows anything.
	exited := rec.GitPID > 0 && !procinfo.Alive(rec.GitPID)
	emitted, failed := false, false
	for i, ref := range rec.Refs {
		if ref.Reported {
			continue
		}
		if ev, ok := pushEvent(ctx, env, rec, i, ref, exited); ok && env.emit(ev) {
			emitted, rec.Refs[i].Reported = true, true
		} else {
			failed = true
		}
	}
	// A ref that could not be reported is retried by a later sweep, for a while; the record
	// written anew starts the sweep's clock again.
	if failed && time.Since(rec.Time) < pushRetention {
		if err := config.WriteJSON(path, rec, 0o600); err != nil {
			env.Logf("keep push record: %v", err)
		}
	}
	if err := config.Remove(taken); err != nil && !errors.Is(err, fs.ErrNotExist) {
		env.Logf("remove push record: %v", err)
	}
	if emitted && env.Flush != nil {
		env.Flush()
	}
}

// pushEvent is the report of one ref of a push; ok is false when its commits cannot be read.
func pushEvent(ctx context.Context, env Env, rec pushRecord, i int, ref PushRef, exited bool) (spool.Event, bool) {
	attrs := maps.Clone(rec.Event.Attrs)
	if attrs == nil {
		attrs = map[string]any{}
	}
	rng, exclude := semconv.TermaPushRangeNewBranch, rec.Excluded
	if rec.Remote == "" {
		rng = semconv.TermaPushRangeFallback
	}
	if !gitx.ZeroOID(ref.RemoteSHA) {
		attrs[semconv.TermaPushOldRevisionKey] = ref.RemoteSHA
		if gitx.HasCommit(ctx, rec.Root, ref.RemoteSHA) {
			rng, exclude = semconv.TermaPushRangeUpdate, []string{ref.RemoteSHA}
			if ancestor, ok := gitx.IsAncestor(ctx, rec.Root, ref.RemoteSHA, ref.LocalSHA); ok {
				attrs[semconv.TermaPushForcedKey] = !ancestor
			}
		} else {
			rng = semconv.TermaPushRangeFallback
		}
	}
	commits, err := gitx.Commits(ctx, rec.Root, ref.LocalSHA, exclude, maxPushWalk)
	if err != nil {
		env.Logf("pushed commits: %v", err)
		return spool.Event{}, false
	}
	shas, sessions := pushedLists(commits)
	ids := sessionIDs(sessions)
	status := semconv.TermaPushStatusUnknown
	if exited && ref.Tracking != "" && ref.TrackingBefore != ref.LocalSHA {
		if after, readable := gitx.RefFS(rec.GitDir, ref.Tracking); readable && after == ref.LocalSHA {
			status = semconv.TermaPushStatusTrackingRefUpdated
		}
	}
	attrs[semconv.TermaPushIDKey] = rec.ID + "-" + strconv.Itoa(i)
	attrs[semconv.TermaPushStatusKey] = status
	attrs[semconv.TermaPushLocalRefKey] = ref.LocalRef
	attrs[semconv.TermaPushRemoteRefKey] = ref.RemoteRef
	attrs[semconv.TermaPushNewRevisionKey] = ref.LocalSHA
	attrs[semconv.TermaPushRangeKey] = rng
	attrs[semconv.TermaPushCommitsKey] = shas
	attrs[semconv.TermaPushCommitCountKey] = len(commits)
	attrs[semconv.TermaPushCommitsTruncatedKey] = len(shas) < len(commits)
	attrs[semconv.TermaPushSessionIDsKey] = ids
	attrs[semconv.TermaPushSessionsKey] = sessionsAttr(sessions)
	ev := rec.Event
	ev.Attrs = attrs
	if len(ids) > 0 {
		ev.SessionID = ids[0]
	}
	return ev, true
}

// pushedLists are the commit ids terma.push.commits lists, at most MaxPushCommits, and the
// sessions stamped into those commits alone, each agent's session once, at most
// maxPushSessions. shas is never nil: an empty list is still the string[] the registry declares.
func pushedLists(commits []gitx.PushedCommit) (shas []string, sessions []trailer.Trailer) {
	listed := commits[:min(len(commits), MaxPushCommits)]
	shas = make([]string, 0, len(listed))
	var stamped []trailer.Trailer
	for _, c := range listed {
		shas = append(shas, c.SHA)
		for _, t := range trailer.Parse(c.Trailers, "") {
			if session.ValidID(t.SessionID) {
				stamped = append(stamped, t)
			}
		}
	}
	sessions = uniqueSessions(stamped)
	return shas, sessions[:min(len(sessions), maxPushSessions)]
}
