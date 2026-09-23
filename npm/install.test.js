'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { execFileSync } = require('node:child_process');
const { test } = require('node:test');
const { installArchive } = require('./install');

function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'terma-installer-test-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  // These characters must remain path data, including inside PowerShell.
  const tmpRoot = path.join(root, "O'Brien ; $() space");
  const vendor = path.join(root, "vendor's directory");
  fs.mkdirSync(tmpRoot);
  return { root, tmpRoot, vendor };
}

test('extracts a real archive through paths with apostrophes and shell characters', (t) => {
  const options = fixture(t);
  const windows = process.platform === 'win32';
  const binary = windows ? 'terma.exe' : 'terma';
  const asset = windows ? 'terma_Windows_x86_64.zip' : 'terma_Darwin_arm64.tar.gz';
  const source = path.join(options.root, binary);
  const archivePath = path.join(options.root, asset);
  fs.writeFileSync(source, 'fixture binary');
  if (windows) {
    execFileSync('powershell', ['-NoProfile', '-NonInteractive', '-Command',
      "$ErrorActionPreference = 'Stop'; Compress-Archive -LiteralPath $env:TEST_SOURCE -DestinationPath $env:TEST_ARCHIVE"], {
      env: { ...process.env, TEST_SOURCE: source, TEST_ARCHIVE: archivePath },
    });
  } else {
    execFileSync('tar', ['-czf', archivePath, '-C', options.root, binary]);
  }
  installArchive(fs.readFileSync(archivePath), asset, options);
  assert.equal(fs.readFileSync(path.join(options.vendor, binary), 'utf8'), 'fixture binary');
  assert.deepEqual(fs.readdirSync(options.tmpRoot), []);
  assert.deepEqual(fs.readdirSync(options.vendor), [binary]);
});

test('uses a private archive and never touches the old predictable filename', (t) => {
  const options = fixture(t);
  const asset = 'terma_Linux_x86_64.tar.gz';
  const predictable = path.join(options.tmpRoot, `${process.pid}-${asset}`);
  fs.writeFileSync(predictable, 'attacker-owned sentinel');
  let temporary;
  installArchive(Buffer.from('verified archive'), asset, {
    ...options,
    run(command, args) {
      assert.equal(command, 'tar');
      const archivePath = args[1];
      temporary = path.dirname(archivePath);
      assert.notEqual(archivePath, predictable);
      assert.equal(path.dirname(temporary), options.tmpRoot);
      assert.equal(fs.readFileSync(archivePath, 'utf8'), 'verified archive');
      if (process.platform !== 'win32') {
        assert.equal(fs.statSync(temporary).mode & 0o777, 0o700);
        assert.equal(fs.statSync(archivePath).mode & 0o777, 0o600);
      }
      fs.writeFileSync(path.join(args[3], 'terma'), 'binary');
    },
  });
  assert.equal(fs.existsSync(temporary), false);
  assert.equal(fs.readFileSync(predictable, 'utf8'), 'attacker-owned sentinel');
});

test('PowerShell receives paths through environment variables, never source text', (t) => {
  const options = fixture(t);
  installArchive(Buffer.from('archive'), 'terma_Windows_x86_64.zip', {
    ...options,
    run(command, args, childOptions) {
      assert.equal(command, 'powershell');
      const script = args[args.length - 1];
      assert.ok(script.includes('$env:TERMA_ARCHIVE_PATH'));
      assert.ok(script.includes('$env:TERMA_EXTRACT_DIR'));
      assert.ok(!script.includes(options.root));
      assert.ok(!script.includes("O'Brien"));
      fs.writeFileSync(path.join(childOptions.env.TERMA_EXTRACT_DIR, 'terma.exe'), 'binary');
    },
  });
});

test('failed extraction cleans temporary files and preserves the installed binary', (t) => {
  const options = fixture(t);
  fs.mkdirSync(options.vendor);
  fs.writeFileSync(path.join(options.vendor, 'terma'), 'existing binary');
  assert.throws(() => installArchive(Buffer.from('bad archive'), 'terma.tar.gz', {
    ...options,
    run() { throw new Error('extraction failed'); },
  }), /extraction failed/);
  assert.deepEqual(fs.readdirSync(options.tmpRoot), []);
  assert.equal(fs.readFileSync(path.join(options.vendor, 'terma'), 'utf8'), 'existing binary');
});

test('an extracted directory cannot masquerade as the release binary', (t) => {
  const options = fixture(t);
  assert.throws(() => installArchive(Buffer.from('archive'), 'terma.tar.gz', {
    ...options,
    run(_command, args) { fs.mkdirSync(path.join(args[3], 'terma')); },
  }), /not a regular file/);
  assert.deepEqual(fs.readdirSync(options.tmpRoot), []);
  assert.equal(fs.existsSync(options.vendor), false);
});

test('an extracted symlink cannot substitute an outside file for the binary', {
  // Creating file symlinks on Windows requires privileges or Developer Mode.
  skip: process.platform === 'win32',
}, (t) => {
  const options = fixture(t);
  const outside = path.join(options.root, 'outside');
  fs.writeFileSync(outside, 'must not be installed');
  fs.mkdirSync(options.vendor);
  fs.writeFileSync(path.join(options.vendor, 'terma'), 'existing binary');
  assert.throws(() => installArchive(Buffer.from('archive'), 'terma.tar.gz', {
    ...options,
    run(_command, args) { fs.symlinkSync(outside, path.join(args[3], 'terma')); },
  }), /not a regular file/);
  assert.deepEqual(fs.readdirSync(options.tmpRoot), []);
  assert.equal(fs.readFileSync(path.join(options.vendor, 'terma'), 'utf8'), 'existing binary');
  assert.equal(fs.readFileSync(outside, 'utf8'), 'must not be installed');
});
