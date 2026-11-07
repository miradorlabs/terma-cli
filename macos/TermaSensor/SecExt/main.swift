// The Endpoint Security extension. One client follows every fork and exec to know which
// processes belong to a watched session (an exec of a root executable outside any session
// starts one, here, before it can fork). A second client, with process muting inverted, is
// told only about those processes: it reports their files and, under a policy, answers
// their opens. Nothing outside a watched tree reaches it.
import EndpointSecurity
import Foundation

final class Sensor {
    private var life: OpaquePointer?
    private var files: OpaquePointer?
    private var link: Link!
    private let lock = NSLock()
    private var rootNames: Set<String> = []
    private var watched: [pid_t: pid_t] = [:] // pid -> its session's root
    // exited keeps an exited process's root a while: the two clients are not ordered, so
    // its last file events can arrive after its exit.
    private var exited: [pid_t: (root: pid_t, at: Date)] = [:]
    private var denyPaths: [String] = []
    private var enforce = false
    private var seen: [String: Int] = [:]
    private var timer: DispatchSourceTimer?

    func start() {
        // A heartbeat for the spike: what the client has seen, and what it was told.
        let t = DispatchSource.makeTimerSource(queue: .global())
        t.schedule(deadline: .now() + 5, repeating: 5)
        t.setEventHandler { [weak self] in
            guard let self else { return }
            self.lock.lock()
            let beat: [String: Any] = ["k": "beat", "seen": self.seen, "root_names": Array(self.rootNames), "watched": self.watched.count]
            self.lock.unlock()
            self.link.send(beat)
        }
        t.resume()
        timer = t
        link = Link(hello: ["k": "hello", "ext": "security", "pid": getpid()]) { [weak self] cfg in
            self?.configure(cfg)
        }
        files = newClient([
            ES_EVENT_TYPE_NOTIFY_CLOSE, ES_EVENT_TYPE_NOTIFY_CREATE, ES_EVENT_TYPE_NOTIFY_RENAME,
            ES_EVENT_TYPE_NOTIFY_UNLINK, ES_EVENT_TYPE_AUTH_OPEN,
        ], invert: true)
        // Exec as an authorization: the new image waits for the answer, so a watched one is
        // known to the files client before it can open anything.
        life = newClient([ES_EVENT_TYPE_NOTIFY_FORK, ES_EVENT_TYPE_AUTH_EXEC, ES_EVENT_TYPE_NOTIFY_EXIT], invert: false)
    }

    // newClient subscribes a client to events. An inverted one hears only about the
    // processes muted for it, which watch() adds.
    private func newClient(_ events: [es_event_type_t], invert: Bool) -> OpaquePointer {
        var c: OpaquePointer?
        let res = es_new_client(&c) { [weak self] client, msg in
            self?.handle(client, msg)
        }
        guard res == ES_NEW_CLIENT_RESULT_SUCCESS, let c else {
            NSLog("terma: es_new_client failed: \(res.rawValue)")
            exit(1)
        }
        if invert { es_invert_muting(c, ES_MUTE_INVERSION_TYPE_PROCESS) }
        es_subscribe(c, events, UInt32(events.count))
        return c
    }

    // watch tells the files client about a watched process, by its audit token: a token
    // names one process image, so an exec needs its own.
    private func watch(_ token: audit_token_t) {
        guard let files else { return }
        var t = token
        es_mute_process(files, &t)
    }

    private func unwatch(_ token: audit_token_t) {
        guard let files else { return }
        var t = token
        es_unmute_process(files, &t)
    }

    private func configure(_ cfg: [String: Any]) {
        lock.lock()
        defer { lock.unlock() }
        if let names = cfg["root_names"] as? [String] { rootNames = Set(names) }
        if let p = cfg["policy"] as? [String: Any] {
            enforce = p["enforce"] as? Bool ?? false
            denyPaths = p["paths"] as? [String] ?? []
        }
        if let adopt = cfg["adopt"] as? [Int] {
            for pid in adopt { watched[pid_t(pid)] = pid_t(pid) }
        }
    }

    private func root(_ pid: pid_t) -> pid_t? {
        lock.lock()
        defer { lock.unlock() }
        return watched[pid] ?? exited[pid]?.root
    }

    private func handle(_ client: OpaquePointer, _ msg: UnsafePointer<es_message_t>) {
        let m = msg.pointee
        let proc = m.process.pointee
        let pid = audit_token_to_pid(proc.audit_token)
        lock.lock()
        seen[String(m.event_type.rawValue), default: 0] += 1
        lock.unlock()
        switch m.event_type {
        case ES_EVENT_TYPE_AUTH_OPEN:
            authOpen(client, msg, pid)
        case ES_EVENT_TYPE_NOTIFY_FORK:
            let childToken = m.event.fork.child.pointee.audit_token
            let child = audit_token_to_pid(childToken)
            lock.lock()
            let r = watched[pid]
            if let r { watched[child] = r }
            lock.unlock()
            if r != nil { watch(childToken) }
            if let r { link.send(["k": "fork", "pid": child, "ppid": pid, "root": r, "t": now()]) }
        case ES_EVENT_TYPE_AUTH_EXEC:
            defer { es_respond_auth_result(client, msg, ES_AUTH_RESULT_ALLOW, false) }
            let target = m.event.exec.target.pointee
            var path = string(target.executable.pointee.path)
            var e = m.event.exec
            var args: [String] = []
            for i in 0..<min(es_exec_arg_count(&e), 64) {
                args.append(string(es_exec_arg(&e, i)))
            }
            // A script's exec names its interpreter; the script, or an interpreter's first
            // argument (`env node …/claude`), is what a root is known by.
            if let script = e.script { path = string(script.pointee.path) }
            let names = ([path] + args.prefix(2)).map { ($0 as NSString).lastPathComponent }
            lock.lock()
            var r = watched[pid]
            if r == nil, names.contains(where: rootNames.contains) {
                watched[pid] = pid
                r = pid
            }
            lock.unlock()
            guard let r else { return }
            watch(target.audit_token)
            link.send(["k": "exec", "pid": pid, "ppid": proc.ppid, "root": r, "path": path, "args": args, "t": now()])
        case ES_EVENT_TYPE_NOTIFY_EXIT:
            lock.lock()
            let r = watched.removeValue(forKey: pid)
            if let r { exited[pid] = (r, Date()) }
            exited = exited.filter { Date().timeIntervalSince($0.value.at) < 30 }
            lock.unlock()
            if let r {
                unwatch(proc.audit_token)
                link.send(["k": "exit", "pid": pid, "root": r, "t": now()])
            }
        default:
            guard let r = root(pid) else { return }
            var event: [String: Any] = ["pid": pid, "root": r, "comm": (string(proc.executable.pointee.path) as NSString).lastPathComponent, "t": now()]
            switch m.event_type {
            case ES_EVENT_TYPE_NOTIFY_CLOSE:
                let c = m.event.close
                event["k"] = "close"
                event["path"] = string(c.target.pointee.path)
                event["modified"] = c.modified
                event["size"] = c.target.pointee.stat.st_size
            case ES_EVENT_TYPE_NOTIFY_CREATE:
                let c = m.event.create
                event["k"] = "create"
                if c.destination_type == ES_DESTINATION_TYPE_EXISTING_FILE {
                    event["path"] = string(c.destination.existing_file.pointee.path)
                } else {
                    let np = c.destination.new_path
                    event["path"] = string(np.dir.pointee.path) + "/" + string(np.filename)
                }
            case ES_EVENT_TYPE_NOTIFY_RENAME:
                let rn = m.event.rename
                event["k"] = "rename"
                event["path"] = string(rn.source.pointee.path)
                if rn.destination_type == ES_DESTINATION_TYPE_EXISTING_FILE {
                    event["to"] = string(rn.destination.existing_file.pointee.path)
                } else {
                    let np = rn.destination.new_path
                    event["to"] = string(np.dir.pointee.path) + "/" + string(np.filename)
                }
            case ES_EVENT_TYPE_NOTIFY_UNLINK:
                event["k"] = "unlink"
                event["path"] = string(m.event.unlink.target.pointee.path)
            default:
                return
            }
            link.send(event)
        }
    }

    // authOpen checks a watched process's open against the policy. Only watched processes
    // are muted for this client, so an unwatched one never asks; no answer is cached, as
    // the kernel caches per executable and file, not per process.
    private func authOpen(_ client: OpaquePointer, _ msg: UnsafePointer<es_message_t>, _ pid: pid_t) {
        guard let r = root(pid) else {
            es_respond_flags_result(client, msg, UInt32.max, false)
            return
        }
        let path = string(msg.pointee.event.open.file.pointee.path)
        lock.lock()
        let denied = denyPaths.contains { path == $0 || path.hasPrefix($0.hasSuffix("/") ? $0 : $0 + "/") }
        let enforcing = enforce
        lock.unlock()
        if denied {
            link.send(["k": "open_block", "pid": pid, "root": r, "path": path, "enforced": enforcing, "t": now()])
            es_respond_flags_result(client, msg, enforcing ? 0 : UInt32.max, false)
            return
        }
        es_respond_flags_result(client, msg, UInt32.max, false)
    }

    private func string(_ t: es_string_token_t) -> String {
        guard t.length > 0, let p = t.data else { return "" }
        return String(decoding: UnsafeRawBufferPointer(start: p, count: t.length), as: UTF8.self)
    }
}

let sensor = Sensor()
sensor.start()
dispatchMain()
