package hookrun

import (
	"context"
	"os"
	"slices"
	"time"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/semconv"
	"github.com/miradorlabs/terma-cli/internal/session"
	"github.com/miradorlabs/terma-cli/internal/spool"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

// PrepareCommitMsg stamps a trailer for every session whose manifest meets the staged
// files; local state only, under 50 ms.
func PrepareCommitMsg(ctx context.Context, env Env) error {
	if len(env.Args) == 0 {
		return nil
	}
	msgPath := env.Args[0]
	source := ""
	if len(env.Args) > 1 {
		source = env.Args[1]
	}
	switch source {
	case "merge", "squash":
		return nil
	}
	r, err := env.Repo(ctx)
	if err != nil || r.GitDir == "" {
		return nil
	}
	env.Cwd = "" // git runs its hooks at the checkout's root, not where the agent works: keep its directory
	manifests, err := r.Store.Manifests()
	if err != nil {
		env.Logf("manifests: %v", err)
	}
	if len(manifests) == 0 {
		return nil // nothing could be attributed; skip the git call entirely
	}
	staged, err := gitx.StagedFiles(ctx, r.Root)
	if err != nil {
		env.Logf("staged files: %v", err)
		return nil
	}
	attributions := session.Attribute(staged, manifests)
	if len(attributions) == 0 {
		return nil
	}
	original, err := os.ReadFile(msgPath)
	if err != nil {
		env.Logf("read message: %v", err)
		return nil
	}
	trailers := make([]trailer.Trailer, 0, len(attributions))
	for _, a := range attributions {
		trailers = append(trailers, trailer.Trailer{SessionID: a.SessionID, Tool: a.Tool})
	}
	stamped, changed := trailer.Stamp(string(original), trailers, gitx.CommentCharFS(r.GitDir))
	if !changed {
		return nil
	}
	if err := os.WriteFile(msgPath, []byte(stamped), 0o644); err != nil {
		env.Logf("write message: %v", err)
		return nil
	}
	ids := sessionIDs(trailers)
	attrs := map[string]any{
		semconv.TermaCommitSessionIDsKey: ids, semconv.TermaCommitSessionsKey: sessionsAttr(trailers),
		semconv.TermaCommitSessionCountKey: len(trailers), semconv.TermaCommitStagedCountKey: len(staged),
	}
	if source != "" {
		attrs[semconv.TermaCommitMessageSourceKey] = source
	}
	vcsAttrs(attrs, r)
	env.EmitFor(r, spool.Event{Name: semconv.TermaCommitStampedEvent, SessionID: ids[0], Attrs: attrs})
	return nil
}

// MaxCommitFileStats bounds per-file detail so one sweeping commit cannot push the 16 MiB
// spool's oldest events out; terma.commit.file.count stays the true total.
const MaxCommitFileStats = 50

// PostCommit records the commit against its stamped sessions and retires its files from
// their manifests; an unstamped commit gets a count-only event.
func PostCommit(ctx context.Context, env Env) error {
	r, err := env.Repo(ctx)
	if err != nil || r.GitDir == "" {
		return nil
	}
	env.Cwd = "" // git runs its hooks at the checkout's root, not where the agent works: keep its directory
	head, err := gitx.LastCommit(ctx, r.Root)
	if err != nil || head.SHA == "" {
		return nil
	}
	stamped := uniqueSessions(trailer.Parse(head.Message, gitx.CommentCharFS(r.GitDir)))
	if len(stamped) == 0 {
		emitUnattributedCommit(env, r, head) // human-only commit: no manifest to retire
		return nil
	}
	files := head.Paths()
	keys := make([]session.Key, 0, len(stamped))
	for _, t := range stamped {
		agent, _ := t.Agent()
		if agent == "" {
			// Not terma's stamp, which always names the agent: no manifest of it to retire.
			env.Logf("session %s stamped with no %s", t.SessionID, trailer.KeyTool)
			continue
		}
		keys = append(keys, session.Key{Tool: agent, ID: t.SessionID})
	}
	// Read before Consume empties the manifests. A single stamped session still names its
	// files, since its commit can carry a hand edit too.
	owners := fileOwners(env, r.Store, keys)
	for _, k := range keys {
		if err := r.Store.Consume(k, files); err != nil {
			env.Logf("consume manifest: %v", err)
		}
	}
	attrs := commitAttrs(r, head)
	attrs[semconv.TermaCommitSessionIDsKey] = sessionIDs(stamped)
	attrs[semconv.TermaCommitSessionsKey] = sessionsAttr(stamped)
	attrs[semconv.TermaCommitSessionCountKey] = len(stamped)
	if agent, ok := soleAgent(stamped); ok {
		attrs[semconv.GenAIMainAgentNameKey] = agent
	}
	addFileStats(attrs, head.Files, owners)
	env.EmitFor(r, spool.Event{Name: semconv.TermaCommitEvent, SessionID: stamped[0].SessionID, Attrs: attrs})
	return nil
}

// uniqueSessions is each agent's session once, in stamping order: an id names a session
// only with its agent, and a version change is no new session.
func uniqueSessions(stamped []trailer.Trailer) []trailer.Trailer {
	type key struct{ agent, id string }
	seen := map[key]bool{}
	var out []trailer.Trailer
	for _, t := range stamped {
		agent, _ := t.Agent()
		if k := (key{agent, t.SessionID}); !seen[k] {
			seen[k] = true
			out = append(out, t)
		}
	}
	return out
}

// sessionIDs are the sessions' ids, each once, as the session.ids attributes list them.
func sessionIDs(sessions []trailer.Trailer) []string {
	ids := make([]string, 0, len(sessions))
	for _, t := range sessions {
		if !slices.Contains(ids, t.SessionID) {
			ids = append(ids, t.SessionID)
		}
	}
	return ids
}

// sessionsAttr is the sessions as terma.commit.sessions and terma.push.sessions list them:
// a session with no agent has no agent key, never a stand-in. Never nil, like the registry's array.
func sessionsAttr(sessions []trailer.Trailer) []map[string]any {
	out := make([]map[string]any, 0, len(sessions))
	for _, t := range sessions {
		e := map[string]any{"session_id": t.SessionID}
		if agent, version := t.Agent(); agent != "" {
			e["agent"] = agent
			if version != "" {
				e["agent_version"] = version
			}
		}
		out = append(out, e)
	}
	return out
}

// soleAgent is the agent of every session, when they share one: a mixed commit has none.
func soleAgent(sessions []trailer.Trailer) (string, bool) {
	agent, _ := sessions[0].Agent()
	for _, t := range sessions[1:] {
		if a, _ := t.Agent(); a != agent {
			return "", false
		}
	}
	return agent, agent != ""
}

// emitUnattributedCommit spools the coverage denominator: a commit's identity and size,
// never its file paths, since terma had no part in it. Merges and squashes are skipped
// as prepare-commit-msg skips them.
func emitUnattributedCommit(env Env, r *Repo, head gitx.Commit) {
	if head.IsMerge() || head.IsSquash() {
		return
	}
	env.EmitFor(r, spool.Event{Name: semconv.TermaCommitUnattributedEvent, Attrs: commitAttrs(r, head)})
}

// commitAttrs is a commit's identity and size, with no second git subprocess and no file names.
func commitAttrs(r *Repo, head gitx.Commit) map[string]any {
	attrs := map[string]any{semconv.VCSRefHeadRevisionKey: head.SHA, semconv.TermaCommitFileCountKey: len(head.Files)}
	// No diff means no totals, rather than zeros.
	if len(head.Files) > 0 {
		attrs[semconv.TermaCommitLinesAddedKey], attrs[semconv.TermaCommitLinesDeletedKey] = lineTotals(head.Files)
	}
	if remote := gitx.RemoteURLFS(r.GitDir); remote != "" {
		attrs[semconv.VCSRepositoryURLFullKey] = remote
	}
	if head.Branch != "" {
		attrs[semconv.VCSRefHeadNameKey], attrs[semconv.VCSRefHeadTypeKey] = head.Branch, semconv.VCSRefHeadTypeBranch
	}
	// The working tree is how the platform names a checkout (delivery withholds it with tool content).
	if root := r.workTree(); root != "" {
		attrs[semconv.TermaRepositoryRootKey] = root
	}
	vcsAttrs(attrs, r)
	return attrs
}

// vcsAttrs names the repository from its origin as admission read it, with no file read.
func vcsAttrs(attrs map[string]any, r *Repo) {
	o, ok := gitx.ParseOrigin(r.Repository.Origin)
	if !ok {
		return
	}
	attrs[semconv.VCSOwnerNameKey], attrs[semconv.VCSRepositoryNameKey] = o.Owner, o.Name
	if o.Provider != "" {
		attrs[semconv.VCSProviderNameKey] = o.Provider
	}
}

func lineTotals(stats []gitx.FileStat) (added, deleted int) {
	for _, f := range stats {
		added += f.Added
		deleted += f.Deleted
	}
	return added, deleted
}

// addFileStats attaches the commit's numstat delta, an upper bound on what an agent wrote;
// a binary file has no line counts, so it is never reported as zero lines changed.
func addFileStats(attrs map[string]any, stats []gitx.FileStat, owners map[string]session.Key) {
	if len(stats) == 0 {
		return
	}
	reported := stats
	if len(reported) > MaxCommitFileStats {
		reported = reported[:MaxCommitFileStats]
	}
	entries := make([]map[string]any, 0, len(reported))
	for _, f := range reported {
		e := map[string]any{"path": f.Path}
		// The stamped session whose manifest names the file; a hand edit beside an agent's has none.
		if k, ok := owners[session.Normalize(f.Path)]; ok {
			e["session_id"], e["agent"] = k.ID, k.Tool
		}
		if !f.Binary {
			e["lines_added"], e["lines_deleted"] = f.Added, f.Deleted
		}
		entries = append(entries, e)
	}
	attrs[semconv.TermaCommitFileStatsKey] = entries
	attrs[semconv.TermaCommitFileStatsReportedKey] = len(entries)
	attrs[semconv.TermaCommitFileStatsTruncatedKey] = len(entries) < len(stats)
}

// fileOwners maps each path to the stamped session that touched it last.
func fileOwners(env Env, store *session.Store, keys []session.Key) map[string]session.Key {
	manifests, err := store.Manifests()
	if err != nil {
		env.Logf("manifests: %v", err)
		return nil
	}
	owners := make(map[string]session.Key)
	touchedAt := make(map[string]time.Time)
	for _, m := range manifests {
		if !slices.Contains(keys, m.Key()) {
			continue
		}
		for f, at := range m.Files {
			// Manifests is oldest first, so a tie keeps the earlier session deterministically.
			if prev, seen := touchedAt[f]; seen && !at.After(prev) {
				continue
			}
			owners[f], touchedAt[f] = m.Key(), at
		}
	}
	return owners
}
