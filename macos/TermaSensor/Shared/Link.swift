// Link is an extension's line to the terma sensor on 127.0.0.1: JSON events out, one per
// line, and the sensor's configuration (roots, policy) in. It reconnects for as long as the
// extension runs, and drops events while the sensor is away: nothing is kept on disk.
import Foundation
import Network

final class Link {
    static let port: NWEndpoint.Port = 7781

    private let queue = DispatchQueue(label: "terma.link")
    private var conn: NWConnection?
    private var ready = false
    private var inbox = Data()
    private let onConfig: ([String: Any]) -> Void
    private let hello: [String: Any]

    init(hello: [String: Any], onConfig: @escaping ([String: Any]) -> Void) {
        self.hello = hello
        self.onConfig = onConfig
        queue.async { self.connect() }
    }

    func send(_ event: [String: Any]) {
        guard let line = try? JSONSerialization.data(withJSONObject: event) else { return }
        queue.async {
            guard self.ready, let conn = self.conn else { return }
            conn.send(content: line + Data([0x0a]), completion: .contentProcessed { _ in })
        }
    }

    private func connect() {
        let c = NWConnection(host: "127.0.0.1", port: Link.port, using: .tcp)
        conn = c
        c.stateUpdateHandler = { [weak self] state in
            guard let self else { return }
            switch state {
            case .ready:
                self.ready = true
                self.send(self.hello)
                self.receive(c)
            case .failed, .cancelled, .waiting:
                self.ready = false
                c.cancel()
                self.queue.asyncAfter(deadline: .now() + 2) {
                    if self.conn === c { self.connect() }
                }
            default:
                break
            }
        }
        c.start(queue: queue)
    }

    private func receive(_ c: NWConnection) {
        c.receive(minimumIncompleteLength: 1, maximumLength: 1 << 16) { [weak self] data, _, done, error in
            guard let self else { return }
            if let data { self.inbox.append(data) }
            while let nl = self.inbox.firstIndex(of: 0x0a) {
                let line = self.inbox[self.inbox.startIndex..<nl]
                self.inbox.removeSubrange(self.inbox.startIndex...nl)
                if let obj = try? JSONSerialization.jsonObject(with: line) as? [String: Any] {
                    self.onConfig(obj)
                }
            }
            if done || error != nil {
                c.cancel()
                return
            }
            self.receive(c)
        }
    }
}

// Proc answers what the kernel knows about a process: its parent and executable.
enum Proc {
    static func parent(of pid: pid_t) -> pid_t? {
        var info = kinfo_proc()
        var size = MemoryLayout<kinfo_proc>.stride
        var mib: [Int32] = [CTL_KERN, KERN_PROC, KERN_PROC_PID, pid]
        guard sysctl(&mib, 4, &info, &size, nil, 0) == 0, size > 0 else { return nil }
        return info.kp_eproc.e_ppid
    }

    static func path(of pid: pid_t) -> String {
        var buf = [CChar](repeating: 0, count: Int(MAXPATHLEN) * 4)
        let n = proc_pidpath(pid, &buf, UInt32(buf.count))
        return n > 0 ? String(cString: buf) : ""
    }

    // ancestors is pid and its parents up to launchd.
    static func ancestors(of pid: pid_t) -> [pid_t] {
        var out: [pid_t] = [pid]
        var p = pid
        while out.count < 64, let pp = parent(of: p), pp > 1, pp != p {
            out.append(pp)
            p = pp
        }
        return out
    }
}

func now() -> Int64 { Int64(Date().timeIntervalSince1970 * 1000) }
