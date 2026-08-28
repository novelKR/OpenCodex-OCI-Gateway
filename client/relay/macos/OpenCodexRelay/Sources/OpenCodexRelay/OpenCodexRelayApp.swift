import AppKit
import Darwin
import SwiftUI
import OpenCodexRelayCore
import OpenCodexRelayLocalization

@main
struct OpenCodexRelayApp: App {
    @NSApplicationDelegateAdaptor(RelayAppDelegate.self) private var appDelegate
    @StateObject private var services: RelayApplicationServices

    init() {
        if let action = HomebrewGuardControlAction.parse(ProcessInfo.processInfo.arguments) {
            Self.runHomebrewGuardControl(action)
        }
        if let action = LoginControlAction.parse(ProcessInfo.processInfo.arguments) {
            Self.runLoginControl(action)
        }
        let services = RelayApplicationServices()
        RelayApplicationServices.current = services
        _services = StateObject(wrappedValue: services)
    }

    var body: some Scene {
        Window(
            services.model.localizer.text(.integrationGuideTitle),
            id: LocalDevelopmentProducerTools.sceneID
        ) {
            LocalDevelopmentIntegrationGuideView(
                model: services.model,
                localizer: services.model.localizer
            )
        }
        .defaultLaunchBehavior(.suppressed)
        .restorationBehavior(.disabled)
        .commands {
            if services.model.canShowLocalDevelopmentIntegrationGuide {
                LocalDevelopmentProducerCommands(
                    title: services.model.localizer.text(.producerToolsMenu),
                    actionTitle: services.model.localizer.text(.integrationGuideOpen)
                )
            }
        }
    }

    private static func runLoginControl(_ action: LoginControlAction) -> Never {
        let registration = MainAppLoginRegistration()
        let result: LoginRegistrationResult
        let exitCode: Int32
        switch action {
        case .installCheck:
            // Preserve the existing installer contract: the printed bounded
            // result is authoritative and this one-shot check exits cleanly.
            result = LoginRegistrationCoordinator.ensureRegistered(registration)
            exitCode = 0
        case .loginStatus:
            // Query-only: no registration or relay/config API is invoked.
            result = LoginRegistrationCoordinator.status(registration)
            exitCode = 0
        case .uninstallLogin:
            result = LoginRegistrationCoordinator.unregister(registration)
            exitCode = result == .disabled ? 0 : 1
        case .invalid:
            result = .failed
            exitCode = 64
        }
        let line = "login_registration=\(result.rawValue)\n"
        FileHandle.standardOutput.write(Data(line.utf8))
        exit(exitCode)
    }
    @MainActor
    private static func runHomebrewGuardControl(
        _ action: HomebrewGuardControlAction
    ) -> Never {
        Task { @MainActor in
            let manager = SystemHomebrewGuardManager()
            let state: HomebrewGuardRegistrationState
            let exitCode: Int32
            switch action {
            case .status:
                state = await manager.availability(candidate: nil).registration
                exitCode = 0
            case .register:
                do {
                    try manager.register()
                    state = await manager.availability(candidate: nil).registration
                    exitCode = state == .ready || state == .approvalRequired ? 0 : 1
                } catch {
                    state = .unavailable
                    exitCode = 1
                }
            case .unregister:
                state = await manager.unregisterIfSafe()
                exitCode = state == .notRegistered ? 0 : 3
            case .invalid:
                state = .unavailable
                exitCode = 64
            }
            let line = "homebrew_guard_registration=\(state.rawValue)\n"
            FileHandle.standardOutput.write(Data(line.utf8))
            exit(exitCode)
        }
        RunLoop.main.run()
        fatalError("Homebrew guard one-shot run loop returned")
    }
}

private let relayOneShotControlFlags: Set<String> = [
    "--install-check",
    "--login-status",
    "--uninstall-login",
    "--homebrew-guard-status",
    "--homebrew-guard-register",
    "--homebrew-guard-unregister",
]

private enum HomebrewGuardControlAction {
    case status
    case register
    case unregister
    case invalid

    static func parse(_ arguments: [String]) -> HomebrewGuardControlAction? {
        let managed = arguments.dropFirst().filter(relayOneShotControlFlags.contains)
        let requested = managed.filter { $0.hasPrefix("--homebrew-guard-") }
        guard !requested.isEmpty else { return nil }
        guard managed.count == 1, requested.count == 1 else { return .invalid }
        switch requested[0] {
        case "--homebrew-guard-status":
            return .status
        case "--homebrew-guard-register":
            return .register
        case "--homebrew-guard-unregister":
            return .unregister
        default:
            return .invalid
        }
    }
}

private enum LoginControlAction {
    case installCheck
    case loginStatus
    case uninstallLogin
    case invalid

    static func parse(_ arguments: [String]) -> LoginControlAction? {
        let managed = arguments.dropFirst().filter(relayOneShotControlFlags.contains)
        let requested = managed.filter {
            $0 == "--install-check" || $0 == "--login-status" || $0 == "--uninstall-login"
        }
        guard !requested.isEmpty else { return nil }
        guard managed.count == 1, requested.count == 1 else { return .invalid }
        switch requested[0] {
        case "--install-check":
            return .installCheck
        case "--login-status":
            return .loginStatus
        case "--uninstall-login":
            return .uninstallLogin
        default:
            return .invalid
        }
    }
}
