package relay

import (
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"

	"github.com/miradorlabs/terma-cli/internal/gitx"
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// checkoutSafeKeys name the checkout a session ran in by its repository and branch, and say
// whether it is a linked worktree: what commit events already send, so they leave under any
// policy, on the resource the relay stamps alone (withhold).
var checkoutSafeKeys = []string{
	semconv.VCSRepositoryURLFullKey, semconv.VCSRepositoryNameKey, semconv.VCSOwnerNameKey, semconv.VCSProviderNameKey,
	semconv.VCSRefHeadNameKey, semconv.VCSRefHeadTypeKey, semconv.TermaRepositoryWorktreeLinkedKey, semconv.TermaHooksFileEditsKey,
}

// checkoutContentKeys name it by local path, and by the worktree's name, which an agent
// harness often takes from the task: they leave only with the tool content that names paths.
var checkoutContentKeys = []string{
	semconv.TermaRepositoryRootKey, semconv.TermaWorkingDirectoryKey, semconv.TermaRepositoryWorktreeNameKey,
}

// stampCheckout names the checkout the part's session ran in, from the claim ran: the
// relay's to name, never the agent's, so whatever the agent sent under these keys goes first.
// fileEdits says terma's hooks report the session's file edits, each with its checkout.
func stampCheckout(p *part, ran claim.Claim, toolContent, fileEdits bool) {
	for _, key := range checkoutSafeKeys {
		unstamp(p, key)
	}
	for _, key := range checkoutContentKeys {
		unstamp(p, key)
	}
	if ran.Remote != "" {
		stamp(p, semconv.VCSRepositoryURLFullKey, ran.Remote)
	}
	if o, ok := gitx.ParseOrigin(ran.Repository.Origin); ok {
		stamp(p, semconv.VCSOwnerNameKey, o.Owner)
		stamp(p, semconv.VCSRepositoryNameKey, o.Name)
		if o.Provider != "" {
			stamp(p, semconv.VCSProviderNameKey, o.Provider)
		}
	}
	if ran.Branch != "" {
		stamp(p, semconv.VCSRefHeadNameKey, ran.Branch)
		stamp(p, semconv.VCSRefHeadTypeKey, semconv.VCSRefHeadTypeBranch)
	}
	// Root is "" outside git, where a checkout is neither.
	if ran.Root != "" {
		stampValue(p, semconv.TermaRepositoryWorktreeLinkedKey,
			&commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: ran.Worktree != ""}})
	}
	if fileEdits {
		stampValue(p, semconv.TermaHooksFileEditsKey, &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}})
	}
	if !toolContent {
		return
	}
	for key, value := range map[string]string{
		semconv.TermaRepositoryRootKey:         ran.Root,
		semconv.TermaWorkingDirectoryKey:       ran.Cwd,
		semconv.TermaRepositoryWorktreeNameKey: ran.Worktree,
	} {
		if value != "" {
			stamp(p, key, value)
		}
	}
}
