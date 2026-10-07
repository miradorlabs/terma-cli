package hookrun

import (
	"bufio"
	"context"
	"io"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

// MaxPushCommits bounds the commits and the refs one push reports, as MaxCommitFileStats
// bounds a commit's files.
const MaxPushCommits = 50

// pushRef is one line git's pre-push hook reads on stdin.
type pushRef struct {
	localRef, localSHA, remoteRef, remoteSHA string
}

// readPushRefs reads at most MaxPushCommits ref updates, leaving out deletions: a deleted
// ref sends no commit.
func readPushRefs(r io.Reader) []pushRef {
	var refs []pushRef
	sc := bufio.NewScanner(r)
	for sc.Scan() && len(refs) < MaxPushCommits {
		f := strings.Fields(sc.Text())
		if len(f) != 4 || zeroSHA(f[1]) {
			continue
		}
		refs = append(refs, pushRef{f[0], f[1], f[2], f[3]})
	}
	return refs
}

// zeroSHA is git's name for no object: a ref being deleted, or one the remote lacks.
func zeroSHA(sha string) bool { return strings.Trim(sha, "0") == "" }

// PrePush records which stamped commits a push is about to send, so a push git reports
// nowhere (-q, or output piped away) still says what reached the remote. git runs it as
// `pre-push <remote> <url>` with one line per ref on stdin. A push of no stamped commit
// sends nothing: there is no session to report it under.
func PrePush(ctx context.Context, env Env) error {
	if len(env.Args) < 2 || env.Stdin == nil {
		return nil
	}
	refs := readPushRefs(env.Stdin)
	if len(refs) == 0 {
		return nil
	}
	r, err := env.Repo(ctx)
	if err != nil || r.GitDir == "" {
		return nil
	}
	remote, url := env.Args[0], env.Args[1]
	if remote == url {
		remote = "" // pushed to a URL: no remote-tracking refs say what it has
	}
	var tips, have []string
	for _, ref := range refs {
		tips = append(tips, ref.localSHA)
		if !zeroSHA(ref.remoteSHA) {
			have = append(have, ref.remoteSHA)
		}
	}
	// One past the bound tells a full list from a cut one.
	commits, err := gitx.PushedCommits(ctx, r.Root, remote, tips, have, MaxPushCommits+1)
	if err != nil {
		env.Logf("pushed commits: %v", err)
		return nil
	}
	truncated := len(commits) > MaxPushCommits
	commits = commits[:min(len(commits), MaxPushCommits)]
	comment := gitx.CommentCharFS(r.GitDir)
	var sessions []string
	tool := ""
	shas := make([]string, 0, len(commits))
	for _, c := range commits {
		shas = append(shas, c.SHA)
		for _, t := range trailer.Parse(c.Message, comment) {
			if !slices.Contains(sessions, t.SessionID) {
				sessions = append(sessions, t.SessionID)
			}
			if tool == "" {
				tool = t.Tool
			}
		}
	}
	if len(sessions) == 0 {
		return nil
	}
	attrs := map[string]any{
		semconv.TermaPushSessionIDsKey: sessions, semconv.GenAIMainAgentNameKey: tool,
		semconv.TermaPushCommitShasKey: shas, semconv.TermaPushCommitCountKey: len(shas),
		semconv.TermaPushCommitShasTruncatedKey: truncated, semconv.TermaPushRefsKey: refMaps(refs),
	}
	if remote != "" {
		attrs[semconv.TermaPushRemoteNameKey] = remote
	}
	if root := r.workTree(); root != "" {
		attrs[semconv.TermaRepositoryRootKey] = root
	}
	// Name the repository pushed to, which need not be origin.
	if full := gitx.NormalizeRemote(url); full != "" {
		attrs[semconv.VCSRepositoryURLFullKey] = full
		if o, ok := gitx.ParseOrigin(gitx.RepositoryID(url)); ok {
			attrs[semconv.VCSOwnerNameKey], attrs[semconv.VCSRepositoryNameKey] = o.Owner, o.Name
			if o.Provider != "" {
				attrs[semconv.VCSProviderNameKey] = o.Provider
			}
		}
	}
	env.EmitFor(r, spool.Event{Name: semconv.TermaPushEvent, SessionID: sessions[0], Attrs: attrs})
	return nil
}

func refMaps(refs []pushRef) []map[string]any {
	out := make([]map[string]any, 0, len(refs))
	for _, ref := range refs {
		out = append(out, map[string]any{
			"local_ref": ref.localRef, "local_sha": ref.localSHA, "remote_ref": ref.remoteRef, "remote_sha": ref.remoteSHA,
		})
	}
	return out
}
