// Package linux is the sensor's Linux backend: eBPF programs that follow an agent session's
// processes, sockets and files in the kernel, and enforce its policy there.
package linux

// The object is generated into the package and committed, so building terma needs neither
// clang nor the kernel headers; scripts/sensor-spike/dev.sh has both. bpf/vmlinux.h, the
// kernel's types, is not committed: `bpftool btf dump file /sys/kernel/btf/vmlinux format c
// > bpf/vmlinux.h` writes it, and CO-RE relocates the object to any kernel with BTF.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -cflags "-O2 -g -Wall -Werror -Wno-missing-declarations" -target bpfel -type header -type event_exec -type event_flow -type event_file -type event_path -type event_block -type flow -type fileio -type ip_key -type inode_key -type comm_key -type lent_key sensor bpf/sensor.bpf.c -- -I/usr/include/aarch64-linux-gnu
