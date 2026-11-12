import AppKit

let app = NSApplication.shared
app.setActivationPolicy(.accessory)
if !CommandLine.arguments.contains("--register-only") {
    let alert = NSAlert()
    alert.messageText = "FVS filesystem extension"
    alert.informativeText = "Enable FVS in System Settings > General > Login Items & Extensions > File System Extensions. Use the bundled fvs2d CLI to mount repositories. macOS may request access to the source folder. FVS requires macOS 26 or later."
    alert.addButton(withTitle: "Open Settings")
    alert.addButton(withTitle: "Close")
    app.activate(ignoringOtherApps: true)
    if alert.runModal() == .alertFirstButtonReturn {
        NSWorkspace.shared.open(URL(string: "x-apple.systempreferences:com.apple.LoginItems-Settings.extension")!)
    }

}
