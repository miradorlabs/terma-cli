'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const crypto = require('node:crypto');
const http = require('node:http');
const { execFileSync } = require('node:child_process');
const { test } = require('node:test');
const { RELEASE_KEYS, download, fetch, installArchive, signedBy } = require('./install');

// A release key for the tests' releases: its public half as hex, as RELEASE_KEYS holds
// one, and sign(tag, data), one line of a checksums.txt.sig as internal/selfupdate.Sign
// writes it.
function releaseKey() {
  const { publicKey, privateKey } = crypto.generateKeyPairSync('ed25519');
  const hex = publicKey.export({ format: 'der', type: 'spki' }).subarray(-32).toString('hex');
  const sign = (tag, data) => `${crypto.sign(null, Buffer.concat([Buffer.from(`terma release ${tag}\n`), data]), privateKey).toString('base64')}\n`;
  return { hex, sign };
}

function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'terma-installer-test-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  // These characters must remain path data, including inside PowerShell.
  const tmpRoot = path.join(root, "O'Brien ; $() space");
  const vendor = path.join(root, "vendor's directory");
  fs.mkdirSync(tmpRoot);
  return { root, tmpRoot, vendor };
}

// The package is published from a clean CI checkout, so a file that exists only on a
// developer's disk ships missing. `npm/bin/terma.js` did exactly that under a root
// `bin/` ignore rule: 0.0.1 installed with no `terma` command at all.
test('every file the package declares is in the checkout', () => {
  const pkg = require('./package.json');
  const declared = [...new Set([...Object.values(pkg.bin), ...pkg.files])];
  const missing = declared.filter((file) => !fs.existsSync(path.join(__dirname, file)));
  assert.deepEqual(missing, []);
});

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

// A running terma maps the installed file, so the new one replaces it by a rename, never by
// a write into it that the running one, or a relay starting meanwhile, would see half done.
test('replaces the installed binary with a new file, leaving nothing staged', {
  skip: process.platform === 'win32', // inode numbers are POSIX
}, (t) => {
  const options = fixture(t);
  fs.mkdirSync(options.vendor);
  const installed = path.join(options.vendor, 'terma');
  fs.writeFileSync(installed, 'old binary');
  const before = fs.statSync(installed).ino;
  installArchive(Buffer.from('archive'), 'terma.tar.gz', {
    ...options,
    run(_command, args) { fs.writeFileSync(path.join(args[3], 'terma'), 'new binary'); },
  });
  assert.equal(fs.readFileSync(installed, 'utf8'), 'new binary');
  assert.notEqual(fs.statSync(installed).ino, before);
  assert.equal(fs.statSync(installed).mode & 0o777, 0o755);
  assert.deepEqual(fs.readdirSync(options.vendor), ['terma']);
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

// v0.2.7's checksums.txt and checksums.txt.sig as published: the built-in keys accept
// what the release workflow signs. Once the key that signed it is retired, take a later
// release's.
test('the built-in keys verify a published release, for its own tag alone', () => {
  const sums = Buffer.from([
    'a14ce2442647e41f8c5df577e35829afb1a6f4a20fcf257a0052a90bba2b1a8a  terma_Darwin_arm64.tar.gz',
    '2d0e814686a4ec5c6fa9ff4682e33f4a03e2cabc47a4d90d40d53b401d46b6e7  terma_Darwin_x86_64.tar.gz',
    'a5528120350ff02cbf34239e1ef38378c2ac6319d39e69d892eba3672fe2825d  terma_Linux_arm64.tar.gz',
    'e16e64dba292bc7f1cdb2f250a7679b8cd47ba51e2ff5e5052ba3bfbad656044  terma_Linux_x86_64.tar.gz',
    '78c3524bd04fd56cb69074fa8070d1b89778d46050b54b40d3c449c5fec6dce1  terma_Windows_arm64.zip',
    'ef72a4134dfd9d32ff00b9f613334dadc6d98a6a98a0495ccb9a22e4f72e7ce5  terma_Windows_x86_64.zip',
    '',
  ].join('\n'));
  const sig = Buffer.from('pQgw2Cz/XJH7qAFoh4xW2wL6cG+twdv2ZNFrelCCW+xQ2xvu0Hc4j+Qv/aYDiCxTXXFEFQ2BZFeauvFuy1+gDw==\n');
  assert.equal(signedBy(RELEASE_KEYS, 'v0.2.7', sums, sig), true);
  assert.equal(signedBy(RELEASE_KEYS, 'v0.2.8', sums, sig), false);
});

// The same cases as internal/selfupdate's TestSignAndVerify: the shim reads a signature
// file exactly as `terma update` does.
test('a signature verifies with the key that made it, for its tag and data alone', () => {
  const key = releaseKey();
  const other = releaseKey();
  const data = Buffer.from('abc  terma_Linux_x86_64.tar.gz\n');
  const sig = Buffer.from(key.sign('v1.2.3', data));
  assert.equal(signedBy([key.hex], 'v1.2.3', data, sig), true);
  const refused = {
    'another key': [[other.hex], 'v1.2.3', data, sig],
    'no key': [[], 'v1.2.3', data, sig],
    'changed data': [[key.hex], 'v1.2.3', Buffer.from('abd  terma_Linux_x86_64.tar.gz\n'), sig],
    'another tag': [[key.hex], 'v1.2.4', data, sig],
    'not base64': [[key.hex], 'v1.2.3', data, Buffer.from('not a signature\n')],
    'too short': [[key.hex], 'v1.2.3', data, Buffer.from('YWJj\n')],
    empty: [[key.hex], 'v1.2.3', data, Buffer.alloc(0)],
    'too long': [[key.hex], 'v1.2.3', data, Buffer.concat([sig, Buffer.alloc(4096)])],
  };
  for (const [name, args] of Object.entries(refused)) {
    assert.equal(signedBy(...args), false, name);
  }
  // A key among several, in any position, and a line among several (a release signed while
  // a key is rotated carries one per key) are enough.
  assert.equal(signedBy([other.hex, key.hex], 'v1.2.3', data, sig), true);
  assert.equal(signedBy([key.hex], 'v1.2.3', data, Buffer.from(other.sign('v1.2.3', data) + key.sign('v1.2.3', data))), true);
});

// A release served as the test describes it: each file by its name. fetched lists the
// files asked for, in order, each with the most bytes it was allowed.
function release(files) {
  const fetched = [];
  const get = async (url, limit) => {
    const name = url.slice(url.lastIndexOf('/') + 1);
    fetched.push([name, limit]);
    if (!(name in files)) throw new Error(`${url}: HTTP 404`);
    return Buffer.from(files[name]);
  };
  return { get, fetched };
}

test('downloads an archive only from a signed release whose checksums name it', async () => {
  const key = releaseKey();
  const asset = 'terma_Linux_x86_64.tar.gz';
  const archive = Buffer.from('archive bytes');
  const sums = Buffer.from(`${crypto.createHash('sha256').update(archive).digest('hex')}  ${asset}\n`);
  const served = release({ [asset]: archive, 'checksums.txt': sums, 'checksums.txt.sig': key.sign('v1.2.3', sums) });
  assert.deepEqual(await download('v1.2.3', asset, { get: served.get, keys: [key.hex] }), archive);
  // Each file is bounded, and the archive is asked for last, once checksums.txt verified.
  assert.deepEqual(served.fetched, [['checksums.txt', 1 << 20], ['checksums.txt.sig', 4096], [asset, 256 << 20]]);

  const other = releaseKey();
  const cases = {
    // No signature, or none by a pinned key, or one made for another release.
    unsigned: [{ [asset]: archive, 'checksums.txt': sums }, /HTTP 404/],
    'signed by another key': [{ [asset]: archive, 'checksums.txt': sums, 'checksums.txt.sig': other.sign('v1.2.3', sums) }, /not signed/],
    'signed for another tag': [{ [asset]: archive, 'checksums.txt': sums, 'checksums.txt.sig': key.sign('v1.2.2', sums) }, /not signed/],
    'checksums changed after signing': [{
      [asset]: Buffer.from('other bytes'),
      'checksums.txt': `${crypto.createHash('sha256').update('other bytes').digest('hex')}  ${asset}\n`,
      'checksums.txt.sig': key.sign('v1.2.3', sums),
    }, /not signed/],
    // A signed checksums.txt still has to name this archive's digest: the one case that
    // gets as far as the archive.
    'archive changed': [{ [asset]: Buffer.from('other bytes'), 'checksums.txt': sums, 'checksums.txt.sig': key.sign('v1.2.3', sums) }, /checksum mismatch/, true],
    'no entry': [{ [asset]: archive, 'checksums.txt': '', 'checksums.txt.sig': key.sign('v1.2.3', Buffer.alloc(0)) }, /no entry/],
  };
  for (const [name, [files, error, fetchesArchive = false]] of Object.entries(cases)) {
    const served = release(files);
    await assert.rejects(download('v1.2.3', asset, { get: served.get, keys: [key.hex] }), error, name);
    assert.equal(served.fetched.some(([file]) => file === asset), fetchesArchive, `${name}: archive fetched`);
  }
});

test('the built-in keys are what a download checks by default', async () => {
  const key = releaseKey();
  const asset = 'terma_Linux_x86_64.tar.gz';
  const archive = Buffer.from('archive bytes');
  const sums = Buffer.from(`${crypto.createHash('sha256').update(archive).digest('hex')}  ${asset}\n`);
  const served = release({ [asset]: archive, 'checksums.txt': sums, 'checksums.txt.sig': key.sign('v1.2.3', sums) });
  await assert.rejects(download('v1.2.3', asset, { get: served.get }), /not signed by a key this installer trusts/);
});

// A local server for fetch: each test's handler answers every request.
async function serve(t, handler) {
  const server = http.createServer(handler);
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => {
    server.closeAllConnections?.();
    server.close();
  });
  return `http://127.0.0.1:${server.address().port}`;
}

test('fetch follows a redirect and returns the body within its limit', async (t) => {
  const base = await serve(t, (req, res) => {
    if (req.url === '/moved') {
      res.writeHead(302, { location: '/file' });
      res.end();
      return;
    }
    res.end('0123456789');
  });
  assert.equal((await fetch(`${base}/moved`, 10, { client: http })).toString(), '0123456789');
});

// endless(res, status, headers) answers with a body that never ends, and resolves, once
// the client has closed it, to how many bytes it wrote.
function endless(res, status, headers) {
  return new Promise((resolve) => {
    res.writeHead(status, headers);
    let written = 0;
    const timer = setInterval(() => {
      res.write('x'.repeat(1024));
      written += 1024;
    }, 2);
    res.on('close', () => {
      clearInterval(timer);
      resolve(written);
    });
  });
}

test('fetch closes a redirect or an error it does not read, never drains it', { timeout: 10_000 }, async (t) => {
  const closed = [];
  const base = await serve(t, (req, res) => {
    if (req.url === '/moved') closed.push(endless(res, 302, { location: '/file' }));
    else if (req.url === '/broken') closed.push(endless(res, 500, {}));
    else res.end('0123456789');
  });
  assert.equal((await fetch(`${base}/moved`, 10, { client: http })).toString(), '0123456789');
  await assert.rejects(fetch(`${base}/broken`, 10, { client: http }), /HTTP 500/);
  for (const written of await Promise.all(closed)) assert.ok(written < 1 << 20, `${written} bytes written`);
});

test('fetch refuses a body past its limit, whatever the server claims', async (t) => {
  const base = await serve(t, (_req, res) => {
    res.writeHead(200); // no content-length: only the bytes say how large it is
    res.write('0123456789');
    res.end('abc');
  });
  await assert.rejects(fetch(`${base}/file`, 10, { client: http }), /larger than 10 bytes/);
});

test('fetch gives up on a connection that stops sending', async (t) => {
  const base = await serve(t, (_req, res) => {
    res.writeHead(200);
    res.write('partial'); // and nothing more
  });
  await assert.rejects(fetch(`${base}/file`, 1 << 20, { client: http, stallMs: 200 }), /no data for 0.2 s/);
});
