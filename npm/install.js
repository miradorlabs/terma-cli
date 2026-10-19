#!/usr/bin/env node
// Downloads the terma release that matches this package's version into
// ./vendor/, verifying it against the release's checksums.txt, which a release key
// must have signed. The package version is set from the git tag by the release
// workflow, so the shim can never drift from the binary it wraps. Nothing runs from
// the network: the archive is verified before it is extracted, and only the single
// binary is kept.
'use strict';

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const https = require('node:https');
const crypto = require('node:crypto');
const { execFileSync } = require('node:child_process');

const pkg = require('./package.json');
const REPO = 'miradorlabs/terma-cli';
const VENDOR = path.join(__dirname, 'vendor');

// The ed25519 public keys a release may be signed with, hex, newest first: a copy of
// internal/selfupdate's releaseKeys, which `terma update` checks the same signature
// with (TestInstallersPinTheReleaseKeys keeps them equal).
const RELEASE_KEYS = [
  'cb6646bf0ec98128bcab6bd5a4b92d0ca4ade2d9e3f17e12e06e33725240667b', // 2026-10-07
];
// An ed25519 key's DER SubjectPublicKeyInfo is this, then the key's 32 bytes.
const ED25519_SPKI = Buffer.from('302a300506032b6570032100', 'hex');
// The most bytes each download may be: the signature file is a few lines of base64
// (internal/selfupdate's maxSignature), checksums.txt a line per archive, and an archive
// at most internal/selfupdate's maxDownload.
const MAX_SIGNATURE = 4096;
const MAX_CHECKSUMS = 1 << 20;
const MAX_ARCHIVE = 256 << 20;
// A connection that sends nothing for this long fails.
const STALL_MS = 60_000;

function assetName() {
  const osName = { darwin: 'Darwin', linux: 'Linux', win32: 'Windows' }[process.platform];
  const arch = { x64: 'x86_64', arm64: 'arm64' }[process.arch];
  if (!osName || !arch) {
    throw new Error(`unsupported platform ${process.platform}/${process.arch}; install from https://github.com/${REPO}/releases`);
  }
  return `terma_${osName}_${arch}.${process.platform === 'win32' ? 'zip' : 'tar.gz'}`;
}

// Fetches url, following redirects, and fails past limit bytes or after stallMs without
// data. A response it does not read (a redirect, an error) is closed, not drained. client
// is node:https; tests pass node:http.
function fetch(url, limit, { redirects = 5, stallMs = STALL_MS, client = https } = {}) {
  return new Promise((resolve, reject) => {
    // Fails the fetch and closes stream: the request, or once it answered, the response.
    const fail = (message, stream = req) => {
      reject(new Error(`${url}: ${message}`));
      stream.destroy();
    };
    const req = client
      .get(url, { headers: { 'user-agent': `terma-npm-shim/${pkg.version}` }, timeout: stallMs }, (res) => {
        if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location && redirects > 0) {
          let next;
          try {
            next = new URL(res.headers.location, url).toString();
          } catch {
            fail('redirect to an invalid URL', res);
            return;
          }
          resolve(fetch(next, limit, { redirects: redirects - 1, stallMs, client }));
          res.destroy();
          return;
        }
        if (res.statusCode !== 200) {
          fail(`HTTP ${res.statusCode}`, res);
          return;
        }
        const chunks = [];
        let size = 0;
        res.on('data', (c) => {
          size += c.length;
          if (size > limit) {
            fail(`larger than ${limit} bytes`, res);
            return;
          }
          chunks.push(c);
        });
        res.on('end', () => resolve(Buffer.concat(chunks)));
        res.on('error', reject);
      })
      .on('timeout', () => fail(`no data for ${stallMs / 1000} s`))
      .on('error', reject);
  });
}

// Whether sig, a checksums.txt.sig, signs sums for tag by one of keys (hex), as
// internal/selfupdate.Verify reads it: any line, the base64 of an ed25519 signature over
// "terma release <tag>\n" and then the file. The tag comes first so a signed file
// cannot be published again under another tag.
function signedBy(keys, tag, sums, sig) {
  if (sig.length > MAX_SIGNATURE) return false;
  const message = Buffer.concat([Buffer.from(`terma release ${tag}\n`), sums]);
  const publicKeys = keys.map((hex) => crypto.createPublicKey({
    key: Buffer.concat([ED25519_SPKI, Buffer.from(hex, 'hex')]),
    format: 'der',
    type: 'spki',
  }));
  return sig.toString('latin1').split('\n').some((line) => {
    const text = line.trim();
    if (!/^[A-Za-z0-9+/]{86}==$/.test(text)) return false;
    const signature = Buffer.from(text, 'base64');
    return publicKeys.some((key) => crypto.verify(null, message, key, signature));
  });
}

// Downloads asset from release tag and returns it once checksums.txt, signed by one of
// keys, names its digest. The archive is downloaded only once checksums.txt verifies.
async function download(tag, asset, { get = fetch, keys = RELEASE_KEYS } = {}) {
  const base = `https://github.com/${REPO}/releases/download/${tag}`;
  const [sums, sig] = await Promise.all([
    get(`${base}/checksums.txt`, MAX_CHECKSUMS),
    get(`${base}/checksums.txt.sig`, MAX_SIGNATURE),
  ]);
  if (!signedBy(keys, tag, sums, sig)) {
    throw new Error(`${tag} is not signed by a key this installer trusts; refusing to install`);
  }
  const line = sums.toString('utf8').split('\n').find((l) => l.trim().endsWith(`  ${asset}`));
  if (!line) throw new Error(`checksums.txt has no entry for ${asset}`);
  const expected = line.trim().split(/\s+/)[0];
  const archive = await get(`${base}/${asset}`, MAX_ARCHIVE);
  const actual = crypto.createHash('sha256').update(archive).digest('hex');
  if (actual !== expected) throw new Error(`checksum mismatch for ${asset}`);
  return archive;
}

async function main() {
  if (process.env.TERMA_SKIP_DOWNLOAD === '1') {
    console.log('terma: TERMA_SKIP_DOWNLOAD=1, not downloading the binary');
    return;
  }
  const version = pkg.version === '0.0.0' ? process.env.TERMA_VERSION : `v${pkg.version}`;
  if (!version) {
    throw new Error('this is a development copy of the shim; set TERMA_VERSION=vX.Y.Z');
  }
  const asset = assetName();
  console.log(`terma: downloading ${asset} (${version})`);
  installArchive(await download(version, asset), asset);
  console.log(`terma: installed ${version}`);
  console.log('terma: next, run `terma setup` in a terminal.');
}

// Keep verified bytes in a private, unpredictable directory until extraction
// finishes. Paths are data, never interpolated into PowerShell program text.
function installArchive(archive, asset, { vendor = VENDOR, tmpRoot = os.tmpdir(), run = execFileSync } = {}) {
  const temporary = fs.mkdtempSync(path.join(tmpRoot, 'terma-install-'));
  try {
    const archivePath = path.join(temporary, asset);
    const extracted = path.join(temporary, 'extracted');
    fs.mkdirSync(extracted, { mode: 0o700 });
    fs.writeFileSync(archivePath, archive, { flag: 'wx', mode: 0o600 });
    if (asset.endsWith('.zip')) {
      // PowerShell ships with every supported Windows; no zip library needed.
      // Expand-Archive has no single-member selector. Extract into the private
      // directory, then validate and copy only terma.exe into the installation.
      run('powershell', ['-NoProfile', '-NonInteractive', '-Command',
        "$ErrorActionPreference = 'Stop'; Expand-Archive -LiteralPath $env:TERMA_ARCHIVE_PATH -DestinationPath $env:TERMA_EXTRACT_DIR -Force"], {
        stdio: 'inherit',
        env: { ...process.env, TERMA_ARCHIVE_PATH: archivePath, TERMA_EXTRACT_DIR: extracted },
      });
    } else {
      run('tar', ['-xzf', archivePath, '-C', extracted, 'terma'], { stdio: 'inherit' });
    }
    const binary = asset.endsWith('.zip') ? 'terma.exe' : 'terma';
    const source = path.join(extracted, binary);
    if (!fs.lstatSync(source).isFile()) throw new Error('release binary is not a regular file');
    fs.mkdirSync(vendor, { recursive: true });
    // Staged beside the binary and renamed over it, so nothing ever runs a half-written terma.
    const staged = path.join(vendor, `.${binary}.${process.pid}`);
    try {
      fs.copyFileSync(source, staged);
      if (!asset.endsWith('.zip')) fs.chmodSync(staged, 0o755);
      fs.renameSync(staged, path.join(vendor, binary));
    } finally {
      fs.rmSync(staged, { force: true });
    }
  } finally {
    fs.rmSync(temporary, { recursive: true, force: true });
  }
}

module.exports = { RELEASE_KEYS, download, fetch, installArchive, signedBy };

if (require.main === module) main().catch((err) => {
  console.error(`terma: ${err.message}`);
  console.error(`terma: install manually from https://github.com/${REPO}/releases, or with:`);
  console.error('  curl -fsSL https://terma.ai/install.sh | bash');
  process.exit(1);
});
