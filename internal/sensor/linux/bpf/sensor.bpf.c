// The sensor's kernel half: which processes an agent session started, the bytes their
// sockets and files move, the files they remove or rename, and, under a policy, the
// connections and opens they may not make. Only a watched process is counted: a tgid
// userspace names as a session's root, any process it forks, and any process in a cgroup
// userspace hands to a session (a container the session started). Everything else costs
// one map lookup and is forgotten.
#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_endian.h>

char LICENSE[] SEC("license") = "Dual MIT/GPL";

#define AF_INET 2
#define AF_INET6 10
#define EPERM 1
#define S_IFMT 00170000
#define S_IFREG 0100000
#define PATH_DEPTH 16
#define NAME_LEN 64
#define ARGS_LEN 512

enum event_type {
	EV_EXEC = 1,
	EV_FORK,
	EV_EXIT,
	EV_FLOW,
	EV_FILE,
	EV_UNLINK,
	EV_RENAME,
	EV_BLOCK,
};

// Every event starts with this, so userspace reads the type before the rest.
struct header {
	u32 type;
	u32 tgid;
	u32 ppid;
	u32 root;
	u64 ts;
	u64 cgroup;
	char comm[16];
};

struct event_exec {
	struct header h;
	u32 args_len;
	char filename[256];
	char args[ARGS_LEN];
};

// A socket's counters, live in the flows map and final in an EV_FLOW event.
struct flow {
	u32 tgid;
	u32 root;
	u64 cgroup;
	u64 start;
	u64 last;
	u64 tx;
	u64 rx;
	u16 family;
	u16 proto;
	u16 sport;
	u16 dport;
	u8 saddr[16];
	u8 daddr[16];
	char comm[16];
};

struct event_flow {
	struct header h;
	struct flow f;
};

struct fileio {
	u32 tgid;
	u32 root;
	u64 start;
	u64 rx;
	u64 tx;
	u32 flags;
	u32 pad;
	char path[256];
};

struct event_file {
	struct header h;
	struct fileio f;
};

// A path as the names from the leaf up; depth counts the names filled.
struct names {
	u32 depth;
	u32 pad;
	char name[PATH_DEPTH][NAME_LEN];
};

struct event_path {
	struct header h;
	struct names from;
	struct names to;
};

struct event_block {
	struct header h;
	u32 kind; // 1 connect, 2 open
	u32 rule;
	u32 enforced;
	u16 family;
	u16 port;
	u8 addr[16];
	char path[256];
};

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 1 << 24);
} events SEC(".maps");

// tgid -> the root tgid of its session.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 65536);
	__type(key, u32);
	__type(value, u32);
} watched SEC(".maps");

// cgroup id -> the root tgid of the session that started it.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 4096);
	__type(key, u64);
	__type(value, u32);
} watched_cgroups SEC(".maps");

// struct sock * -> its counters.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 65536);
	__type(key, u64);
	__type(value, struct flow);
} flows SEC(".maps");

// struct file * -> its counters.
struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 16384);
	__type(key, u64);
	__type(value, struct fileio);
} files SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, u32);
	__type(value, struct fileio);
} file_scratch SEC(".maps");

// Policy: a destination address, or an inode (with every file under it when it is a
// directory), mapped to the rule that denies it.
struct ip_key {
	u16 family;
	u16 pad;
	u8 addr[16];
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 16384);
	__type(key, struct ip_key);
	__type(value, u32);
} deny_ips SEC(".maps");

struct inode_key {
	u64 ino;
	u32 dev;
	u32 pad;
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 4096);
	__type(key, struct inode_key);
	__type(value, u32);
} deny_inodes SEC(".maps");

// sensor_config[0]: 1 enforces the policy, 0 only reports what it would deny.
// sensor_config[1]: 1 while lent_comms names engines lent to a session.
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 2);
	__type(key, u32);
	__type(value, u32);
} sensor_config SEC(".maps");

// The container engine's processes, by comm, lent to a session while its container
// command runs: the engine pulls and builds on its behalf.
struct lent_key {
	char comm[16];
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 8);
	__type(key, struct lent_key);
	__type(value, u32);
} lent_comms SEC(".maps");

static __always_inline u32 root_of_current(void)
{
	u32 tgid = bpf_get_current_pid_tgid() >> 32;
	u32 *r = bpf_map_lookup_elem(&watched, &tgid);
	if (r)
		return *r;
	u64 cg = bpf_get_current_cgroup_id();
	r = bpf_map_lookup_elem(&watched_cgroups, &cg);
	if (r)
		return *r;
	u32 one = 1;
	u32 *lending = bpf_map_lookup_elem(&sensor_config, &one);
	if (!lending || !*lending)
		return 0;
	struct lent_key k = {};
	bpf_get_current_comm(k.comm, sizeof(k.comm));
	r = bpf_map_lookup_elem(&lent_comms, &k);
	return r ? *r : 0;
}

static __always_inline void fill_header(struct header *h, u32 type, u32 root)
{
	struct task_struct *t = (struct task_struct *)bpf_get_current_task_btf();
	h->type = type;
	h->tgid = bpf_get_current_pid_tgid() >> 32;
	h->ppid = BPF_CORE_READ(t, real_parent, tgid);
	h->root = root;
	h->ts = bpf_ktime_get_boot_ns();
	h->cgroup = bpf_get_current_cgroup_id();
	bpf_get_current_comm(h->comm, sizeof(h->comm));
}

// Executables that start a session, by the comm exec gives them (their base name, cut to
// 15 bytes): a process that execs one outside any watched tree becomes a root right here,
// before it can fork, which userspace could not do in time.
struct comm_key {
	char comm[16];
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 64);
	__type(key, struct comm_key);
	__type(value, u8);
} root_comms SEC(".maps");

SEC("tp_btf/sched_process_exec")
int BPF_PROG(on_exec, struct task_struct *p, pid_t old_pid, struct linux_binprm *bprm)
{
	u32 root = root_of_current();
	if (!root) {
		struct comm_key k = {};
		bpf_get_current_comm(k.comm, sizeof(k.comm));
		if (bpf_map_lookup_elem(&root_comms, &k)) {
			root = bpf_get_current_pid_tgid() >> 32;
			bpf_map_update_elem(&watched, &root, &root, BPF_ANY);
		}
	}
	struct event_exec *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e)
		return 0;
	fill_header(&e->h, EV_EXEC, root);
	bpf_probe_read_kernel_str(e->filename, sizeof(e->filename), BPF_CORE_READ(bprm, filename));
	unsigned long start = BPF_CORE_READ(p, mm, arg_start);
	unsigned long end = BPF_CORE_READ(p, mm, arg_end);
	u32 n = end > start ? end - start : 0;
	if (n > ARGS_LEN)
		n = ARGS_LEN;
	e->args_len = n;
	if (bpf_probe_read_user(e->args, n, (void *)start))
		e->args_len = 0;
	bpf_ringbuf_submit(e, 0);
	return 0;
}

SEC("tp_btf/sched_process_fork")
int BPF_PROG(on_fork, struct task_struct *parent, struct task_struct *child)
{
	u32 ptgid = BPF_CORE_READ(parent, tgid);
	u32 ctgid = BPF_CORE_READ(child, tgid);
	if (ptgid == ctgid)
		return 0; // a thread
	u32 *root = bpf_map_lookup_elem(&watched, &ptgid);
	if (!root)
		return 0;
	u32 r = *root;
	bpf_map_update_elem(&watched, &ctgid, &r, BPF_ANY);
	struct header *h = bpf_ringbuf_reserve(&events, sizeof(*h), 0);
	if (!h)
		return 0;
	fill_header(h, EV_FORK, r);
	h->tgid = ctgid;
	h->ppid = ptgid;
	bpf_ringbuf_submit(h, 0);
	return 0;
}

SEC("tp_btf/sched_process_exit")
int BPF_PROG(on_exit, struct task_struct *p)
{
	if (BPF_CORE_READ(p, pid) != BPF_CORE_READ(p, tgid))
		return 0;
	u32 tgid = BPF_CORE_READ(p, tgid);
	u32 *root = bpf_map_lookup_elem(&watched, &tgid);
	if (!root)
		return 0;
	u32 r = *root;
	bpf_map_delete_elem(&watched, &tgid);
	struct header *h = bpf_ringbuf_reserve(&events, sizeof(*h), 0);
	if (!h)
		return 0;
	fill_header(h, EV_EXIT, r);
	bpf_ringbuf_submit(h, 0);
	return 0;
}

static __always_inline void read_addrs(struct sock *sk, struct flow *f)
{
	f->family = BPF_CORE_READ(sk, __sk_common.skc_family);
	f->proto = BPF_CORE_READ(sk, sk_protocol);
	f->sport = BPF_CORE_READ(sk, __sk_common.skc_num);
	f->dport = bpf_ntohs(BPF_CORE_READ(sk, __sk_common.skc_dport));
	if (f->family == AF_INET) {
		u32 s = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
		u32 d = BPF_CORE_READ(sk, __sk_common.skc_daddr);
		__builtin_memcpy(f->saddr, &s, 4);
		__builtin_memcpy(f->daddr, &d, 4);
	} else if (f->family == AF_INET6) {
		BPF_CORE_READ_INTO(&f->saddr, sk, __sk_common.skc_v6_rcv_saddr.in6_u.u6_addr8);
		BPF_CORE_READ_INTO(&f->daddr, sk, __sk_common.skc_v6_daddr.in6_u.u6_addr8);
	}
}

// An unconnected UDP socket names its destination per datagram.
static __always_inline void read_msg_dest(struct msghdr *msg, struct flow *f)
{
	struct sockaddr *sa = BPF_CORE_READ(msg, msg_name);
	if (!sa)
		return;
	u16 fam = BPF_CORE_READ(sa, sa_family);
	if (fam == AF_INET) {
		struct sockaddr_in *in = (struct sockaddr_in *)sa;
		u32 d = BPF_CORE_READ(in, sin_addr.s_addr);
		__builtin_memcpy(f->daddr, &d, 4);
		f->dport = bpf_ntohs(BPF_CORE_READ(in, sin_port));
	} else if (fam == AF_INET6) {
		struct sockaddr_in6 *in6 = (struct sockaddr_in6 *)sa;
		BPF_CORE_READ_INTO(&f->daddr, in6, sin6_addr.in6_u.u6_addr8);
		f->dport = bpf_ntohs(BPF_CORE_READ(in6, sin6_port));
	}
}

static __always_inline void account(struct sock *sk, struct msghdr *msg, long tx, long rx)
{
	if (tx <= 0 && rx <= 0)
		return;
	u32 root = root_of_current();
	if (!root)
		return;
	u64 key = (u64)sk;
	struct flow *f = bpf_map_lookup_elem(&flows, &key);
	if (!f) {
		struct flow nf = {};
		nf.tgid = bpf_get_current_pid_tgid() >> 32;
		nf.root = root;
		nf.cgroup = bpf_get_current_cgroup_id();
		nf.start = bpf_ktime_get_boot_ns();
		bpf_get_current_comm(nf.comm, sizeof(nf.comm));
		read_addrs(sk, &nf);
		if (msg && !nf.dport)
			read_msg_dest(msg, &nf);
		bpf_map_update_elem(&flows, &key, &nf, BPF_NOEXIST);
		f = bpf_map_lookup_elem(&flows, &key);
		if (!f)
			return;
	}
	if (tx > 0)
		__sync_fetch_and_add(&f->tx, tx);
	if (rx > 0)
		__sync_fetch_and_add(&f->rx, rx);
	f->last = bpf_ktime_get_boot_ns();
}

SEC("fexit/tcp_sendmsg")
int BPF_PROG(tcp_send, struct sock *sk, struct msghdr *msg, size_t size, int ret)
{
	account(sk, 0, ret, 0);
	return 0;
}

// tcp_cleanup_rbuf and vfs_read sit where some kernels (OrbStack's among them) never run an
// fentry attached to them; tcp_recvmsg, tracepoints and LSM hooks always run.
SEC("fexit/tcp_recvmsg")
int BPF_PROG(tcp_recv, struct sock *sk, struct msghdr *msg, size_t len, int flags, int ret)
{
	account(sk, 0, 0, ret);
	return 0;
}

SEC("fexit/udp_sendmsg")
int BPF_PROG(udp_send, struct sock *sk, struct msghdr *msg, size_t len, int ret)
{
	account(sk, msg, ret, 0);
	return 0;
}

SEC("fexit/udpv6_sendmsg")
int BPF_PROG(udp6_send, struct sock *sk, struct msghdr *msg, size_t len, int ret)
{
	account(sk, msg, ret, 0);
	return 0;
}

SEC("fexit/udp_recvmsg")
int BPF_PROG(udp_recv, struct sock *sk, struct msghdr *msg, size_t len, int flags, int ret)
{
	account(sk, 0, 0, ret);
	return 0;
}

SEC("fexit/udpv6_recvmsg")
int BPF_PROG(udp6_recv, struct sock *sk, struct msghdr *msg, size_t len, int flags, int ret)
{
	account(sk, 0, 0, ret);
	return 0;
}

SEC("fentry/inet_release")
int BPF_PROG(sock_release, struct socket *sock)
{
	u64 key = (u64)BPF_CORE_READ(sock, sk);
	struct flow *f = bpf_map_lookup_elem(&flows, &key);
	if (!f)
		return 0;
	struct event_flow *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (e) {
		fill_header(&e->h, EV_FLOW, f->root);
		e->h.tgid = f->tgid;
		__builtin_memcpy(&e->f, f, sizeof(*f));
		bpf_ringbuf_submit(e, 0);
	}
	bpf_map_delete_elem(&flows, &key);
	return 0;
}

SEC("fentry/security_file_open")
int BPF_PROG(file_open, struct file *file)
{
	u32 root = root_of_current();
	if (!root)
		return 0;
	if ((BPF_CORE_READ(file, f_inode, i_mode) & S_IFMT) != S_IFREG)
		return 0;
	u32 zero = 0;
	struct fileio *s = bpf_map_lookup_elem(&file_scratch, &zero);
	if (!s)
		return 0;
	s->tgid = bpf_get_current_pid_tgid() >> 32;
	s->root = root;
	s->start = bpf_ktime_get_boot_ns();
	s->rx = 0;
	s->tx = 0;
	s->flags = BPF_CORE_READ(file, f_flags);
	if (bpf_d_path((struct path *)&file->f_path, s->path, sizeof(s->path)) < 0)
		s->path[0] = 0;
	u64 key = (u64)file;
	bpf_map_update_elem(&files, &key, s, BPF_ANY);
	return 0;
}

// The read and write syscalls (numbered per architecture, so userspace fills this) -> 1 for
// a read, 2 for a write.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 16);
	__type(key, u32);
	__type(value, u32);
} rw_syscalls SEC(".maps");

struct pending_io {
	u64 file;
	u32 dir;
	u32 pad;
};

// thread -> the file its read or write syscall is on.
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 16384);
	__type(key, u32);
	__type(value, struct pending_io);
} pending SEC(".maps");

SEC("tracepoint/raw_syscalls/sys_enter")
int sys_enter(struct trace_event_raw_sys_enter *ctx)
{
	u32 id = ctx->id;
	u32 *dir = bpf_map_lookup_elem(&rw_syscalls, &id);
	if (!dir || !root_of_current())
		return 0;
	u32 fd = ctx->args[0];
	struct task_struct *t = (struct task_struct *)bpf_get_current_task_btf();
	struct fdtable *fdt = BPF_CORE_READ(t, files, fdt);
	if (fd >= BPF_CORE_READ(fdt, max_fds))
		return 0;
	struct file **fds = BPF_CORE_READ(fdt, fd);
	struct file *file = 0;
	bpf_probe_read_kernel(&file, sizeof(file), &fds[fd]);
	u64 key = (u64)file;
	if (!file || !bpf_map_lookup_elem(&files, &key))
		return 0;
	struct pending_io io = {.file = key, .dir = *dir};
	u32 tid = (u32)bpf_get_current_pid_tgid();
	bpf_map_update_elem(&pending, &tid, &io, BPF_ANY);
	return 0;
}

SEC("tracepoint/raw_syscalls/sys_exit")
int sys_exit(struct trace_event_raw_sys_exit *ctx)
{
	u32 tid = (u32)bpf_get_current_pid_tgid();
	struct pending_io *io = bpf_map_lookup_elem(&pending, &tid);
	if (!io)
		return 0;
	long ret = ctx->ret;
	u64 key = io->file;
	u32 dir = io->dir;
	bpf_map_delete_elem(&pending, &tid);
	if (ret <= 0)
		return 0;
	struct fileio *f = bpf_map_lookup_elem(&files, &key);
	if (!f)
		return 0;
	if (dir == 1)
		__sync_fetch_and_add(&f->rx, ret);
	else
		__sync_fetch_and_add(&f->tx, ret);
	return 0;
}

SEC("lsm/file_release")
int BPF_PROG(file_release, struct file *file)
{
	u64 key = (u64)file;
	struct fileio *f = bpf_map_lookup_elem(&files, &key);
	if (!f)
		return 0;
	struct event_file *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (e) {
		fill_header(&e->h, EV_FILE, f->root);
		e->h.tgid = f->tgid;
		__builtin_memcpy(&e->f, f, sizeof(*f));
		bpf_ringbuf_submit(e, 0);
	}
	bpf_map_delete_elem(&files, &key);
	return 0;
}

// walk fills n with the names from dentry d up to the root of the mount namespace,
// crossing mount points as d_path does; the leaf comes first.
static __always_inline void walk(struct dentry *d, struct vfsmount *vm, struct names *n)
{
	struct mount *m = container_of(vm, struct mount, mnt);
	n->depth = 0;
	for (int i = 0; i < PATH_DEPTH; i++) {
		struct dentry *mroot = BPF_CORE_READ(m, mnt.mnt_root);
		if (d == mroot) {
			struct mount *parent = BPF_CORE_READ(m, mnt_parent);
			if (parent == m)
				break;
			d = BPF_CORE_READ(m, mnt_mountpoint);
			m = parent;
			continue;
		}
		struct dentry *up = BPF_CORE_READ(d, d_parent);
		if (up == d)
			break;
		bpf_probe_read_kernel_str(n->name[i & (PATH_DEPTH - 1)], NAME_LEN, BPF_CORE_READ(d, d_name.name));
		n->depth = i + 1;
		d = up;
	}
}

SEC("fentry/security_path_unlink")
int BPF_PROG(path_unlink, const struct path *dir, struct dentry *dentry)
{
	u32 root = root_of_current();
	if (!root)
		return 0;
	struct event_path *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e)
		return 0;
	fill_header(&e->h, EV_UNLINK, root);
	// from holds the removed name alone; to, its directory.
	bpf_probe_read_kernel_str(e->from.name[0], NAME_LEN, BPF_CORE_READ(dentry, d_name.name));
	e->from.depth = 1;
	walk(BPF_CORE_READ(dir, dentry), BPF_CORE_READ(dir, mnt), &e->to);
	bpf_ringbuf_submit(e, 0);
	return 0;
}

SEC("fentry/security_path_rename")
int BPF_PROG(path_rename, const struct path *old_dir, struct dentry *old_dentry,
	     const struct path *new_dir, struct dentry *new_dentry)
{
	u32 root = root_of_current();
	if (!root)
		return 0;
	struct event_path *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e)
		return 0;
	fill_header(&e->h, EV_RENAME, root);
	walk(old_dentry, BPF_CORE_READ(old_dir, mnt), &e->from);
	walk(new_dentry, BPF_CORE_READ(new_dir, mnt), &e->to);
	bpf_ringbuf_submit(e, 0);
	return 0;
}

static __always_inline int enforcing(void)
{
	u32 zero = 0;
	u32 *on = bpf_map_lookup_elem(&sensor_config, &zero);
	return on && *on;
}

SEC("lsm/socket_connect")
int BPF_PROG(deny_connect, struct socket *sock, struct sockaddr *address, int addrlen, int ret)
{
	if (ret)
		return ret;
	u32 root = root_of_current();
	if (!root)
		return 0;
	struct ip_key k = {};
	u16 port = 0;
	k.family = BPF_CORE_READ(address, sa_family);
	if (k.family == AF_INET) {
		struct sockaddr_in *in = (struct sockaddr_in *)address;
		u32 a = BPF_CORE_READ(in, sin_addr.s_addr);
		__builtin_memcpy(k.addr, &a, 4);
		port = bpf_ntohs(BPF_CORE_READ(in, sin_port));
	} else if (k.family == AF_INET6) {
		struct sockaddr_in6 *in6 = (struct sockaddr_in6 *)address;
		BPF_CORE_READ_INTO(&k.addr, in6, sin6_addr.in6_u.u6_addr8);
		port = bpf_ntohs(BPF_CORE_READ(in6, sin6_port));
	} else {
		return 0;
	}
	u32 *rule = bpf_map_lookup_elem(&deny_ips, &k);
	if (!rule)
		return 0;
	int enforce = enforcing();
	struct event_block *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (e) {
		fill_header(&e->h, EV_BLOCK, root);
		e->kind = 1;
		e->rule = *rule;
		e->enforced = enforce;
		e->family = k.family;
		e->port = port;
		__builtin_memcpy(e->addr, k.addr, 16);
		e->path[0] = 0;
		bpf_ringbuf_submit(e, 0);
	}
	return enforce ? -EPERM : 0;
}

SEC("lsm/file_open")
int BPF_PROG(deny_open, struct file *file, int ret)
{
	if (ret)
		return ret;
	u32 root = root_of_current();
	if (!root)
		return 0;
	struct inode_key k = {};
	k.dev = BPF_CORE_READ(file, f_inode, i_sb, s_dev);
	struct dentry *d = BPF_CORE_READ(file, f_path.dentry);
	u32 *rule = 0;
	for (int i = 0; i < PATH_DEPTH && !rule; i++) {
		k.ino = BPF_CORE_READ(d, d_inode, i_ino);
		rule = bpf_map_lookup_elem(&deny_inodes, &k);
		struct dentry *up = BPF_CORE_READ(d, d_parent);
		if (up == d)
			break;
		d = up;
	}
	if (!rule)
		return 0;
	int enforce = enforcing();
	struct event_block *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (e) {
		fill_header(&e->h, EV_BLOCK, root);
		e->kind = 2;
		e->rule = *rule;
		e->enforced = enforce;
		e->family = 0;
		e->port = 0;
		__builtin_memset(e->addr, 0, sizeof(e->addr));
		bpf_probe_read_kernel_str(e->path, sizeof(e->path), BPF_CORE_READ(file, f_path.dentry, d_name.name));
		bpf_ringbuf_submit(e, 0);
	}
	return enforce ? -EPERM : 0;
}

#define ETH_P_IP 0x0800
#define ETH_P_IPV6 0x86DD
#define IPPROTO_TCP 6
#define IPPROTO_UDP 17

// names_filter keeps the packets that name hosts, for the capture socket: DNS answers, TLS
// ClientHellos and QUIC long-header packets. Every other packet is dropped in the kernel.
SEC("socket")
int names_filter(struct __sk_buff *skb)
{
	u16 proto;
	if (bpf_skb_load_bytes(skb, 12, &proto, 2))
		return 0;
	proto = bpf_ntohs(proto);
	u32 off = 14;
	u8 l4;
	if (proto == ETH_P_IP) {
		u8 vihl;
		if (bpf_skb_load_bytes(skb, off, &vihl, 1) || bpf_skb_load_bytes(skb, off + 9, &l4, 1))
			return 0;
		off += (vihl & 0x0f) * 4;
	} else if (proto == ETH_P_IPV6) {
		if (bpf_skb_load_bytes(skb, off + 6, &l4, 1))
			return 0;
		off += 40;
	} else {
		return 0;
	}
	u16 ports[2];
	if (bpf_skb_load_bytes(skb, off, ports, 4))
		return 0;
	u16 sport = bpf_ntohs(ports[0]), dport = bpf_ntohs(ports[1]);
	if (l4 == IPPROTO_UDP) {
		if (sport == 53)
			return skb->len;
		u8 first;
		if (dport == 443 && !bpf_skb_load_bytes(skb, off + 8, &first, 1) && (first & 0xc0) == 0xc0)
			return skb->len;
		return 0;
	}
	if (l4 != IPPROTO_TCP)
		return 0;
	if (sport == 53)
		return skb->len;
	u8 doff;
	if (bpf_skb_load_bytes(skb, off + 12, &doff, 1))
		return 0;
	off += (doff >> 4) * 4;
	u8 hello[6];
	if (bpf_skb_load_bytes(skb, off, hello, 6))
		return 0;
	if (hello[0] == 0x16 && hello[5] == 0x01)
		return skb->len;
	return 0;
}

// In-kernel DNS: a host rule denies the addresses a DNS answer gives the host, or any name
// under it, before the answer reaches the socket that asked, so the asker never holds an
// address the deny map lacks. Userspace fills deny_hosts with dns_hash of each rule's
// host in wire form (length-prefixed labels, lowercase); the program hashes every suffix
// of the question's name the same way, in one backward pass.
#define DNS_NAME_MAX 128
#define DNS_LABELS 16
#define DNS_ANSWERS 8
#define DNS_HASH_P 1099511628211ULL

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 1024);
	__type(key, u64);
	__type(value, u32);
} deny_hosts SEC(".maps");

struct dns_scratch {
	u8 name[DNS_NAME_MAX];
};

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, 1);
	__type(key, u32);
	__type(value, struct dns_scratch);
} dns_scratch SEC(".maps");

struct dns_walk {
	u64 h;
	u32 end;
	int k;
	u32 rule;
	u32 starts[DNS_LABELS];
};

// dns_suffix takes the name's bytes from the last back, one per call.
static long dns_suffix(u64 i, void *data)
{
	struct dns_walk *w = data;
	u32 zero = 0;
	struct dns_scratch *s = bpf_map_lookup_elem(&dns_scratch, &zero);
	if (!s)
		return 1;
	u32 idx = w->end - 1 - (u32)i;
	if (idx >= DNS_NAME_MAX)
		return 1;
	u8 c = s->name[idx & (DNS_NAME_MAX - 1)];
	if (c >= 'A' && c <= 'Z')
		c += 32;
	w->h = w->h * DNS_HASH_P + c;
	int k = w->k;
	if (k >= 0 && k < DNS_LABELS && w->starts[k] == idx) {
		u64 h = w->h;
		u32 *r = bpf_map_lookup_elem(&deny_hosts, &h);
		if (r && !w->rule)
			w->rule = *r;
		w->k = k - 1;
	}
	return 0;
}

SEC("cgroup_skb/ingress")
int dns_ingress(struct __sk_buff *skb)
{
	u8 vihl, proto;
	u32 off;
	if (bpf_skb_load_bytes(skb, 0, &vihl, 1))
		return 1;
	if ((vihl >> 4) == 4) {
		off = (vihl & 0x0f) * 4;
		if (bpf_skb_load_bytes(skb, 9, &proto, 1))
			return 1;
	} else if ((vihl >> 4) == 6) {
		off = 40;
		if (bpf_skb_load_bytes(skb, 6, &proto, 1))
			return 1;
	} else {
		return 1;
	}
	if (proto != IPPROTO_UDP)
		return 1;
	u16 sport;
	if (bpf_skb_load_bytes(skb, off, &sport, 2) || bpf_ntohs(sport) != 53)
		return 1;
	off += 8;
	u8 hdr[12];
	if (bpf_skb_load_bytes(skb, off, hdr, 12) || !(hdr[2] & 0x80))
		return 1;
	u16 qd = (u16)hdr[4] << 8 | hdr[5], an = (u16)hdr[6] << 8 | hdr[7];
	if (qd != 1 || an == 0)
		return 1;
	u32 qoff = off + 12;
	if (qoff + 2 > skb->len)
		return 1;
	// The name is read whole into scratch, at the largest of a few fixed sizes the packet
	// holds: the verifier proves a constant size where clang's rewrite of a variable one
	// defeats it. A long name in a short answer is skipped, never misread.
	u32 left = skb->len - qoff, avail;
	u32 zero = 0;
	struct dns_scratch *s = bpf_map_lookup_elem(&dns_scratch, &zero);
	if (!s)
		return 1;
	if (left >= 128) {
		avail = 128;
		if (bpf_skb_load_bytes(skb, qoff, s->name, 128))
			return 1;
	} else if (left >= 64) {
		avail = 64;
		if (bpf_skb_load_bytes(skb, qoff, s->name, 64))
			return 1;
	} else if (left >= 32) {
		avail = 32;
		if (bpf_skb_load_bytes(skb, qoff, s->name, 32))
			return 1;
	} else if (left >= 16) {
		avail = 16;
		if (bpf_skb_load_bytes(skb, qoff, s->name, 16))
			return 1;
	} else {
		return 1;
	}

	// Forward: where each label starts, and where the name ends.
	u32 starts[DNS_LABELS] = {};
	int nl = 0;
	u32 pos = 0, end = 0;
	bool done = false;
	for (int i = 0; i < DNS_LABELS; i++) {
		if (pos >= avail)
			break;
		u8 l = s->name[pos & (DNS_NAME_MAX - 1)];
		if (l == 0) {
			end = pos;
			done = true;
			break;
		}
		if (l > 63)
			return 1; // a pointer: not in a question
		starts[i] = pos;
		nl = i + 1;
		pos += l + 1;
	}
	if (!done || nl == 0)
		return 1;

	// Backward: hash each suffix, lowercase, and look it up at each label's start. A
	// bpf_loop callback is verified once, where an unrolled pass over the name would not
	// fit the verifier's budget.
	struct dns_walk w = {.end = end, .k = nl - 1};
	for (int i = 0; i < DNS_LABELS; i++)
		w.starts[i] = starts[i];
	bpf_loop(end, dns_suffix, &w, 0);
	u32 rule = w.rule;
	if (!rule)
		return 1;

	// Every address the answer gives, CNAME chains included, is denied.
	u32 a = qoff + end + 1 + 4;
	for (int i = 0; i < DNS_ANSWERS && i < an; i++) {
		u8 b;
		if (bpf_skb_load_bytes(skb, a, &b, 1))
			break;
		// The owner's name: a pointer, or labels (some resolvers do not compress them),
		// perhaps ending in a pointer.
		for (int j = 0; j < 12; j++) {
			if ((b & 0xc0) == 0xc0) {
				a += 2;
				break;
			}
			a += 1 + b;
			if (b == 0)
				break;
			if (bpf_skb_load_bytes(skb, a, &b, 1))
				return 1;
		}
		u8 rr[10];
		if (bpf_skb_load_bytes(skb, a, rr, 10))
			break;
		u16 type = (u16)rr[0] << 8 | rr[1], rdlen = (u16)rr[8] << 8 | rr[9];
		a += 10;
		struct ip_key key = {};
		if (type == 1 && rdlen == 4) {
			key.family = AF_INET;
			if (!bpf_skb_load_bytes(skb, a, key.addr, 4))
				bpf_map_update_elem(&deny_ips, &key, &rule, BPF_ANY);
		} else if (type == 28 && rdlen == 16) {
			key.family = AF_INET6;
			if (!bpf_skb_load_bytes(skb, a, key.addr, 16))
				bpf_map_update_elem(&deny_ips, &key, &rule, BPF_ANY);
		}
		a += rdlen;
	}
	return 1;
}

// Keeps the event types in the object's BTF, where bpf2go reads them for the Go side.
const struct header *unused_header __attribute__((used));
const struct event_exec *unused_exec __attribute__((used));
const struct event_flow *unused_flow __attribute__((used));
const struct event_file *unused_file __attribute__((used));
const struct event_path *unused_path __attribute__((used));
const struct event_block *unused_block __attribute__((used));
