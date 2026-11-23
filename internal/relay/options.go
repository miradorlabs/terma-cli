package relay

import (
	"context"
	"net/http"
	"net/netip"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/miradorlabs/terma-cli/internal/relay/claim"
	"github.com/miradorlabs/terma-cli/internal/relay/shape"
)

// Options configure a relay.
type Options struct {
	// Correlators and Capturers say how each agent's records name their session and carry content.
	Correlators []shape.Correlator
	Capturers   []shape.Capturer
	// Token is what the agents' exporters present as `Authorization: Bearer <token>`.
	Token string
	// Hold and TraceHold: DefaultHold and DefaultTraceHold (at least Hold) when zero.
	Hold      time.Duration
	TraceHold time.Duration
	// Lookup finds a session's claim; none when nil.
	Lookup func(sessionID string, now time.Time) (claim.Claim, bool)
	// Resolve turns a claim into a Policy, or ErrNoKey.
	Resolve func(c claim.Claim) (Policy, error)
	// SignIn is who parts without a server key are sent as; none leave without it. A part
	// is bound to the lane of the sign-in when it arrives: a person's account, which it
	// leaves under only while that sign-in is the one in place, so a backlog never goes out
	// in another person's name or to another environment (it ages out of the outbox
	// meanwhile); or no person's, under a server key, which only a server key sends. One
	// that arrives signed out, or while the sign-in can't be read, is lost, and counted.
	// Nil is a server key's relay.
	SignIn SignIn
	// CatchAll, in global mode, is where a part whose hold ran out unplaced goes instead of being dropped.
	CatchAll func() (claim.Claim, bool)
	// HeartbeatSend, set, turns the heartbeat on; HeartbeatEvery is its period.
	HeartbeatSend  func(ctx context.Context, beat *logspb.LogsData) error
	HeartbeatEvery time.Duration
	// ClaimCacheTTL and PolicyCacheTTL keep Lookup's and Resolve's answers that long (0: ask every time).
	ClaimCacheTTL  time.Duration
	PolicyCacheTTL time.Duration
	// PeerPID names the process behind a connection from its two ends, the client's and the
	// relay's; without it the session alone decides.
	PeerPID func(client, server netip.AddrPort) (int, bool)
	// ProcessAlive reports whether a sender still runs; sessionless parts go by process once it exits.
	ProcessAlive func(pid int) bool
	// HTTP sends upstream; a no-redirect client with a 15-second timeout when nil.
	HTTP *http.Client
	// HomeDir is the home directory of the user the relay runs as, symlink-resolved, which it
	// stamps as terma.home_directory with tool content: an agent that writes it as `~` in the
	// paths it exports is read against it. "" stamps none.
	HomeDir string
	// Dir is the on-disk outbox, so a restart loses nothing; required for anything to leave.
	Dir string
	// Grace is how long a stopping relay keeps delivering (5 s); the rest waits in the outbox.
	Grace time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Version is terma's, sent as the User-Agent.
	Version string
	// Launch is what started the relay, on the heartbeat's resource: the service manager,
	// a hook on demand, or a developer. A string; the daemon owns the vocabulary.
	Launch string
	// InstallMethod is how this terma was installed, on the heartbeat's resource as
	// terma.install.method; "" sends none.
	InstallMethod string
	// Installation is, asked at every heartbeat, this installation's stable id, on its
	// resource as terma.installation.id; nil or "" sends none.
	Installation func() string
	// HookLosses are, asked at every heartbeat, what hooks lost since it was last asked, by
	// reason (spool.TakeNewLosses): counted as dropped.<reason>.hooks, so the beat carries
	// the hooks' losses with the relay's.
	HookLosses func() map[string]int
	// UpdateAuto, set, is asked at every heartbeat whether this terma installs new releases
	// by itself, sent on its record as terma.update.auto: the developer can turn automatic
	// updates on or off while the relay runs.
	UpdateAuto func() string
	// Logf, when set, is told why each part that could not leave was dropped (TERMA_RELAY_DEBUG=1).
	Logf func(format string, args ...any)
	// Warnf, when set, is told what an operator should see whatever the debug setting: a
	// delivery the backend refused, a retry, an outbox that cannot be written, a failed
	// heartbeat. Logf is told too.
	Warnf func(format string, args ...any)
}
