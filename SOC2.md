# SOC 2 requirements

Mirador Labs is preparing a SOC 2 Type 1 audit. This file lists the engineering rules that contributors and coding agents follow in this repository. It is maintained in every Mirador repository and given to coding agents through `AGENTS.md`. This repository is public, so internal infrastructure details are kept out of it.

**How to use it**
- Read it before changing anything it covers: secrets, dependencies, CI, releases or data handling.
- Never weaken a control here to get a change through. If a change needs one to change, stop and ask a maintainer.
- A change that alters a control updates this file in the same pull request.

## Change management

- Every change reaches the default branch through a pull request; branch rules block force-pushes and deletion. Required CI checks must pass and are never skipped or loosened.
- Where this repository publishes releases, only Mirador release managers create release tags, and the release pipeline publishes them, never a person by hand.

## Secure development

- Never commit a secret: no keys, tokens, passwords or credentials in code, tests, fixtures, examples or CI logs. Secrets live in the CI secret store. Secret scanning with push protection stays on.
- Dependency alerts and security updates stay on. Fix confirmed vulnerabilities within **Critical 7 days, High 30 days, Medium 90 days**.
- Add dependencies only from their official registries, pinned by lockfile. Do not add a third-party SDK that sends data off the user's machine (analytics, crash reporting, AI) without Mirador's vendor review.
- Never log secrets, tokens, passwords or user content.

## Data

- No customer or personal data in this repository: tests and examples use made-up data.
- Software that runs on a user's machine sends data only to Mirador's own documented endpoints over TLS 1.2 or higher, and stores credentials in the operating system's secure store where one exists.

## Reporting security issues

- Report a suspected vulnerability privately by email to support@mirador.org (support@terma.ai for Terma) with "Security" in the subject line, never in a public issue.

## This repository

terma-cli is the `terma` command-line tool. It connects coding agents to Terma and stamps the commits they produce with the agent session that wrote them.

- **Every change needs one approving review from someone other than its author**, and the required checks must pass.
- **Releases come only from `.github/workflows/release.yml`** on a `v*` tag, in protected environments. GoReleaser builds the binaries, attests their build provenance and updates the Homebrew cask, and npm publishes `@miradorlabs/terma` by trusted publishing, with no npm token. Never publish a binary, package or cask by hand.
- **Credentials live in the system's credential store**: the macOS Keychain, the Windows Credential Manager or the Linux Secret Service (`internal/account/secret`). `user-credentials.json` and `keys.json` in the config directory only index them, and hold a secret only where there is no keychain or `--insecure-storage` asks. Never write a token anywhere else or print it.
- **Data goes only to Terma's public endpoints over https**: `api.terma.ai`, `auth.terma.ai`, `app.terma.ai`, `otel.terma.ai` and `mcp.terma.ai` (`internal/config/endpoints.go`). Agent telemetry passes through the local relay on `127.0.0.1`. Hooks never touch the network: they append to a local spool, which `terma spool flush` delivers. What a session sends is decided by its team's collection policy and content settings (README); never widen it in code.
- **`internal/boundary` keeps `net/http` out of the hook, spool and agent packages**, and `prepare-commit-msg` stays local. Keep both.
