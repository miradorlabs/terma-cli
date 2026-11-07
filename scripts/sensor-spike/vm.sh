#!/bin/sh
# Runs a command on a fresh copy of the worktree in the Linux dev container, so a file
# edited a moment ago is never read through OrbStack's file sharing cache. Whatever the
# command leaves in /w/out is copied back to the worktree's scripts/sensor-spike/out.
#   scripts/sensor-spike/vm.sh 'go generate ./internal/sensor/linux/ && cp internal/sensor/linux/sensor_bpfel.* /src/internal/sensor/linux/'
set -eu
root=$(cd "$(dirname "$0")/../.." && pwd)
COPYFILE_DISABLE=1 tar -C "$root" --no-xattrs --exclude .git --exclude scripts/sensor-spike/out -cf - . |
  "$root/scripts/sensor-spike/dev.sh" sh -c 'mkdir -p /w && tar -C /w -xf - && cd /w && '"$1"
