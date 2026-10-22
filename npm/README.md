# @miradorlabs/terma

npm shim for the [terma CLI](https://github.com/miradorlabs/terma-cli). Installing the
package downloads the matching release binary from GitHub Releases, verifies it against
the release's `checksums.txt`, and exposes it as `terma`.

```bash
npm install -g @miradorlabs/terma   # or: npx @miradorlabs/terma setup
terma setup
```

A global install keeps itself up to date: terma replaces the package's binary with each new
release in place, as the install script's does, so `npm ls -g` shows the version npm last
installed until your next `npm i -g` or `terma update`. A project's dependency is upgraded
in that project.

Prefer the native install (Homebrew or `install.sh`) on machines where terma runs inside
git hooks: the shim adds Node's startup time to every commit.
