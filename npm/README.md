# @miradorlabs/terma

npm shim for the [terma CLI](https://github.com/miradorlabs/terma-cli). Installing the
package downloads the matching release binary from GitHub Releases, verifies it against
the release's `checksums.txt`, and exposes it as `terma`.

```bash
npm install -g @miradorlabs/terma   # or: npx @miradorlabs/terma setup
terma setup
```

Prefer the native install (Homebrew or `install.sh`) on machines where terma runs inside
git hooks: the shim adds Node's startup time to every commit.
