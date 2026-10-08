import AppKit
import ApplicationServices
import Carbon.HIToolbox
import CoreGraphics
import CoreServices
import Foundation

struct SavedItem: Codable, Equatable { let values: [String: Data] }
enum ProbeError: Error { case failed(String) }
let args = CommandLine.arguments
let pasteboard = NSPasteboard.general

func require(_ condition: Bool, _ message: String) throws {
    if !condition { throw ProbeError.failed(message) }
}

// All advertised types must be readable. Never silently discard a type.
func capture() throws -> [SavedItem] {
    let generation = pasteboard.changeCount
    var saved: [SavedItem] = []
    for item in pasteboard.pasteboardItems ?? [] {
        var values: [String: Data] = [:]
        for type in item.types {
            guard let data = item.data(forType: type) else {
                throw ProbeError.failed("Unreadable advertised pasteboard type; no mutation performed")
            }
            values[type.rawValue] = data
        }
        saved.append(SavedItem(values: values))
    }
    try require(pasteboard.changeCount == generation, "Pasteboard changed during capture")
    return saved
}

// GPUI reconstructs printable keys from keyCode + the active TIS layout. A
// Unicode payload with virtual key 0 becomes "a", not the requested character.
// This CLI only calls TIS on its main thread and never changes the input source.
func physicalKeys(_ text: String) throws -> [(CGKeyCode, CGEventFlags)] {
    guard let source = TISCopyCurrentKeyboardLayoutInputSource()?.takeRetainedValue(),
          let property = TISGetInputSourceProperty(source, kTISPropertyUnicodeKeyLayoutData) else {
        throw ProbeError.failed("Active keyboard layout unavailable")
    }
    let data = unsafeBitCast(property, to: CFData.self)
    let layout = unsafeBitCast(CFDataGetBytePtr(data), to: UnsafePointer<UCKeyboardLayout>.self)
    // Ordinary ANSI positions only: exclude ISO section (10), Return (36),
    // Tab (48), Delete (51), keypad, navigation and function-key aliases.
    let codes: [CGKeyCode] = Array(0...9) + Array(11...35) + Array(37...47) + [49, 50]
    var inverse: [String: (CGKeyCode, CGEventFlags)] = [:]
    for shifted in [false, true] {
        for code in codes {
            var deadState: UInt32 = 0
            var output = [UniChar](repeating: 0, count: 4)
            var length = 0
            let status = UCKeyTranslate(layout, code, UInt16(kUCKeyActionDown),
                shifted ? UInt32((shiftKey >> 8) & 0xff) : 0, UInt32(LMGetKbdType()),
                OptionBits(kUCKeyTranslateNoDeadKeysMask), &deadState,
                output.count, &length, &output)
            guard status == noErr, length == 1, (32...126).contains(output[0]) else { continue }
            let character = String(utf16CodeUnits: output, count: length)
            if inverse[character] == nil { inverse[character] = (code, shifted ? .maskShift : []) }
        }
    }
    return try text.map { character in
        guard let stroke = inverse[String(character)] else {
            throw ProbeError.failed("Probe character not directly reachable with active layout + Shift")
        }
        return stroke
    }
}

func postKey(_ pid: pid_t, _ code: CGKeyCode, _ flags: CGEventFlags) throws {
    let source = CGEventSource(stateID: .privateState)
    for down in [true, false] {
        guard let event = CGEvent(keyboardEventSource: source, virtualKey: code, keyDown: down) else {
            throw ProbeError.failed("Cannot construct keyboard event")
        }
        event.flags = flags
        event.postToPid(pid)
        usleep(20_000)
    }
}

func attribute(_ element: AXUIElement, _ name: String) -> CFTypeRef? {
    var value: CFTypeRef?
    return AXUIElementCopyAttributeValue(element, name as CFString, &value) == .success ? value : nil
}

func elementAttribute(_ element: AXUIElement, _ name: String) -> AXUIElement? {
    guard let value = attribute(element, name), CFGetTypeID(value) == AXUIElementGetTypeID() else { return nil }
    return unsafeBitCast(value, to: AXUIElement.self)
}

func named(_ element: AXUIElement, _ names: Set<String>) -> Bool {
    // GPUI/accesskit may expose aria_label as AXTitle or AXDescription.
    for name in [kAXTitleAttribute, kAXDescriptionAttribute, "AXLabel", kAXHelpAttribute] {
        if let value = attribute(element, name) as? String, names.contains(value) { return true }
    }
    return false
}

do {
    let mode = args[1]
    if mode == "layout" {
        // Preflight the fixed synthetic probe before launching apps or mutating
        // the clipboard. Report strokes, never input-source identity or text.
        let strokes = try physicalKeys("/zzdurianprobeacvtq")
        let data = try JSONSerialization.data(withJSONObject: strokes.map {
            ["key_code": UInt64($0.0), "flags": $0.1.rawValue]
        }, options: [.sortedKeys])
        print(String(data: data, encoding: .utf8)!)
    } else if mode == "save" {
        let saved = try capture()
        let path = args[2]
        let parent = URL(fileURLWithPath: path).deletingLastPathComponent().path
        let attrs = try FileManager.default.attributesOfItem(atPath: parent)
        try require((attrs[.posixPermissions] as? NSNumber)?.intValue == 0o700, "Backup directory must have mode 0700")
        try require(!FileManager.default.fileExists(atPath: path), "Backup already exists")
        let data = try JSONEncoder().encode(saved)
        try require(FileManager.default.createFile(atPath: path, contents: data, attributes: [.posixPermissions: 0o600]), "Backup write failed")
        let stored = try JSONDecoder().decode([SavedItem].self, from: Data(contentsOf: URL(fileURLWithPath: path)))
        let permissions = try FileManager.default.attributesOfItem(atPath: path)
        try require((permissions[.posixPermissions] as? NSNumber)?.intValue == 0o600 && stored == saved, "Backup verification failed")
    } else if mode == "restore" {
        let saved = try JSONDecoder().decode([SavedItem].self, from: Data(contentsOf: URL(fileURLWithPath: args[2])))
        let items = try saved.map { saved -> NSPasteboardItem in
            let item = NSPasteboardItem()
            for (type, data) in saved.values {
                try require(item.setData(data, forType: .init(type)), "Restore item write failed")
            }
            return item
        }
        pasteboard.clearContents()
        if !items.isEmpty { try require(pasteboard.writeObjects(items), "Restore pasteboard write failed") }
        try require(try capture() == saved, "Restore Item/Type/Data equality failed; backup retained")
        print("RESTORE verified Item/Type/Data equality (contents suppressed)")
    } else if mode == "sentinel" {
        pasteboard.clearContents()
        try require(pasteboard.setString(args[2], forType: .string), "Sentinel write failed")
        try require(pasteboard.string(forType: .string) == args[2], "Sentinel readback failed")
    } else {
        let pid = pid_t(args[2])!
        if mode == "activate" {
            try require(NSRunningApplication(processIdentifier: pid)?.activate(options: [.activateAllWindows]) == true, "Activation failed")
        } else if mode == "terminate" {
            try require(NSRunningApplication(processIdentifier: pid)?.terminate() == true, "App termination request failed")
        } else if mode == "key" {
            try postKey(pid, CGKeyCode(args[3])!, CGEventFlags(rawValue: UInt64(args[4])!))
        } else if mode == "chord" {
            let strokes = try physicalKeys(args[3])
            try require(strokes.count == 1, "A shortcut requires exactly one character")
            try postKey(pid, strokes[0].0, strokes[0].1.union(CGEventFlags(rawValue: UInt64(args[4])!)))
        } else if mode == "text" || mode == "burst" || mode == "type" {
            // One process; no settling delay after slash, only 20ms between edges.
            let strokes = try physicalKeys((mode == "burst" ? "/" : "") + args[3])
            for (code, flags) in strokes { try postKey(pid, code, flags) }
        } else if mode == "ax" || mode == "ax-tree" || mode == "press-search" {
            guard AXIsProcessTrusted() else { print("{\"available\":false}"); exit(0) }
            let app = AXUIElementCreateApplication(pid)
            AXUIElementSetMessagingTimeout(app, 0.25)
            var queue: [(AXUIElement, Int)] = [(app, -1)]
            var count = 0
            var search = false
            var roles: [String: Int] = [:]
            var nodes: [[String: Any]] = []
            var searchButtons: [AXUIElement] = []
            let deadline = Date().addingTimeInterval(4)
            while !queue.isEmpty && count < 256 && Date() < deadline {
                let (element, parent) = queue.removeFirst()
                let index = count
                count += 1
                let role = attribute(element, kAXRoleAttribute) as? String ?? "unavailable"
                roles[role, default: 0] += 1
                if role == kAXButtonRole && named(element, ["Search (/)"]) {
                    searchButtons.append(element)
                }
                if mode == "ax-tree" {
                    var node: [String: Any] = ["index": index, "parent": parent, "role": role]
                    for name in [kAXTitleAttribute, kAXDescriptionAttribute, "AXLabel", kAXHelpAttribute] {
                        if let value = attribute(element, name) as? String {
                            node[name] = String(value.prefix(200))
                        }
                    }
                    node["focused"] = attribute(element, kAXFocusedAttribute) as? Bool ?? false
                    // Never include AXValue text: only its length, even in demo mode.
                    if let value = attribute(element, kAXValueAttribute) as? String {
                        node["value_length"] = value.utf16.count
                    }
                    nodes.append(node)
                }
                if named(element, ["Search mail", "Search all mail", "Search all mail…"]) { search = true }
                if let children = attribute(element, kAXChildrenAttribute) as? [AXUIElement] {
                    queue.append(contentsOf: children.map { ($0, index) })
                }
            }
            var searchEditorFocused = false
            var focusedRole = "unavailable"
            if let focused = elementAttribute(app, kAXFocusedUIElementAttribute) {
                let role = attribute(focused, kAXRoleAttribute) as? String
                focusedRole = role ?? "unavailable"
                let editable = role == kAXTextFieldRole || role == kAXTextAreaRole || role == kAXComboBoxRole
                if editable {
                    searchEditorFocused = named(focused, ["Search all mail", "Search all mail…"])
                    var ancestor = elementAttribute(focused, kAXParentAttribute)
                    var depth = 0
                    while !searchEditorFocused, let parent = ancestor, depth < 30 {
                        // Must be an actual ancestor of this editor, not an unrelated search control.
                        if named(parent, ["Search mail"]) { searchEditorFocused = true }
                        ancestor = elementAttribute(parent, kAXParentAttribute)
                        depth += 1
                    }
                }
            }
            var result: [String: Any] = ["available": true, "search": search,
                "search_editor_focused": searchEditorFocused, "nodes": count,
                "roles": roles, "focused_role": focusedRole, "truncated": !queue.isEmpty]
            if mode == "ax-tree" { result["tree"] = nodes }
            if mode == "press-search" {
                result["matching_buttons"] = searchButtons.count
                // Independent of the keyboard path; never guess by position.
                result["pressed"] = searchButtons.count == 1 && queue.isEmpty
                    && AXUIElementPerformAction(searchButtons[0], kAXPressAction as CFString) == .success
            }
            let data = try JSONSerialization.data(withJSONObject: result, options: [.sortedKeys])
            print(String(data: data, encoding: .utf8)!)
        } else { throw ProbeError.failed("Unknown helper mode") }
    }
} catch {
    // Errors contain no clipboard payload, type names, or mail contents.
    FileHandle.standardError.write(Data("Keyboard helper failed: \(error)\n".utf8))
    exit(2)
}
