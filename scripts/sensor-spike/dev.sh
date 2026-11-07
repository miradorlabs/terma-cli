#!/bin/sh
# Runs a command in the sensor's Linux dev container: privileged, in the VM's pid and network
# namespaces, with the worktree at /src and the Docker socket for the container backend.
#   scripts/sensor-spike/dev.sh go generate ./internal/sensor/linux/...
set -eu
root=$(cd "$(dirname "$0")/../.." && pwd)
tty=; [ -t 0 ] && tty=-t
exec docker run --rm -i $tty --privileged --pid=host --network=host --cgroupns=host \
  -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$root":/src -v terma-sensor-gomod:/go/pkg/mod -v terma-sensor-gocache:/root/.cache \
  ${SENSOR_DEV_ARGS:-} --name "${SENSOR_DEV_NAME:-terma-sensor-dev-$$}" \
  terma-sensor-dev "$@"
