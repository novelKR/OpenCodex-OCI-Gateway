import AppKit
import OSLog

extension Notification.Name {
    static let relayDockReopenRequested = Notification.Name(
        "io.github.novelkr.opencodex-relay.dock-reopen-requested"
    )
}

@MainActor
final class RelayAppDelegate: NSObject, NSApplicationDelegate {
    private let logger = Logger(
        subsystem: Bundle.main.bundleIdentifier ?? "io.github.novelkr.opencodex-relay",
        category: "AppLifecycle"
    )

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApplication.shared.setActivationPolicy(.regular)
        RelayApplicationServices.current?.start()
        logger.info("Dock-visible activation policy enabled")
    }

    func applicationShouldHandleReopen(
        _ sender: NSApplication,
        hasVisibleWindows flag: Bool
    ) -> Bool {
        logger.info("Dock reopen requested")
        NotificationCenter.default.post(name: .relayDockReopenRequested, object: nil)
        RelayApplicationServices.current?.presentControlCenter()
        return true
    }
}
