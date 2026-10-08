package hookrun

import (
	"context"
	"os"
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
	ids := make([]string, 0, len(attributions))
	for _, a := range attributions {
		ids = append(ids, a.SessionID)
	}
	attrs := map[string]any{
		semconv.TermaCommitSessionIDsKey: ids, semconv.TermaCommitSessionCountKey: len(ids), semconv.TermaCommitStagedCountKey: len(staged),
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
	stamped := trailer.Parse(head.Message, gitx.CommentCharFS(r.GitDir))
	if len(stamped) == 0 {
		emitUnattributedCommit(env, r, head) // human-only commit: no manifest to retire
		return nil
	}
	files := head.Paths()
	ids := make([]string, 0, len(stamped))
	for _, t := range stamped {
		ids = append(ids, t.SessionID)
	}
	// Read before Consume empties the manifests. A single stamped session still names its
	// files, since its commit can carry a hand edit too.
	owners := fileOwners(env, r.Store, ids)
	for _, id := range ids {
		if err := r.Store.Consume(id, files); err != nil {
			env.Logf("consume manifest: %v", err)
		}
	}
	attrs := commitAttrs(r, head)
	attrs[semconv.TermaCommitSessionIDsKey] = ids
	attrs[semconv.TermaCommitSessionCountKey] = len(ids)
	attrs[semconv.GenAIMainAgentNameKey] = stamped[0].Tool
	addFileStats(attrs, head.Files, owners)
	env.EmitFor(r, spool.Event{Name: semconv.TermaCommitEvent, SessionID: ids[0], Attrs: attrs})
	return nil
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
func addFileStats(attrs map[string]any, stats []gitx.FileStat, owners map[string]string) {
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
		if id := owners[session.Normalize(f.Path)]; id != "" {
			e["session_id"] = id
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
func fileOwners(env Env, store *session.Store, ids []string) map[string]string {
	manifests, err := store.Manifests()
	if err != nil {
		env.Logf("manifests: %v", err)
		return nil
	}
	stamped := make(map[string]bool, len(ids))
	for _, id := range ids {
		stamped[id] = true
	}
	owners := make(map[string]string)
	touchedAt := make(map[string]time.Time)
	for _, m := range manifests {
		if !stamped[m.SessionID] {
			continue
		}
		for f, at := range m.Files {
			// Manifests is oldest first, so a tie keeps the earlier session deterministically.
			if prev, seen := touchedAt[f]; seen && !at.After(prev) {
				continue
			}
			owners[f], touchedAt[f] = m.SessionID, at
		}
	}
	return owners
}
