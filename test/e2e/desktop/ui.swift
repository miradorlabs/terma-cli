// desktopui drives a desktop app's window the way a person does, through the macOS
// accessibility API: the desktop profiles (profile_desktop_test.go) build it with swiftc and
// run it to press an app's buttons and keys, where the app offers no other way in.
//
//	desktopui dump <pid>              the window's accessibility tree, one element a line
//	desktopui press <pid> <label>     click the first element whose title, description or value is label
//	desktopui key <pid> <name>        bring the app forward and press a key: return, tab, escape
//
// It exits 0 on success, 1 when what it was asked to press is not there, 2 on a usage error,
// and 3 when the process may not use the accessibility API.
import ApplicationServices
import Cocoa

func attr(_ e: AXUIElement, _ name: String) -> AnyObject? {
    var v: AnyObject?
    return AXUIElementCopyAttributeValue(e, name as CFString, &v) == .success ? v : nil
}

func text(_ e: AXUIElement, _ name: String) -> String {
    if let s = attr(e, name) as? String { return s }
    return ""
}

func children(_ e: AXUIElement) -> [AXUIElement] {
    return (attr(e, kAXChildrenAttribute as String) as? [AXUIElement]) ?? []
}

func labels(_ e: AXUIElement) -> [String] {
    return [text(e, kAXTitleAttribute as String), text(e, kAXDescriptionAttribute as String),
            text(e, kAXValueAttribute as String), text(e, kAXHelpAttribute as String)]
        .map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }.filter { !$0.isEmpty }
}

// The walk stops at maxElements elements or after maxSeconds, whichever comes first: a page's
// tree can be large, and an app busy elsewhere answers each question slowly.
let maxElements = 6000
let maxSeconds = 25.0
var visited = 0
let started = Date()

func walk(_ e: AXUIElement, _ depth: Int, _ visit: (AXUIElement, Int) -> Bool) -> Bool {
    visited += 1
    if visited > maxElements || Date().timeIntervalSince(started) > maxSeconds { return false }
    if visit(e, depth) { return true }
    if depth > 80 { return false }
    for c in children(e) where walk(c, depth + 1, visit) { return true }
    return false
}

func center(_ e: AXUIElement) -> CGPoint? {
    guard let p = attr(e, kAXPositionAttribute as String), let s = attr(e, kAXSizeAttribute as String) else { return nil }
    var point = CGPoint.zero
    var size = CGSize.zero
    AXValueGetValue(p as! AXValue, .cgPoint, &point)
    AXValueGetValue(s as! AXValue, .cgSize, &size)
    return CGPoint(x: point.x + size.width / 2, y: point.y + size.height / 2)
}

func click(_ at: CGPoint) {
    let src = CGEventSource(stateID: .hidSystemState)
    CGEvent(mouseEventSource: src, mouseType: .mouseMoved, mouseCursorPosition: at, mouseButton: .left)?.post(tap: .cghidEventTap)
    usleep(200_000)
    for type in [CGEventType.leftMouseDown, .leftMouseUp] {
        CGEvent(mouseEventSource: src, mouseType: type, mouseCursorPosition: at, mouseButton: .left)?.post(tap: .cghidEventTap)
        usleep(50_000)
    }
}

func activate(_ pid: pid_t) {
    NSRunningApplication(processIdentifier: pid)?.activate(options: [.activateAllWindows])
    usleep(500_000)
}

let args = CommandLine.arguments
guard args.count >= 3, let pid = pid_t(args[2]) else {
    FileHandle.standardError.write("usage: desktopui dump|press|key <pid> [arg]\n".data(using: .utf8)!)
    exit(2)
}
if !AXIsProcessTrusted() {
    FileHandle.standardError.write("this process may not use the accessibility API\n".data(using: .utf8)!)
    exit(3)
}
let app = AXUIElementCreateApplication(pid)
AXUIElementSetMessagingTimeout(app, 2.0)
// Chromium builds its accessibility tree only for an assistive client that asks for it.
AXUIElementSetAttributeValue(app, "AXManualAccessibility" as CFString, kCFBooleanTrue)
AXUIElementSetAttributeValue(app, "AXEnhancedUserInterface" as CFString, kCFBooleanTrue)
usleep(300_000)

switch args[1] {
case "dump":
    _ = walk(app, 0) { e, depth in
        let role = text(e, kAXRoleAttribute as String)
        let l = labels(e).map { String($0.prefix(80)).replacingOccurrences(of: "\n", with: " ") }
        print(String(repeating: " ", count: min(depth, 40)) + role + " | " + l.joined(separator: " | "))
        return false
    }
case "press":
    guard args.count >= 4 else { exit(2) }
    let want = args[3]
    var found: AXUIElement?
    // Only what the app's windows show, not its menu bar's items: a person clicks the window.
    let windows = (attr(app, kAXWindowsAttribute as String) as? [AXUIElement]) ?? []
    for w in windows where found == nil {
        _ = walk(w, 0) { e, _ in
            guard labels(e).contains(want) else { return false }
            var enabled: AnyObject?
            if AXUIElementCopyAttributeValue(e, kAXEnabledAttribute as CFString, &enabled) == .success, let on = enabled as? Bool, !on {
                return false
            }
            var names: CFArray?
            AXUIElementCopyActionNames(e, &names)
            if let n = names as? [String], n.contains(kAXPressAction as String) {
                found = e
                return true
            }
            return false
        }
    }
    guard let e = found else { exit(1) }
    activate(pid)
    // A click where it is on the screen, as a person's: a web page's button ignores an
    // accessibility press it was not built for. Where it has no place on the screen, the press.
    if let at = center(e) {
        click(at)
        print("clicked \(want) at \(Int(at.x)),\(Int(at.y))")
    } else if AXUIElementPerformAction(e, kAXPressAction as CFString) == .success {
        print("pressed \(want)")
    } else {
        exit(1)
    }
case "key":
    guard args.count >= 4 else { exit(2) }
    let codes: [String: CGKeyCode] = ["return": 36, "tab": 48, "escape": 53, "space": 49]
    guard let code = codes[args[3]] else { exit(2) }
    activate(pid)
    let src = CGEventSource(stateID: .hidSystemState)
    for down in [true, false] {
        CGEvent(keyboardEventSource: src, virtualKey: code, keyDown: down)?.post(tap: .cghidEventTap)
        usleep(50_000)
    }
    print("pressed key \(args[3])")
default:
    exit(2)
}
