#!/usr/bin/env node
// Launches the downloaded binary with this process's arguments, stdio, and
// exit code. Kept minimal on purpose: this runs inside git hooks, where every
// millisecond of Node startup is paid on each commit — prefer the native
// binary (brew / install.sh) for hook-heavy use.
//
// terma doctor trusts this file by its digest (npmLauncherDigests in
// internal/doctor/binary.go), and never runs it. A global install's binary
// updates itself in place under whichever launcher npm last installed, so when
// this file changes, add its new digest there and keep every old one.
'use strict';

const path = require('node:path');
const { spawnSync } = require('node:child_process');

const bin = path.join(__dirname, '..', 'vendor', process.platform === 'win32' ? 'terma.exe' : 'terma');
const result = spawnSync(bin, process.argv.slice(2), { stdio: 'inherit' });
if (result.error) {
  if (result.error.code === 'ENOENT') {
    console.error('terma: binary not found; reinstall the package (npm rebuild @miradorlabs/terma) or run install.sh');
    process.exit(127);
  }
  console.error(`terma: ${result.error.message}`);
  process.exit(1);
}
if (result.signal) {
  process.kill(process.pid, result.signal);
}
process.exit(result.status ?? 1);
