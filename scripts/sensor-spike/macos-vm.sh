#!/bin/sh
# Drives the spike's macOS VM (tart): SIP and AMFI off inside it, so TermaSensor.app's
# ad-hoc signed system extensions load as Developer ID signed ones will. The host is not
# touched.
#   macos-vm.sh recovery   # boot into Recovery for the one manual step (below)
#   macos-vm.sh up         # boot, with a window for the approval prompts
#   macos-vm.sh push       # build and copy in TermaSensor.app, terma (as /tmp/terma), fake agents
#   macos-vm.sh x <cmd>    # run a command in the VM as admin, over ssh (sudo needs no password)
#   macos-vm.sh down
set -eu
VM=${VM:-terma-sensor-vm}
TART=${TART:-$HOME/.local/bin/tart}
root=$(cd "$(dirname "$0")/../.." && pwd)

KEY=$HOME/.ssh/terma-sensor-vm
# x runs a command in the VM over ssh, with the key key() installs.
x() {
  ip=$("$TART" ip "$VM" --wait 120)
  ssh -q -i "$KEY" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "admin@$ip" "$@"
}
# key puts a fresh key on the VM's admin account, the one time it asks for the image's
# password (admin) through SSH_ASKPASS.
key() {
  [ -f "$KEY" ] || ssh-keygen -q -t ed25519 -N "" -f "$KEY" -C terma-sensor-vm
  ip=$("$TART" ip "$VM" --wait 120)
  askpass=$(mktemp); printf '#!/bin/sh\necho admin\n' > "$askpass"; chmod +x "$askpass"
  SSH_ASKPASS=$askpass SSH_ASKPASS_REQUIRE=force DISPLAY=: ssh -q -o StrictHostKeyChecking=no \
    -o UserKnownHostsFile=/dev/null -o PubkeyAuthentication=no "admin@$ip" \
    "mkdir -p ~/.ssh && cat >> ~/.ssh/authorized_keys && echo admin | sudo -S sh -c 'echo \"admin ALL=(ALL) NOPASSWD: ALL\" > /etc/sudoers.d/admin'" < "$KEY.pub"
  rm -f "$askpass"
}

case "${1:-}" in
recovery)
  cat <<'EOF'
In the VM window: Options > Continue, pick the admin user (password: admin), then
Utilities > Terminal, and run:
    csrutil disable
    nvram boot-args="amfi_get_out_of_my_way=1"
    shutdown -h now
EOF
  exec "$TART" run --recovery "$VM"
  ;;
up)
  "$TART" run "$VM" >/tmp/terma-sensor-vm.log 2>&1 &
  "$TART" ip "$VM" --wait 300 >/dev/null
  sleep 10
  key
  x 'csrutil status; nvram boot-args 2>/dev/null || true; sw_vers -productVersion'
  ;;
push)
  "$root/macos/TermaSensor/build.sh" >/dev/null
  (cd "$root" && GOOS=darwin GOARCH=arm64 go build -o /tmp/terma-sensor-vm-bin ./cmd/terma)
  COPYFILE_DISABLE=1 tar -C "$root/macos/TermaSensor/build" -cf - TermaSensor.app |
    x 'sudo rm -rf /Applications/TermaSensor.app && sudo tar -C /Applications -xf -'
  x 'cat > /tmp/terma && chmod +x /tmp/terma' </tmp/terma-sensor-vm-bin
  COPYFILE_DISABLE=1 tar -C "$root/scripts/sensor-spike" -cf - fake report.py policy-example.json policy-audit.json |
    x 'mkdir -p /tmp/spike && tar -C /tmp/spike -xf -'
  # An ad-hoc build fails the code requirement the filter configuration recorded for the
  # last one, so each push recreates it (a Developer ID build keeps its requirement).
  x 'open -W -a /Applications/TermaSensor.app --args reset' || true
  echo pushed
  ;;
x)
  shift
  x "$@"
  ;;
down)
  "$TART" stop "$VM"
  ;;
*)
  sed -n '2,10p' "$0"
  exit 2
  ;;
esac
