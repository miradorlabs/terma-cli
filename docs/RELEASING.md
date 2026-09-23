# Releasing terma

Tagging is the whole process — `.github/workflows/release.yml` runs GoReleaser, which
builds every platform, publishes the GitHub Release (archives, `checksums.txt`,
`install.sh`) with signed provenance, and pushes the Homebrew cask; the npm shim is
published when `NPM_TOKEN` is set. A `smoke` job then installs the release the way the
README says to — `install.sh` on Linux and macOS, `brew install miradorlabs/tap/terma`
on macOS — and fails the workflow if `terma version` does not print the tag.

```bash
git tag v0.1.0 && git push origin v0.1.0
```

The tag is the version; see [Versioning](#versioning) for how to pick one and what
each kind of build reports.

Every push already rehearses this: CI renders the release from a throwaway tag with the
committed GoReleaser config, then runs `install.sh` and installs the rendered cask
against those archives over loopback (`make test-install` does the same locally; the
cask half needs macOS). A change to the archive names, the checksum format, the cask
template or Homebrew itself fails there, not on release day.

Before the first release: the `miradorlabs/homebrew-tap` repo must exist and the
`HOMEBREW_TAP_TOKEN` secret (a PAT with `contents:write` on it) must be set. Without the
token the release still publishes; only the cask push — and with it the macOS smoke
test — fails.

Before announcing public availability, verify the unauthenticated download path at
`https://terma.ai/install.sh` follows through to a successful release asset, and
`npm view @miradorlabs/terma version` returns the intended public version. Configure
`NPM_TOKEN` before tagging if npm is an advertised installation path: the workflow
currently skips npm publication when that secret is absent. A green release workflow
alone does not establish that the npm package is available.

The npm installer's extraction and path-handling tests run on Linux, macOS and
Windows in CI (`npm test --prefix npm`). They exercise real archives using paths with
apostrophes and shell metacharacters, private temporary files, and failure cleanup.

## Versioning

The git tag is the version. There is no version file to bump.

To cut a release:

1. Pick the next [semantic version](https://semver.org) after the latest release
   (`gh release list --limit 1`): patch for fixes, minor for features, major for
   breaking changes.
2. Tag the commit on `main` with a `v` prefix and push the tag:
   `git tag v0.2.0 && git push origin v0.2.0`.

The release workflow stamps that tag into every binary, and installed copies pick it
up: interactive commands check once a day and print a notice when a newer release
exists, `terma update --check` checks immediately, and `terma update --auto on` installs
new releases automatically.

What `terma version` reports depends on how the binary was built, and only a release
is ever compared or replaced:

| Built by | Reports | Update checks |
|---|---|---|
| The release workflow (GoReleaser) | the tag, e.g. `0.2.0` | notified and updated |
| CI's release rehearsal (snapshot) | `<next patch>-next` | never |
| `make build` | `git describe`, e.g. `v0.2.0-3-gabc1234-dirty` | never (a clean checkout of a tag reports the tag itself and counts as that release) |
| `go build` | `dev` | never |

A source build is never nagged or replaced behind your back; `terma update --force`
switches one to the latest release when you ask. The updater verifies the release
archive against `checksums.txt` before swapping the binary in place.

## What a release publishes

| Asset | Consumer |
|---|---|
| `terma_<Os>_<arch>.tar.gz` (`.zip` on Windows), `x86_64` for amd64 | `install.sh`, `terma update`, the npm shim, the Homebrew cask |
| `checksums.txt` | every installer verifies against it before running anything |
| `install.sh` | `https://terma.ai/install.sh` redirects to the copy on the latest release |
| signed provenance | `gh attestation verify <file> --owner miradorlabs` |

The asset names are a contract: `internal/selfupdate.AssetName`, `.goreleaser.yaml`,
`install.sh` and `npm/install.js` all spell them the same way, and
`scripts/test-install.sh` fails when one of them drifts.
