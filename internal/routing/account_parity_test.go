package routing_test

import (
	"strings"
	"testing"

	"github.com/miradorlabs/terma-cli/internal/account/auth"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/sendas"
)

// routing.ValidAccount duplicates auth.ValidAccountKey's length/hex check to keep
// keychain dependencies out of routing; pin exact parity across edge cases.
func TestValidAccountMatchesAuthAccountKey(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		sendas.NoPersonLane,
		strings.Repeat("a", auth.AccountKeyLen),
		"0123456789abcdef",
		strings.Repeat("A", auth.AccountKeyLen),
		strings.Repeat("a", auth.AccountKeyLen-1),
		strings.Repeat("a", auth.AccountKeyLen+1),
		"0123456789abcdeg",
		"no-person",
	}
	for _, k := range cases {
		want := k == sendas.NoPersonLane || auth.ValidAccountKey(k)
		if got := routing.ValidAccount(k); got != want {
			t.Errorf("routing.ValidAccount(%q) = %v, want %v", k, got, want)
		}
	}
}
