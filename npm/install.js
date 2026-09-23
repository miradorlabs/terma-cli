#!/usr/bin/env node
// Downloads the terma release that matches this package's version into
// ./vendor/, verifying it against the release's checksums.txt. The package
// version is set from the git tag by the release workflow, so the shim can never
// drift from the binary it wraps. Nothing runs from the network: the archive is
// verified before it is extracted, and only the single binary is kept.
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

function assetName() {
  const osName = { darwin: 'Darwin', linux: 'Linux', win32: 'Windows' }[process.platform];
  const arch = { x64: 'x86_64', arm64: 'arm64' }[process.arch];
  if (!osName || !arch) {
    throw new Error(`unsupported platform ${process.platform}/${process.arch}; install from https://github.com/${REPO}/releases`);
  }
  return `terma_${osName}_${arch}.${process.platform === 'win32' ? 'zip' : 'tar.gz'}`;
}

function fetch(url, redirects = 5) {
  return new Promise((resolve, reject) => {
    https
      .get(url, { headers: { 'user-agent': `terma-npm-shim/${pkg.version}` } }, (res) => {
        if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location && redirects > 0) {
          res.resume();
          resolve(fetch(new URL(res.headers.location, url).toString(), redirects - 1));
          return;
        }
        if (res.statusCode !== 200) {
          res.resume();
          reject(new Error(`${url}: HTTP ${res.statusCode}`));
          return;
        }
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () => resolve(Buffer.concat(chunks)));
        res.on('error', reject);
      })
      .on('error', reject);
  });
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
  const base = `https://github.com/${REPO}/releases/download/${version}`;
  console.log(`terma: downloading ${asset} (${version})`);
  const [archive, sums] = await Promise.all([fetch(`${base}/${asset}`), fetch(`${base}/checksums.txt`)]);

  const line = sums.toString('utf8').split('\n').find((l) => l.trim().endsWith(`  ${asset}`));
  if (!line) throw new Error(`checksums.txt has no entry for ${asset}`);
  const expected = line.trim().split(/\s+/)[0];
  const actual = crypto.createHash('sha256').update(archive).digest('hex');
  if (actual !== expected) throw new Error(`checksum mismatch for ${asset}`);

  installArchive(archive, asset);
  console.log(`terma: installed ${version}`);
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
    fs.copyFileSync(source, path.join(vendor, binary));
    if (!asset.endsWith('.zip')) fs.chmodSync(path.join(vendor, binary), 0o755);
  } finally {
    fs.rmSync(temporary, { recursive: true, force: true });
  }
}

module.exports = { installArchive };

if (require.main === module) main().catch((err) => {
  console.error(`terma: ${err.message}`);
  console.error(`terma: install manually from https://github.com/${REPO}/releases, or with:`);
  console.error('  curl -fsSL https://terma.ai/install.sh | bash');
  process.exit(1);
});
