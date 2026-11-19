package relay

import (
	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// credentialSafeKeys name the kind of credential the relay stamps (stampCredential): a closed
// set naming no account or key, safe under any policy on the resource the relay stamps alone
// (withhold), as checkoutSafeKeys are.
var credentialSafeKeys = []string{semconv.TermaAccountCredentialKey}

// stampCredential names the kind of credential the part's session runs on, from the claim ran,
// as its agent's hooks read it, and none where they could not tell. It is the relay's to name,
// never the agent's: whatever the agent sent under the key goes first.
func stampCredential(p *part, ran claim.Claim) {
	unstamp(p, semconv.TermaAccountCredentialKey)
	if ran.Credential != "" {
		stamp(p, semconv.TermaAccountCredentialKey, ran.Credential)
	}
}
