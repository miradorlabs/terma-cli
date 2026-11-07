# The sensor spike

An opt-in, root-level sensor that follows what an agent session does on the machine:
its processes, the bytes its connections and files move, the hosts it reaches, the containers
it starts. It can also deny a session hosts and files. It started from a customer's question,
"Codex sent 60 GB in a day; what was it?", which the agents' own telemetry cannot answer.

Status: a spike, hidden as `terma sensor`, installed by nothing. Linux and macOS work end to
end, with real Claude Code and Codex; Windows is designed, not built.

## How it fits together

```
agent process (root of a session) ─ forks, execs ─▶ everything it starts
        │                                                │
        ▼                                                ▼
   platform backend: Linux eBPF │ macOS system extensions (content filter + Endpoint Security)
        │  process tree, socket and file bytes, names, blocks
        ▼
   terma sensor run (root) ── State: sessions by root ── query API (unix socket): top, sessions
        │                                        └── policy in, enforced by the backend
        └── every minute, per session: what moved ─▶ relay POST /v1/sensor
                                                     └▶ placed by the session the root's own
                                                        telemetry named, under its claim
```

- **Sessions start in the kernel.** A process that execs an agent's executable outside any
  session becomes a root before it can fork. The names come from `internal/agents` (the
  `Processes` capability). Userspace is milliseconds too slow: a shell pipeline forks first.
- **Only watched trees are kept.** A process outside one costs a map lookup and leaves
  nothing behind, not even in userspace. Containers a session starts join it by cgroup.
  The container engine is lent to it while the session's pull or build runs.
- **Telemetry goes through the relay.** The relay places a rollup the way it places an
  export: the one session the root process itself named, under that session's claim,
  policy and key. A root that has named none yet is handed back and re-sent, so nothing
  unclaimed leaves the machine.
  - The wire names are `terma.sensor.network`, `terma.sensor.files`, `terma.sensor.block`
    and `terma.sensor.container`.
  - The sensor sends counts and hosts, never a file's path or a block's target.

Code: `internal/sensor` (state, query API, DNS, TLS and QUIC name parsing, Docker, policy,
reporting), `internal/sensor/linux` (eBPF; `internal/sensor/linux/bpf/sensor.bpf.c`),
`internal/sensor/darwin`, `internal/sensor/platform`, `internal/sensor/sensorctl`, the command
in `internal/cli/sensor.go`, the relay side in `internal/relay/sensor.go`, and the macOS app
and extensions in `macos/TermaSensor`.

## What it does, by platform

| | Linux (eBPF, root) | macOS (content filter + Endpoint Security) |
|---|---|---|
| Process tree | sched tracepoints; roots by comm at exec | ES fork and auth-exec; roots by executable, script or argv[0] at exec |
| Network bytes | per socket in the kernel, live every second, exact | counted as data passes the filter (watched TCP only), live every second |
| Hosts | DNS answers, TLS and QUIC (Initial) hellos, from an eBPF socket filter | TLS ClientHello peeked in the filter; `remoteHostname` where set |
| File bytes | exact, live: read and write syscalls by fd | none from ES; a modified file's size at close (an upper bound) |
| Renames, removals | LSM path hooks | ES notify events |
| Containers | cgroup id from Docker events and the session's `docker` command | engine runs in a VM: Docker events only, no bytes yet |
| Image pulls | the engine lent to the session by comm while its command runs | not yet |
| Deny hosts | LSM `socket_connect`, by the addresses DNS answers give the host, read in the kernel before the asker gets them | filter drops on the ClientHello's name, whatever the address |
| Deny files | LSM `file_open`, by inode and every directory above it | ES `AUTH_OPEN`, on a client that only hears watched processes |
| Audit mode | yes | yes |

## What the spike found

- **Real agents traffic that nobody asked for.** Run with a fresh home directory:
  - Codex's CLI cloned OpenAI's plugins repository from github.com: a 25.5 MB pack on
    every start. It also sent about 680 KB to `ab.chatgpt.com` per short task.
  - Claude Code sent about 70 KB to a Datadog intake.
  - The sensor named all of it without being told what to look for.
- **Hook stable interfaces, not internal functions.** On OrbStack's kernel an fentry on
  `vfs_read`, `vfs_write`, `tcp_cleanup_rbuf` or `__fput` attaches and never fires. The
  sensor now uses tracepoints, LSM hooks and `tcp_recvmsg`, and a product version should
  self-test its hooks at start.
- **Pids are not portable; comms and cgroups are.** In a nested pid namespace `/proc`'s pids
  are not the kernel's. The sensor lends the engine by comm.
- **File bytes cost the most on Linux.** The `raw_syscalls` tracepoints made a syscall-bound
  loop 2–3× slower machine-wide, watched or not. A product build should prefer
  fexit `vfs_read`/`vfs_write` where they fire, and keep the tracepoints as the fallback.
- **macOS byte counts need the data path.** The filter's statistics reports carry no
  bytes, and its close report is short once the filter has seen the data. The sensor
  counts as data passes, for watched connections only.
- **Endpoint Security caches answers per executable and file, not per process.** An allow
  for an unwatched `cat` answered a watched one. Hence two ES clients: one follows every
  process, the other, with muting inverted, hears only watched ones. It also needs exec as
  an authorization, so a new image is known before its first open, and a grace period for
  file events that arrive after a process's exit.
- **A host rule holds only where the name is seen.** CDNs rotate addresses, so denying
  the addresses a host resolved to when the policy loaded leaks.
  - On macOS the filter drops on the ClientHello's name.
  - On Linux, `dns_ingress` (`cgroup_skb` ingress, at the root cgroup) reads every UDP DNS
    answer before it is queued to the socket that asked. When the question's name, or a
    name above it, is a host rule, the program puts the answer's addresses in the deny
    map, so the asker never holds an address the kernel would let it reach.
  - Matching hashes each suffix of the name in one backward pass inside `bpf_loop`. An
    unrolled pass does not fit the verifier's budget, and string copies would not either.
  - Tested with sslip.io names, whose address no lookup at policy load can know: 40 of
    40 fresh names, and 20 repeats, blocked on their first connection.
  - It costs about 200 ns per packet received, 0.6 ms of CPU over a 12.5 MB download.
  - Two lessons. Some resolvers (OrbStack's) do not compress answer names, so the
    parser walks labels. And a policy update must change the deny maps in place: a clear
    and rebuild opened windows where about 5% of connections passed.

## Gaps

- macOS: Docker bytes and pulls. The engine's VM reaches the network through the engine's
  own helper process, which the filter would lend as Linux lends the engine.
- Linux: DNS over TCP, DoH and DoT are not read in the kernel. A host's answer that
  arrived before the policy did is denied only through the addresses resolved when the
  policy loaded. An address a denied host shares with others (a CDN) is denied for all
  of them: SNI-level denial, as on macOS, would be the finer cut. Files read or written
  through `mmap`, `sendfile`, `splice` and io_uring are not counted.
- A process forked from a watched one can open files before the macOS files client
  hears of it: fork has no authorization event.
- The relay places a rollup only by a root that exported its own telemetry. A shared
  daemon serving several sessions (Codex's app server) is never placed.
- QUIC version 2, and Encrypted Client Hello, hide the name.
- Windows: designed, not built. ETW (Kernel-Process, Kernel-Network, Kernel-File) would
  observe, as administrator. The Windows Filtering Platform would deny hosts per
  application without a driver. Denying files needs a signed minifilter driver.

## Shipping it

- **Linux:** a root service (systemd) running `terma sensor run`. The eBPF object is
  generated and committed, so terma still builds without cgo or clang. It needs BTF and,
  to block, the bpf LSM (`lsm=…,bpf`).
- **macOS:** `TermaSensor.app` in /Applications, signed with Developer ID, with provisioning
  profiles for the Network Extension and Endpoint Security entitlements, and notarized.
  - The developer approves both extensions and the extension's Full Disk Access; an MDM
    profile can pre-approve them by team.
  - `macos/TermaSensor/build.sh` builds it ad hoc today, or with `SIGN_ID` set for the real
    signature.
  - The app stays separate from terma: terma would install, start and query it.
- **Both:** `terma sensor run` would run as a service that setup installs on request,
  reading the relay's address and token itself, as the relay's other clients do.

## Running the spike

- Linux, in OrbStack's VM: `scripts/sensor-spike/vm.sh '<command>'` runs a command on a
  fresh copy of the worktree in a privileged dev container (`scripts/sensor-spike/Dockerfile.linux`).
  The stand-in agents are in `scripts/sensor-spike/fake`.
- macOS, in a tart VM with SIP and AMFI off (the host untouched): `scripts/sensor-spike/macos-vm.sh`
  boots it, pushes the app and terma, and runs commands. A push recreates the filter's
  configuration, which an ad-hoc rebuild invalidates.
- `scripts/sensor-spike/install-macos-sudoers.sh` was the eslogger/pktap alternative,
  superseded by the system extensions.
