#!/bin/sh
# Lets the sensor spike start Apple's own eslogger and tcpdump as root, with exactly these
# command lines and no others; the sensor itself runs as you. Run with sudo. Undo:
#   sudo rm -rf /etc/sudoers.d/terma-sensor-spike /etc/terma-sensor
#
# What this grants any process running as you, agents included, for as long as it is
# installed: the machine's Endpoint Security events (every process's exec arguments and
# the files it touches) and its packet headers, DNS answers and TLS/QUIC hellos. Nothing
# here can write a file or run a command as root: the filter is a root-owned file, and
# tcpdump only writes to stdout.
set -eu

[ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }
user=${SUDO_USER:?run with sudo from your own account}

install -d -o root -g wheel -m 755 /etc/terma-sensor
# DNS, TLS ClientHellos (handshake record, type client_hello) and QUIC long-header
# packets, whole, for the names; everything else the header capture counts.
cat > /etc/terma-sensor/hello.filter <<'EOF'
udp port 53 or tcp port 53 or (tcp[((tcp[12:1] & 0xf0) >> 2):1] = 0x16 and tcp[((tcp[12:1] & 0xf0) >> 2) + 5:1] = 0x01) or (udp port 443 and udp[8:1] & 0xc0 = 0xc0)
EOF
chown root:wheel /etc/terma-sensor/hello.filter
chmod 644 /etc/terma-sensor/hello.filter

tmp=$(mktemp)
cat > "$tmp" <<EOF
# terma sensor spike; see scripts/sensor-spike/install-macos-sudoers.sh
Cmnd_Alias TERMA_SENSOR = /usr/bin/eslogger fork exec exit open close create rename unlink truncate, \\
    /usr/sbin/tcpdump -i pktap\\,all -n -U -s 128 -w -, \\
    /usr/sbin/tcpdump -i pktap\\,all -n -U -s 0 -w - -F /etc/terma-sensor/hello.filter
$user ALL=(root) NOPASSWD: TERMA_SENSOR
EOF
visudo -cf "$tmp"
install -o root -g wheel -m 440 "$tmp" /etc/sudoers.d/terma-sensor-spike
rm -f "$tmp"
echo "installed. eslogger also needs Full Disk Access for the terminal app it starts from."
