import AppKit
import Combine
import Foundation
import ServiceManagement
import OpenCodexRelayCore
import OpenCodexRelayLocalization
import OpenCodexRelayHomebrewGuard

enum DesktopTargetState: Equatable {
    case notRegistered
    case registered(running: Bool)
    case unavailable
    case trustConfigurationMissing
    case untrusted
    case ambiguous

    var canControl: Bool {
        if case .registered = self { return true }
        return false
    }

    @MainActor
    func title(using localizer: AppLocalizer) -> String {
        switch self {
        case .notRegistered:
            return localizer.text(.desktopNotRegistered)
        case let .registered(running):
            return running ? localizer.text(.desktopRegisteredRunning) : localizer.text(.desktopRegisteredStopped)
        case .unavailable:
            return localizer.text(.desktopUnavailable)
        case .trustConfigurationMissing:
            return localizer.text(.desktopTrustConfigurationMissing)
        case .untrusted:
            return localizer.text(.desktopUntrusted)
        case .ambiguous:
            return localizer.text(.desktopAmbiguous)
        }
    }

    @MainActor
    func accessibilityLabel(using localizer: AppLocalizer) -> String {
        switch self {
        case .notRegistered:
            return localizer.text(.desktopNotRegisteredAccessibility)
        case let .registered(running):
            return running
                ? localizer.text(.desktopRegisteredRunningAccessibility)
                : localizer.text(.desktopRegisteredStoppedAccessibility)
        case .unavailable:
            return localizer.text(.desktopUnavailableAccessibility)
        case .trustConfigurationMissing:
            return localizer.text(.desktopTrustConfigurationMissingAccessibility)
        case .untrusted:
            return localizer.text(.desktopUntrustedAccessibility)
        case .ambiguous:
            return localizer.text(.desktopAmbiguousAccessibility)
        }
    }
}

/// Arguments retained by a status message must be semantic when their visible
/// form depends on the selected UI language. This lets a pending operation
/// redraw fully in the newly selected language without changing the relayctl
/// command that is already in flight.
enum SafeStatusMessageArgument: Equatable {
    case literal(String)
    case routingRequestTarget(RoutingRequestTarget)

    @MainActor
    fileprivate func rendered(using localizer: AppLocalizer) -> String {
        switch self {
        case let .literal(value):
            return value
        case let .routingRequestTarget(target):
            return localizer.displayName(target)
        }
    }
}

struct SafeStatusMessage: Equatable {
    let code: String
    let key: AppStringKey
    let arguments: [SafeStatusMessageArgument]

    init(code: String, key: AppStringKey, arguments: [SafeStatusMessageArgument] = []) {
        self.code = code
        self.key = key
        self.arguments = arguments
    }

    @MainActor
    func text(using localizer: AppLocalizer) -> String {
        localizer.text(key, arguments: arguments.map { $0.rendered(using: localizer) as CVarArg })
    }
}

enum OpenCodexDiscoveryState: Equatable {
    case idle
    case searching(OpenCodexDiscoveryTier)
    case candidates(OpenCodexDiscoveryResult)
    case broadScanApprovalRequired(OpenCodexDiscoveryResult)
    case notFound(OpenCodexDiscoveryResult)
    case failed(String)
}

struct MainAppLoginRegistration: LoginRegistrationManaging {
    var registrationState: LoginRegistrationState {
        switch SMAppService.mainApp.status {
        case .enabled:
            return .enabled
        case .notRegistered:
            return .disabled
        default:
            return .pending
        }
    }

    func register() throws {
        try SMAppService.mainApp.register()
    }

    func unregister() throws {
        try SMAppService.mainApp.unregister()
    }
}

@MainActor
final class MenuBarModel: ObservableObject {
    private enum InteractiveSurface: Hashable {
        case menuBarPopover
        case controlCenter
    }

    @Published private(set) var status: RoutingStatus?
    @Published private(set) var selectedDesktopTarget: DesktopTarget?
    @Published private(set) var desktopTargetState: DesktopTargetState = .notRegistered
    @Published private(set) var lastStatusUpdatedAt: Date?
    @Published private(set) var statusError: SafeStatusMessage? {
        didSet {
            guard oldValue?.code != statusError?.code, let statusError else { return }
            activityLog.record(
                .error,
                category: .status,
                code: "status_error",
                fields: ["failure_code": statusError.code]
            )
        }
    }
    @Published private(set) var isBusy = false
    @Published private(set) var isRefreshing = false
    @Published private(set) var message: SafeStatusMessage? {
        didSet {
            guard oldValue?.code != message?.code, let message else { return }
            activityLog.record(
                category: .operation,
                code: "status_message_changed",
                fields: ["message_code": message.code]
            )
        }
    }
    @Published private(set) var loginItemMessage: AppStringKey?
    @Published private(set) var openCodexDiscoveryState: OpenCodexDiscoveryState = .idle {
        didSet {
            recordDiscoveryActivity(openCodexDiscoveryState)
        }
    }
    @Published private(set) var openCodexRemovalFlow: OpenCodexRemovalFlow? {
        didSet {
            recordRemovalActivity(openCodexRemovalFlow)
        }
    }
    @Published private(set) var hasPendingOpenCodexRemovalRecovery = false
    @Published private(set) var nativeRepairInspection: NativeRepairInspection?
     private(set) var nativeRepairOwnerInspection: NativeRepairOwnerInspection?
    @Published private(set) var nativeRepairDiscoveryState: OpenCodexDiscoveryState = .idle {
        didSet { recordDiscoveryActivity(nativeRepairDiscoveryState) }
    }
    @Published private(set) var nativeRepairOpenCodexCandidate: OpenCodexInstallationCandidate?
    @Published private(set) var nativeRepairProgress: NativeRepairProgress?
    @Published private(set) var homebrewGuardAvailability: HomebrewGuardAvailability = .notRequired
    @Published private(set) var integrationAvailability: RelayIntegrationAvailability = .missing

    private let helperURL: URL
    private let bindingURL: URL
    private let injectedClient: (any RelayctlExecuting)?
    private let injectedDiscoveryClient: (any OpenCodexDiscovering)?
    private let injectedRemovalClient: (any OpenCodexRemovalExecuting)?
    private let injectedNativeRepairClient: (any NativeRepairExecuting)?
    private let removalRecoveryStore: any OpenCodexRemovalRecoverySessionStoring
    private let targetStore: any DesktopTargetStoring
    private let desktopApplication: any DesktopApplicationControlling
    private let desktopTrustPolicy: CodexDesktopTrustPolicy
    private let desktopTrustValidator: any CodexDesktopTrustValidating
    private let desktopDiscoverer: any CodexDesktopDiscovering
	private let loginRegistration: any LoginRegistrationManaging
    private let homebrewGuard: any HomebrewGuardManaging
	private let distributionFlavor: DistributionFlavor
    private let runtimeMode: RelayRuntimeMode
    private let producerToolsEnabled: Bool
    let localization: LocalizationStore
    let activityLog: RelayActivityLogStore
    private var pollingTask: Task<Void, Never>?
    private var pollingInterval: TimeInterval?
    private var isStatusRefreshInFlight = false
    private var pendingManualRefresh = false
    private var visibleInteractiveSurfaces: Set<InteractiveSurface> = []
    private var lastRemovalActivitySignature: String?

    init(
        client: (any RelayctlExecuting)? = nil,
        discoveryClient: (any OpenCodexDiscovering)? = nil,
        removalClient: (any OpenCodexRemovalExecuting)? = nil,
        nativeRepairClient: (any NativeRepairExecuting)? = nil,
        removalRecoveryStore: (any OpenCodexRemovalRecoverySessionStoring)? = nil,
        targetStore: (any DesktopTargetStoring)? = nil,
        desktopApplication: any DesktopApplicationControlling = DesktopApplicationController(),
        desktopTrustPolicy: CodexDesktopTrustPolicy = .current,
        desktopTrustValidator: any CodexDesktopTrustValidating = SecurityFrameworkCodexDesktopTrustValidator(),
        desktopDiscoverer: any CodexDesktopDiscovering = WorkspaceCodexDesktopDiscoverer(),
		loginRegistration: any LoginRegistrationManaging = MainAppLoginRegistration(),
        homebrewGuard: (any HomebrewGuardManaging)? = nil,
		bindingURL: URL? = nil,
		helperURL: URL? = nil,
		startsPolling: Bool = true,
		distributionFlavor: DistributionFlavor = .current,
        runtimeMode: RelayRuntimeMode = .current,
        producerToolsEnabled: Bool = ProcessInfo.processInfo.arguments.contains("--enable-producer-tools"),
        localization: LocalizationStore = LocalizationStore(),
        activityLog: RelayActivityLogStore? = nil
    ) {
        self.helperURL = helperURL ?? RelayctlHelperLocation.resolve()
        self.bindingURL = bindingURL ?? RoutingBindingReader.defaultURL()
        self.injectedClient = client
        self.injectedDiscoveryClient = discoveryClient
        self.injectedRemovalClient = removalClient
        self.injectedNativeRepairClient = nativeRepairClient
        self.removalRecoveryStore = removalRecoveryStore ?? UserDefaultsOpenCodexRemovalRecoverySessionStore()
        self.targetStore = targetStore ?? UserDefaultsDesktopTargetStore()
        self.desktopApplication = desktopApplication
        self.desktopTrustPolicy = desktopTrustPolicy
        self.desktopTrustValidator = desktopTrustValidator
        self.desktopDiscoverer = desktopDiscoverer
		self.loginRegistration = loginRegistration
        self.homebrewGuard = homebrewGuard ?? SystemHomebrewGuardManager()
		self.distributionFlavor = distributionFlavor
        self.runtimeMode = runtimeMode
        self.producerToolsEnabled = producerToolsEnabled
        self.localization = localization
        self.activityLog = activityLog ?? RelayActivityLogStore()
        self.integrationAvailability = runtimeMode == .preview
            ? .preview
            : client == nil
            ? RelayIntegrationInspector.inspect(
                runtimeMode: runtimeMode,
                bindingURL: self.bindingURL,
                helperURL: self.helperURL
            )
            : .ready
        self.selectedDesktopTarget = self.targetStore.desktopTarget
        do {
            if let recoverySession = try self.removalRecoveryStore.load() {
                self.openCodexRemovalFlow = OpenCodexRemovalFlow(recoverySession: recoverySession)
                self.hasPendingOpenCodexRemovalRecovery = true
            }
        } catch {
            self.message = SafeStatusMessage(
                code: "opencodex_recovery_context_invalid",
                key: .messageRemovalRecoveryUnavailable
            )
        }
        refreshDesktopTargetState()
        if !desktopTargetState.canControl {
            discoverCodexDesktopApplication(showMessage: false)
        }
        recordRemovalActivity(openCodexRemovalFlow)
        self.activityLog.record(
            category: .lifecycle,
            code: "app_model_started",
            fields: [
                "distribution": distributionFlavor.rawValue,
                "runtime_mode": runtimeMode.rawValue,
            ]
        )
        Task { [weak self] in
            await self?.refreshHomebrewGuardAvailability()
        }
        // Polling begins with the model lifecycle, rather than waiting for a
        // popover open, so the MenuBar icon reflects current routing state.
		if startsPolling {
			start()
		}
    }

    deinit {
        pollingTask?.cancel()
    }

    var presentation: RoutingPresentation {
        status?.presentation ?? .relayUnavailable
    }

    var localizer: AppLocalizer { localization.localizer }

    var menuSymbolName: String { presentation.symbolName }
    var menuBarLabel: String {
        let label = localizer.compactLabel(presentation)
        guard distributionFlavor.isLocalDevelopment else { return label }
        return localizer.text(.menuLocalDevelopmentPrefix, label)
    }
    var menuAccessibilityLabel: String {
        let label = localizer.accessibilityLabel(presentation)
        guard distributionFlavor.isLocalDevelopment else { return label }
        return localizer.text(.menuLocalDevelopmentAccessibility, label)
    }
    var statusTitle: String { localizer.title(presentation) }
    var isLocalDevelopmentBuild: Bool { distributionFlavor.isLocalDevelopment }

    @available(*, deprecated, renamed: "isLocalDevelopmentBuild")
    var isUnsignedLocalDevelopmentBuild: Bool { isLocalDevelopmentBuild }

    var isPreviewRuntime: Bool { runtimeMode == .preview }

    var shouldOpenSelfHostedOnboarding: Bool {
        integrationAvailability == .missing || integrationAvailability == .preview
    }

    var canShowLocalDevelopmentIntegrationGuide: Bool {
        distributionFlavor.isLocalDevelopment && producerToolsEnabled
    }

    var canRequestRouting: Bool {
        guard integrationAvailability.permitsManagedOperations else { return false }
        guard let status else { return false }
        return status.phase != .recoveryRequired && status.phase != .applying
    }

    var canStartOpenCodexHandoff: Bool {
        guard let status,
              status.phase == .relayActive || status.phase == .nativeActive else {
            return false
        }
        return status.relayRunning &&
            status.connection.localRelay == .healthy &&
            (status.connection.routingSync == .acknowledged ||
                status.connection.routingSync == .invalid)
    }

    var openCodexHandoffAvailabilityMessage: SafeStatusMessage? {
        canStartOpenCodexHandoff ? nil : routingPreflightFailureMessage()
    }

    var canRepairNative: Bool {
        guard distributionFlavor.isLocalDevelopment,
              let status, status.generation > 0,
              status.phase == .recoveryRequired,
              let capabilities = status.recoveryCapabilities else {
            return false
        }
        return !capabilities.canComplete && !capabilities.canRollback
    }

    var canBeginOpenCodexRemoval: Bool {
        openCodexRemovalFlow?.automaticRemovalEligible == true
    }

    var canRegisterHomebrewGuard: Bool {
        homebrewGuard.backend == .smAppService &&
            homebrewGuardAvailability.registration == .notRegistered
    }

    var homebrewGuardBackend: HomebrewGuardBackend { homebrewGuard.backend }

    var canShowDevelopmentHomebrewGuardSetup: Bool {
        guard runtimeMode == .managed,
              homebrewGuard.backend == .manualAdmin,
              developmentHomebrewGuardSetupFailureCode == nil else {
            return false
        }
        switch homebrewGuardAvailability.registration {
        case .manualInstallRequired, .manualUpdateRequired,
             .manualInstallerRecoveryRequired, .daemonLaunchFailed:
            return true
        default:
            return false
        }
    }

    var developmentHomebrewGuardSetupFailureCode: String? {
        guard let action = developmentHomebrewGuardSetupAction else { return nil }
        switch homebrewGuard.setupCommand(for: action) {
        case .available:
            return nil
        case let .unavailable(code):
            return code
        }
    }

    var integrationStatusMessage: SafeStatusMessage? {
        switch integrationAvailability {
        case .ready:
            nil
        case .preview:
            SafeStatusMessage(code: "preview_mode", key: .integrationPreview)
        case .missing:
            SafeStatusMessage(code: "routing_binding_missing", key: .bindingMissing)
        case .unsafe:
            SafeStatusMessage(code: "routing_binding_unsafe", key: .bindingUnsafe)
        case .invalid:
            SafeStatusMessage(code: "routing_binding_invalid", key: .bindingInvalid)
        case .helperUnavailable:
            SafeStatusMessage(code: "relayctl_unavailable", key: .integrationHelperUnavailable)
        }
    }

    func recordIntegrationGuidePresented() {
        activityLog.record(category: .lifecycle, code: "integration_guide_presented")
    }

    func copyIntegrationGuideCommand(_ command: String, kind: String) {
        guard ["signing_setup", "build", "install"].contains(kind) else { return }
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(command, forType: .string)
        activityLog.record(
            category: .lifecycle,
            code: "integration_guide_command_copied",
            fields: ["command_kind": kind]
        )
    }

    var canOpenHomebrewGuardSystemSettings: Bool {
        homebrewGuardAvailability.registration == .approvalRequired
    }

    var canRecoverHomebrewGuard: Bool {
        homebrewGuardAvailability.registration == .recoveryRequired &&
            homebrewGuardAvailability.operationID != nil
    }

    var selectedDesktopDisplayName: String {
        selectedDesktopTarget?.displayName ?? localizer.text(.desktopNoneSelected)
    }

    var activeRequestsDisplay: String {
        guard let status else { return localizer.text(.genericUnavailable) }
        guard let activeRequests = status.activeRequests else { return localizer.text(.genericUnavailable) }
        return localizer.formattedNumber(activeRequests)
    }

    var drainDisplay: String {
        guard let status else { return localizer.text(.genericUnavailable) }
        if status.isDraining {
            return localizer.text(.genericDraining)
        }
        if status.phase == .applying {
            return localizer.text(.genericWaitingForRequests)
        }
        return localizer.text(.genericNotDraining)
    }

    var lastStatusUpdatedDisplay: String {
        guard let lastStatusUpdatedAt else { return localizer.text(.genericNever) }
        return localizer.formattedDate(lastStatusUpdatedAt)
    }

    var localRelayDisplay: String {
        status.map { localizer.displayName($0.connection.localRelay) } ?? localizer.text(.genericUnavailable)
    }

    var routingSyncDisplay: String {
        status.map { localizer.displayName($0.connection.routingSync) } ?? localizer.text(.genericUnavailable)
    }

    var remoteGatewayDisplay: String {
        status.map { localizer.displayName($0.connection.remoteGateway) } ?? localizer.text(.genericUnknown)
    }

    var localOpenCodexDisplay: String {
        status.map { localizer.displayName($0.connection.localOpenCodex) } ?? localizer.text(.genericUnknown)
    }

    var catalogDisplay: String {
        status.map { localizer.displayName($0.connection.catalog) } ?? localizer.text(.genericUnknown)
    }

    func start() {
        guard pollingTask == nil else { return }
		if !distributionFlavor.isLocalDevelopment {
			registerAtLogin()
		} else {
			loginItemMessage = .messageLoginOptional
		}
        startPollingTask()
    }

	func enableLaunchAtLogin() {
		registerAtLogin()
	}

    func setPopoverVisible(_ visible: Bool) {
        setInteractiveSurface(.menuBarPopover, visible: visible)
    }

    func setControlCenterVisible(_ visible: Bool) {
        setInteractiveSurface(.controlCenter, visible: visible)
    }

    var isInteractiveSurfaceVisible: Bool {
        !visibleInteractiveSurfaces.isEmpty
    }

    func refresh() {
        guard !isBusy else {
            activityLog.record(.warning, category: .refresh, code: "refresh_blocked")
            return
        }
        isRefreshing = true
        Task { [weak self] in
            await self?.refreshStatus(showFailureMessage: true)
        }
    }

    func refreshHomebrewGuardAvailability() async {
        await refreshHomebrewGuardAvailability(for: openCodexRemovalFlow?.candidate)
    }

    func registerHomebrewGuard() {
        guard canRegisterHomebrewGuard, !isBusy else { return }
        isBusy = true
        activityLog.record(
            category: .removal,
            code: "homebrew_guard_registration_requested",
            fields: ["distribution": distributionFlavor.rawValue, "phase": "registration"]
        )
        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            do {
                try self.homebrewGuard.register()
            } catch {
                await self.refreshHomebrewGuardAvailability()
                let failure = self.homebrewGuardMessage(
                    for: self.homebrewGuardAvailability.errorCode ?? .homebrewGuardNotRegistered
                )
                self.message = failure
                self.activityLog.record(
                    .error,
                    category: .removal,
                    code: "homebrew_guard_registration_finished",
                    fields: [
                        "distribution": self.distributionFlavor.rawValue,
                        "failure_code": failure.code,
                        "result_code": failure.code,
                    ]
                )
                return
            }
            await self.refreshHomebrewGuardAvailability()
            let availability = self.homebrewGuardAvailability
            if availability.canPrepare {
                self.message = SafeStatusMessage(
                    code: "homebrew_guard_ready",
                    key: .homebrewGuardDetailReady
                )
            } else if let code = availability.errorCode {
                self.message = self.homebrewGuardMessage(for: code)
            }
            self.activityLog.record(
                category: .removal,
                code: "homebrew_guard_registration_finished",
                fields: [
                    "distribution": self.distributionFlavor.rawValue,
                    "phase": availability.registration.rawValue,
                    "result_code": availability.errorCode?.rawValue ?? "ready",
                ]
            )
        }
    }

    func developmentHomebrewGuardSetupCommand() -> String? {
        guard canShowDevelopmentHomebrewGuardSetup,
              let action = developmentHomebrewGuardSetupAction else { return nil }
        guard case let .available(command) = homebrewGuard.setupCommand(for: action) else {
            activityLog.record(
                .error,
                category: .removal,
                code: "homebrew_guard_setup_command_unavailable",
                fields: [
                    "backend": homebrewGuard.backend.rawValue,
                    "distribution": distributionFlavor.rawValue,
                    "phase": action.rawValue,
                    "result_code": "artifact_invalid",
                ]
            )
            return nil
        }
        activityLog.record(
            category: .removal,
            code: "homebrew_guard_setup_presented",
            fields: [
                "backend": homebrewGuard.backend.rawValue,
                "distribution": distributionFlavor.rawValue,
                "phase": action.rawValue,
                "result_code": "presented",
            ]
        )
        return command
    }

    func copyDevelopmentHomebrewGuardSetupCommand(_ command: String) {
        guard canShowDevelopmentHomebrewGuardSetup,
              command == developmentCommandWithoutLogging() else { return }
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(command, forType: .string)
        activityLog.record(
            category: .removal,
            code: "homebrew_guard_setup_command_copied",
            fields: [
                "backend": homebrewGuard.backend.rawValue,
                "distribution": distributionFlavor.rawValue,
                "result_code": "copied",
            ]
        )
    }

    func openHomebrewGuardSystemSettings() {
        guard canOpenHomebrewGuardSystemSettings, !isBusy else { return }
        homebrewGuard.openSystemSettingsLoginItems()
        activityLog.record(
            category: .removal,
            code: "homebrew_guard_settings_opened",
            fields: [
                "distribution": distributionFlavor.rawValue,
                "phase": "approval",
                "result_code": "opened",
            ]
        )
    }

    func recoverHomebrewGuardProtection() {
        guard canRecoverHomebrewGuard,
              let operationID = homebrewGuardAvailability.operationID,
              !isBusy else { return }
        let desktopURL = resolveSelectedDesktopTarget(missingKey: .messageDesktopNotSelectedHandoff)
        isBusy = true
        activityLog.record(
            category: .removal,
            code: "homebrew_guard_recovery_started",
            fields: ["distribution": distributionFlavor.rawValue, "phase": "permission_restore"]
        )
        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            do {
                try await self.homebrewGuard.recover(operationID: operationID)
                await self.refreshHomebrewGuardAvailability()
                if let desktopURL, let relaunchURL = self.revalidateDesktopURL(desktopURL) {
                    try await self.desktopApplication.relaunch(at: relaunchURL)
                    self.refreshDesktopTargetState()
                }
                await self.refreshStatus(showFailureMessage: false)
                self.message = SafeStatusMessage(
                    code: "homebrew_guard_recovered",
                    key: .homebrewGuardDetailReady
                )
                self.activityLog.record(
                    category: .removal,
                    code: "homebrew_guard_recovery_finished",
                    fields: [
                        "distribution": self.distributionFlavor.rawValue,
                        "phase": "permission_restore",
                        "result_code": "recovered",
                    ]
                )
            } catch let code as HomebrewGuardErrorCode {
                await self.refreshHomebrewGuardAvailability()
                let failure = self.homebrewGuardMessage(for: code)
                self.message = failure
                self.activityLog.record(
                    .error,
                    category: .removal,
                    code: "homebrew_guard_recovery_finished",
                    fields: [
                        "distribution": self.distributionFlavor.rawValue,
                        "failure_code": failure.code,
                        "phase": "permission_restore",
                        "result_code": failure.code,
                    ]
                )
            } catch {
                let failure = self.homebrewGuardMessage(for: .restoreFailed)
                self.message = failure
            }
        }
    }

    private func refreshHomebrewGuardAvailability(
        for candidate: OpenCodexInstallationCandidate?
    ) async {
        guard runtimeMode == .managed else {
            homebrewGuardAvailability = .preview
            return
        }
        let guardCandidate: HomebrewGuardCandidate?
        if let candidate {
            guard candidate.requiresHomebrewGuard else {
                if homebrewGuardAvailability.registration != .recoveryRequired {
                    homebrewGuardAvailability = .notRequired
                }
                return
            }
            do {
                guardCandidate = try candidate.homebrewGuardCandidate()
            } catch {
                homebrewGuardAvailability = HomebrewGuardAvailability(
                    registration: .unavailable,
                    helperVersion: nil,
                    protocolVersion: homebrewGuardProtocolVersion,
                    errorCode: .candidateChanged,
                    operationID: nil
                )
                return
            }
        } else {
            guardCandidate = nil
        }
        let previousRegistration = homebrewGuardAvailability.registration
        let refreshedAvailability = await homebrewGuard.availability(candidate: guardCandidate)
        homebrewGuardAvailability = refreshedAvailability
        if previousRegistration != refreshedAvailability.registration {
            activityLog.record(
                category: .removal,
                code: "homebrew_guard_availability_changed",
                fields: [
                    "backend": homebrewGuard.backend.rawValue,
                    "phase": refreshedAvailability.registration.rawValue,
                    "result_code": refreshedAvailability.errorCode?.rawValue ?? "ready",
                ]
            )
        }
    }

    private func homebrewGuardMessage(
        for code: HomebrewGuardErrorCode
    ) -> SafeStatusMessage {
        let key: AppStringKey = switch code {
        case .homebrewGuardNotRegistered: .messageHomebrewGuardNotRegistered
        case .approvalRequired: .messageHomebrewGuardApprovalRequired
        case .busy: .messageHomebrewGuardBusy
        case .candidateChanged: .messageHomebrewGuardCandidateChanged
        case .protectionFailed: .messageHomebrewGuardProtectionFailed
        case .recoveryRequired: .messageHomebrewGuardRecoveryRequired
        case .restoreFailed: .messageHomebrewGuardRestoreFailed
        }
        return SafeStatusMessage(code: code.rawValue, key: key)
    }

    private func developmentCommandWithoutLogging() -> String? {
        guard let action = developmentHomebrewGuardSetupAction else { return nil }
        guard case let .available(command) = homebrewGuard.setupCommand(for: action) else {
            return nil
        }
        return command
    }

    private var developmentHomebrewGuardSetupAction: HomebrewGuardSetupAction? {
        switch homebrewGuardAvailability.registration {
        case .manualInstallRequired:
            .install
        case .manualUpdateRequired, .daemonLaunchFailed:
            .update
        case .manualInstallerRecoveryRequired:
            .recover
        default:
            nil
        }
    }

    func makeCodexConfigurationController() -> CodexConfigurationController {
        CodexConfigurationController(
            bindingURL: bindingURL,
            reader: SecureCodexConfigurationReader(runtimeMode: runtimeMode),
            activityLog: activityLog
        ) { [weak self] in
            self?.refresh()
        }
    }

    func makeGatewaySettingsController() -> GatewaySettingsController {
        let receiptStore = UserDefaultsGatewayVerificationReceiptStore(
            key: "externalGatewayVerificationReceipt.\(distributionFlavor.rawValue).v1"
        )
        return GatewaySettingsController(
            resolver: { [weak self] in
                self?.resolveGatewaySettings() ?? GatewaySettingsResolution(
                    client: nil,
                    unavailability: .helperUnavailable
                )
            },
            integrationClient: runtimeMode == .managed
                ? ProcessSelfHostedIntegrationClient(executableURL: helperURL)
                : nil,
            credentialStore: SystemGatewayCredentialStore(),
            receiptStore: receiptStore,
            activityLog: activityLog
        ) { [weak self] in
            self?.refresh()
        }
    }

    private func resolveGatewaySettings() -> GatewaySettingsResolution {
        let gatewayClient: (any GatewayManaging)?
        var availability = refreshedIntegrationAvailability()
        if availability == .ready {
            do {
                let binding = try RoutingBindingReader.load(at: bindingURL)
                gatewayClient = ProcessGatewayClient(
                    executableURL: helperURL,
                    additionalArguments: binding.relayctlArguments
                )
            } catch let error as RoutingBindingError {
                availability = self.availability(for: error)
                integrationAvailability = availability
                gatewayClient = nil
            } catch {
                availability = .invalid
                integrationAvailability = availability
                gatewayClient = nil
            }
        } else {
            gatewayClient = nil
        }
        return GatewaySettingsResolution(
            client: gatewayClient,
            unavailability: GatewaySettingsUnavailability(availability)
        )
    }

	// A direct asynchronous refresh is useful to callers that need one
	// acknowledged local snapshot without creating a second polling loop. The
	// production UI uses `refresh()`/`start()`; XCTest uses this seam with
	// polling disabled to exercise quit/apply/relaunch deterministically.
	func refreshStatusNow() async {
		await refreshStatus(showFailureMessage: true)
	}

    func selectDesktopApplication() {
        let panel = desktopApplicationPicker()
        guard panel.runModal() == .OK, let url = panel.url else { return }

        registerDesktopApplication(at: url)
    }

    /// The picker is intentionally broad for package compatibility; a target
    /// is persisted only after exact bundle-ID, valid-signature, and reviewed
    /// Team-ID verification succeeds.
    func registerDesktopApplication(at url: URL) {
        switch desktopTrustValidator.verify(url, policy: desktopTrustPolicy) {
        case let .trusted(verified):
            commitDesktopTarget(verified, messageKey: .messageDesktopSelected, messageCode: "desktop_selected")
        case let .rejected(failure):
            refreshDesktopTargetState()
            if failure == .unavailable {
                message = SafeStatusMessage(
                    code: "desktop_selection_invalid",
                    key: .messageDesktopSelectionInvalid
                )
            } else {
                message = trustFailureMessage(for: failure)
            }
        }
    }

    func discoverCodexDesktopApplication() {
        discoverCodexDesktopApplication(showMessage: true)
    }

    private func discoverCodexDesktopApplication(showMessage: Bool) {
        guard let identity = desktopTrustPolicy.reviewedIdentity else {
            desktopTargetState = .trustConfigurationMissing
            if showMessage {
                message = trustFailureMessage(for: .configurationMissing)
            }
            return
        }
        let candidates = desktopDiscoverer.candidates(for: identity.bundleIdentifier)
        var verifiedByPath: [String: VerifiedCodexDesktop] = [:]
        for candidate in candidates {
            guard case let .trusted(verified) = desktopTrustValidator.verify(candidate, policy: desktopTrustPolicy) else {
                continue
            }
            verifiedByPath[verified.url.path] = verified
        }
        let verified = verifiedByPath.values.sorted { $0.url.path < $1.url.path }
        switch verified.count {
        case 1:
            commitDesktopTarget(
                verified[0],
                messageKey: showMessage ? .messageDesktopDiscovered : nil,
                messageCode: "desktop_discovered"
            )
        case 0:
            if !candidates.isEmpty {
                desktopTargetState = .untrusted
                if showMessage {
                    message = trustFailureMessage(for: .invalidSignature)
                }
            } else if selectedDesktopTarget == nil {
                desktopTargetState = .notRegistered
                if showMessage {
                    message = SafeStatusMessage(code: "desktop_not_found", key: .messageDesktopNotFound)
                }
            }
        default:
            desktopTargetState = .ambiguous
            if showMessage {
                message = SafeStatusMessage(code: "desktop_discovery_ambiguous", key: .messageDesktopDiscoveryAmbiguous)
            }
        }
    }

    private func commitDesktopTarget(
        _ verified: VerifiedCodexDesktop,
        messageKey: AppStringKey?,
        messageCode: String
    ) {
        let bookmark = try? verified.url.bookmarkData(
            options: [.withSecurityScope],
            includingResourceValuesForKeys: nil,
            relativeTo: nil
        )
        let target = DesktopTarget(url: verified.url, bookmark: bookmark)
        targetStore.desktopTarget = target
        selectedDesktopTarget = target
        desktopTargetState = .registered(running: desktopApplication.isRunning(at: verified.url))
        if let messageKey {
            message = SafeStatusMessage(
                code: messageCode,
                key: messageKey,
                arguments: [.literal(target.displayName)]
            )
        }
    }

    /// Keep the picker free of OS-level type filtering: macOS can surface an
    /// `.app` package as either a file or a directory depending on its UTI
    /// metadata. The trust validator remains the sole persistence boundary for
    /// an existing, correctly signed Codex application bundle.
    func desktopApplicationPicker() -> NSOpenPanel {
        let panel = NSOpenPanel()
        panel.title = localizer.text(.panelDesktopTitle)
        panel.message = localizer.text(.panelDesktopMessage)
        panel.prompt = localizer.text(.panelDesktopPrompt)
        panel.canChooseFiles = true
        panel.canChooseDirectories = true
        panel.treatsFilePackagesAsDirectories = false
        panel.allowsMultipleSelection = false
        panel.allowedContentTypes = []
        return panel
    }

    /// Presents the explicit, user-approved OpenCodex handoff. The MenuBar
    /// never searches PATH or invokes `ocx` directly: relayctl receives the
    /// fingerprinted absolute executable and carries out the selected action.
    func addLocalOpenCodexBackend() {
        guard requireOpenCodexDiscoveryAccess() else { return }
        guard resolveSelectedDesktopTarget(missingKey: .messageDesktopNotSelectedHandoff) != nil else { return }
        guard let discoveryClient = configuredOpenCodexDiscoveryClient(), !isBusy else { return }
        guard requireOpenCodexDiscoveryAccess() else { return }
        isBusy = true
        openCodexDiscoveryState = .searching(.a)
        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            do {
                let tierA = try await discoveryClient.discover(tier: .a, broadScanApproved: false)
                guard self.requireOpenCodexDiscoveryAccess() else { return }
                if !tierA.candidates.isEmpty {
                    self.openCodexDiscoveryState = .candidates(tierA)
                    return
                }
                self.openCodexDiscoveryState = .searching(.b)
                let tierB = try await discoveryClient.discover(tier: .b, broadScanApproved: false)
                guard self.requireOpenCodexDiscoveryAccess() else { return }
                if !tierB.candidates.isEmpty {
                    self.openCodexDiscoveryState = .candidates(tierB)
                } else {
                    self.openCodexDiscoveryState = .broadScanApprovalRequired(tierB)
                }
            } catch {
                guard self.requireOpenCodexDiscoveryAccess() else { return }
                let safe = self.safeMessage(for: error)
                self.openCodexDiscoveryState = .failed(safe.code)
                self.message = safe
            }
        }
    }

    func approveBroadOpenCodexDiscovery() {
        guard case .broadScanApprovalRequired = openCodexDiscoveryState,
              requireOpenCodexDiscoveryAccess(),
              let discoveryClient = configuredOpenCodexDiscoveryClient(),
              requireOpenCodexDiscoveryAccess(),
              !isBusy else { return }
        isBusy = true
        openCodexDiscoveryState = .searching(.c)
        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            do {
                let result = try await discoveryClient.discover(tier: .c, broadScanApproved: true)
                guard self.requireOpenCodexDiscoveryAccess() else { return }
                self.openCodexDiscoveryState = result.candidates.isEmpty ? .notFound(result) : .candidates(result)
            } catch {
                guard self.requireOpenCodexDiscoveryAccess() else { return }
                let safe = self.safeMessage(for: error)
                self.openCodexDiscoveryState = .failed(safe.code)
                self.message = safe
            }
        }
    }

    func cancelOpenCodexDiscovery() {
        guard !isBusy else { return }
        openCodexDiscoveryState = .idle
    }

    var discoveredOpenCodexCandidates: [OpenCodexInstallationCandidate] {
        switch openCodexDiscoveryState {
        case let .candidates(result): result.candidates
        default: []
        }
    }

    @discardableResult
    func chooseDiscoveredOpenCodexCandidate(id: String) -> Bool {
        guard requireOpenCodexDiscoveryAccess() else { return false }
        guard let candidate = discoveredOpenCodexCandidates.first(where: { $0.id == id }) else {
            message = SafeStatusMessage(code: "ocx_selection_invalid", key: .messageOCXSelectionInvalid)
            return false
        }
        do {
            openCodexRemovalFlow = OpenCodexRemovalFlow(
                candidate: candidate,
                selection: try OpenCodexRemovalSelection(candidate: candidate)
            )
            openCodexDiscoveryState = .idle
            Task { [weak self] in
                await self?.refreshHomebrewGuardAvailability(for: candidate)
            }
            return true
        } catch {
            message = SafeStatusMessage(code: "ocx_selection_invalid", key: .messageOCXSelectionInvalid)
            return false
        }
    }

    func selectOpenCodexExecutableManually() {
        guard requireOpenCodexDiscoveryAccess() else { return }
        let panel = NSOpenPanel()
        panel.title = localizer.text(.panelOpenCodexTitle)
        panel.message = localizer.text(.panelOpenCodexMessage)
        panel.prompt = localizer.text(.panelOpenCodexPrompt)
        panel.canChooseFiles = true
        panel.canChooseDirectories = false
        panel.allowsMultipleSelection = false
        guard panel.runModal() == .OK, let url = panel.url else { return }
        do {
            presentOpenCodexHandoff(for: try OpenCodexExecutableResolver.select(url))
        } catch let error as OpenCodexExecutableError {
            message = SafeStatusMessage(code: error.safeCode, key: key(for: error))
        } catch {
            message = SafeStatusMessage(code: "ocx_selection_invalid", key: .messageOCXSelectionInvalid)
        }
    }

    private func presentOpenCodexHandoff(for executable: OpenCodexExecutable) {
        let alert = NSAlert()
        alert.messageText = localizer.text(.handoffAlertTitle)
        alert.informativeText = localizer.text(.handoffAlertDetail)
        alert.addButton(withTitle: localizer.displayName(.retainProxyRemoveShim))
        alert.addButton(withTitle: localizer.displayName(.retainProxyKeepShim))
        alert.addButton(withTitle: localizer.text(.handoffCancel))
        let response = alert.runModal()
        let action: OpenCodexHandoffAction
        switch response {
        case .alertFirstButtonReturn:
            action = .retainProxyRemoveShim
        case .alertSecondButtonReturn:
            action = .retainProxyKeepShim
        default:
            return
        }
        performOpenCodexHandoff(executable: executable, action: action)
    }

    private func performOpenCodexHandoff(
        executable: OpenCodexExecutable,
        action: OpenCodexHandoffAction
    ) {
        guard canStartOpenCodexHandoff else {
            message = routingPreflightFailureMessage()
            activityLog.record(
                .warning,
                category: .handoff,
                code: "handoff_blocked",
                fields: ["failure_code": message?.code ?? "routing_status_unavailable", "handoff_phase": "preflight"]
            )
            return
        }
        do {
            let revalidated = try OpenCodexExecutableResolver.revalidate(executable)
            runDesktopExitCheckedCommand(
                command: .handoff(revalidated, action),
                successCode: "opencodex_handoff_completed",
                successKey: .messageOpenCodexHandoffCompleted
            )
        } catch let error as OpenCodexExecutableError {
            message = SafeStatusMessage(code: error.safeCode, key: key(for: error))
        } catch {
            message = SafeStatusMessage(code: "ocx_selection_invalid", key: .messageOCXSelectionInvalid)
        }
    }

    /// Request only records intent. It deliberately leaves the active relay
    /// route untouched until a later explicit graceful quit and apply action.
    func requestMode(_ target: RoutingRequestTarget) {
        guard resolveSelectedDesktopTarget(missingKey: .messageDesktopNotSelectedRouting) != nil else { return }
        guard canRequestRouting else {
            message = routingPreflightFailureMessage()
            return
        }
		if target == .localOpenCodex, status?.canAttemptLocalOpenCodex != true {
			message = SafeStatusMessage(
				code: "local_opencodex_unavailable",
				key: .messageLocalUnavailable
			)
			return
		}
        runCommand(
            activityKey: .messageRequesting,
            activityArguments: [.routingRequestTarget(target)],
            command: .request(target)
        )
    }

    /// Consumer onboarding action: record the External intent, gracefully
    /// stop the exact verified Codex Desktop app, apply, then relaunch it.
    /// Each relayctl call still enforces the current generation/config digest.
    func switchCodexToExternalGateway(
        expectedConfigDigest: String,
        expectedRoutingGeneration: UInt64
    ) {
        guard let currentStatus = status,
              currentStatus.phase != .applying,
              currentStatus.phase != .recoveryRequired,
              let desktopURL = resolveSelectedDesktopTarget(
                missingKey: .messageDesktopNotSelectedRouting
              ),
              let client = configuredRelayctlClient(),
              GatewayInspection.isDigest(expectedConfigDigest),
              expectedRoutingGeneration > 0,
              !isBusy else { return }
        if currentStatus.appliedBackend == .external &&
            currentStatus.desiredBackend == .external &&
            !currentStatus.needsDesktopApply {
            return
        }

        isBusy = true
        message = SafeStatusMessage(
            code: "routing_command_running",
            key: .messageRequesting,
            arguments: [.routingRequestTarget(.external)]
        )
        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            do {
                let requested = try await client.execute(.requestExternalMigratingKnownLegacy(
                    expectedConfigDigest: expectedConfigDigest,
                    expectedRoutingGeneration: expectedRoutingGeneration
                ))
                self.consume(requested)
            } catch {
                self.recordCommandFailure(error)
                return
            }
            guard await self.ensureVerifiedDesktopExited(at: desktopURL) != nil else {
                return
            }
            do {
                self.consume(try await client.execute(.apply))
            } catch {
                self.recordCommandFailure(error)
                return
            }
            guard let relaunchURL = self.revalidateDesktopURL(desktopURL) else { return }
            do {
                try await self.desktopApplication.relaunch(at: relaunchURL)
                self.refreshDesktopTargetState()
                try await self.refreshAfterModeAction(using: client)
                self.message = SafeStatusMessage(
                    code: "routing_applied",
                    key: .messageRoutingApplied
                )
            } catch {
                self.refreshDesktopTargetState()
                self.message = SafeStatusMessage(
                    code: "desktop_relaunch_failed",
                    key: .messageDesktopRelaunchFailed
                )
            }
        }
    }

    func completePendingTransition() {
        guard let currentStatus = status else {
            refresh()
            return
        }
		guard currentStatus.needsDesktopApply else {
            message = SafeStatusMessage(
                code: "no_pending_transition",
                key: .messageNoPendingTransition
            )
            return
        }
        runDesktopExitCheckedCommand(
            command: .apply,
            successCode: "routing_applied",
            successKey: .messageRoutingApplied
        )
    }

    func cancelPendingTransition() {
        guard canRequestRouting else {
            message = routingPreflightFailureMessage()
            return
        }
        runCommand(activityKey: .messageCanceling, command: .cancel)
    }

    func canRecover(_ action: RoutingRecoveryAction) -> Bool {
        desktopTargetState.canControl && status?.canRecover(action) == true
    }

    func recoveryReasonKey(for action: RoutingRecoveryAction) -> AppStringKey {
        switch status?.recoveryReason(for: action) {
        case "observed_state_unavailable": .controlCenterRecoveryObservedUnavailable
        case "journal_missing": .controlCenterRecoveryJournalMissing
        case "journal_malformed": .controlCenterRecoveryJournalMalformed
        case "journal_mismatch": .controlCenterRecoveryEvidenceMismatch
        case "origin_not_authoritative": .controlCenterRecoveryOriginUnavailable
        default: .controlCenterRecoveryUnavailable
        }
    }

	func recover(_ action: RoutingRecoveryAction) {
        guard let status, status.phase == .recoveryRequired else {
            message = SafeStatusMessage(
                code: "recovery_not_required",
                key: .messageRecoveryNotRequired
            )
            return
		}
        guard canRecover(action) else {
            message = SafeStatusMessage(
                code: "recovery_action_unavailable",
                key: .messageRecoveryNotRequired
            )
            return
        }
        let complete = action == .complete
        runDesktopExitCheckedCommand(
            command: complete ? .recoverComplete : .recoverRollback,
            successCode: complete ? "recovery_completed" : "recovery_rolled_back",
            successKey: complete ? .messageRecoveryCompleted : .messageRecoveryRolledBack
        )
    }


    var nativeRepairCandidates: [OpenCodexInstallationCandidate] {
        guard case let .candidates(result) = nativeRepairDiscoveryState else { return [] }
        return result.candidates
    }

    var canRunOwnedNativeRepair: Bool {
        guard canRepairNative,
              let status,
              let inspection = nativeRepairInspection,
              inspection.generation == status.generation else { return false }
        switch inspection.kind {
        case .localRelay:
            return true
        case .openCodex:
            guard nativeRepairOpenCodexCandidate?.nativeRepairSelection != nil,
                  let owner = nativeRepairOwnerInspection,
                  owner.generation == status.generation,
                  owner.owner == .openCodex,
                  owner.configuration == .valid else { return false }
            return owner.integration == .enabled || owner.integration == .disabled
        case .stateOnly, .unavailable:
            return false
        }
    }

    func inspectNativeRepair() {
        guard canRepairNative, let currentStatus = status else {
            message = SafeStatusMessage(code: "native_repair_unavailable", key: .messageNativeRepairUnavailable)
            return
        }
        guard let repairClient = configuredNativeRepairClient(), !isBusy else { return }
        isBusy = true
        nativeRepairInspection = nil
        nativeRepairOwnerInspection = nil
        nativeRepairOpenCodexCandidate = nil
        nativeRepairDiscoveryState = .idle
        nativeRepairProgress = nil
        activityLog.record(
            category: .repair,
            code: "native_repair_inspection_started",
            fields: ["generation": String(currentStatus.generation)]
        )
        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            do {
                let inspection = try await repairClient.inspect(expectedGeneration: currentStatus.generation)
                guard self.status?.generation == inspection.generation,
                      self.status?.phase == .recoveryRequired else {
                    throw RelayctlError.reported(.routingGenerationChanged)
                }
                self.nativeRepairInspection = inspection
                self.message = SafeStatusMessage(code: "native_repair_inspected", key: .messageNativeRepairInspected)
                self.activityLog.record(
                    category: .repair,
                    code: "native_repair_inspection_finished",
                    fields: self.nativeRepairFields(inspection, resultCode: "native_repair_inspected")
                )
                if inspection.kind == .openCodex,
                   let discoveryClient = self.configuredOpenCodexDiscoveryClient() {
                    await self.discoverNativeRepairCandidates(using: discoveryClient)
                }
            } catch {
                let safe = self.safeMessage(for: error)
                self.statusError = safe
                self.message = safe
                self.activityLog.record(
                    .error,
                    category: .repair,
                    code: "native_repair_inspection_finished",
                    fields: [
                        "failure_code": safe.code,
                        "generation": String(currentStatus.generation),
                        "result_code": safe.code,
                    ]
                )
            }
        }
    }

    func rediscoverNativeRepairOpenCodex() {
        guard nativeRepairInspection?.kind == .openCodex,
              let discoveryClient = configuredOpenCodexDiscoveryClient(),
              !isBusy else { return }
        isBusy = true
        nativeRepairOwnerInspection = nil
        nativeRepairOpenCodexCandidate = nil
        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            await self.discoverNativeRepairCandidates(using: discoveryClient)
        }
    }

    func chooseNativeRepairOpenCodexCandidate(id: String) {
        guard nativeRepairInspection?.kind == .openCodex,
              let candidate = nativeRepairCandidates.first(where: { $0.id == id }),
              candidate.nativeRepairSelection != nil,
              !isBusy else {
            nativeRepairOwnerInspection = nil
            nativeRepairOpenCodexCandidate = nil
            message = SafeStatusMessage(code: "ocx_selection_invalid", key: .messageOCXSelectionInvalid)
            return
        }
        do {
            _ = try nativeRepairSelection(for: candidate)
            nativeRepairOwnerInspection = nil
            nativeRepairOpenCodexCandidate = candidate
            inspectNativeRepairOwner(candidate: candidate)
        } catch {
            nativeRepairOwnerInspection = nil
            nativeRepairOpenCodexCandidate = nil
            message = SafeStatusMessage(code: "ocx_selection_changed", key: .messageOCXSelectionInvalid)
        }
    }

    private func inspectNativeRepairOwner(candidate: OpenCodexInstallationCandidate) {
        guard let inspection = nativeRepairInspection,
              inspection.kind == .openCodex,
              let repairClient = configuredNativeRepairClient(),
              !isBusy else { return }
        isBusy = true
        activityLog.record(
            category: .repair,
            code: "native_repair_owner_inspection_started",
            fields: nativeRepairFields(inspection, resultCode: "native_repair_owner_inspection_started")
        )
        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            do {
                let selection = try self.nativeRepairSelection(for: candidate)
                let owner = try await repairClient.inspectOwner(
                    expectedGeneration: inspection.generation,
                    owner: .openCodex,
                    openCodexSelection: selection
                )
                guard self.nativeRepairOpenCodexCandidate?.id == candidate.id,
                      self.status?.generation == owner.generation,
                      self.status?.phase == .recoveryRequired else {
                    throw RelayctlError.reported(.routingGenerationChanged)
                }
                self.nativeRepairOwnerInspection = owner
                self.message = SafeStatusMessage(code: "native_repair_owner_inspected", key: .messageNativeRepairOwnerInspected)
                self.recordNativeOwnerInspection(owner, resultCode: "native_repair_owner_inspected")
            } catch {
                self.nativeRepairOwnerInspection = nil
                let safe = self.safeMessage(for: error)
                self.statusError = safe
                self.message = safe
                self.activityLog.record(
                    .error,
                    category: .repair,
                    code: "native_repair_owner_inspection_finished",
                    fields: [
                        "failure_code": safe.code,
                        "generation": String(inspection.generation),
                        "owner": inspection.kind.rawValue,
                        "result_code": safe.code,
                    ]
                )
            }
        }
    }

    func repairNativeRouting() {
        guard canRunOwnedNativeRepair,
              let inspection = nativeRepairInspection,
              let currentStatus = status,
              let desktopURL = resolveSelectedDesktopTarget(missingKey: .messageDesktopNotSelectedRouting),
              let repairClient = configuredNativeRepairClient(),
              let routingClient = configuredRelayctlClient(),
              !isBusy else {
            message = SafeStatusMessage(code: "native_repair_unavailable", key: .messageNativeRepairUnavailable)
            return
        }
        let selection: OpenCodexNativeRepairSelection?
        do {
            selection = try nativeRepairSelection(for: inspection)
        } catch {
            message = safeMessage(for: error)
            return
        }
        isBusy = true
        statusError = nil
        message = SafeStatusMessage(code: "native_owner_repair_running", key: .messageNativeOwnerRepairRunning)
        nativeRepairProgress = NativeRepairProgress(
            inspection: inspection,
            currentStep: .preflight,
            failedStep: nil,
            result: nil,
            receipt: nil
        )
        activityLog.record(
            category: .repair,
            code: "native_routing_repair_started",
            fields: nativeRepairFields(inspection, resultCode: "native_routing_repair_started")
        )
        recordNativeRepairStep(.preflight, inspection: inspection)

        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            var desktopExited = false
            do {
                if let selection {
                    let owner = try await repairClient.inspectOwner(
                        expectedGeneration: currentStatus.generation,
                        owner: .openCodex,
                        openCodexSelection: selection
                    )
                    self.nativeRepairOwnerInspection = owner
                    self.recordNativeOwnerInspection(owner, resultCode: "native_repair_owner_preflight_completed")
                    guard owner.configuration == .valid,
                          owner.integration == .enabled || owner.integration == .disabled else {
                        if owner.configuration == .invalid {
                            throw RelayctlError.reported(.nativeOwnerConfigurationInvalid)
                        }
                        throw RelayctlError.reported(.nativeOwnerResultInvalid)
                    }
                }

                self.setNativeRepairStep(.desktopExit)
                guard await self.ensureVerifiedDesktopExited(at: desktopURL) != nil else {
                    throw RelayctlError.reported(.desktopExitConfirmationRequired)
                }
                desktopExited = true

                self.setNativeRepairStep(.ownerRepair)
                let receipt = try await repairClient.repair(
                    expectedGeneration: currentStatus.generation,
                    owner: inspection.kind,
                    openCodexSelection: selection
                )
                self.consume(receipt.status)
                self.setNativeRepairStep(.nativeVerification, receipt: receipt)
                await Task.yield()
                self.setNativeRepairStep(.stateCommit, receipt: receipt)
                await Task.yield()

                self.setNativeRepairStep(.desktopRelaunch, receipt: receipt)
                guard await self.relaunchNativeRepairDesktop(at: desktopURL) else {
                    throw RelayctlError.reported(.operationFailed)
                }
                desktopExited = false

                self.setNativeRepairStep(.statusRefresh, receipt: receipt)
                self.consume(try await routingClient.execute(.status))
                guard let repaired = self.status,
                      repaired.generation > currentStatus.generation,
                      repaired.phase == .nativeActive,
                      repaired.desiredBackend == .none,
                      repaired.appliedBackend == .none else {
                    throw RelayctlError.invalidStatus
                }
                let completed = SafeStatusMessage(
                    code: receipt.nonRoutingCleanupIncomplete
                        ? "native_owner_repair_completed_with_warning"
                        : "native_owner_repair_completed",
                    key: receipt.nonRoutingCleanupIncomplete
                        ? .messageNativeOwnerRepairCompletedWithWarning
                        : .messageNativeOwnerRepairCompleted
                )
                self.message = completed
                self.statusError = nil
                self.finishNativeRepairProgress(receipt: receipt, result: completed)
                self.nativeRepairInspection = nil
                self.nativeRepairOwnerInspection = nil
                self.nativeRepairOpenCodexCandidate = nil
                self.nativeRepairDiscoveryState = .idle
                var fields = self.nativeRepairFields(inspection, resultCode: completed.code)
                fields["generation"] = String(repaired.generation)
                fields["backup_created"] = String(receipt.backupCreated)
                fields["nonrouting_cleanup_incomplete"] = String(receipt.nonRoutingCleanupIncomplete)
                fields["owner_restore_attempts"] = String(receipt.ownerRestoreAttempts)
                fields["owner_restore_result"] = receipt.ownerRestoreResult.rawValue
                self.activityLog.record(
                    receipt.nonRoutingCleanupIncomplete ? .warning : .info,
                    category: .repair,
                    code: "native_routing_repair_finished",
                    fields: fields
                )
            } catch {
                let failedStep = self.nativeRepairProgress?.currentStep ?? .preflight
                if desktopExited {
                    let relaunched = await self.relaunchNativeRepairDesktop(at: desktopURL)
                    self.activityLog.record(
                        relaunched ? .info : .error,
                        category: .repair,
                        code: "desktop_relaunch_after_failure",
                        fields: [
                            "repair_phase": failedStep.rawValue,
                            "result_code": relaunched ? "completed" : "failed",
                        ]
                    )
                }
                if let refreshed = try? await routingClient.execute(.status) {
                    self.consume(refreshed)
                }
                await self.rediagnoseNativeRepairAfterFailure(
                    using: repairClient,
                    candidate: self.nativeRepairOpenCodexCandidate,
                    generation: currentStatus.generation
                )
                let safe = self.safeMessage(for: error)
                self.statusError = safe
                self.message = safe
                self.failNativeRepairProgress(at: failedStep, result: safe)
                var fields = self.nativeRepairFields(inspection, resultCode: safe.code)
                fields["failure_code"] = safe.code
                fields["repair_phase"] = failedStep.rawValue
                fields["retry_exhausted"] = String(safe.code == "native_owner_busy")
                self.activityLog.record(
                    .error,
                    category: .repair,
                    code: "native_routing_repair_finished",
                    fields: fields
                )
            }
        }
    }

    private func rediagnoseNativeRepairAfterFailure(
        using client: any NativeRepairExecuting,
        candidate: OpenCodexInstallationCandidate?,
        generation: UInt64
    ) async {
        guard status?.generation == generation, status?.phase == .recoveryRequired,
              let inspection = try? await client.inspect(expectedGeneration: generation) else { return }
        nativeRepairInspection = inspection
        if inspection.kind == .stateOnly {
            nativeRepairOwnerInspection = nil
            nativeRepairOpenCodexCandidate = nil
            nativeRepairDiscoveryState = .idle
            return
        }
        guard inspection.kind == .openCodex,
              let candidate,
              let selection = try? nativeRepairSelection(for: candidate),
              let owner = try? await client.inspectOwner(
                  expectedGeneration: generation,
                  owner: .openCodex,
                  openCodexSelection: selection
              ) else { return }
        nativeRepairOpenCodexCandidate = candidate
        nativeRepairOwnerInspection = owner
        recordNativeOwnerInspection(owner, resultCode: "native_repair_owner_reinspected")
    }

    private func discoverNativeRepairCandidates(using client: any OpenCodexDiscovering) async {
        do {
            nativeRepairDiscoveryState = .searching(.a)
            let tierA = try await client.discover(tier: .a, broadScanApproved: false)
            if !tierA.candidates.isEmpty {
                nativeRepairDiscoveryState = .candidates(tierA)
                return
            }
            nativeRepairDiscoveryState = .searching(.b)
            let tierB = try await client.discover(tier: .b, broadScanApproved: false)
            nativeRepairDiscoveryState = tierB.candidates.isEmpty ? .notFound(tierB) : .candidates(tierB)
        } catch {
            let safe = safeMessage(for: error)
            nativeRepairDiscoveryState = .failed(safe.code)
            message = safe
        }
    }

    private func nativeRepairSelection(for inspection: NativeRepairInspection) throws -> OpenCodexNativeRepairSelection? {
        switch inspection.kind {
        case .localRelay:
            return nil
        case .openCodex:
            guard let candidate = nativeRepairOpenCodexCandidate else {
                throw OpenCodexExecutableError.invalidSelection
            }
            return try nativeRepairSelection(for: candidate)
        case .stateOnly, .unavailable:
            throw RelayctlError.invalidStatus
        }
    }

    private func nativeRepairSelection(
        for candidate: OpenCodexInstallationCandidate
    ) throws -> OpenCodexNativeRepairSelection {
        guard let selection = candidate.nativeRepairSelection else {
            throw OpenCodexExecutableError.invalidSelection
        }
        let exact = try OpenCodexExecutableResolver.revalidate(selection.executable)
        return try OpenCodexNativeRepairSelection(
            installationID: selection.installationID,
            installationFingerprint: selection.installationFingerprint,
            nativeRestoreFingerprint: selection.nativeRestoreFingerprint,
            executable: exact
        )
    }

    private func setNativeRepairStep(
        _ step: NativeRepairFlowStep,
        receipt: NativeRoutingRepairReceipt? = nil
    ) {
        guard var progress = nativeRepairProgress else { return }
        progress.currentStep = step
        if let receipt { progress.receipt = receipt }
        nativeRepairProgress = progress
        recordNativeRepairStep(step, inspection: progress.inspection)
    }

    private func finishNativeRepairProgress(
        receipt: NativeRoutingRepairReceipt,
        result: SafeStatusMessage
    ) {
        guard var progress = nativeRepairProgress else { return }
        progress.currentStep = .statusRefresh
        progress.receipt = receipt
        progress.result = result
        nativeRepairProgress = progress
    }

    private func failNativeRepairProgress(at step: NativeRepairFlowStep, result: SafeStatusMessage) {
        guard var progress = nativeRepairProgress else { return }
        progress.currentStep = step
        progress.failedStep = step
        progress.result = result
        nativeRepairProgress = progress
    }

    private func recordNativeRepairStep(
        _ step: NativeRepairFlowStep,
        inspection: NativeRepairInspection
    ) {
        var fields = nativeRepairFields(inspection, resultCode: "native_repair_step")
        fields["repair_phase"] = step.rawValue
        activityLog.record(
            category: .repair,
            code: "native_routing_repair_step",
            fields: fields
        )
    }

    private func nativeRepairFields(
        _ inspection: NativeRepairInspection,
        resultCode: String
    ) -> [String: String] {
        [
            "generation": String(inspection.generation),
            "owner": inspection.kind.rawValue,
            "openai_base_url": String(inspection.openAIBaseURL),
            "model_catalog_json": String(inspection.modelCatalogJSON),
            "result_code": resultCode,
        ]
    }

    private func recordNativeOwnerInspection(
        _ owner: NativeRepairOwnerInspection,
        resultCode: String
    ) {
        activityLog.record(
            owner.configuration == .valid ? .info : .warning,
            category: .repair,
            code: "native_repair_owner_inspection_finished",
            fields: [
                "configuration": owner.configuration.rawValue,
                "generation": String(owner.generation),
                "integration": owner.integration.rawValue,
                "owner": owner.owner.rawValue,
                "result_code": resultCode,
            ]
        )
    }

    private func relaunchNativeRepairDesktop(at originalURL: URL) async -> Bool {
        guard let relaunchURL = revalidateDesktopURL(originalURL) else { return false }
        do {
            try await desktopApplication.relaunch(at: relaunchURL)
            refreshDesktopTargetState()
            return true
        } catch {
            refreshDesktopTargetState()
            return false
        }
    }


    func repairNative() {
        guard canRepairNative, let currentStatus = status else {
            message = SafeStatusMessage(code: "native_repair_unavailable", key: .messageNativeRepairUnavailable)
            return
        }
        guard let client = configuredRelayctlClient(), !isBusy else { return }
        isBusy = true
        message = SafeStatusMessage(code: "native_repair_running", key: .messageNativeRepairRunning)
        activityLog.record(
            category: .operation,
            code: "native_repair_started",
            fields: ["generation": String(currentStatus.generation), "phase": currentStatus.phase.rawValue]
        )
        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            do {
                self.consume(try await client.execute(.repairNative(expectedGeneration: currentStatus.generation)))
                try await self.refreshAfterModeAction(using: client)
                guard let repaired = self.status,
                      repaired.generation > currentStatus.generation,
                      repaired.phase == .nativeActive,
                      repaired.desiredBackend == .none,
                      repaired.appliedBackend == .none else {
                    throw RelayctlError.invalidStatus
                }
                self.message = SafeStatusMessage(code: "native_repair_completed", key: .messageNativeRepairCompleted)
                self.nativeRepairProgress = nil
                self.nativeRepairInspection = nil
                self.activityLog.record(
                    category: .operation,
                    code: "native_repair_finished",
                    fields: [
                        "generation": String(repaired.generation),
                        "phase": repaired.phase.rawValue,
                        "result_code": "native_repair_completed",
                    ]
                )
            } catch {
                let safe = self.safeMessage(for: error)
                self.statusError = safe
                self.message = safe
                self.activityLog.record(
                    .error,
                    category: .operation,
                    code: "native_repair_finished",
                    fields: [
                        "failure_code": safe.code,
                        "generation": String(currentStatus.generation),
                        "result_code": safe.code,
                    ]
                )
            }
        }
    }

	func relaunchSelectedDesktop() {
		guard let desktopURL = resolveSelectedDesktopTarget(), !isBusy else { return }
		isBusy = true
		Task { [weak self] in
			guard let self else { return }
			defer { self.isBusy = false }
            guard let trustedURL = self.revalidateDesktopURL(desktopURL) else { return }
			do {
				try await self.desktopApplication.relaunch(at: trustedURL)
				self.refreshDesktopTargetState()
				self.message = SafeStatusMessage(code: "desktop_relaunched", key: .messageDesktopRelaunched)
			} catch {
				self.refreshDesktopTargetState()
				self.message = SafeStatusMessage(code: "desktop_relaunch_failed", key: .messageDesktopRelaunchFailed)
			}
		}
	}

    private func runDesktopExitCheckedCommand(
        command: RelayctlCommand,
        successCode: String,
        successKey: AppStringKey
    ) {
        guard let desktopURL = resolveSelectedDesktopTarget() else { return }
        guard let client = configuredRelayctlClient() else {
            reportBindingFailure()
            return
        }
        guard !isBusy else { return }

        isBusy = true
        message = SafeStatusMessage(
            code: "desktop_quit_requested",
            key: .messageDesktopQuitRequested,
            arguments: [.literal(desktopURL.deletingPathExtension().lastPathComponent)]
        )
        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            guard await self.ensureVerifiedDesktopExited(at: desktopURL) != nil else {
                return
            }

			let appliedStatus: RoutingStatus
			do {
				appliedStatus = try await client.execute(command)
			} catch {
				self.recordCommandFailure(error)
				return
			}
			self.consume(appliedStatus)
            guard let relaunchURL = self.revalidateDesktopURL(desktopURL) else { return }
			do {
				try await self.desktopApplication.relaunch(at: relaunchURL)
				self.refreshDesktopTargetState()
			} catch {
				// The helper has already returned a verified post-mutation status.
				// Preserve it rather than clearing controls as if routing were
				// unknown; offer only an explicit retry for this exact app.
				self.refreshDesktopTargetState()
				self.message = SafeStatusMessage(code: "desktop_relaunch_failed", key: .messageDesktopRelaunchFailed)
				return
			}
			do {
				try await self.refreshAfterModeAction(using: client)
				self.message = SafeStatusMessage(code: successCode, key: successKey)
			} catch {
				// The route was acknowledged and the exact Desktop app relaunched;
				// only the confirmation refresh failed. Keep the acknowledged state
				// and let normal polling retry instead of inventing recovery.
				self.statusError = self.safeMessage(for: error)
				self.message = SafeStatusMessage(code: "routing_applied_refresh_pending", key: .messageRoutingAppliedRefreshPending)
			}
		}
    }

    private func pollLoop() async {
        while !Task.isCancelled {
            await refreshStatus(showFailureMessage: false)
            let interval = RoutingStatusPolling.intervalSeconds(
                status: status,
                isPopoverVisible: isInteractiveSurfaceVisible
            )
            do {
                try await Task.sleep(nanoseconds: UInt64(interval * 1_000_000_000))
            } catch {
                return
            }
        }
    }

    private func refreshStatus(showFailureMessage: Bool) async {
        guard !isBusy else {
            if showFailureMessage {
                activityLog.record(.warning, category: .refresh, code: "refresh_blocked")
            }
            if showFailureMessage { isRefreshing = false }
            return
        }
        if showFailureMessage {
            isRefreshing = true
            activityLog.record(category: .refresh, code: "refresh_requested")
        }
        guard !isStatusRefreshInFlight else {
            if showFailureMessage {
                pendingManualRefresh = true
                activityLog.record(category: .refresh, code: "refresh_queued")
            }
            return
        }
        guard let client = configuredRelayctlClient() else {
            if showFailureMessage {
                if integrationAvailability == .preview {
                    activityLog.record(
                        category: .refresh,
                        code: "refresh_unavailable",
                        fields: ["result_code": integrationAvailability.safeCode]
                    )
                } else {
                    reportBindingFailure()
                    activityLog.record(
                        .error,
                        category: .refresh,
                        code: "refresh_failed",
                        fields: ["failure_code": statusError?.code ?? "routing_binding_invalid"]
                    )
                }
                isRefreshing = false
            }
            return
        }
        isStatusRefreshInFlight = true
        defer {
            isStatusRefreshInFlight = false
            if showFailureMessage {
                isRefreshing = false
            }
            if pendingManualRefresh {
                pendingManualRefresh = false
                Task { [weak self] in
                    await self?.refreshStatus(showFailureMessage: true)
                }
            }
        }
        do {
            let changed = consume(try await client.execute(.status))
            if showFailureMessage {
                message = SafeStatusMessage(
                    code: changed ? "status_refreshed" : "status_unchanged",
                    key: changed ? .messageStatusRefreshed : .messageStatusUnchanged
                )
                var fields = ["changed": String(changed)]
                if let status {
                    fields["generation"] = String(status.generation)
                    fields["phase"] = status.phase.rawValue
                }
                activityLog.record(category: .refresh, code: "refresh_completed", fields: fields)
            }
        } catch {
            // Do not retain a stale phase after the local relay becomes
            // unreachable: status presentation must favour an unavailable
            // surface over a potentially obsolete active route label.
            status = nil
            refreshDesktopTargetState()
            let safe = safeMessage(for: error)
            statusError = safe
            if showFailureMessage {
                message = safe
                activityLog.record(
                    .error,
                    category: .refresh,
                    code: "refresh_failed",
                    fields: ["failure_code": safe.code]
                )
            }
        }
    }

    private func runCommand(
        activityKey: AppStringKey,
        activityArguments: [SafeStatusMessageArgument] = [],
        command: RelayctlCommand
    ) {
        guard !isBusy else { return }
        guard let client = configuredRelayctlClient() else {
            reportBindingFailure()
            return
        }
        isBusy = true
        message = SafeStatusMessage(code: "routing_command_running", key: activityKey, arguments: activityArguments)
        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            do {
                self.consume(try await client.execute(command))
                try await self.refreshAfterModeAction(using: client)
                self.message = nil
            } catch {
                self.recordCommandFailure(error)
            }
        }
    }

    @discardableResult
    private func consume(_ nextStatus: RoutingStatus) -> Bool {
        let previousSignature = status.map {
            activitySignature(
                routingActivityFields($0, desktopTargetState: desktopTargetState)
            )
        }
        status = nextStatus
        if let inspection = nativeRepairInspection,
           inspection.generation != nextStatus.generation || nextStatus.phase != .recoveryRequired {
            nativeRepairInspection = nil
            nativeRepairOwnerInspection = nil
            nativeRepairOpenCodexCandidate = nil
            nativeRepairDiscoveryState = .idle
        }
        lastStatusUpdatedAt = Date()
        statusError = nil
        refreshDesktopTargetState()
        updatePollingCadence()
        let fields = routingActivityFields(
            nextStatus,
            desktopTargetState: desktopTargetState
        )
        let changed = previousSignature != activitySignature(fields)
        if changed {
            activityLog.record(
                category: .status,
                code: "routing_snapshot_updated",
                fields: fields
            )
        }
        return changed
    }

    /// The status returned by a mutation is an acknowledgement from relayctl,
    /// not proof that the resident watcher has consumed the new generation.
    /// Re-query once after 400 ms, within the 250–500 ms action confirmation
    /// window, before returning the UI to its normal polling cadence.
    private func refreshAfterModeAction(using client: any RelayctlExecuting) async throws {
        try await Task.sleep(nanoseconds: 400_000_000)
        consume(try await client.execute(.status))
    }

    private func configuredRelayctlClient() -> (any RelayctlExecuting)? {
        guard runtimeMode == .managed else {
            integrationAvailability = .preview
            applyIntegrationFailure()
            return nil
        }
        if let injectedClient {
            return injectedClient
        }
        guard let binding = configuredBinding() else { return nil }
        return ProcessRelayctlClient(
            executableURL: helperURL,
            additionalArguments: binding.relayctlArguments
        )
    }


    private func configuredNativeRepairClient() -> (any NativeRepairExecuting)? {
        guard runtimeMode == .managed else {
            integrationAvailability = .preview
            applyIntegrationFailure()
            return nil
        }
        if let injectedNativeRepairClient { return injectedNativeRepairClient }
        guard let binding = configuredBinding() else {
            reportBindingFailure()
            return nil
        }
        return ProcessNativeRepairClient(
            executableURL: helperURL,
            additionalArguments: binding.relayctlArguments
        )
    }

    private func configuredOpenCodexDiscoveryClient() -> (any OpenCodexDiscovering)? {
        guard runtimeMode == .managed else {
            integrationAvailability = .preview
            applyIntegrationFailure()
            return nil
        }
        if let injectedDiscoveryClient {
            return injectedDiscoveryClient
        }
        guard let binding = configuredBinding() else {
            reportBindingFailure()
            return nil
        }
        return ProcessOpenCodexDiscoveryClient(
            executableURL: helperURL,
            additionalArguments: ["--config", binding.relayConfig]
        )
    }

    /// Discovery is an integration-owned operation even when a test or local
    /// development build injects its discovery transport. Re-read the binding
    /// before every direct entry point and after every asynchronous result so a
    /// removed or replaced binding cannot leave stale discovery controls live.
    private func requireOpenCodexDiscoveryAccess() -> Bool {
        let availability = refreshedIntegrationAvailability()
        guard availability.permitsManagedOperations else {
            openCodexDiscoveryState = .idle
            applyIntegrationFailure()
            reportBindingFailure()
            return false
        }
        guard canRequestRouting else {
            openCodexDiscoveryState = .idle
            message = routingPreflightFailureMessage()
            return false
        }
        return true
    }

    private func configuredOpenCodexRemovalClient() -> (any OpenCodexRemovalExecuting)? {
        guard runtimeMode == .managed else {
            integrationAvailability = .preview
            applyIntegrationFailure()
            return nil
        }
        if let injectedRemovalClient {
            return injectedRemovalClient
        }
        guard let binding = configuredBinding() else {
            reportBindingFailure()
            return nil
        }
        return ProcessOpenCodexRemovalClient(
            executableURL: helperURL,
            relayConfig: binding.relayConfig,
            codexConfig: binding.codexConfig
        )
    }

    private func configuredBinding() -> RoutingBinding? {
        guard refreshedIntegrationAvailability() == .ready else {
            applyIntegrationFailure()
            return nil
        }
        do {
            let binding = try RoutingBindingReader.load(at: bindingURL)
            statusError = nil
            return binding
        } catch let error as RoutingBindingError {
            integrationAvailability = availability(for: error)
        } catch {
            integrationAvailability = .invalid
        }
        applyIntegrationFailure()
        return nil
    }

    @discardableResult
    private func refreshedIntegrationAvailability() -> RelayIntegrationAvailability {
        if runtimeMode == .preview {
            integrationAvailability = .preview
            return .preview
        }
        if injectedClient != nil {
            integrationAvailability = .ready
            return .ready
        }
        let next = RelayIntegrationInspector.inspect(
            runtimeMode: runtimeMode,
            bindingURL: bindingURL,
            helperURL: helperURL
        )
        if next != integrationAvailability {
            activityLog.record(
                next == .ready ? .info : .warning,
                category: .status,
                code: "integration_availability_changed",
                fields: ["result_code": next.safeCode]
            )
        }
        integrationAvailability = next
        return next
    }

    private func applyIntegrationFailure() {
        status = nil
        if integrationAvailability == .preview {
            statusError = nil
        } else {
            statusError = integrationStatusMessage
        }
    }

    private func availability(for error: RoutingBindingError) -> RelayIntegrationAvailability {
        switch error {
        case .missing: .missing
        case .unsafeFile: .unsafe
        case .malformed: .invalid
        }
    }

    private func reportBindingFailure() {
        if let statusError {
            message = statusError
        }
    }

	private func recordCommandFailure(_ error: Error) {
        // A failed control invocation cannot prove the previously rendered
        // phase is still current. Surface an unavailable state until the next
        // successful status poll rather than leaving stale routing controls on
        // screen.
        let safe = safeMessage(for: error)
        status = nil
        statusError = safe
        message = safe
        refreshDesktopTargetState()
        updatePollingCadence()
    }

    private func resolveSelectedDesktopTarget(
        missingKey: AppStringKey = .messageDesktopNotSelectedApply
    ) -> URL? {
        guard let selectedDesktopTarget else {
            message = SafeStatusMessage(code: "desktop_not_selected", key: missingKey)
            return nil
        }
        do {
            let resolved = try DesktopTargetResolver.resolve(selectedDesktopTarget)
            return revalidateDesktopURL(resolved)
        } catch {
            desktopTargetState = .unavailable
            message = trustFailureMessage(for: .unavailable)
            return nil
        }
    }

    private func revalidateDesktopURL(_ url: URL) -> URL? {
        switch desktopTrustValidator.verify(url, policy: desktopTrustPolicy) {
        case let .trusted(verified):
            let expected = url.resolvingSymlinksInPath().standardizedFileURL
            guard verified.url == expected else {
                desktopTargetState = .untrusted
                message = trustFailureMessage(for: .invalidSignature)
                return nil
            }
            return verified.url
        case let .rejected(failure):
            desktopTargetState = state(for: failure)
            message = trustFailureMessage(for: failure)
            return nil
        }
    }

    private func refreshDesktopTargetState() {
        guard desktopTrustPolicy.reviewedIdentity != nil else {
            desktopTargetState = .trustConfigurationMissing
            return
        }
        guard let selectedDesktopTarget else {
            desktopTargetState = .notRegistered
            return
        }
        do {
            let url = try DesktopTargetResolver.resolve(selectedDesktopTarget)
            switch desktopTrustValidator.verify(url, policy: desktopTrustPolicy) {
            case let .trusted(verified):
                desktopTargetState = .registered(running: desktopApplication.isRunning(at: verified.url))
            case let .rejected(failure):
                desktopTargetState = state(for: failure)
            }
        } catch {
            desktopTargetState = .unavailable
        }
    }

    private func state(for failure: CodexDesktopTrustFailure) -> DesktopTargetState {
        switch failure {
        case .configurationMissing:
            return .trustConfigurationMissing
        case .unavailable:
            return .unavailable
        case .bundleIdentifierMismatch, .invalidSignature, .teamIdentifierMismatch:
            return .untrusted
        }
    }

    private func trustFailureMessage(for failure: CodexDesktopTrustFailure) -> SafeStatusMessage {
        switch failure {
        case .configurationMissing:
            return SafeStatusMessage(
                code: "desktop_trust_configuration_missing",
                key: .messageDesktopTrustConfigurationMissing
            )
        case .unavailable:
            return SafeStatusMessage(code: "desktop_unavailable", key: .messageDesktopUnavailable)
        case .bundleIdentifierMismatch, .invalidSignature, .teamIdentifierMismatch:
            return SafeStatusMessage(code: "desktop_trust_rejected", key: .messageDesktopTrustRejected)
        }
    }


    private func routingPreflightFailureMessage() -> SafeStatusMessage {
        if status?.phase == .recoveryRequired {
            return SafeStatusMessage(code: "routing_recovery_required", key: .messageRoutingRecoveryRequired)
        }
        return SafeStatusMessage(code: "routing_status_unavailable", key: .messageRoutingStatusUnavailable)
    }

    private func safeMessage(for error: Error) -> SafeStatusMessage {
        if let relayError = error as? RelayctlError {
            return SafeStatusMessage(code: relayError.safeCode, key: key(for: relayError))
        }
        return SafeStatusMessage(
            code: "routing_operation_failed",
            key: .messageRoutingOperationFailed
        )
    }

    private func key(for error: RoutingBindingError) -> AppStringKey {
        switch error {
        case .missing: .bindingMissing
        case .unsafeFile: .bindingUnsafe
        case .malformed: .bindingInvalid
        }
    }

    private func key(for error: RelayctlError) -> AppStringKey {
        switch error {
        case .helperUnavailable: .relayctlUnavailable
        case .invocationFailed: .relayctlFailed
        case let .reported(code):
            switch code {
            case .routingRecoveryRequired: .messageRoutingRecoveryRequired
            case .routingGenerationChanged: .messageRoutingGenerationChanged
            case .nativeRoutingUnverified: .messageNativeRoutingUnverified
            case .nativeRepairUnavailable: .messageNativeRepairUnavailable
            case .nativeRepairOwnerChanged: .messageNativeRepairOwnerChanged
            case .nativeOwnerRepairFailed: .messageNativeOwnerRepairFailed
            case .nativeOwnerBusy: .messageNativeOwnerBusy
            case .nativeOwnerConfigurationInvalid: .messageNativeOwnerConfigurationInvalid
            case .nativeOwnerRestoreFailed: .messageNativeOwnerRestoreFailed
            case .nativeOwnerResultInvalid: .messageNativeOwnerResultInvalid
            case .nativeStateRepairPending: .messageNativeStateRepairPending
            default: .relayctlFailed
            }
        case .invalidJSON, .invalidStatus: .relayctlInvalidStatus
        case .launchFailed: .relayctlLaunchFailed
        case .timedOut: .relayctlTimedOut
        case .cancelled: .relayctlCancelled
        case .outputTooLarge: .relayctlOutputTooLarge
        }
    }

    private func key(for error: OpenCodexExecutableError) -> AppStringKey {
        switch error {
        case .invalidSelection: .ocxInvalid
        case .unavailable: .ocxUnavailable
        case .tooLarge: .ocxTooLarge
        case .changed: .ocxChanged
        }
    }

    private func registerAtLogin() {
		switch LoginRegistrationCoordinator.ensureRegistered(loginRegistration) {
        case .enabled:
            loginItemMessage = .messageLoginEnabled
        case .pending:
            loginItemMessage = .messageLoginPending
        case .disabled:
            loginItemMessage = .messageLoginDisabled
        case .failed:
            loginItemMessage = .messageLoginFailed
        }
    }

    private func startPollingTask() {
        pollingTask = Task { [weak self] in
            await self?.pollLoop()
        }
    }

    private func updatePollingCadence(force: Bool = false) {
        let next = RoutingStatusPolling.intervalSeconds(
            status: status,
            isPopoverVisible: isInteractiveSurfaceVisible
        )
        guard force || pollingInterval != next else { return }
        pollingInterval = next
        // A queued manual refresh has priority over a cadence restart. Let the
        // current poll finish and its defer launch the one serialized manual
        // request; starting a replacement poll here would race and consume the
        // queued snapshot first.
        guard !(isStatusRefreshInFlight && pendingManualRefresh) else { return }
        guard pollingTask != nil else { return }
		let previousPollingTask = pollingTask
		pollingTask = nil
		previousPollingTask?.cancel()
        startPollingTask()
    }

    private func setInteractiveSurface(_ surface: InteractiveSurface, visible: Bool) {
        let wasInteractive = isInteractiveSurfaceVisible
        if visible {
            visibleInteractiveSurfaces.insert(surface)
            refresh()
        } else {
            visibleInteractiveSurfaces.remove(surface)
        }
        if wasInteractive != isInteractiveSurfaceVisible {
            updatePollingCadence(force: true)
        }
    }

    func recordControlCenterSection(_ section: String) {
        activityLog.record(
            category: .window,
            code: "control_center_section_selected",
            fields: ["section": section]
        )
    }

    private func routingActivityFields(
        _ status: RoutingStatus,
        desktopTargetState: DesktopTargetState
    ) -> [String: String] {
        [
            "schema": String(status.schemaVersion),
            "generation": String(status.generation),
            "phase": status.phase.rawValue,
            "desired_backend": status.desiredBackend.rawValue,
            "applied_backend": status.appliedBackend.rawValue,
            "local_relay": status.connection.localRelay.rawValue,
            "routing_sync": status.connection.routingSync.rawValue,
            "remote_gateway": status.connection.remoteGateway.rawValue,
            "local_opencodex": status.connection.localOpenCodex.rawValue,
            "catalog": status.connection.catalog.rawValue,
            "active_requests": status.activeRequests.map { String($0) } ?? "unavailable",
            "drain": drainActivityCode(status),
            "relay_running": String(status.relayRunning),
            "relay_admission": status.relayAdmission.rawValue,
            "catalog_refresh": status.catalogRefresh.rawValue,
            "desktop_restart_required": String(status.desktopRestartRequired),
            "desktop_target": desktopTargetActivityCode(desktopTargetState),
        ]
    }

    private func activitySignature(_ fields: [String: String]) -> String {
        fields.sorted { $0.key < $1.key }
            .map { "\($0.key)=\($0.value)" }
            .joined(separator: ":")
    }

    private func drainActivityCode(_ status: RoutingStatus) -> String {
        if status.isDraining { return "draining" }
        if status.phase == .applying { return "waiting_for_requests" }
        return "not_draining"
    }

    private func desktopTargetActivityCode(_ state: DesktopTargetState) -> String {
        switch state {
        case .notRegistered: "not_registered"
        case let .registered(running): running ? "registered_running" : "registered_stopped"
        case .unavailable: "unavailable"
        case .trustConfigurationMissing: "trust_configuration_missing"
        case .untrusted: "untrusted"
        case .ambiguous: "ambiguous"
        }
    }

    private func recordDiscoveryActivity(_ state: OpenCodexDiscoveryState) {
        let code: String
        var fields: [String: String] = [:]
        switch state {
        case .idle:
            code = "discovery_idle"
        case let .searching(tier):
            code = "discovery_searching"
            fields["tier"] = tier.rawValue
        case let .candidates(result):
            code = "discovery_candidates_found"
            fields["count"] = String(result.candidates.count)
        case let .broadScanApprovalRequired(result):
            code = "discovery_approval_required"
            fields["count"] = String(result.candidates.count)
        case let .notFound(result):
            code = "discovery_not_found"
            fields["count"] = String(result.candidates.count)
        case let .failed(failureCode):
            code = "discovery_failed"
            fields["failure_code"] = failureCode
        }
        activityLog.record(
            code == "discovery_failed" ? .error : .info,
            category: .discovery,
            code: code,
            fields: fields
        )
    }

    private func recordRemovalActivity(_ flow: OpenCodexRemovalFlow?) {
        guard let flow else {
            guard lastRemovalActivitySignature != nil else { return }
            lastRemovalActivitySignature = nil
            activityLog.record(category: .removal, code: "removal_flow_closed")
            return
        }
        let phase = removalPhaseCode(flow.phase)
        let handoffPhase = flow.handoffProgress.map { handoffProgressCode($0.phase) }
        let signature = [phase, handoffPhase ?? "none", flow.mode.rawValue].joined(separator: ":")
        guard signature != lastRemovalActivitySignature else { return }
        lastRemovalActivitySignature = signature
        var fields = [
            "phase": phase,
            "mode": flow.mode.rawValue,
            "automatic": String(flow.automaticRemovalEligible),
            "teardown_capability": String(flow.usesRelayPreservingTeardown),
            "data_preserved": String(flow.mode == .preserveData),
        ]
        if let adapterID = flow.teardownAdapterID {
            fields["adapter_id"] = adapterID
        }
        if let handoffPhase {
            fields["handoff_phase"] = handoffPhase
        }
        activityLog.record(category: .removal, code: "removal_phase_changed", fields: fields)
    }

    private func removalPhaseCode(_ phase: OpenCodexRemovalPhase) -> String {
        switch phase {
        case .actions: "actions"
        case .handoff: "handoff"
        case .loadingInventory: "loading_inventory"
        case .options: "options"
        case .confirmRemoval: "confirm_removal"
        case .confirmTrash: "confirm_trash"
        case .quittingDesktop: "quitting_desktop"
        case .removing: "removing"
        case .dataRefreshRequired: "data_refresh_required"
        case .rebootRequired: "reboot_required"
        case .routingRecoveryRequired: "routing_recovery_required"
        case .result: "result"
        case .failed: "failed"
        }
    }

    private func handoffProgressCode(_ phase: OpenCodexHandoffProgressPhase) -> String {
        switch phase {
        case .preflight: "preflight"
        case .desktopExit: "desktop_exit"
        case .openCodexOperation: "opencodex_operation"
        case .desktopRelaunch: "desktop_relaunch"
        case .statusRefresh: "status_refresh"
        case .completed: "completed"
        case .failed: "failed"
        }
    }
}



extension MenuBarModel {
    var canDismissOpenCodexRemoval: Bool {
        guard let flow = openCodexRemovalFlow else { return true }
        switch flow.phase {
        case .handoff, .loadingInventory, .quittingDesktop, .removing:
            return false
        case .actions, .options, .confirmRemoval, .confirmTrash, .dataRefreshRequired,
             .rebootRequired, .routingRecoveryRequired, .result, .failed:
            return true
        }
    }

    func dismissOpenCodexRemoval() {
        guard canDismissOpenCodexRemoval, !isBusy else { return }
        openCodexRemovalFlow = nil
    }

    func resumePendingOpenCodexRemoval() {
        guard openCodexRemovalFlow == nil, !isBusy else { return }
        do {
            guard let session = try removalRecoveryStore.load() else {
                hasPendingOpenCodexRemovalRecovery = false
                message = SafeStatusMessage(
                    code: "opencodex_recovery_context_missing",
                    key: .messageRemovalRecoveryUnavailable
                )
                return
            }
            openCodexRemovalFlow = OpenCodexRemovalFlow(recoverySession: session)
            hasPendingOpenCodexRemovalRecovery = true
        } catch {
            message = SafeStatusMessage(
                code: "opencodex_recovery_context_invalid",
                key: .messageRemovalRecoveryUnavailable
            )
        }
    }

    func chooseOpenCodexHandoffAction(_ action: OpenCodexHandoffAction) {
        guard !isBusy,
              var flow = openCodexRemovalFlow,
              flow.phase == .actions,
              let executable = flow.candidate?.handoffExecutable else {
            message = SafeStatusMessage(code: "ocx_selection_invalid", key: .messageOCXSelectionInvalid)
            return
        }
        guard canStartOpenCodexHandoff else {
            let blocked = routingPreflightFailureMessage()
            flow.handoffProgress = OpenCodexHandoffProgress(action: action)
            openCodexRemovalFlow = flow
            finishOpenCodexHandoff(action: action, failedAt: .preflight, result: blocked)
            activityLog.record(
                .warning,
                category: .handoff,
                code: "handoff_blocked",
                fields: ["failure_code": blocked.code, "handoff_phase": "preflight"]
            )
            return
        }

        let revalidated: OpenCodexExecutable
        do {
            revalidated = try OpenCodexExecutableResolver.revalidate(executable)
        } catch let error as OpenCodexExecutableError {
            finishOpenCodexHandoff(
                action: action,
                failedAt: .preflight,
                result: SafeStatusMessage(code: error.safeCode, key: key(for: error))
            )
            return
        } catch {
            finishOpenCodexHandoff(
                action: action,
                failedAt: .preflight,
                result: SafeStatusMessage(code: "ocx_selection_invalid", key: .messageOCXSelectionInvalid)
            )
            return
        }
        guard let desktopURL = resolveSelectedDesktopTarget(missingKey: .messageDesktopNotSelectedHandoff),
              let client = configuredRelayctlClient(),
              let discoveryClient = configuredOpenCodexDiscoveryClient() else {
            finishOpenCodexHandoff(
                action: action,
                failedAt: .preflight,
                result: message ?? SafeStatusMessage(
                    code: "routing_binding_invalid",
                    key: .messageRoutingBindingInvalid
                )
            )
            return
        }

        flow.phase = .handoff
        flow.handoffProgress = OpenCodexHandoffProgress(action: action)
        openCodexRemovalFlow = flow
        message = nil
        isBusy = true
        activityLog.record(
            category: .handoff,
            code: "handoff_started",
            fields: ["action": action.rawValue, "handoff_phase": "preflight"]
        )

        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }

            self.updateOpenCodexHandoffProgress(.desktopExit)
            guard await self.ensureVerifiedDesktopExited(at: desktopURL) != nil else {
                self.finishOpenCodexHandoff(
                    action: action,
                    failedAt: .desktopExit,
                    result: self.message ?? SafeStatusMessage(
                        code: "desktop_quit_timeout",
                        key: .messageDesktopQuitTimeout
                    )
                )
                return
            }

            // From this point the helper may have changed OpenCodex even when
            // it ultimately reports an error. Keep package removal locked until
            // a fresh candidate and status are both observed.
            self.requireOpenCodexCandidateRevalidation()
            self.updateOpenCodexHandoffProgress(.openCodexOperation)
            var commandFailure: SafeStatusMessage?
            do {
                self.consume(try await client.execute(.handoff(revalidated, action)))
            } catch {
                commandFailure = self.safeMessage(for: error)
                self.status = nil
                self.statusError = commandFailure
                self.updatePollingCadence()
            }

            self.updateOpenCodexHandoffProgress(.desktopRelaunch)
            var relaunchFailure: SafeStatusMessage?
            if let relaunchURL = self.revalidateDesktopURL(desktopURL) {
                do {
                    try await self.desktopApplication.relaunch(at: relaunchURL)
                    self.refreshDesktopTargetState()
                } catch {
                    self.refreshDesktopTargetState()
                    relaunchFailure = SafeStatusMessage(
                        code: "desktop_relaunch_failed",
                        key: .messageDesktopRelaunchFailed
                    )
                }
            } else {
                relaunchFailure = self.message ?? SafeStatusMessage(
                    code: "desktop_unavailable",
                    key: .messageDesktopUnavailable
                )
            }

            self.updateOpenCodexHandoffProgress(.statusRefresh)
            var refreshFailure: SafeStatusMessage?
            do {
                self.consume(try await client.execute(.status))
            } catch {
                refreshFailure = self.safeMessage(for: error)
                self.statusError = refreshFailure
            }

            var candidateFailure: SafeStatusMessage?
            if commandFailure == nil && refreshFailure == nil {
                do {
                    try await self.refreshOpenCodexCandidateAfterHandoff(
                        using: discoveryClient,
                        authorizesRemoval: relaunchFailure == nil
                    )
                } catch {
                    candidateFailure = SafeStatusMessage(
                        code: "opencodex_candidate_changed",
                        key: .messageRemovalCandidateChanged
                    )
                }
            }

            if let commandFailure {
                self.finishOpenCodexHandoff(
                    action: action,
                    failedAt: .openCodexOperation,
                    result: commandFailure
                )
            } else if let relaunchFailure {
                self.finishOpenCodexHandoff(
                    action: action,
                    failedAt: .desktopRelaunch,
                    result: relaunchFailure
                )
            } else if let refreshFailure {
                self.finishOpenCodexHandoff(
                    action: action,
                    failedAt: .statusRefresh,
                    result: refreshFailure
                )
            } else if candidateFailure != nil {
                // The OpenCodex operation itself completed, but package
                // removal remains locked until discovery can bind a unique
                // fresh fingerprint to the same canonical installation.
                self.finishOpenCodexHandoff(
                    action: action,
                    result: SafeStatusMessage(
                        code: "opencodex_handoff_candidate_refresh_required",
                        key: .messageOpenCodexHandoffCandidateRefreshRequired
                    )
                )
            } else {
                self.finishOpenCodexHandoff(
                    action: action,
                    result: SafeStatusMessage(
                        code: "opencodex_handoff_completed",
                        key: .messageOpenCodexHandoffCompleted
                    )
                )
            }
        }
    }

    private func requireOpenCodexCandidateRevalidation() {
        guard var flow = openCodexRemovalFlow else { return }
        flow.candidateRevalidationRequired = true
        openCodexRemovalFlow = flow
    }

    private func refreshOpenCodexCandidateAfterHandoff(
        using discoveryClient: any OpenCodexDiscovering,
        authorizesRemoval: Bool
    ) async throws {
        guard var flow = openCodexRemovalFlow,
              let previous = flow.candidate else {
            throw RelayctlError.reported(.openCodexCandidateChanged)
        }
        let result = try await discoveryClient.discover(
            tier: previous.tier,
            broadScanApproved: previous.tier == .c
        )
        let matches = result.candidates.filter {
            $0.packageRoot == previous.packageRoot && $0.executable == previous.executable
        }
        guard matches.count == 1, let candidate = matches.first else {
            throw RelayctlError.reported(.openCodexCandidateChanged)
        }
        flow.candidate = candidate
        flow.selection = try OpenCodexRemovalSelection(candidate: candidate)
        flow.candidateRevalidationRequired = !authorizesRemoval
        openCodexRemovalFlow = flow
        activityLog.record(
            category: .discovery,
            code: "discovery_candidate_revalidated",
            fields: ["count": "1", "tier": candidate.tier.rawValue]
        )
    }

    private func updateOpenCodexHandoffProgress(_ phase: OpenCodexHandoffProgressPhase) {
        guard var flow = openCodexRemovalFlow,
              var progress = flow.handoffProgress else { return }
        progress.phase = phase
        flow.phase = .handoff
        flow.handoffProgress = progress
        openCodexRemovalFlow = flow
    }

    private func finishOpenCodexHandoff(
        action: OpenCodexHandoffAction,
        failedAt: OpenCodexHandoffProgressPhase? = nil,
        result: SafeStatusMessage
    ) {
        guard var flow = openCodexRemovalFlow else {
            message = result
            return
        }
        var progress = flow.handoffProgress ?? OpenCodexHandoffProgress(action: action)
        progress.phase = failedAt == nil ? .completed : .failed
        progress.failedPhase = failedAt
        progress.result = result
        flow.phase = .actions
        flow.handoffProgress = progress
        openCodexRemovalFlow = flow
        message = nil
        activityLog.record(
            failedAt == nil ? .info : .error,
            category: .handoff,
            code: "handoff_finished",
            fields: ["action": action.rawValue, "result_code": result.code]
        )
    }

    func beginOpenCodexRemoval() {
        guard var flow = openCodexRemovalFlow,
              flow.phase == .actions,
              !isBusy else { return }
        guard flow.automaticRemovalEligible else {
            setOpenCodexRemovalFailure(
                SafeStatusMessage(
                    code: "opencodex_manual_removal_required",
                    key: .messageRemovalManualOnly
                )
            )
            return
        }
        guard resolveSelectedDesktopTarget(missingKey: .messageDesktopNotSelectedHandoff) != nil else {
            setOpenCodexRemovalFailure(
                message ?? SafeStatusMessage(
                    code: "desktop_not_selected",
                    key: .messageDesktopNotSelectedHandoff
                )
            )
            return
        }
        guard let routingClient = configuredRelayctlClient() else {
            setOpenCodexRemovalFailure(
                message ?? SafeStatusMessage(
                    code: "routing_binding_invalid",
                    key: .messageRoutingBindingInvalid
                )
            )
            return
        }

        flow.failure = nil
        openCodexRemovalFlow = flow
        isBusy = true
        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            do {
                let nextStatus = try await routingClient.execute(.status)
                self.consume(nextStatus)
                guard nextStatus.canUninstallOpenCodex else {
                    self.setOpenCodexRemovalFailure(SafeStatusMessage(
                        code: "opencodex_removal_route_unsafe",
                        key: .messageOpenCodexUninstallUnsafe
                    ))
                    return
                }
                guard var current = self.openCodexRemovalFlow,
                      current.selection == flow.selection else { return }
                current.inventory = nil
                current.mode = .preserveData
                current.selectedDataItemIDs = []
                current.expectedRoutingGeneration = nil
                current.phase = .options
                current.failure = nil
                self.openCodexRemovalFlow = current
                self.activityLog.record(
                    category: .removal,
                    code: "removal_review_started",
                    fields: [
                        "adapter_id": current.teardownAdapterID ?? "none",
                        "teardown_capability": String(current.usesRelayPreservingTeardown),
                        "data_preserved": "true",
                    ]
                )
            } catch {
                self.setOpenCodexRemovalFailure(self.safeOpenCodexRemovalMessage(for: error))
            }
        }
    }

    func setOpenCodexRemovalMode(_ mode: OpenCodexRemovalMode) {
        guard var flow = openCodexRemovalFlow,
              flow.phase == .options,
              !isBusy else { return }
        if mode == .trashSelected {
            guard flow.supportsSelectiveTrash,
                  let removalClient = configuredOpenCodexRemovalClient() else { return }
            flow.mode = .trashSelected
            flow.inventory = nil
            flow.selectedDataItemIDs = []
            flow.expectedRoutingGeneration = nil
            flow.phase = .loadingInventory
            openCodexRemovalFlow = flow
            isBusy = true
            Task { [weak self] in
                guard let self else { return }
                defer { self.isBusy = false }
                do {
                    let inventory = try await removalClient.inspect(selection: flow.selection)
                    guard var current = self.openCodexRemovalFlow,
                          current.selection == flow.selection,
                          current.phase == .loadingInventory,
                          inventory.installationFingerprint == flow.selection.installationFingerprint else {
                        return
                    }
                    current.inventory = inventory
                    current.recoveryInventoryRevision = nil
                    current.phase = .options
                    self.openCodexRemovalFlow = current
                } catch {
                    self.setOpenCodexRemovalFailure(self.safeOpenCodexRemovalMessage(for: error))
                }
            }
            return
        }
        flow.mode = mode
        flow.recoveryInventoryRevision = nil
        flow.expectedRoutingGeneration = nil
        flow.selectedDataItemIDs = []
        openCodexRemovalFlow = flow
        persistOpenCodexRecoveryDraftIfNeeded(flow)
    }

    func toggleOpenCodexDataItem(id: String) {
        guard var flow = openCodexRemovalFlow,
              flow.phase == .options,
              flow.mode == .trashSelected,
              let item = flow.inventoryItems.first(where: { $0.id == id }),
              flow.isSelectable(item),
              !isBusy else { return }
        if let index = flow.selectedDataItemIDs.firstIndex(of: id) {
            flow.selectedDataItemIDs.remove(at: index)
        } else if flow.selectedDataItemIDs.count < 128 {
            flow.selectedDataItemIDs.append(id)
        }
        flow.expectedRoutingGeneration = nil
        openCodexRemovalFlow = flow
        persistOpenCodexRecoveryDraftIfNeeded(flow)
    }

    func reviewOpenCodexRemoval() {
        guard var flow = openCodexRemovalFlow,
              flow.phase == .options,
              flow.canContinueFromOptions,
              !isBusy,
              let routingClient = configuredRelayctlClient() else { return }
        isBusy = true
        flow.failure = nil
        openCodexRemovalFlow = flow
        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            do {
                let nextStatus = try await routingClient.execute(.status)
                self.consume(nextStatus)
                guard nextStatus.canUninstallOpenCodex else {
                    self.invalidateOpenCodexRemovalReview()
                    return
                }
                guard var current = self.openCodexRemovalFlow,
                      current.selection == flow.selection,
                      current.phase == .options else { return }
                if current.mode == .trashSelected {
                    guard current.inventory?.routingGeneration == nextStatus.generation else {
                        current.inventory = nil
                        current.selectedDataItemIDs = []
                        current.phase = .options
                        current.failure = SafeStatusMessage(
                            code: "inventory_changed",
                            key: .messageRemovalGenerationChanged
                        )
                        self.openCodexRemovalFlow = current
                        return
                    }
                }
                current.expectedRoutingGeneration = nextStatus.generation
                current.phase = .confirmRemoval
                self.openCodexRemovalFlow = current
            } catch {
                self.setOpenCodexRemovalFailure(self.safeOpenCodexRemovalMessage(for: error))
            }
        }
    }

    func confirmOpenCodexPackageRemoval() {
        guard var flow = openCodexRemovalFlow,
              flow.phase == .confirmRemoval,
              removalReviewIsCurrent(flow) else {
            invalidateOpenCodexRemovalReview()
            return
        }
        if flow.mode == .trashSelected {
            flow.phase = .confirmTrash
            openCodexRemovalFlow = flow
            return
        }
        executeConfirmedOpenCodexRemoval(flow)
    }

    func confirmOpenCodexDataTrash() {
        guard let flow = openCodexRemovalFlow,
              flow.phase == .confirmTrash,
              flow.mode == .trashSelected,
              !flow.selectedDataItemIDs.isEmpty,
              removalReviewIsCurrent(flow) else {
            invalidateOpenCodexRemovalReview()
            return
        }
        executeConfirmedOpenCodexRemoval(flow)
    }

    func returnToOpenCodexRemovalOptions() {
        guard var flow = openCodexRemovalFlow,
              !isBusy else { return }
        switch flow.phase {
        case .confirmRemoval, .confirmTrash:
            flow.phase = .options
            flow.expectedRoutingGeneration = nil
            openCodexRemovalFlow = flow
        default:
            break
        }
    }

    func refreshInterruptedOpenCodexInventory() {
        guard let flow = openCodexRemovalFlow,
              flow.phase == .dataRefreshRequired,
              !isBusy,
              let routingClient = configuredRelayctlClient(),
              let discoveryClient = configuredOpenCodexDiscoveryClient(),
              let removalClient = configuredOpenCodexRemovalClient() else { return }
        isBusy = true
        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            do {
                let nextStatus = try await routingClient.execute(.status)
                self.consume(nextStatus)
                guard nextStatus.canUninstallOpenCodex else {
                    throw RelayctlError.invalidStatus
                }
                let candidate = try await self.rediscoverPreservingRemovalCandidate(
                    matching: flow.selection,
                    using: discoveryClient
                )
                guard candidate.dataCapability == .selectiveTrashV1 else {
                    throw OpenCodexRemovalContractError.invalidInventoryReceipt
                }
                let inventory = try await removalClient.inspect(selection: flow.selection)
                guard inventory.routingGeneration == nextStatus.generation,
                      inventory.installationFingerprint == flow.selection.installationFingerprint else {
                    throw OpenCodexRemovalContractError.invalidInventoryReceipt
                }
                guard var current = self.openCodexRemovalFlow,
                      current.selection == flow.selection,
                      current.phase == .dataRefreshRequired else { return }
                current.candidate = candidate
                current.candidateRevalidationRequired = false
                current.inventory = inventory
                current.recoveryInventoryRevision = nil
                current.mode = .trashSelected
                current.selectedDataItemIDs = []
                current.expectedRoutingGeneration = nil
                current.phase = .options
                current.failure = nil
                self.openCodexRemovalFlow = current
            } catch {
                self.setOpenCodexRemovalFailure(self.safeOpenCodexRemovalMessage(for: error))
            }
        }
    }

    func prepareRebootedOpenCodexRecovery() {
        prepareOpenCodexRecoveryConfirmation(requiredPhase: .rebootRequired, rebootConfirmation: true)
    }

    func checkOpenCodexRoutingRecovery() {
        guard var flow = openCodexRemovalFlow,
              flow.phase == .routingRecoveryRequired,
              flow.isSavedRoutingRecovery,
              let parkedGeneration = flow.expectedRoutingGeneration,
              parkedGeneration > 0,
              let desktopURL = resolveSelectedDesktopTarget(
                  missingKey: .messageDesktopNotSelectedHandoff
              ),
              !isBusy,
              let routingClient = configuredRelayctlClient() else {
            return
        }
        isBusy = true
        flow.failure = nil
        openCodexRemovalFlow = flow
        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            do {
                let gatedStatus = try await routingClient.execute(.status)
                self.consume(gatedStatus)
                guard let current = self.openCodexRemovalFlow,
                      current.selection == flow.selection,
                      current.phase == .routingRecoveryRequired,
                      current.expectedRoutingGeneration == parkedGeneration,
                      current.isSavedRoutingRecovery else {
                    return
                }
                guard gatedStatus.generation == parkedGeneration else {
                    if gatedStatus.generation > parkedGeneration &&
                        (gatedStatus.canUninstallOpenCodex ||
                            gatedStatus.canCheckpointSavedOpenCodexRoutingRecovery) {
                        var reboundFlow = current
                        reboundFlow.expectedRoutingGeneration = gatedStatus.generation
                        do {
                            // A strictly newer validated epoch invalidates the
                            // current action. Persist the same opaque selector
                            // at that epoch, but require another explicit user
                            // action before invoking the exact recovery helper.
                            try self.saveOpenCodexRecovery(
                                reboundFlow,
                                kind: .routingRecoveryRequired,
                                lastCode: "routing_generation_rebound"
                            )
                            self.openCodexRemovalFlow = reboundFlow
                        } catch {
                            self.setOpenCodexRemovalFailure(SafeStatusMessage(
                                code: "opencodex_recovery_context_invalid",
                                key: .messageRemovalRecoveryUnavailable
                            ))
                            return
                        }
                    }
                    self.message = SafeStatusMessage(
                        code: "routing_generation_changed",
                        key: .messageRemovalGenerationChanged
                    )
                    return
                }
                guard gatedStatus.canUninstallOpenCodex ||
                        gatedStatus.canReviewSavedOpenCodexRoutingRecovery else {
                    self.message = SafeStatusMessage(
                        code: "opencodex_removal_route_unsafe",
                        key: .messageOpenCodexUninstallUnsafe
                    )
                    return
                }
                guard let discoveryClient = self.configuredOpenCodexDiscoveryClient() else {
                    self.setOpenCodexRemovalFailure(SafeStatusMessage(
                        code: "routing_binding_invalid",
                        key: .messageRoutingBindingInvalid
                    ))
                    return
                }
                let reboundCandidate = try await self.rediscoverPreservingRemovalCandidate(
                    matching: current.selection,
                    using: discoveryClient
                )
                // A prior recover may already have succeeded just before the
                // app exited. Its checkpoint retains that resulting durable
                // generation, so a matching normal status can resume the
                // still-confirmed removal without attempting recovery again.
                if gatedStatus.canUninstallOpenCodex {
                    var resumedFlow = current
                    resumedFlow.candidate = reboundCandidate
                    resumedFlow.candidateRevalidationRequired = false
                    resumedFlow.mode = .preserveData
                    resumedFlow.selectedDataItemIDs = []
                    resumedFlow.phase = .confirmRemoval
                    self.openCodexRemovalFlow = resumedFlow
                    self.message = nil
                    return
                }
                self.message = SafeStatusMessage(
                    code: "desktop_quit_requested",
                    key: .messageDesktopQuitRequested,
                    arguments: [.literal(desktopURL.deletingPathExtension().lastPathComponent)]
                )
                guard await self.ensureVerifiedDesktopExited(at: desktopURL) != nil else {
                    return
                }

                let recoveredStatus = try await routingClient.execute(
                    .recoverOpenCodexRemoval(
                        selection: current.selection,
                        expectedRoutingGeneration: parkedGeneration
                    )
                )
                self.consume(recoveredStatus)
                // Go may have already durably released this exact recovery
                // token, in which case recover returns the same generation.
                // That equality is safe only for the fully stable strict
                // uninstall projection; lower generations always fail.
                guard recoveredStatus.generation >= parkedGeneration,
                      recoveredStatus.canUninstallOpenCodex else {
                    self.message = SafeStatusMessage(
                        code: "opencodex_removal_route_unsafe",
                        key: .messageOpenCodexUninstallUnsafe
                    )
                    return
                }
                guard var recoveredFlow = self.openCodexRemovalFlow,
                      recoveredFlow.selection == flow.selection,
                      recoveredFlow.phase == .routingRecoveryRequired,
                      recoveredFlow.expectedRoutingGeneration == parkedGeneration,
                      recoveredFlow.isSavedRoutingRecovery else {
                    return
                }
                recoveredFlow.expectedRoutingGeneration = recoveredStatus.generation
                recoveredFlow.candidate = reboundCandidate
                recoveredFlow.candidateRevalidationRequired = false
                recoveredFlow.mode = .preserveData
                recoveredFlow.selectedDataItemIDs = []
                do {
                    // Persist the post-recovery epoch before exposing the
                    // next confirmation. A termination in this interval then
                    // resumes against the stable route rather than a stale
                    // parked generation.
                    try self.saveOpenCodexRecovery(
                        recoveredFlow,
                        kind: .routingRecoveryRequired,
                        lastCode: "routing_recovery_completed"
                    )
                } catch {
                    self.setOpenCodexRemovalFailure(SafeStatusMessage(
                        code: "opencodex_recovery_context_invalid",
                        key: .messageRemovalRecoveryUnavailable
                    ))
                    return
                }
                recoveredFlow.phase = .confirmRemoval
                self.openCodexRemovalFlow = recoveredFlow
                self.message = nil
            } catch {
                self.message = self.safeOpenCodexRemovalMessage(for: error)
            }
        }
    }

    func requireRebootForOpenCodexResultRecovery() {
        guard var flow = openCodexRemovalFlow,
              flow.phase == .result,
              flow.receipt?.isSuccessful != true,
              !isBusy else { return }
        flow.phase = .rebootRequired
        flow.confirmsRebootedProcessRecovery = true
        flow.expectedRoutingGeneration = nil
        openCodexRemovalFlow = flow
        persistOpenCodexRecovery(flow, kind: .rebootRequired, lastCode: "reboot_required")
    }

    private func prepareOpenCodexRecoveryConfirmation(
        requiredPhase: OpenCodexRemovalPhase,
        rebootConfirmation: Bool
    ) {
        guard var flow = openCodexRemovalFlow,
              flow.phase == requiredPhase,
              !isBusy,
              let routingClient = configuredRelayctlClient() else { return }
        isBusy = true
        flow.failure = nil
        openCodexRemovalFlow = flow
        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }
            do {
                let nextStatus = try await routingClient.execute(.status)
                self.consume(nextStatus)
                let allowsSavedRecoveryReview =
                    flow.isSavedRebootOrInFlightRecovery &&
                    rebootConfirmation &&
                    nextStatus.canReviewSavedOpenCodexRemovalRecovery
                guard nextStatus.canUninstallOpenCodex || allowsSavedRecoveryReview else {
                    self.message = SafeStatusMessage(
                        code: "opencodex_removal_route_unsafe",
                        key: .messageOpenCodexUninstallUnsafe
                    )
                    return
                }
                guard let discoveryClient = self.configuredOpenCodexDiscoveryClient() else {
                    self.setOpenCodexRemovalFailure(SafeStatusMessage(
                        code: "opencodex_recovery_context_invalid",
                        key: .messageRemovalRecoveryUnavailable
                    ))
                    return
                }
                let reboundCandidate = try await self.rediscoverPreservingRemovalCandidate(
                    matching: flow.selection,
                    using: discoveryClient
                )
                guard var current = self.openCodexRemovalFlow,
                      current.selection == flow.selection,
                      current.phase == requiredPhase else { return }
                current.expectedRoutingGeneration = nextStatus.generation
                current.candidate = reboundCandidate
                current.candidateRevalidationRequired = false
                if current.mode == .trashSelected {
                    guard !current.selectedDataItemIDs.isEmpty,
                          current.recoveryInventoryRevision != nil else {
                        self.setOpenCodexRemovalFailure(SafeStatusMessage(
                            code: "opencodex_recovery_context_invalid",
                            key: .messageRemovalRecoveryUnavailable
                        ))
                        return
                    }
                } else {
                    current.selectedDataItemIDs = []
                }
                current.confirmsRebootedProcessRecovery = rebootConfirmation
                current.phase = .confirmRemoval
                self.openCodexRemovalFlow = current
            } catch {
                self.message = self.safeOpenCodexRemovalMessage(for: error)
            }
        }
    }

    private func executeConfirmedOpenCodexRemoval(_ confirmedFlow: OpenCodexRemovalFlow) {
        guard !isBusy,
              removalReviewIsCurrent(confirmedFlow),
              let generation = confirmedFlow.expectedRoutingGeneration,
              let desktopURL = resolveSelectedDesktopTarget(missingKey: .messageDesktopNotSelectedHandoff),
              let removalClient = configuredOpenCodexRemovalClient(),
              let routingClient = configuredRelayctlClient() else { return }
        guard confirmedFlow.usesRelayPreservingTeardown,
              confirmedFlow.automaticRemovalEligible else {
            setOpenCodexRemovalFailure(SafeStatusMessage(
                code: "teardown_unsupported",
                key: .messageRemovalTeardownUnsupported
            ))
            return
        }

        let guardCandidate: HomebrewGuardCandidate?
        let discoveryClient: (any OpenCodexDiscovering)?
        if confirmedFlow.requiresHomebrewGuard {
            guard canBeginOpenCodexRemoval,
                  let candidate = confirmedFlow.candidate else {
                message = homebrewGuardMessage(
                    for: homebrewGuardAvailability.errorCode ?? .homebrewGuardNotRegistered
                )
                return
            }
            do {
                guardCandidate = try candidate.homebrewGuardCandidate()
            } catch {
                message = homebrewGuardMessage(for: .candidateChanged)
                return
            }
            discoveryClient = configuredOpenCodexDiscoveryClient()
            guard discoveryClient != nil else {
                setOpenCodexRemovalFailure(SafeStatusMessage(
                    code: "routing_binding_invalid",
                    key: .messageRoutingBindingInvalid
                ))
                return
            }
        } else {
            guardCandidate = nil
            discoveryClient = nil
        }

        var progress = confirmedFlow
        progress.phase = .quittingDesktop
        progress.failure = nil
        progress.removalProgress = OpenCodexRemovalExecutionProgress(
            phase: .preflight,
            failedPhase: nil,
            result: nil,
            usesHomebrewGuard: guardCandidate != nil
        )
        openCodexRemovalFlow = progress
        isBusy = true

        Task { [weak self] in
            guard let self else { return }
            defer { self.isBusy = false }

            var desktopExited = false
            var guardOperationID: String?
            var guardPrepared = false
            var guardCommitted = false
            var removalStarted = false
            var removalOutcomeKnown = false

            do {
                self.updateOpenCodexRemovalProgress(.preflight)
                if let guardCandidate {
                    let availability = await self.homebrewGuard.availability(candidate: guardCandidate)
                    self.homebrewGuardAvailability = availability
                    guard availability.canPrepare else {
                        throw availability.errorCode ?? HomebrewGuardErrorCode.protectionFailed
                    }
                }

                self.updateOpenCodexRemovalProgress(.desktopExit)
                self.message = SafeStatusMessage(
                    code: "desktop_quit_requested",
                    key: .messageDesktopQuitRequested,
                    arguments: [.literal(desktopURL.deletingPathExtension().lastPathComponent)]
                )
                guard await self.ensureVerifiedDesktopExited(at: desktopURL) != nil else {
                    let failure = self.message ?? SafeStatusMessage(
                        code: "desktop_exit_unverified",
                        key: .messageRemovalFailed
                    )
                    self.finishOpenCodexRemovalProgress(failedAt: .desktopExit, result: failure)
                    self.setOpenCodexRemovalFailure(failure)
                    return
                }
                desktopExited = true

                if let guardCandidate, let discoveryClient {
                    let operationID = UUID().uuidString.lowercased()
                    guardOperationID = operationID
                    self.updateOpenCodexRemovalProgress(.homebrewProtection)
                    try await self.homebrewGuard.prepare(
                        candidate: guardCandidate,
                        operationID: operationID
                    )
                    guardPrepared = true
                    self.recordHomebrewGuardStep(
                        phase: .homebrewProtection,
                        resultCode: "prepared"
                    )

                    try await self.revalidateGuardedOpenCodexCandidate(
                        confirmedFlow,
                        using: discoveryClient
                    )
                    try await self.homebrewGuard.commit(operationID: operationID)
                    guardCommitted = true
                    self.homebrewGuardAvailability = HomebrewGuardAvailability(
                        registration: .busy,
                        helperVersion: self.homebrewGuardAvailability.helperVersion,
                        protocolVersion: homebrewGuardProtocolVersion,
                        errorCode: nil,
                        operationID: operationID
                    )
                    self.recordHomebrewGuardStep(
                        phase: .homebrewProtection,
                        resultCode: "committed"
                    )
                }

                guard var current = self.openCodexRemovalFlow,
                      current.selection == confirmedFlow.selection else {
                    throw HomebrewGuardErrorCode.candidateChanged
                }
                current.phase = .removing
                self.openCodexRemovalFlow = current
                self.updateOpenCodexRemovalProgress(.teardown)
                self.message = SafeStatusMessage(
                    code: "opencodex_removal_running",
                    key: .messageRemovalRunning
                )

                let request = try OpenCodexRemovalRequest(
                    selection: confirmedFlow.selection,
                    mode: confirmedFlow.mode,
                    dataItemIDs: confirmedFlow.selectedDataItemIDs,
                    expectedRoutingGeneration: generation,
                    expectedInventoryRevision: confirmedFlow.mode == .trashSelected
                        ? confirmedFlow.inventory?.inventoryRevision ?? confirmedFlow.recoveryInventoryRevision
                        : nil,
                    confirmsRemoval: true,
                    confirmsTrash: confirmedFlow.mode == .trashSelected,
                    confirmsInterruptedDataRefresh: confirmedFlow.confirmsInterruptedDataRefresh,
                    confirmsRebootedProcessRecovery: confirmedFlow.confirmsRebootedProcessRecovery,
                    confirmsDesktopExited: true
                )
                try self.saveOpenCodexRecovery(
                    confirmedFlow,
                    kind: .inFlight,
                    lastCode: "removal_started"
                )
                removalStarted = true
                let receipt = try await removalClient.remove(request)
                removalOutcomeKnown = true
                self.updateOpenCodexRemovalProgress(.packageRemoval)
                self.updateOpenCodexRemovalProgress(.resultVerification)

                if let operationID = guardOperationID {
                    self.updateOpenCodexRemovalProgress(.permissionRestore)
                    try await self.homebrewGuard.release(operationID: operationID)
                    guardPrepared = false
                    guardCommitted = false
                    await self.refreshHomebrewGuardAvailability()
                    self.recordHomebrewGuardStep(
                        phase: .permissionRestore,
                        resultCode: "released"
                    )
                }

                await self.consumeOpenCodexRemovalReceipt(
                    receipt,
                    request: request,
                    routingClient: routingClient,
                    desktopURL: desktopURL
                )
            } catch {
                let failedAt = self.openCodexRemovalFlow?.removalProgress?.phase ?? .preflight
                var cleanupFailure: HomebrewGuardErrorCode?

                if guardPrepared, !guardCommitted, let operationID = guardOperationID {
                    do {
                        self.updateOpenCodexRemovalProgress(.permissionRestore)
                        try await self.homebrewGuard.release(operationID: operationID)
                        guardPrepared = false
                        await self.refreshHomebrewGuardAvailability()
                        self.recordHomebrewGuardStep(
                            phase: .permissionRestore,
                            resultCode: "released_after_failure"
                        )
                    } catch let code as HomebrewGuardErrorCode {
                        cleanupFailure = code
                        await self.refreshHomebrewGuardAvailability()
                    } catch {
                        cleanupFailure = .restoreFailed
                        await self.refreshHomebrewGuardAvailability()
                    }
                } else if guardCommitted {
                    await self.refreshHomebrewGuardAvailability()
                }

                let failure: SafeStatusMessage
                if let cleanupFailure {
                    failure = self.homebrewGuardMessage(for: cleanupFailure)
                } else if let code = error as? HomebrewGuardErrorCode {
                    failure = self.homebrewGuardMessage(for: code)
                } else {
                    failure = self.safeOpenCodexRemovalMessage(for: error)
                }
                self.finishOpenCodexRemovalProgress(failedAt: failedAt, result: failure)

                if desktopExited, !guardCommitted, !removalStarted {
                    self.updateOpenCodexRemovalProgress(.desktopRelaunch)
                    _ = await self.relaunchDesktopAfterOpenCodexRemoval(at: desktopURL)
                }

                if removalStarted, !removalOutcomeKnown {
                    var interrupted = self.openCodexRemovalFlow ?? confirmedFlow
                    interrupted.phase = .rebootRequired
                    interrupted.failure = failure
                    interrupted.confirmsRebootedProcessRecovery = true
                    interrupted.expectedRoutingGeneration = nil
                    self.openCodexRemovalFlow = interrupted
                    self.persistOpenCodexRecovery(
                        interrupted,
                        kind: .rebootRequired,
                        lastCode: "process_outcome_unverified"
                    )
                    self.message = SafeStatusMessage(
                        code: "opencodex_process_outcome_unverified",
                        key: .messageRemovalRebootRequired
                    )
                } else {
                    self.setOpenCodexRemovalFailure(failure)
                }
            }
        }
    }

    private func updateOpenCodexRemovalProgress(
        _ phase: OpenCodexRemovalExecutionPhase
    ) {
        guard var flow = openCodexRemovalFlow,
              var progress = flow.removalProgress else { return }
        progress.phase = phase
        flow.removalProgress = progress
        openCodexRemovalFlow = flow
        activityLog.record(
            category: .removal,
            code: "removal_execution_step",
            fields: removalActivityFields(
                flow: flow,
                additional: [
                "distribution": distributionFlavor.rawValue,
                "phase": removalExecutionPhaseCode(phase),
                "result_code": "running",
                ]
            )
        )
    }

    private func finishOpenCodexRemovalProgress(
        failedAt: OpenCodexRemovalExecutionPhase? = nil,
        result: SafeStatusMessage
    ) {
        guard var flow = openCodexRemovalFlow,
              var progress = flow.removalProgress else { return }
        progress.phase = failedAt == nil ? .completed : .failed
        progress.failedPhase = failedAt
        progress.result = result
        flow.removalProgress = progress
        openCodexRemovalFlow = flow
        activityLog.record(
            failedAt == nil ? .info : .error,
            category: .removal,
            code: "removal_execution_finished",
            fields: removalActivityFields(
                flow: flow,
                additional: [
                "distribution": distributionFlavor.rawValue,
                "phase": failedAt.map(removalExecutionPhaseCode) ?? "completed",
                "result_code": result.code,
                ]
            )
        )
    }

    private func recordHomebrewGuardStep(
        phase: OpenCodexRemovalExecutionPhase,
        resultCode: String
    ) {
        activityLog.record(
            category: .removal,
            code: "homebrew_guard_step",
            fields: [
                "distribution": distributionFlavor.rawValue,
                "phase": removalExecutionPhaseCode(phase),
                "result_code": resultCode,
            ]
        )
    }

    private func removalExecutionPhaseCode(
        _ phase: OpenCodexRemovalExecutionPhase
    ) -> String {
        switch phase {
        case .preflight: "preflight"
        case .desktopExit: "desktop_exit"
        case .homebrewProtection: "homebrew_protection"
        case .candidateRevalidation: "candidate_revalidation"
        case .teardown: "teardown"
        case .packageRemoval: "package_removal"
        case .resultVerification: "result_verification"
        case .permissionRestore: "permission_restore"
        case .desktopRelaunch: "desktop_relaunch"
        case .statusRefresh: "status_refresh"
        case .completed: "completed"
        case .failed: "failed"
        }
    }

    private func revalidateGuardedOpenCodexCandidate(
        _ confirmedFlow: OpenCodexRemovalFlow,
        using discoveryClient: any OpenCodexDiscovering
    ) async throws {
        guard let previous = confirmedFlow.candidate,
              previous.requiresHomebrewGuard else {
            throw HomebrewGuardErrorCode.candidateChanged
        }
        let result = try await discoveryClient.discover(
            tier: previous.tier,
            broadScanApproved: previous.tier == .c
        )
        let matches = result.candidates.filter {
            $0.id == previous.id &&
                $0.fingerprint == previous.fingerprint &&
                $0.packageRoot == previous.packageRoot &&
                $0.executable == previous.executable &&
                $0.requiresHomebrewGuard &&
                $0.isAutomaticRemovalEligible &&
                $0.teardownCapability == .relayPreserveV1 &&
                ($0.dataCapability == .preserveOnly ||
                    $0.dataCapability == .selectiveTrashV1) &&
                $0.teardownAdapterID == previous.teardownAdapterID
        }
        guard matches.count == 1,
              let candidate = matches.first,
              try OpenCodexRemovalSelection(candidate: candidate) == confirmedFlow.selection else {
            throw HomebrewGuardErrorCode.candidateChanged
        }
        _ = try candidate.homebrewGuardCandidate()
    }

    private func consumeOpenCodexRemovalReceipt(
        _ receipt: OpenCodexRemovalReceipt,
        request: OpenCodexRemovalRequest,
        routingClient: any RelayctlExecuting,
        desktopURL: URL
    ) async {
        guard var flow = openCodexRemovalFlow,
              flow.selection == request.selection else { return }
        flow.receipt = receipt
        flow.failure = nil
        flow.expectedRoutingGeneration = nil

        if let failureCode = receipt.verifiedPreMutationFailureCode {
            let failure = preMutationOpenCodexRemovalMessage(code: failureCode)
            flow.phase = .result
            flow.failure = failure
            openCodexRemovalFlow = flow
            removalRecoveryStore.clear()
            hasPendingOpenCodexRemovalRecovery = false

            updateOpenCodexRemovalProgress(.desktopRelaunch)
            let relaunched = await relaunchDesktopAfterOpenCodexRemoval(at: desktopURL)
            activityLog.record(
                relaunched ? .info : .error,
                category: .removal,
                code: "desktop_relaunch_after_failure",
                fields: ["result_code": relaunched ? "completed" : "desktop_relaunch_failed"]
            )
            updateOpenCodexRemovalProgress(.statusRefresh)
            await refreshRoutingAfterRemoval(using: routingClient)
            message = failure
            finishOpenCodexRemovalProgress(failedAt: .resultVerification, result: failure)
            return
        }

        // A reboot is the stronger interruption boundary. In the unusual
        // combined receipt, never carry the data-refresh acknowledgement over
        // the restart; restore as reboot recovery and obtain a fresh inventory
        // only after that continuation has been safely reviewed.
        if receipt.requiresWholeMacReboot {
            flow.confirmsInterruptedDataRefresh = false
            flow.phase = .rebootRequired
            flow.confirmsRebootedProcessRecovery = true
            openCodexRemovalFlow = flow
            persistOpenCodexRecovery(
                flow,
                kind: .rebootRequired,
                lastCode: "process_cleanup_unverified"
            )
            message = SafeStatusMessage(
                code: "process_cleanup_unverified",
                key: .messageRemovalRebootRequired
            )
            return
        }

        if receipt.routingRecoveryRequired {
            flow.phase = .routingRecoveryRequired
            openCodexRemovalFlow = flow
            await refreshRoutingAfterRemoval(using: routingClient)
            guard let parkedStatus = status,
                  parkedStatus.canReviewSavedOpenCodexRemovalRecovery else {
                message = SafeStatusMessage(
                    code: "opencodex_recovery_context_invalid",
                    key: .messageRemovalRecoveryUnavailable
                )
                return
            }
            // Normalize the live flow to the same restricted shape that the
            // persisted session restores. Without this, the in-memory
            // candidate-bearing flow cannot pass the saved-continuation
            // predicate until the app is relaunched.
            flow.candidate = nil
            flow.recoveryKind = .routingRecoveryRequired
            flow.expectedRoutingGeneration = parkedStatus.generation
            openCodexRemovalFlow = flow
            persistOpenCodexRecovery(
                flow,
                kind: .routingRecoveryRequired,
                lastCode: "routing_recovery_required"
            )
            message = SafeStatusMessage(
                code: "routing_recovery_required",
                key: .messageRemovalRoutingRecoveryRequired
            )
            return
        }

        if receipt.requiresDataSelectionRefresh {
            flow.retiredDataItemIDs.formUnion(request.dataItemIDs)
            flow.selectedDataItemIDs = []
            flow.inventory = nil
            flow.mode = .trashSelected
            flow.confirmsInterruptedDataRefresh = true
            flow.confirmsRebootedProcessRecovery = false
            flow.phase = .dataRefreshRequired
            openCodexRemovalFlow = flow
            persistOpenCodexRecovery(
                flow,
                kind: .dataSelectionRefreshRequired,
                lastCode: "data_selection_refresh_required"
            )
            message = SafeStatusMessage(
                code: "data_selection_refresh_required",
                key: .messageRemovalDataRefreshRequired
            )
            return
        }

        flow.phase = .result
        openCodexRemovalFlow = flow
        if receipt.isSuccessful {
            removalRecoveryStore.clear()
            hasPendingOpenCodexRemovalRecovery = false
            updateOpenCodexRemovalProgress(.desktopRelaunch)
            guard await relaunchDesktopAfterOpenCodexRemoval(at: desktopURL) else {
                let failure = SafeStatusMessage(
                    code: "desktop_relaunch_failed",
                    key: .messageDesktopRelaunchFailed
                )
                finishOpenCodexRemovalProgress(failedAt: .desktopRelaunch, result: failure)
                return
            }
            updateOpenCodexRemovalProgress(.statusRefresh)
            await refreshRoutingAfterRemoval(using: routingClient)
            let completed = SafeStatusMessage(code: "opencodex_removed", key: .messageOpenCodexRemoved)
            message = completed
            finishOpenCodexRemovalProgress(result: completed)
        } else {
            persistOpenCodexRecovery(
                flow,
                kind: .inFlight,
                lastCode: receipt.stages.last?.code ?? "operation_failed"
            )
            message = SafeStatusMessage(
                code: "opencodex_removal_partial",
                key: .messageRemovalPartial
            )
            if let result = message {
                finishOpenCodexRemovalProgress(failedAt: .resultVerification, result: result)
            }
        }
    }

    private func refreshRoutingAfterRemoval(using client: any RelayctlExecuting) async {
        do {
            consume(try await client.execute(.status))
        } catch {
            status = nil
            statusError = safeMessage(for: error)
            refreshDesktopTargetState()
            updatePollingCadence()
        }
    }

    private func relaunchDesktopAfterOpenCodexRemoval(at originalURL: URL) async -> Bool {
        guard let relaunchURL = revalidateDesktopURL(originalURL) else { return false }
        do {
            try await desktopApplication.relaunch(at: relaunchURL)
            refreshDesktopTargetState()
            return true
        } catch {
            refreshDesktopTargetState()
            message = SafeStatusMessage(
                code: "desktop_relaunch_failed",
                key: .messageDesktopRelaunchFailed
            )
            return false
        }
    }

    private func preMutationOpenCodexRemovalMessage(code: String) -> SafeStatusMessage {
        let key: AppStringKey
        switch code {
        case "candidate_changed":
            key = .messageRemovalCandidateChanged
        case "manual_removal_required":
            key = .messageRemovalManualOnly
        case "teardown_unsupported":
            key = .messageRemovalTeardownUnsupported
        case "teardown_candidate_changed":
            key = .messageRemovalTeardownCandidateChanged
        case "teardown_preflight_failed":
            key = .messageRemovalTeardownPreflightFailed
        default:
            key = .messageRemovalFailed
        }
        return SafeStatusMessage(code: code, key: key)
    }

    private func ensureVerifiedDesktopExited(at desktopURL: URL) async -> URL? {
        if desktopApplication.isRunning(at: desktopURL) {
            guard desktopApplication.requestGracefulQuit(at: desktopURL) else {
                message = SafeStatusMessage(
                    code: "desktop_quit_declined",
                    key: .messageDesktopQuitDeclined
                )
                return nil
            }
            guard await desktopApplication.waitForExit(at: desktopURL, timeout: 30) else {
                message = SafeStatusMessage(
                    code: "desktop_quit_timeout",
                    key: .messageDesktopQuitTimeout
                )
                return nil
            }
        }
        guard let preflightURL = revalidateDesktopURL(desktopURL) else { return nil }
        guard !desktopApplication.isRunning(at: preflightURL) else {
            refreshDesktopTargetState()
            message = SafeStatusMessage(
                code: "desktop_restarted",
                key: .messageDesktopRestarted
            )
            return nil
        }
        return preflightURL
    }

    private func removalReviewIsCurrent(_ flow: OpenCodexRemovalFlow) -> Bool {
        guard let generation = flow.expectedRoutingGeneration,
              let status,
              status.generation == generation else {
            return false
        }
        return status.canUninstallOpenCodex ||
            (flow.isSavedRebootOrInFlightRecovery &&
                status.canReviewSavedOpenCodexRemovalRecovery)
    }

    private func invalidateOpenCodexRemovalReview() {
        guard var flow = openCodexRemovalFlow else { return }
        if flow.candidate != nil {
            flow.phase = .options
        } else if flow.confirmsRebootedProcessRecovery {
            flow.phase = .rebootRequired
        } else {
            flow.phase = .routingRecoveryRequired
        }
        flow.expectedRoutingGeneration = nil
        openCodexRemovalFlow = flow
        message = SafeStatusMessage(
            code: "routing_generation_changed",
            key: .messageRemovalGenerationChanged
        )
    }

    private func setOpenCodexRemovalFailure(_ failure: SafeStatusMessage) {
        guard var flow = openCodexRemovalFlow else {
            message = failure
            return
        }
        flow.phase = .failed
        flow.failure = failure
        flow.expectedRoutingGeneration = nil
        openCodexRemovalFlow = flow
        message = failure
    }

    private func safeOpenCodexRemovalMessage(for error: Error) -> SafeStatusMessage {
        if let contract = error as? OpenCodexRemovalContractError {
            switch contract {
            case .invalidSelection, .invalidRequest:
                return SafeStatusMessage(code: contract.safeCode, key: .messageRemovalRequestInvalid)
            case .invalidInventoryReceipt:
                return SafeStatusMessage(code: contract.safeCode, key: .messageRemovalInventoryInvalid)
            case .invalidRemovalReceipt:
                return SafeStatusMessage(code: contract.safeCode, key: .messageRemovalReceiptInvalid)
            }
        }
        if let relay = error as? RelayctlError {
            if case let .reported(code) = relay {
                switch code {
                case .openCodexCandidateChanged:
                    return SafeStatusMessage(code: relay.safeCode, key: .messageRemovalCandidateChanged)
                case .openCodexManualRemovalRequired, .permissionRequired:
                    return SafeStatusMessage(code: relay.safeCode, key: .messageRemovalManualOnly)
                case .openCodexCleanupJournalUnsafe:
                    return SafeStatusMessage(code: relay.safeCode, key: .messageRemovalRecoveryUnavailable)
                case .teardownUnsupported:
                    return SafeStatusMessage(code: relay.safeCode, key: .messageRemovalTeardownUnsupported)
                case .teardownCandidateChanged:
                    return SafeStatusMessage(code: relay.safeCode, key: .messageRemovalTeardownCandidateChanged)
                case .teardownPreflightFailed:
                    return SafeStatusMessage(code: relay.safeCode, key: .messageRemovalTeardownPreflightFailed)
                case .teardownRefused:
                    return SafeStatusMessage(code: relay.safeCode, key: .messageRemovalTeardownRefused)
                case .teardownResultInvalid:
                    return SafeStatusMessage(code: relay.safeCode, key: .messageRemovalTeardownResultInvalid)
                case .teardownVerificationFailed:
                    return SafeStatusMessage(code: relay.safeCode, key: .messageRemovalTeardownVerificationFailed)
                case .routingRecoveryRequired:
                    return SafeStatusMessage(code: relay.safeCode, key: .messageRemovalRoutingRecoveryRequired)
                default:
                    break
                }
            }
            return SafeStatusMessage(code: relay.safeCode, key: key(for: relay))
        }
        return SafeStatusMessage(code: "opencodex_removal_failed", key: .messageRemovalFailed)
    }

    private func rediscoverPreservingRemovalCandidate(
        matching selection: OpenCodexRemovalSelection,
        using discoveryClient: any OpenCodexDiscovering
    ) async throws -> OpenCodexInstallationCandidate {
        for tier in [OpenCodexDiscoveryTier.a, .b] {
            let result = try await discoveryClient.discover(tier: tier, broadScanApproved: false)
            let matches = result.candidates.filter { candidate in
                candidate.id == selection.installationID &&
                    candidate.fingerprint == selection.installationFingerprint &&
                    candidate.isAutomaticRemovalEligible &&
                    candidate.teardownCapability == .relayPreserveV1 &&
                    (candidate.dataCapability == .preserveOnly ||
                        candidate.dataCapability == .selectiveTrashV1) &&
                    candidate.teardownCompatibilityReason == "compatible" &&
                    candidate.teardownAdapterID != nil
            }
            guard matches.count <= 1 else {
                throw RelayctlError.reported(.teardownCandidateChanged)
            }
            if let candidate = matches.first,
               try OpenCodexRemovalSelection(candidate: candidate) == selection {
                return candidate
            }
        }
        throw RelayctlError.reported(.teardownCandidateChanged)
    }

    private func removalActivityFields(
        flow: OpenCodexRemovalFlow,
        additional: [String: String]
    ) -> [String: String] {
        var fields = additional
        fields["teardown_capability"] = String(flow.usesRelayPreservingTeardown)
        fields["data_preserved"] = String(flow.mode == .preserveData)
        if let adapterID = flow.teardownAdapterID {
            fields["adapter_id"] = adapterID
        }
        return fields
    }

    private func persistOpenCodexRecoveryDraftIfNeeded(_ flow: OpenCodexRemovalFlow) {
        guard flow.confirmsInterruptedDataRefresh || flow.confirmsRebootedProcessRecovery || flow.candidate == nil else {
            return
        }
        let kind: OpenCodexRemovalRecoveryKind
        if flow.confirmsInterruptedDataRefresh {
            kind = .dataSelectionRefreshRequired
        } else if flow.confirmsRebootedProcessRecovery {
            kind = .rebootRequired
        } else {
            kind = .routingRecoveryRequired
        }
        persistOpenCodexRecovery(flow, kind: kind, lastCode: kind.rawValue)
    }

    private func persistOpenCodexRecovery(
        _ flow: OpenCodexRemovalFlow,
        kind: OpenCodexRemovalRecoveryKind,
        lastCode: String
    ) {
        do {
            try saveOpenCodexRecovery(flow, kind: kind, lastCode: lastCode)
        } catch {
            setOpenCodexRemovalFailure(SafeStatusMessage(
                code: "opencodex_recovery_context_invalid",
                key: .messageRemovalRecoveryUnavailable
            ))
        }
    }

    private func saveOpenCodexRecovery(
        _ flow: OpenCodexRemovalFlow,
        kind: OpenCodexRemovalRecoveryKind,
        lastCode: String
    ) throws {
        let session = try OpenCodexRemovalRecoverySession(
            selection: flow.selection,
            mode: flow.mode,
            orderedDataItemIDs: flow.selectedDataItemIDs,
            retiredDataItemIDs: flow.retiredDataItemIDs.sorted(),
            recoveryKind: kind,
            lastCode: lastCode,
            inventoryRevision: flow.mode == .trashSelected
                ? flow.inventory?.inventoryRevision ?? flow.recoveryInventoryRevision
                : nil,
            expectedRoutingGeneration: kind == .routingRecoveryRequired
                ? flow.expectedRoutingGeneration
                : nil
        )
        try removalRecoveryStore.save(session)
        hasPendingOpenCodexRemovalRecovery = true
    }
}
