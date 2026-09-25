package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/miradorlabs/terma-cli/internal/api"
	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/output"
	"github.com/miradorlabs/terma-cli/internal/trailer"
)

// blameWindow is how far to widen the log query on each side of the commit's own time.
// The log store caps a query's span, so blame centres a tight window on the commit
// rather than scanning back from now — a commit of any age is found in a 2h window.
const blameWindow = time.Hour

func newBlameCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "blame [<commit>]",
		Short: "Show which agent session produced a commit",
		Long: `blame resolves a commit to the agent session that produced it: the session terma
stamped as its Agent-Session-Id trailer, joined to the terma.commit record the backend
holds — the tool, the line counts, and the repository.

The commit defaults to HEAD; pass any revision git understands. Attribution is reported
for every connected harness that stamps commits. Per-commit cost is not shown yet — it
needs the AI-session join and lands in a follow-up.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runBlame,
	}
}

func runBlame(cmd *cobra.Command, args []string) error {
	ctx, client, format, err := setupProjectCommand(cmd)
	if err != nil {
		return err
	}
	rev := "HEAD"
	if len(args) == 1 && strings.TrimSpace(args[0]) != "" {
		rev = strings.TrimSpace(args[0])
	}

	root, gitDir, err := repoHere(ctx, "")
	if err != nil {
		return fmt.Errorf("blame runs inside the git repository that holds the commit: %w", err)
	}
	sha, when, err := commitIdentity(ctx, root, rev)
	if err != nil {
		return err
	}

	rec, err := client.CommitLog(ctx, sha, when.Add(-blameWindow), when.Add(blameWindow))
	if err != nil {
		return err
	}
	if rec == nil {
		return blameNotFound(ctx, root, gitDir, sha)
	}

	view := blameViewOf(rec, sha)
	return output.Render(cmd.OutOrStdout(), format, blameTable(view), view)
}

// commitIdentity reads a commit's full sha and committer time in one git call, so the
// query window can be centred on the commit itself.
func commitIdentity(ctx context.Context, root, rev string) (sha string, when time.Time, err error) {
	out, err := gitx.Git(ctx, root, "show", "-s", "--format=%H%x00%cI", rev)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("no such commit %q in this repository", rev)
	}
	shaStr, iso, ok := strings.Cut(strings.TrimSpace(out), "\x00")
	if !ok {
		return "", time.Time{}, fmt.Errorf("could not read commit %q", rev)
	}
	when, err = time.Parse(time.RFC3339, strings.TrimSpace(iso))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("could not read the commit time for %q: %w", rev, err)
	}
	return shaStr, when, nil
}

// blameNotFound explains a miss from what the local commit carries. A commit with no
// trailer was never terma's to attribute; a stamped one simply has not been reported
// yet, which the developer can act on.
func blameNotFound(ctx context.Context, root, gitDir, sha string) error {
	msg, _ := gitx.CommitMessage(ctx, root, sha)
	if len(trailer.Parse(msg, gitx.CommentCharFS(gitDir))) == 0 {
		return fmt.Errorf("%s carries no Agent-Session-Id trailer — no agent commit for terma to attribute", shortSHA(sha))
	}
	return fmt.Errorf("%s is stamped for an agent session but has not reached Terma yet — "+
		"run `terma spool flush` (or wait for delivery) and retry", shortSHA(sha))
}

// blameView is the stable JSON and table shape of a blamed commit. Fields are drawn
// from the backend's terma.commit record.
type blameView struct {
	SHA          string   `json:"sha"`
	Tool         string   `json:"tool,omitempty"`
	Source       string   `json:"source_system,omitempty"`
	SessionID    string   `json:"session_id,omitempty"`
	Sessions     []string `json:"sessions,omitempty"`
	LinesAdded   int      `json:"lines_added"`
	LinesDeleted int      `json:"lines_deleted"`
	FileCount    int      `json:"file_count"`
	Repository   string   `json:"repository,omitempty"`
	Worktree     string   `json:"worktree,omitempty"`
	Branch       string   `json:"branch,omitempty"`
	RepoURL      string   `json:"repo_url,omitempty"`
	AuthorEmail  string   `json:"author_email,omitempty"`
	ReportedAt   string   `json:"reported_at,omitempty"`
}

func blameViewOf(r *api.LogRecord, sha string) blameView {
	tool := r.Attr("tool")
	sessions := splitCommas(r.Attr("sessions"))
	primary := r.Attr("session.id")
	if primary == "" && len(sessions) > 0 {
		primary = sessions[0]
	}
	v := blameView{
		SHA:          firstNonEmpty(r.Attr("sha"), sha),
		Tool:         tool,
		Source:       sourceFromTool(tool),
		SessionID:    primary,
		Sessions:     sessions,
		LinesAdded:   r.Int("lines_added"),
		LinesDeleted: r.Int("lines_deleted"),
		FileCount:    r.Int("file_count"),
		Repository:   r.Attr("terma.repo"),
		Worktree:     r.Attr("worktree"),
		Branch:       r.Attr("branch"),
		RepoURL:      r.Attr("repo_url"),
		AuthorEmail:  r.Attr("author_email"),
	}
	if !r.Time.IsZero() {
		v.ReportedAt = r.Time.UTC().Format(time.RFC3339)
	}
	return v
}

func blameTable(v blameView) output.Table {
	session := strings.Join(v.Sessions, ", ")
	if session == "" {
		session = v.SessionID
	}
	if v.Source != "" && session != "" {
		session = fmt.Sprintf("%s  (source: %s)", session, v.Source)
	}
	repo := v.Repository
	if v.Branch != "" {
		repo = strings.TrimSpace(v.Repository + " @ " + v.Branch)
	}
	if v.Worktree != "" {
		repo = strings.TrimSpace(repo + " (worktree " + v.Worktree + ")")
	}

	t := output.Table{Headers: []string{"FIELD", "VALUE"}}
	add := func(field, value string) {
		if value != "" {
			t.Rows = append(t.Rows, []string{field, value})
		}
	}
	add("commit", shortSHA(v.SHA))
	add("tool", v.Tool)
	add("session", session)
	add("lines", fmt.Sprintf("+%d / -%d across %d file(s)", v.LinesAdded, v.LinesDeleted, v.FileCount))
	add("repo", repo)
	add("author", v.AuthorEmail)
	add("reported", v.ReportedAt)
	return t
}

// sourceFromTool takes the source system off an Agent-Tool value ("claude-code/2.1.0"
// -> "claude-code"); the plain "claude-code" form passes through unchanged.
func sourceFromTool(tool string) string {
	source, _, _ := strings.Cut(tool, "/")
	return source
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
