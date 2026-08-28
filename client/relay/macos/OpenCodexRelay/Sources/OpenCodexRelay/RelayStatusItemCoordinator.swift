import AppKit
import Combine
import SwiftUI
import OpenCodexRelayLocalization

enum RelayQuickMenuAction: String, CaseIterable, Sendable {
    case openControlCenter
    case refresh
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
    private let statusBar: NSStatusBar
    private let statusItem: NSStatusItem
    private let popover = NSPopover()
    private var observations = Set<AnyCancellable>()

    init(
        model: MenuBarModel,
        localization: LocalizationStore,
        windowCoordinator: ControlCenterWindowCoordinator,
        relocation: ApplicationRelocationController,
        statusBar: NSStatusBar = .system
    ) {
        self.model = model
        self.localization = localization
        self.windowCoordinator = windowCoordinator
        self.relocation = relocation
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
    }

    private func updateStatusItem() {
        guard let button = statusItem.button else { return }
        let image = NSImage(systemSymbolName: model.menuSymbolName, accessibilityDescription: nil)
        image?.isTemplate = true
        button.image = image
        button.title = model.menuBarLabel
        button.toolTip = model.menuAccessibilityLabel
        button.setAccessibilityLabel(model.menuAccessibilityLabel)
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
        for action in RelayQuickMenuAction.allCases {
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
        menu.addItem(item)
    }

    private func presentControlCenter() {
        windowCoordinator.present(
            model: model,
            localization: localization,
            relocation: relocation
        )
    }
}
