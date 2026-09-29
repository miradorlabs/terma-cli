package compat

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Resolve describes the supplied installation. Callers resolve the real binary
// past PATH shims; desktop identities must never borrow the CLI's version.
// Both subprocesses share one budget, leaving launcher preparation headroom.
func Resolve(ctx context.Context, installation Installation) Profile {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	return resolve(ctx, installation)
}

// resolve uses the caller's budget. Keeping the deadline at the public boundary
// lets cache-behavior tests avoid depending on host process-start scheduling.
func resolve(ctx context.Context, installation Installation) Profile {
	p := ForVersion(installation)
	if installation.Harness != "codex" || installation.Surface != CLI || installation.Path == "" {
		return p
	}
	identity, identityErr := binaryIdentity(installation.Path)
	path := capabilityCachePath(identity.Path)
	cached, hit := readCache(path, identity)
	hit = hit && identityErr == nil
	if hit && installation.Version != "" && installation.Version != cached.Version {
		hit = false
	}
	if hit {
		installation.Version = cached.Version
		p = ForVersion(installation) // Re-evaluate rules after a Terma upgrade.
		if cached.NoDaemon != nil {
			p.Capabilities[CodexNoDaemon] = probeCapability(*cached.NoDaemon)
			return p
		}
		if p.Capability(CodexNoDaemon).Support != Unknown {
			return p
		}
	}

	if installation.Version == "" {
		if out, err := run(ctx, installation.Path, "--version"); err == nil {
			installation.Version = codexVersion(out)
		}
		p = ForVersion(installation)
	}
	var observed *bool
	if p.Capability(CodexNoDaemon).Support == Unknown {
		out, err := run(ctx, installation.Path, "--help")
		if err != nil {
			p.Capabilities[CodexNoDaemon] = Capability{Support: Unknown, Source: "unknown", Reason: "capability probe failed or timed out"}
		} else {
			supported := false
			for line := range strings.SplitSeq(string(out), "\n") {
				if strings.TrimSpace(line) == "--no-daemon" {
					supported = true
					break
				}
			}
			observed = &supported
			p.Capabilities[CodexNoDaemon] = probeCapability(supported)
		}
	}
	// Persist observations, not derived rules. Unknown capability results are
	// never cached as unsupported; a known version can still save a later probe.
	if after, err := binaryIdentity(installation.Path); identityErr == nil && err == nil && after == identity &&
		(observed != nil || installation.Version != "") {
		saveCache(path, capabilityCache{Schema: 1, Binary: identity, Version: installation.Version, NoDaemon: observed})
	}
	return p
}

func run(ctx context.Context, path, arg string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, arg)
	cmd.WaitDelay = 50 * time.Millisecond
	return cmd.Output()
}

func codexVersion(out []byte) string {
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 2 && fields[0] == "codex-cli" {
		fields = fields[1:]
	}
	if len(fields) != 1 {
		return ""
	}
	if v, ok := ParseVersion(fields[0]); ok {
		return v
	}
	return ""
}

func probeCapability(supported bool) Capability {
	s := Unsupported
	if supported {
		s = Supported
	}
	return Capability{Support: s, Source: "probe", Evidence: "codex --help"}
}
