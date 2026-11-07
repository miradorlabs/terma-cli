// TermaSensor.app turns the sensor's two system extensions on and off: the content filter
// that counts watched sessions' connections, and the Endpoint Security client that follows
// their processes and files. `terma sensor enable|disable` would start it; for the spike:
//   open TermaSensor.app --args enable | disable | status
import Foundation
import NetworkExtension
import SystemExtensions

let appID = Bundle.main.bundleIdentifier ?? "org.mirador.terma.sensor"
let netExt = appID + ".network"
let secExt = appID + ".security"

final class Activator: NSObject, OSSystemExtensionRequestDelegate {
    private var pending = 0
    private let done: () -> Void

    init(done: @escaping () -> Void) { self.done = done }

    func submit(_ id: String, activate: Bool) {
        pending += 1
        let req = activate
            ? OSSystemExtensionRequest.activationRequest(forExtensionWithIdentifier: id, queue: .main)
            : OSSystemExtensionRequest.deactivationRequest(forExtensionWithIdentifier: id, queue: .main)
        req.delegate = self
        OSSystemExtensionManager.shared.submitRequest(req)
    }

    func request(_ request: OSSystemExtensionRequest, actionForReplacingExtension existing: OSSystemExtensionProperties,
                 withExtension ext: OSSystemExtensionProperties) -> OSSystemExtensionRequest.ReplacementAction {
        log("replacing \(existing.bundleShortVersion) with \(ext.bundleShortVersion)")
        return .replace
    }

    func requestNeedsUserApproval(_ request: OSSystemExtensionRequest) {
        log("\(request.identifier): approve it in System Settings > General > Login Items & Extensions")
    }

    func request(_ request: OSSystemExtensionRequest, didFinishWithResult result: OSSystemExtensionRequest.Result) {
        log("\(request.identifier): \(result == .completed ? "done" : "done after a reboot")")
        finish()
    }

    func request(_ request: OSSystemExtensionRequest, didFailWithError error: Error) {
        log("\(request.identifier) failed: \(error.localizedDescription)")
        finish()
    }

    private func finish() {
        pending -= 1
        if pending == 0 { done() }
    }
}

func log(_ s: String) {
    FileHandle.standardError.write(Data(("terma-sensor-app: " + s + "\n").utf8))
    NSLog("terma-sensor-app: %@", s)
}

// configureFilter switches the content filter on or off. It is an inspector: it sees
// connections after any firewall-grade filter has had its say.
func configureFilter(enabled: Bool, then: @escaping () -> Void) {
    let manager = NEFilterManager.shared()
    manager.loadFromPreferences { error in
        if let error { log("load filter: \(error.localizedDescription)") }
        if enabled {
            let cfg = NEFilterProviderConfiguration()
            cfg.filterSockets = true
            cfg.filterPackets = false
            cfg.filterDataProviderBundleIdentifier = netExt
            manager.providerConfiguration = cfg
            manager.localizedDescription = "terma sensor"
            manager.grade = .inspector
        }
        manager.isEnabled = enabled
        manager.saveToPreferences { error in
            log(error.map { "save filter: \($0.localizedDescription)" } ?? "filter \(enabled ? "on" : "off")")
            then()
        }
    }
}

// keep holds the request delegate for as long as the requests run: they hold it weakly.
var keep: Activator?

let cmd = CommandLine.arguments.dropFirst().first ?? "status"
switch cmd {
case "reset": // drop the filter configuration, with the code requirement it recorded, and enable anew
    NEFilterManager.shared().loadFromPreferences { _ in
        NEFilterManager.shared().removeFromPreferences { error in
            log(error.map { "remove filter: \($0.localizedDescription)" } ?? "filter configuration removed")
            keep = Activator { configureFilter(enabled: true) { exit(0) } }
            keep?.submit(secExt, activate: true)
            keep?.submit(netExt, activate: true)
        }
    }
    dispatchMain()
case "enable":
    let a = Activator { configureFilter(enabled: true) { exit(0) } }
    a.submit(secExt, activate: true)
    a.submit(netExt, activate: true)
    withExtendedLifetime(a) { dispatchMain() }
case "disable":
    configureFilter(enabled: false) {
        keep = Activator { exit(0) }
        keep?.submit(netExt, activate: false)
        keep?.submit(secExt, activate: false)
    }
    dispatchMain()
default:
    NEFilterManager.shared().loadFromPreferences { _ in
        let m = NEFilterManager.shared()
        log("filter enabled=\(m.isEnabled) provider=\(m.providerConfiguration?.filterDataProviderBundleIdentifier ?? "-")")
        exit(0)
    }
    dispatchMain()
}
