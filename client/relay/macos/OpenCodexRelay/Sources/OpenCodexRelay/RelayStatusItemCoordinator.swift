import AppKit
import Combine
import SwiftUI
import OpenCodexRelayCore
import OpenCodexRelayLocalization

enum RelayQuickMenuAction: String, CaseIterable, Sendable {
    case openControlCenter
    case refresh
    case checkForUpdates
    case openUpdateRelease
    case openLoginItemsSettings
    case quit
}

enum RelayStatusItemActivation: Equatable {
    case popover
    case quickMenu

    static func resolve(eventType: NSEvent.EventType?) -> Self {
        eventType == .rightMouseUp ? .quickMenu : .popover
    }
}

@MainActor
final class RelayStatusItemCoordinator: NSObject, NSPopoverDelegate {
    private let model: MenuBarModel
    private let localization: LocalizationStore
    private let windowCoordinator: ControlCenterWindowCoordinator
    private let relocation: ApplicationRelocationController
    private let updates: ReleaseUpdateController
    private let runtimeUpgrade: RuntimeUpgradeController
    private let statusBar: NSStatusBar
    private let statusItem: NSStatusItem
    private let popover = NSPopover()
    private var observations = Set<AnyCancellable>()

    init(
        model: MenuBarModel,
        localization: LocalizationStore,
        windowCoordinator: ControlCenterWindowCoordinator,
        relocation: ApplicationRelocationController,
        updates: ReleaseUpdateController,
        runtimeUpgrade: RuntimeUpgradeController,
        statusBar: NSStatusBar = .system
    ) {
        self.model = model
        self.localization = localization
        self.windowCoordinator = windowCoordinator
        self.relocation = relocation
        self.updates = updates
        self.runtimeUpgrade = runtimeUpgrade
        self.statusBar = statusBar
        self.statusItem = statusBar.statusItem(withLength: NSStatusItem.variableLength)
        super.init()
        configureStatusItem()
        configurePopover()
        observePresentation()
    }

    func remove() {
        popover.performClose(nil)
        statusBar.removeStatusItem(statusItem)
        observations.removeAll()
    }

    @objc private func statusItemActivated(_ sender: NSStatusBarButton) {
        if RelayStatusItemActivation.resolve(
            eventType: NSApplication.shared.currentEvent?.type
        ) == .quickMenu {
            showQuickMenu(from: sender)
        } else {
            togglePopover(from: sender)
        }
    }

    @objc private func openControlCenter() {
        presentControlCenter()
    }

    @objc private func refresh() {
        model.refresh()
    }

    @objc private func openLoginItemsSettings() {
        guard let url = URL(
            string: "x-apple.systempreferences:com.apple.LoginItems-Settings.extension"
        ) else { return }
        NSWorkspace.shared.open(url)
    }

    @objc private func checkForUpdates() {
        updates.checkNow()
    }

    @objc private func openUpdateRelease() {
        guard let releaseURL = updates.releaseURL else { return }
        NSWorkspace.shared.open(releaseURL)
    }

    @objc private func quit() {
        NSApplication.shared.terminate(nil)
    }

    func popoverDidClose(_ notification: Notification) {
        model.setPopoverVisible(false)
    }

    private func configureStatusItem() {
        guard let button = statusItem.button else { return }
        button.target = self
        button.action = #selector(statusItemActivated(_:))
        button.sendAction(on: [.leftMouseUp, .rightMouseUp])
        updateStatusItem()
    }

    private func configurePopover() {
        let content = MenuBarContentView(model: model, onOpenControlCenter: { [weak self] in
            self?.popover.performClose(nil)
            self?.presentControlCenter()
        })
        .environmentObject(localization)
        .environment(\.locale, localization.locale)
        popover.contentViewController = NSHostingController(rootView: content)
        popover.behavior = .transient
        popover.animates = true
        popover.delegate = self
    }

    private func observePresentation() {
        model.objectWillChange
            .receive(on: RunLoop.main)
            .sink { [weak self] (_: Void) in
                Task { @MainActor in self?.updateStatusItem() }
            }
            .store(in: &observations)
        localization.objectWillChange
            .receive(on: RunLoop.main)
            .sink { [weak self] _ in
                Task { @MainActor in
                    self?.updateStatusItem()
                    self?.configurePopover()
                }
            }
            .store(in: &observations)
        updates.objectWillChange
            .receive(on: RunLoop.main)
            .sink { [weak self] (_: Void) in
                Task { @MainActor in self?.updateStatusItem() }
            }
            .store(in: &observations)
    }

    private func updateStatusItem() {
        guard let button = statusItem.button else { return }
        let image = NSImage(systemSymbolName: model.menuSymbolName, accessibilityDescription: nil)
        image?.isTemplate = true
        button.image = image
        button.title = model.menuBarLabel + (updates.isUpdateBadgeVisible ? " •" : "")
        let accessibilityLabel = updates.isUpdateBadgeVisible
            ? model.menuAccessibilityLabel + ". " + model.localizer.text(.updateStatusAvailable)
            : model.menuAccessibilityLabel
        button.toolTip = accessibilityLabel
        button.setAccessibilityLabel(accessibilityLabel)
    }

    private func togglePopover(from button: NSStatusBarButton) {
        if popover.isShown {
            popover.performClose(nil)
        } else {
            popover.show(relativeTo: button.bounds, of: button, preferredEdge: .minY)
            model.setPopoverVisible(true)
        }
    }

    private func showQuickMenu(from button: NSStatusBarButton) {
        popover.performClose(nil)
        let menu = NSMenu()
        addUpdateSummary(to: menu)
        menu.addItem(.separator())
        for action in RelayQuickMenuAction.allCases {
            if action == .openUpdateRelease, updates.releaseURL == nil {
                continue
            }
            if action == .openLoginItemsSettings || action == .quit {
                menu.addItem(.separator())
            }
            addItem(to: menu, action: action)
        }
        menu.popUp(
            positioning: nil,
            at: NSPoint(x: button.bounds.minX, y: button.bounds.minY),
            in: button
        )
    }

    private func addItem(to menu: NSMenu, action: RelayQuickMenuAction) {
        let descriptor: (AppStringKey, Selector, String)
        switch action {
        case .openControlCenter:
            descriptor = (.menuOpenControlCenter, #selector(openControlCenter), "")
        case .refresh:
            descriptor = (.menuRefresh, #selector(refresh), "r")
        case .checkForUpdates:
            descriptor = (.updateCheckNow, #selector(checkForUpdates), "")
        case .openUpdateRelease:
            descriptor = (.updateOpenRelease, #selector(openUpdateRelease), "")
        case .openLoginItemsSettings:
            descriptor = (.menuOpenLoginItemsSettings, #selector(openLoginItemsSettings), "")
        case .quit:
            descriptor = (.menuQuit, #selector(quit), "q")
        }
        let item = NSMenuItem(
            title: model.localizer.text(descriptor.0),
            action: descriptor.1,
            keyEquivalent: descriptor.2
        )
        item.target = self
        if action == .checkForUpdates {
            item.isEnabled = !updates.isChecking
        }
        menu.addItem(item)
    }

    private func addUpdateSummary(to menu: NSMenu) {
        let channel = updates.channel == .stable
            ? model.localizer.text(.updateChannelStable)
            : model.localizer.text(.updateChannelPreview)
        let checkedAt = updates.lastCheckedAt?.formatted(date: .abbreviated, time: .shortened)
            ?? model.localizer.text(.genericNever)
        for title in [
            model.localizer.text(.updateMenuChannel, channel),
            model.localizer.text(.updateMenuLastChecked, checkedAt),
            model.localizer.text(.updateMenuStatus, updateStatusTitle),
        ] {
            let item = NSMenuItem(title: title, action: nil, keyEquivalent: "")
            item.isEnabled = false
            menu.addItem(item)
        }
    }

    private var updateStatusTitle: String {
        if updates.lastCheckFailed { return model.localizer.text(.updateStatusFailed) }
        guard let status = updates.status else { return model.localizer.text(.updateStatusNotChecked) }
        switch status {
        case .current: return model.localizer.text(.updateStatusCurrent)
        case .newerThanSelectedChannel: return model.localizer.text(.updateStatusNewerThanChannel)
        case .updateAvailable: return model.localizer.text(.updateStatusAvailable)
        case .offline: return model.localizer.text(.updateStatusOffline)
        case .rateLimited: return model.localizer.text(.updateStatusRateLimited)
        case .invalidRelease: return model.localizer.text(.updateStatusInvalidRelease)
        case .updaterTooOld: return model.localizer.text(.updateStatusUpdaterTooOld)
        case .unsupportedSystem: return model.localizer.text(.updateStatusUnsupportedSystem)
        }
    }

    private func presentControlCenter() {
        windowCoordinator.present(
            model: model,
            localization: localization,
            relocation: relocation,
            updates: updates,
            runtimeUpgrade: runtimeUpgrade
        )
    }
}
