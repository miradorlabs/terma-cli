# Developing terma

How to build and check the CLI, and how to run it against the dev backend. Cutting a
release is in [RELEASING.md](RELEASING.md); the reasoning behind the hook path is in
[DESIGN.md](DESIGN.md).

## Build and check

```bash
make build          # → ./bin/terma
make check          # gofmt (verifies only), vet, lint, tests
make lint           # the pinned golangci-lint, built with your Go on first use
make bench-hook     # the 50 ms budget, measured
make release-dry-run  # goreleaser snapshot, nothing published
make test-install     # what CI runs: a tagged goreleaser render, then install.sh (and the cask on macOS) over loopback
```

One test: `TERMA_ENV=dev go test ./internal/hookrun/ -run TestName`. The Makefile sets
`TERMA_ENV=dev` for everything it runs; outside it, set it yourself — `terma install` and
`terma setup` sign in, and a bare run that reaches them opens a browser login on
production (see [Working against the dev backend](#working-against-the-dev-backend)).

The linter's version lives in `.golangci-lint-version`, which CI reads too. `make lint`
builds it from source rather than using one from your PATH, because a released
golangci-lint refuses a module whose `go` directive is newer than the Go it was built with.
Test files are linted, and exported identifiers need a doc comment. The OpenCode plugin is driven the
way OpenCode would with `bun test internal/harness/opencode`; `make check` runs it when
bun is present.

Go 1.27, no CGO. Packages under `internal/`: `hookmgr` (what install writes),
`hookrun` (what hooks do), `session` (manifests + attribution), `trailer`, `spool`,
`gitx` (fast paths that avoid a git subprocess), `doctor`, `selfupdate`, plus the
`auth`/`api`/`config`/`harness` core shared with `mirador-cli`.

## Working against the dev backend

Terma is a product on the shared Mirador backend rather than a separate stack, so
pre-production is a mix: the app is Terma's, while auth, data, and ingest are the Mirador
dev deployment (which serves Terma organizations and projects). Two built-in environments
cover it. Both are hidden — production is the only environment `terma --help` mentions.

| `TERMA_ENV` | app (serves `/cli/auth`) | auth / data / ingest |
|---|---|---|
| `dev` | `https://dev.terma.ai` | `*-dev.mirador.org` |
| `local` | `http://localhost:3000` | `*-dev.mirador.org` |

`local` is the one to use while working on the app itself: only the browser half is
local, because there is no local account service to mint a CLI credential.

```bash
cd ../terma-frontend && corepack pnpm dev   # Express :3001 + Rsbuild :3000
TERMA_ENV=local terma login                 # approve in the browser, on localhost:3000
```

To stop repeating the variable, store the hosts on your profile — then plain `terma`
works, and `terma status` says `Endpoints: custom` so a dev profile never reads as a
production one:

```bash
terma config set \
  --app-url  http://localhost:3000 \
  --auth-url https://auth-dev.mirador.org \
  --api-url  https://api-dev.mirador.org \
  --otlp-url https://otel-dev.mirador.org
terma whoami && terma project list && terma install --project "<name>"
```

The login needs a terma-frontend that carries the `/cli/auth` page and the
`POST /api/cli/authorize` BFF route. Until that reaches `dev.terma.ai`, a locally run
frontend is the only place the handoff completes.

Pointing your own coding agent at a repository's Terma project is `terma install` (or,
for a machine-wide connection, `terma connect claude`). It configures the harness's
telemetry, so an agent already reporting to a Mirador project stops doing that — `terma status` and
`terma doctor` both call out a harness exporting to a different project than the one
selected, which is otherwise a silent "everything is wired and no spend arrives".


### Launcher terminal contracts

Launcher tests require Python 3 for a standard-library PTY driver. Run
`go test -race ./internal/shim -count=1 -timeout=3m` to exercise failure handling,
interactive input and terminal resizing, and Ctrl-C before and after agent startup.
Both PATH and shell-function activation are checked under installed sh, bash, zsh,
and dash shells. The `launcher-contracts` CI job runs on Linux and macOS; it installs
zsh on Linux so both common interactive shells are covered. These tests use fake
agents and need no provider credentials or API calls.
