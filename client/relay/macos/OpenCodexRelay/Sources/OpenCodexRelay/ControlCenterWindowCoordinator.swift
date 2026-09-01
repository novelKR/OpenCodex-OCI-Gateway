import AppKit
import OSLog
import SwiftUI
import OpenCodexRelayLocalization

@MainActor
final class ControlCenterWindowCoordinator: NSObject, ObservableObject, NSWindowDelegate {
    static let sceneID = "connection-details"

    @Published private(set) var requestedSection: RelayControlCenterSection?

    private let activityLog: RelayActivityLogStore?
    private let logger = Logger(
        subsystem: Bundle.main.bundleIdentifier ?? "io.github.novelkr.opencodex-relay",
        category: "Windowing"
    )
    private var window: NSWindow?

    init(activityLog: RelayActivityLogStore? = nil) {
        self.activityLog = activityLog
        super.init()
    }

    func present(
        model: MenuBarModel,
        localization: LocalizationStore,
        relocation: ApplicationRelocationController,
        updates: ReleaseUpdateController,
        runtimeUpgrade: RuntimeUpgradeController,
        section: RelayControlCenterSection? = nil
    ) {
        logger.info("Control Center presentation requested")
        requestedSection = section
        activityLog?.record(category: .window, code: "control_center_present_requested")
        let window = window ?? makeWindow(
            model: model,
            localization: localization,
            relocation: relocation,
            updates: updates,
            runtimeUpgrade: runtimeUpgrade
        )
        window.title = model.localizer.text(.viewControlCenter)
        configure(window)
        NSApplication.shared.activate()
        window.collectionBehavior.insert(.moveToActiveSpace)
        window.makeKeyAndOrderFront(nil)
        window.orderFrontRegardless()
        activityLog?.record(category: .window, code: "control_center_fronted")
    }

    func windowWillClose(_ notification: Notification) {
        activityLog?.record(category: .window, code: "control_center_closed")
    }

    private func makeWindow(
        model: MenuBarModel,
        localization: LocalizationStore,
        relocation: ApplicationRelocationController,
        updates: ReleaseUpdateController,
        runtimeUpgrade: RuntimeUpgradeController
    ) -> NSWindow {
        let root = RelayControlCenterView(
            model: model,
            windowCoordinator: self,
            relocation: relocation,
            updates: updates,
            runtimeUpgrade: runtimeUpgrade
        )
        .environmentObject(localization)
        .environment(\.locale, localization.locale)
        let hostingController = NSHostingController(rootView: root)
        let visibleSize = NSScreen.main?.visibleFrame.size ?? ControlCenterLayout.defaultWindowSize
        let window = NSWindow(
            contentRect: NSRect(
                origin: .zero,
                size: ControlCenterLayout.initialWindowSize(for: visibleSize)
            ),
            styleMask: [.titled, .closable, .miniaturizable, .resizable, .fullSizeContentView],
            backing: .buffered,
            defer: false
        )
        window.title = model.localizer.text(.viewControlCenter)
        window.titleVisibility = .hidden
        window.titlebarAppearsTransparent = true
        window.toolbarStyle = .unified
        window.contentViewController = hostingController
        window.contentMinSize = ControlCenterLayout.minimumWindowSize
        window.isReleasedWhenClosed = false
        window.delegate = self
        window.center()
        self.window = window
        activityLog?.record(category: .window, code: "control_center_attached")
        return window
    }

    private func configure(_ window: NSWindow) {
        window.contentMinSize = ControlCenterLayout.minimumWindowSize
        guard let visibleFrame = window.screen?.visibleFrame ?? NSScreen.main?.visibleFrame else { return }
        var frame = window.frame
        frame.size.width = min(
            frame.width,
            visibleFrame.width - ControlCenterLayout.visibleFrameInset * 2
        )
        frame.size.height = min(
            frame.height,
            visibleFrame.height - ControlCenterLayout.visibleFrameInset * 2
        )
        if !visibleFrame.contains(frame) {
            window.setFrame(frame, display: false)
            window.center()
        }
    }
}
