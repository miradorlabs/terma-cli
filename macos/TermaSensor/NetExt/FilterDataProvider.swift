// The content filter: it allows everything a policy does not deny, and reports the
// connections of watched sessions (a root the sensor names, and everything under it) with
// their bytes. Connections outside a watched tree are allowed without a report and leave
// nothing behind.
import Foundation
import Network
import NetworkExtension

@objc(FilterDataProvider)
final class FilterDataProvider: NEFilterDataProvider {
    private let lock = NSLock()
    private var roots: Set<pid_t> = []
    private var denyHosts: [String] = []
    private var denyIPs: Set<String> = []
    private var enforce = false
    private var flows: [UUID: [String: Any]] = [:]
    // The bytes a watched TCP connection has carried so far, counted as they pass; the
    // close report's counts are the final ones.
    private var counts: [UUID: (tx: Int, rx: Int, sent: Int)] = [:]
    private var link: Link!
    private var timer: DispatchSourceTimer?

    override func startFilter(completionHandler: @escaping (Error?) -> Void) {
        link = Link(hello: ["k": "hello", "ext": "network", "pid": getpid()]) { [weak self] cfg in
            self?.configure(cfg)
        }
        let t = DispatchSource.makeTimerSource(queue: .global())
        t.schedule(deadline: .now() + 1, repeating: 1)
        t.setEventHandler { [weak self] in self?.reportLive() }
        t.resume()
        timer = t
        let settings = NEFilterSettings(rules: [], defaultAction: .filterData)
        apply(settings) { error in completionHandler(error) }
    }

    override func stopFilter(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        completionHandler()
    }

    private func configure(_ cfg: [String: Any]) {
        lock.lock()
        defer { lock.unlock() }
        if let r = cfg["roots"] as? [Int] { roots = Set(r.map { pid_t($0) }) }
        if let p = cfg["policy"] as? [String: Any] {
            enforce = p["enforce"] as? Bool ?? false
            denyHosts = (p["hosts"] as? [String] ?? []).map { $0.lowercased() }
            denyIPs = Set(p["ips"] as? [String] ?? [])
        }
    }

    override func handleNewFlow(_ flow: NEFilterFlow) -> NEFilterNewFlowVerdict {
        guard let socket = flow as? NEFilterSocketFlow,
              let token = flow.sourceProcessAuditToken ?? flow.sourceAppAuditToken else {
            return .allow()
        }
        let pid = auditPID(token)
        let appPID = flow.sourceAppAuditToken.map(auditPID) ?? pid
        lock.lock()
        let root = Proc.ancestors(of: pid).first(where: roots.contains)
            ?? Proc.ancestors(of: appPID).first(where: roots.contains)
        let (hosts, ips, enforcing) = (denyHosts, denyIPs, enforce)
        lock.unlock()
        guard let root else { return .allow() }

        let (remote, local) = endpoints(socket)
        let host = socket.remoteHostname ?? ""
        let ip = remote.split(separator: ":").dropLast().joined(separator: ":")
        var event: [String: Any] = [
            "k": "flow_open", "id": flow.identifier.uuidString, "pid": pid, "app_pid": appPID,
            "root": root, "path": Proc.path(of: pid), "host": host, "remote": remote, "local": local,
            "proto": socket.socketProtocol == IPPROTO_UDP ? "udp" : "tcp", "t": now(),
        ]
        let deniedHost = !host.isEmpty && hosts.contains { host == $0 || host.hasSuffix("." + $0) }
        if deniedHost || ips.contains(ip) {
            event["k"] = "flow_block"
            event["enforced"] = enforcing
            link.send(event)
            if enforcing { return .drop() }
        } else {
            link.send(event)
        }
        lock.lock()
        flows[flow.identifier] = ["root": root, "pid": pid]
        lock.unlock()
        // A watched TCP connection's data passes through here, both ways: its first bytes
        // are the TLS ClientHello, which names the host for a program that connected to an
        // address it resolved itself, and the rest is counted as it goes.
        let verdict: NEFilterNewFlowVerdict
        if socket.socketProtocol == IPPROTO_TCP {
            verdict = .filterDataVerdict(withFilterInbound: true, peekInboundBytes: peek,
                                         filterOutbound: true, peekOutboundBytes: peek)
            lock.lock()
            counts[flow.identifier] = (0, 0, 0)
            lock.unlock()
        } else {
            verdict = .allow()
        }
        verdict.shouldReport = true
        verdict.statisticsReportFrequency = .medium
        return verdict
    }

    private let peek = 64 * 1024

    override func handleOutboundData(from flow: NEFilterFlow, readBytesStartOffset offset: Int,
                                     readBytes: Data) -> NEFilterDataVerdict {
        if offset == 0, readBytes.first == 0x16 {
            link.send(["k": "flow_hello", "id": flow.identifier.uuidString,
                       "hello": readBytes.prefix(1024).base64EncodedString()])
            // The hello names the host whatever address it was resolved to: a host rule
            // holds here, where a CDN's rotating addresses would slip past one by address.
            if let host = serverName(readBytes) {
                lock.lock()
                let (hosts, enforcing, known) = (denyHosts, enforce, flows[flow.identifier])
                lock.unlock()
                if hosts.contains(where: { host == $0 || host.hasSuffix("." + $0) }) {
                    var event: [String: Any] = ["k": "flow_block", "id": flow.identifier.uuidString, "host": host,
                                                "enforced": enforcing, "t": now()]
                    if let known { event.merge(known) { a, _ in a } }
                    link.send(event)
                    if enforcing { return .drop() }
                }
            }
        }
        return pass(flow, readBytes.count, outbound: true)
    }

    override func handleInboundData(from flow: NEFilterFlow, readBytesStartOffset offset: Int,
                                    readBytes: Data) -> NEFilterDataVerdict {
        pass(flow, readBytes.count, outbound: false)
    }

    // pass counts n bytes and lets them through, asking to see what follows.
    private func pass(_ flow: NEFilterFlow, _ n: Int, outbound: Bool) -> NEFilterDataVerdict {
        lock.lock()
        if var c = counts[flow.identifier] {
            if outbound { c.tx += n } else { c.rx += n }
            counts[flow.identifier] = c
        }
        lock.unlock()
        let verdict = NEFilterDataVerdict(passBytes: n, peekBytes: peek)
        verdict.shouldReport = true
        return verdict
    }

    override func handleOutboundDataComplete(for flow: NEFilterFlow) -> NEFilterDataVerdict { .allow() }
    override func handleInboundDataComplete(for flow: NEFilterFlow) -> NEFilterDataVerdict { .allow() }

    // reportLive sends the counts of the connections that moved since the last report.
    private func reportLive() {
        lock.lock()
        var events: [[String: Any]] = []
        for (id, c) in counts where c.tx + c.rx != c.sent {
            counts[id]?.sent = c.tx + c.rx
            var e: [String: Any] = ["k": "flow_stats", "id": id.uuidString, "tx": c.tx, "rx": c.rx, "t": now()]
            if let known = flows[id] { e.merge(known) { a, _ in a } }
            events.append(e)
        }
        lock.unlock()
        events.forEach(link.send)
    }

    override func handle(_ report: NEFilterReport) {
        guard let flow = report.flow else { return }
        let kind: String
        switch report.event {
        case .flowClosed: kind = "flow_close"
        case .statistics: return // carries no bytes; reportLive counts them
        default: return
        }
        lock.lock()
        let known = flows[flow.identifier]
        let counted = counts[flow.identifier]
        if report.event == .flowClosed {
            flows[flow.identifier] = nil
            counts[flow.identifier] = nil
        }
        lock.unlock()
        guard let known else { return }
        // A connection whose data passed through here reports, at close, only part of its
        // bytes; what was counted on the way is the truer total then.
        var event: [String: Any] = [
            "k": kind, "id": flow.identifier.uuidString,
            "tx": max(report.bytesOutboundCount, counted?.tx ?? 0),
            "rx": max(report.bytesInboundCount, counted?.rx ?? 0), "t": now(),
        ]
        event.merge(known) { a, _ in a }
        link.send(event)
    }

    private func endpoints(_ flow: NEFilterSocketFlow) -> (String, String) {
        func render(_ e: Network.NWEndpoint?) -> String {
            guard case let .hostPort(host, port) = e else { return "" }
            var h = "\(host)"
            if let pct = h.firstIndex(of: "%") { h = String(h[..<pct]) } // drop an IPv6 zone
            return h.contains(":") ? "[\(h)]:\(port.rawValue)" : "\(h):\(port.rawValue)"
        }
        return (render(flow.remoteFlowEndpoint), render(flow.localFlowEndpoint))
    }

    // serverName is the server_name a TLS ClientHello record asks for.
    private func serverName(_ d: Data) -> String? {
        let b = [UInt8](d.prefix(4096))
        guard b.count > 43, b[0] == 0x16, b[5] == 0x01 else { return nil }
        var p = 43
        func skip(_ n: Int) -> Bool {
            guard p + n <= b.count else { return false }
            var l = 0
            for i in 0..<n { l = l << 8 | Int(b[p + i]) }
            p += n + l
            return p <= b.count
        }
        guard skip(1), skip(2), skip(1), p + 2 <= b.count else { return nil } // session id, suites, compression
        let end = min(p + 2 + (Int(b[p]) << 8 | Int(b[p + 1])), b.count)
        p += 2
        while p + 4 <= end {
            let typ = Int(b[p]) << 8 | Int(b[p + 1]), n = Int(b[p + 2]) << 8 | Int(b[p + 3])
            p += 4
            if typ == 0, p + 5 <= end, b[p + 2] == 0 {
                let l = Int(b[p + 3]) << 8 | Int(b[p + 4])
                guard p + 5 + l <= end else { return nil }
                return String(decoding: b[(p + 5)..<(p + 5 + l)], as: UTF8.self).lowercased()
            }
            p += n
        }
        return nil
    }

    private func auditPID(_ token: Data) -> pid_t {
        guard token.count == MemoryLayout<audit_token_t>.size else { return 0 }
        let t = token.withUnsafeBytes { $0.load(as: audit_token_t.self) }
        return audit_token_to_pid(t)
    }
}
