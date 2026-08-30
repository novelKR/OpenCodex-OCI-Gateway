import AppKit
import Combine
import Foundation
import SwiftUI
import XCTest
@testable import OpenCodexRelay
import OpenCodexRelayCore
import OpenCodexRelayHomebrewGuard
import OpenCodexRelayLocalization

@MainActor
final class MenuBarModelTests: XCTestCase {
    private final class TargetStore: DesktopTargetStoring {
        var desktopTarget: DesktopTarget?

        init(_ desktopTarget: DesktopTarget?) {
            self.desktopTarget = desktopTarget
        }
    }

    private var trustedPolicy: CodexDesktopTrustPolicy {
        CodexDesktopTrustPolicy(
            bundleIdentifier: "com.example.reviewed-codex",
            teamIdentifier: "ABCDE12345"
        )
    }

    @MainActor
    private final class TrustValidator: CodexDesktopTrustValidating {
        private var failures: [CodexDesktopTrustFailure?]
        private(set) var verifiedURLs: [URL] = []

        init(failures: [CodexDesktopTrustFailure?] = []) {
            self.failures = failures
        }

        func verify(_ url: URL, policy: CodexDesktopTrustPolicy) -> CodexDesktopTrustResult {
            verifiedURLs.append(url)
            guard let identity = policy.reviewedIdentity else {
                return .rejected(.configurationMissing)
            }
            guard let resolved = try? DesktopTargetResolver.validate(url) else {
                return .rejected(.unavailable)
            }
            if !failures.isEmpty, let failure = failures.removeFirst() {
                return .rejected(failure)
            }
            return .trusted(VerifiedCodexDesktop(
                url: resolved.standardizedFileURL,
                bundleIdentifier: identity.bundleIdentifier,
                teamIdentifier: identity.teamIdentifier
            ))
        }
    }

    @MainActor
    private final class DesktopDiscoverer: CodexDesktopDiscovering {
        var candidateURLs: [URL]
        private(set) var bundleIdentifiers: [String] = []

        init(candidateURLs: [URL] = []) {
            self.candidateURLs = candidateURLs
        }

        func candidates(for bundleIdentifier: String) -> [URL] {
            bundleIdentifiers.append(bundleIdentifier)
            return candidateURLs
        }
    }

    private final class LoginRegistration: LoginRegistrationManaging {
        // Start disabled so the local-development test proves the explicit
        // menu action is what invokes ServiceManagement registration.
        var registrationState: LoginRegistrationState = .disabled
        private(set) var registerCalls = 0

        func register() throws { registerCalls += 1 }
        func unregister() throws {}
    }

    private final class RemovalEventRecorder: @unchecked Sendable {
        private let lock = NSLock()
        private var recordedEvents: [String] = []

        var events: [String] {
            lock.lock()
            defer { lock.unlock() }
            return recordedEvents
        }

        func record(_ event: String) {
            lock.lock()
            recordedEvents.append(event)
            lock.unlock()
        }

        func reset() {
            lock.lock()
            recordedEvents.removeAll()
            lock.unlock()
        }
    }

    private final class DesktopController: DesktopApplicationControlling {
        var runningValues: [Bool]
        var requestAccepted = true
        var waitResult = true
        var waitAction: (() -> Void)?
        private let eventRecorder: RemovalEventRecorder?
        private(set) var runningTargets: [URL] = []
        private(set) var quitTargets: [URL] = []
        private(set) var waitTargets: [URL] = []
        private(set) var relaunchTargets: [URL] = []

        var quitRequests: Int { quitTargets.count }
        var relaunches: Int { relaunchTargets.count }

        init(runningValues: [Bool], eventRecorder: RemovalEventRecorder? = nil) {
            self.runningValues = runningValues
            self.eventRecorder = eventRecorder
        }

        func isRunning(at target: URL) -> Bool {
            runningTargets.append(target)
            guard !runningValues.isEmpty else { return false }
            return runningValues.removeFirst()
        }

        func requestGracefulQuit(at target: URL) -> Bool {
            quitTargets.append(target)
            eventRecorder?.record("desktop_quit")
            return requestAccepted
        }

        func waitForExit(at target: URL, timeout _: TimeInterval) async -> Bool {
            waitTargets.append(target)
            waitAction?()
            return waitResult
        }

        func relaunch(at target: URL) async throws {
            relaunchTargets.append(target)
            eventRecorder?.record("desktop_relaunch")
        }
    }

    @MainActor
    private final class HomebrewGuardClient: HomebrewGuardManaging {
        var backendValue: HomebrewGuardBackend = .smAppService
        var availabilityValue: HomebrewGuardAvailability
        var availabilityAfterRegister: HomebrewGuardAvailability?
        var setupCommand = "/usr/bin/sudo -- '/Applications/Test.app/installer' install"
        private let eventRecorder: RemovalEventRecorder?
        private(set) var operations: [String] = []

        var backend: HomebrewGuardBackend { backendValue }

        init(
            availability: HomebrewGuardAvailability,
            eventRecorder: RemovalEventRecorder? = nil
        ) {
            self.availabilityValue = availability
            self.eventRecorder = eventRecorder
        }

        func availability(candidate _: HomebrewGuardCandidate?) async -> HomebrewGuardAvailability {
            operations.append("availability")
            eventRecorder?.record("guard_availability")
            return availabilityValue
        }

        func register() throws {
            operations.append("register")
            eventRecorder?.record("guard_register")
            if let availabilityAfterRegister {
                availabilityValue = availabilityAfterRegister
            }
        }

        func openSystemSettingsLoginItems() {
            operations.append("open_settings")
            eventRecorder?.record("guard_open_settings")
        }

        func setupCommand(for action: HomebrewGuardSetupAction) -> HomebrewGuardSetupCommandAvailability {
            operations.append("setup_\(action.rawValue)")
            return .available(
                setupCommand.replacingOccurrences(of: " install", with: " \(action.rawValue)")
            )
        }

        func prepare(candidate _: HomebrewGuardCandidate, operationID _: String) async throws {
            operations.append("prepare")
            eventRecorder?.record("guard_prepare")
        }

        func commit(operationID _: String) async throws {
            operations.append("commit")
            eventRecorder?.record("guard_commit")
        }

        func release(operationID _: String) async throws {
            operations.append("release")
            eventRecorder?.record("guard_release")
        }

        func recover(operationID _: String) async throws {
            operations.append("recover")
            eventRecorder?.record("guard_recover")
        }
    }

    private actor RelayctlClient: RelayctlExecuting {
        enum Outcome: Sendable {
            case response(RoutingStatus)
            case failure(RelayctlError)
        }

        private var outcomes: [Outcome]
        private var recorded: [RelayctlCommand] = []

        init(response: RoutingStatus) {
            self.outcomes = [.response(response)]
        }

        init(responses: [RoutingStatus]) {
            precondition(!responses.isEmpty)
            self.outcomes = responses.map(Outcome.response)
        }

        init(outcomes: [Outcome]) {
            precondition(!outcomes.isEmpty)
            self.outcomes = outcomes
        }

        func execute(_ command: RelayctlCommand) async throws -> RoutingStatus {
            recorded.append(command)
            let outcome = outcomes.count > 1 ? outcomes.removeFirst() : outcomes[0]
            switch outcome {
            case let .response(response):
                return response
            case let .failure(error):
                throw error
            }
        }

        func setResponse(_ response: RoutingStatus) {
            outcomes = [.response(response)]
        }

        func setResponses(_ responses: [RoutingStatus]) {
            precondition(!responses.isEmpty)
            outcomes = responses.map(Outcome.response)
        }

        func commands() -> [RelayctlCommand] { recorded }
    }

    private actor NativeRepairClient: NativeRepairExecuting {
        enum RepairOutcome: Sendable {
            case receipt(NativeRoutingRepairReceipt)
            case failure(RelayctlError)
        }

        struct RepairCall: Equatable {
            let generation: UInt64
            let owner: NativeRepairKind
            let selection: OpenCodexNativeRepairSelection?
        }

        private var inspectionResponses: [NativeRepairInspection]
        private let outcome: RepairOutcome
        private let ownerConfiguration: NativeOwnerConfiguration
        private let ownerIntegration: NativeOwnerIntegration
        private let ownerReason: NativeOwnerInspectionReason
        private var inspections: [UInt64] = []
        private var ownerInspections: [OpenCodexNativeRepairSelection] = []
        private var repairs: [RepairCall] = []

        init(
            inspection: NativeRepairInspection,
            outcome: RepairOutcome,
            ownerConfiguration: NativeOwnerConfiguration = .valid,
            ownerIntegration: NativeOwnerIntegration = .enabled,
            ownerReason: NativeOwnerInspectionReason = .ready
        ) {
            self.inspectionResponses = [inspection]
            self.outcome = outcome
            self.ownerConfiguration = ownerConfiguration
            self.ownerIntegration = ownerIntegration
            self.ownerReason = ownerReason
        }

        init(
            inspections: [NativeRepairInspection],
            outcome: RepairOutcome,
            ownerConfiguration: NativeOwnerConfiguration = .valid,
            ownerIntegration: NativeOwnerIntegration = .enabled,
            ownerReason: NativeOwnerInspectionReason = .ready
        ) {
            precondition(!inspections.isEmpty)
            self.inspectionResponses = inspections
            self.outcome = outcome
            self.ownerConfiguration = ownerConfiguration
            self.ownerIntegration = ownerIntegration
            self.ownerReason = ownerReason
        }

        func inspect(expectedGeneration: UInt64) async throws -> NativeRepairInspection {
            inspections.append(expectedGeneration)
            if inspectionResponses.count > 1 {
                return inspectionResponses.removeFirst()
            }
            return inspectionResponses[0]
        }

        func inspectOwner(
            expectedGeneration: UInt64,
            owner: NativeRepairKind,
            openCodexSelection: OpenCodexNativeRepairSelection
        ) async throws -> NativeRepairOwnerInspection {
            ownerInspections.append(openCodexSelection)
            let data = Data("""
            {"schema_version":1,"generation":\(expectedGeneration),"owner":"opencodex","configuration":"\(ownerConfiguration.rawValue)","integration":"\(ownerIntegration.rawValue)","reason":"\(ownerReason.rawValue)"}
            """.utf8)
            return try JSONDecoder().decode(NativeRepairOwnerInspection.self, from: data).validated()
        }

        func repair(
            expectedGeneration: UInt64,
            owner: NativeRepairKind,
            openCodexSelection: OpenCodexNativeRepairSelection?
        ) async throws -> NativeRoutingRepairReceipt {
            repairs.append(RepairCall(
                generation: expectedGeneration,
                owner: owner,
                selection: openCodexSelection
            ))
            switch outcome {
            case let .receipt(receipt): return receipt
            case let .failure(error): throw error
            }
        }

        func inspectCalls() -> [UInt64] { inspections }
        func ownerInspectionCalls() -> [OpenCodexNativeRepairSelection] { ownerInspections }
        func repairCalls() -> [RepairCall] { repairs }
    }

    private actor DiscoveryClient: OpenCodexDiscovering {
        struct Call: Equatable {
            let tier: OpenCodexDiscoveryTier
            let broadScanApproved: Bool
        }

        private var responses: [OpenCodexDiscoveryResult]
        private var recorded: [Call] = []

        init(responses: [OpenCodexDiscoveryResult]) {
            self.responses = responses
        }

        func discover(tier: OpenCodexDiscoveryTier, broadScanApproved: Bool) async throws -> OpenCodexDiscoveryResult {
            recorded.append(Call(tier: tier, broadScanApproved: broadScanApproved))
            guard !responses.isEmpty else { throw RelayctlError.invalidStatus }
            return responses.removeFirst()
        }

        func calls() -> [Call] { recorded }
    }

    private actor DelayedDiscoveryClient: OpenCodexDiscovering {
        private let response: OpenCodexDiscoveryResult
        private var recordedCount = 0
        private var started = false
        private var startWaiters: [CheckedContinuation<Void, Never>] = []
        private var responseContinuation: CheckedContinuation<Void, Never>?

        init(response: OpenCodexDiscoveryResult) {
            self.response = response
        }

        func discover(
            tier _: OpenCodexDiscoveryTier,
            broadScanApproved _: Bool
        ) async throws -> OpenCodexDiscoveryResult {
            recordedCount += 1
            started = true
            let waiters = startWaiters
            startWaiters.removeAll()
            waiters.forEach { $0.resume() }
            await withCheckedContinuation { continuation in
                precondition(responseContinuation == nil)
                responseContinuation = continuation
            }
            return response
        }

        func waitForStart() async {
            guard !started else { return }
            await withCheckedContinuation { continuation in
                startWaiters.append(continuation)
            }
        }

        func resume() {
            responseContinuation?.resume()
            responseContinuation = nil
        }

        func callCount() -> Int { recordedCount }
    }

    private actor RemovalClient: OpenCodexRemovalExecuting {
        private let inventory: OpenCodexDataInventoryReceipt
        private let removal: OpenCodexRemovalReceipt?
        private let eventRecorder: RemovalEventRecorder?
        private var inspectedSelections: [OpenCodexRemovalSelection] = []
        private var requests: [OpenCodexRemovalRequest] = []

        init(
            inventory: OpenCodexDataInventoryReceipt,
            removal: OpenCodexRemovalReceipt? = nil,
            eventRecorder: RemovalEventRecorder? = nil
        ) {
            self.inventory = inventory
            self.removal = removal
            self.eventRecorder = eventRecorder
        }

        func inspect(selection: OpenCodexRemovalSelection) async throws -> OpenCodexDataInventoryReceipt {
            inspectedSelections.append(selection)
            return inventory
        }

        func remove(_ request: OpenCodexRemovalRequest) async throws -> OpenCodexRemovalReceipt {
            requests.append(request)
            if let eventRecorder {
                eventRecorder.record("remove")
            }
            guard let removal else { throw RelayctlError.invalidStatus }
            return try removal.validated(for: request)
        }

        func inspectCount() -> Int { inspectedSelections.count }
        func recordedRequests() -> [OpenCodexRemovalRequest] { requests }
    }

    private actor NativeRemovalClient: OpenCodexNativeRemovalExecuting {
        private var discoveryResponses: [OpenCodexNativeDiscoveryResult]
        private let terminalDiscoveryResponse: OpenCodexNativeDiscoveryResult?
        private let inspection: OpenCodexNativeRemovalInspection
        private let inventory: OpenCodexNativeDataInventoryReceipt?
        private let removal: OpenCodexNativeRemovalReceipt?
        private let eventRecorder: RemovalEventRecorder?
        private var removalFailuresRemaining: Int
        private var terminalDiscoveryFailuresRemaining: Int
        private var discoveryCount = 0
        private var acknowledgementCount = 0
        private var acknowledgedReceiptDigests: [String] = []
        private var inspectedSelections: [OpenCodexNativeRemovalSelection] = []
        private var dataSelections: [OpenCodexNativeRemovalSelection] = []
        private var requests: [OpenCodexNativeRemovalRequest] = []

        init(
            discovery: OpenCodexNativeDiscoveryResult,
            terminalDiscovery: OpenCodexNativeDiscoveryResult? = nil,
            inspection: OpenCodexNativeRemovalInspection,
            inventory: OpenCodexNativeDataInventoryReceipt? = nil,
            removal: OpenCodexNativeRemovalReceipt? = nil,
            removalFailuresRemaining: Int = 0,
            terminalDiscoveryFailuresRemaining: Int = 0,
            eventRecorder: RemovalEventRecorder? = nil
        ) {
            self.discoveryResponses = [discovery]
            self.terminalDiscoveryResponse = terminalDiscovery
            self.inspection = inspection
            self.inventory = inventory
            self.removal = removal
            self.removalFailuresRemaining = removalFailuresRemaining
            self.terminalDiscoveryFailuresRemaining = terminalDiscoveryFailuresRemaining
            self.eventRecorder = eventRecorder
        }

        func discover() async throws -> OpenCodexNativeDiscoveryResult {
            discoveryCount += 1
            if let eventRecorder {
                eventRecorder.record("native_discover_\(discoveryCount)")
            }
            guard !discoveryResponses.isEmpty else { throw RelayctlError.invalidStatus }
            return discoveryResponses.removeFirst()
        }

        func acknowledgeTerminal(
            receiptDigest: String
        ) async throws -> OpenCodexNativeDiscoveryResult {
            acknowledgementCount += 1
            acknowledgedReceiptDigests.append(receiptDigest)
            if let eventRecorder {
                eventRecorder.record("native_ack_\(acknowledgementCount)")
            }
            if terminalDiscoveryFailuresRemaining > 0 {
                terminalDiscoveryFailuresRemaining -= 1
                throw RelayctlError.invocationFailed(exitCode: 1)
            }
            guard let terminalDiscoveryResponse else { throw RelayctlError.invalidStatus }
            return terminalDiscoveryResponse
        }

        func inspect(
            selection: OpenCodexNativeRemovalSelection
        ) async throws -> OpenCodexNativeRemovalInspection {
            inspectedSelections.append(selection)
            return inspection
        }

        func inspectData(
            selection: OpenCodexNativeRemovalSelection
        ) async throws -> OpenCodexNativeDataInventoryReceipt {
            dataSelections.append(selection)
            guard let inventory else { throw RelayctlError.invalidStatus }
            return try inventory.validated(for: selection)
        }

        func remove(
            _ request: OpenCodexNativeRemovalRequest
        ) async throws -> OpenCodexNativeRemovalReceipt {
            requests.append(request)
            if removalFailuresRemaining > 0 {
                removalFailuresRemaining -= 1
                throw RelayctlError.invocationFailed(exitCode: 1)
            }
            guard let removal else { throw RelayctlError.invalidStatus }
            return try removal.validated(for: request)
        }

        func calls() -> (
            discoveries: Int,
            acknowledgements: Int,
            inspections: Int,
            dataInspections: Int,
            removals: Int
        ) {
            (
                discoveryCount,
                acknowledgementCount,
                inspectedSelections.count,
                dataSelections.count,
                requests.count
            )
        }

        func recordedRequests() -> [OpenCodexNativeRemovalRequest] { requests }
        func recordedAcknowledgementDigests() -> [String] { acknowledgedReceiptDigests }
    }

    private final class RecoveryStore: OpenCodexRemovalRecoverySessionStoring {
        var session: OpenCodexRemovalRecoverySession?
        private let eventRecorder: RemovalEventRecorder?
        private let clearSucceeds: Bool
        private var clearFailuresRemaining: Int
        private(set) var saveCount = 0
        private(set) var clearCount = 0

        init(
            eventRecorder: RemovalEventRecorder? = nil,
            clearSucceeds: Bool = true,
            clearFailuresRemaining: Int = 0
        ) {
            self.eventRecorder = eventRecorder
            self.clearSucceeds = clearSucceeds
            self.clearFailuresRemaining = clearFailuresRemaining
        }

        func load() throws -> OpenCodexRemovalRecoverySession? { session }
        func save(_ session: OpenCodexRemovalRecoverySession) throws {
            self.session = session
            saveCount += 1
            eventRecorder?.record("recovery_save_\(session.recoveryKind.rawValue)")
        }
        func clear() {
            if clearFailuresRemaining > 0 {
                clearFailuresRemaining -= 1
            } else if clearSucceeds {
                session = nil
            }
            clearCount += 1
            eventRecorder?.record("recovery_clear")
        }
    }

    /// Holds a request after recording its exact relayctl command so the test
    /// can change the UI language while the operation is visibly in progress.
    private actor DelayedRelayctlClient: RelayctlExecuting {
        private let response: RoutingStatus
        private var recorded: [RelayctlCommand] = []
        private var requestStarted = false
        private var startWaiters: [CheckedContinuation<Void, Never>] = []
        private var requestContinuation: CheckedContinuation<Void, Never>?

        init(response: RoutingStatus) {
            self.response = response
        }

        func execute(_ command: RelayctlCommand) async throws -> RoutingStatus {
            recorded.append(command)
            guard case .request = command else { return response }

            requestStarted = true
            let waiters = startWaiters
            startWaiters.removeAll()
            waiters.forEach { $0.resume() }
            await withCheckedContinuation { continuation in
                precondition(requestContinuation == nil)
                requestContinuation = continuation
            }
            return response
        }

        func waitForRequestToStart() async {
            guard !requestStarted else { return }
            await withCheckedContinuation { continuation in
                startWaiters.append(continuation)
            }
        }

        func resumeRequest() {
            requestContinuation?.resume()
            requestContinuation = nil
        }

        func commands() -> [RelayctlCommand] { recorded }
    }

    private actor DelayedFirstStatusClient: RelayctlExecuting {
        private var responses: [RoutingStatus]
        private var recorded: [RelayctlCommand] = []
        private var firstStarted = false
        private var startWaiters: [CheckedContinuation<Void, Never>] = []
        private var firstContinuation: CheckedContinuation<Void, Never>?

        init(responses: [RoutingStatus]) {
            precondition(responses.count >= 2)
            self.responses = responses
        }

        func execute(_ command: RelayctlCommand) async throws -> RoutingStatus {
            recorded.append(command)
            if recorded.count == 1 {
                firstStarted = true
                let waiters = startWaiters
                startWaiters.removeAll()
                waiters.forEach { $0.resume() }
                await withCheckedContinuation { continuation in
                    precondition(firstContinuation == nil)
                    firstContinuation = continuation
                }
            }
            if responses.count > 1 {
                return responses.removeFirst()
            }
            return responses[0]
        }

        func waitForFirstStatusToStart() async {
            guard !firstStarted else { return }
            await withCheckedContinuation { continuation in
                startWaiters.append(continuation)
            }
        }

        func resumeFirstStatus() {
            firstContinuation?.resume()
            firstContinuation = nil
        }

        func commands() -> [RelayctlCommand] { recorded }
    }

    func testApplyIsNotInvokedIfTheExactDesktopAppRestartsBeforeConfirmation() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let client = RelayctlClient(response: pendingNativeStatus())
        // First call observes a running selected app. The second is the final
        // pre-apply check and simulates that exact app relaunching.
        let desktop = DesktopController(runningValues: [true, true, true, true])
        let model = MenuBarModel(
            client: client,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )
		await model.refreshStatusNow()
		XCTAssertNotNil(model.status)

        model.completePendingTransition()
        try await waitUntil { model.message?.code == "desktop_restarted" }

        XCTAssertEqual(desktop.quitRequests, 1)
        let commands = await client.commands()
        XCTAssertFalse(commands.contains(.apply))
    }

    func testRelaunchTargetsOnlyTheRegisteredDesktopApplication() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let client = RelayctlClient(response: externalStatus())
        let desktop = DesktopController(runningValues: [false])
        let model = MenuBarModel(
            client: client,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        model.relaunchSelectedDesktop()
        try await waitUntil { desktop.relaunches == 1 }
        XCTAssertEqual(desktop.relaunches, 1)
        XCTAssertEqual(desktop.relaunchTargets, [appURL.resolvingSymlinksInPath().standardizedFileURL])
    }

    func testConsumerSwitchBindsRequestToValidatedGatewayWitness() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let digest = String(repeating: "c", count: 64)
        let generation: UInt64 = 41
        let client = RelayctlClient(responses: [
            nativeStatus(generation: generation),
            pendingExternalStatus(generation: generation + 1),
            externalStatus(generation: generation + 3),
            externalStatus(generation: generation + 3),
        ])
        let desktop = DesktopController(runningValues: [true, true, true, true, false])
        let model = MenuBarModel(
            client: client,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        await model.refreshStatusNow()
        model.switchCodexToExternalGateway(
            expectedConfigDigest: digest,
            expectedRoutingGeneration: generation
        )
        try await waitUntil { !model.isBusy && desktop.relaunches == 1 }

        let commands = await client.commands()
        XCTAssertEqual(
            commands,
            [
                .status,
                .requestExternalMigratingKnownLegacy(
                    expectedConfigDigest: digest,
                    expectedRoutingGeneration: generation
                ),
                .apply,
                .status,
            ]
        )
        XCTAssertEqual(desktop.quitRequests, 1)
        XCTAssertEqual(desktop.relaunches, 1)
    }

    func testApplyRemainsPendingWhenGracefulQuitTimesOut() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let client = RelayctlClient(response: pendingNativeStatus())
        let desktop = DesktopController(runningValues: [true, true, true])
        desktop.waitResult = false
        let model = MenuBarModel(
            client: client,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )
		await model.refreshStatusNow()
		XCTAssertNotNil(model.status)

        model.completePendingTransition()
        try await waitUntil { model.message?.code == "desktop_quit_timeout" }

        let commands = await client.commands()
        XCTAssertFalse(commands.contains(.apply))
    }

    func testLocalDevelopmentBuildDoesNotRegisterAtLoginUntilUserRequestsIt() async throws {
        let client = RelayctlClient(response: externalStatus())
        let registration = LoginRegistration()
        let model = MenuBarModel(
            client: client,
            targetStore: TargetStore(nil),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: registration,
            startsPolling: false,
            distributionFlavor: .localDevelopment,
            localization: englishLocalization()
        )

        model.start()
        XCTAssertEqual(registration.registerCalls, 0)
        XCTAssertTrue(model.menuBarLabel.hasPrefix("Local development build"))
        XCTAssertTrue(model.menuAccessibilityLabel.hasPrefix("Local development build"))
        model.enableLaunchAtLogin()
        XCTAssertEqual(registration.registerCalls, 1)
    }

    func testPreviewRuntimeBlocksInjectedRelayAndProjectsConcreteUnavailableStates() async {
        let client = RelayctlClient(response: externalStatus())
        let activityLog = RelayActivityLogStore(subsystem: "test.relay.preview")
        let model = MenuBarModel(
            client: client,
            targetStore: TargetStore(nil),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            distributionFlavor: .localDevelopment,
            runtimeMode: .preview,
            producerToolsEnabled: true,
            localization: englishLocalization(),
            activityLog: activityLog
        )

        await model.refreshStatusNow()
        await model.refreshHomebrewGuardAvailability()

        XCTAssertEqual(model.integrationAvailability, .preview)
        XCTAssertTrue(model.shouldOpenSelfHostedOnboarding)
        XCTAssertNil(model.status)
        XCTAssertFalse(model.canRequestRouting)
        XCTAssertTrue(model.canShowLocalDevelopmentIntegrationGuide)
        XCTAssertEqual(model.homebrewGuardAvailability.registration, .preview)
        let relayCommands = await client.commands()
        XCTAssertTrue(relayCommands.isEmpty)
        XCTAssertEqual(
            activityLog.events.last(where: { $0.code == "refresh_unavailable" })?.fields,
            ["result_code": "preview_mode"]
        )

        let gateway = model.makeGatewaySettingsController()
        await gateway.refresh()
        XCTAssertEqual(gateway.state, .integrationRequired)
        XCTAssertEqual(gateway.lastErrorCode, "preview_mode")

        let sensitiveCommand = "cd '/private/source' && install --upstream https://secret.example/v1"
        model.copyIntegrationGuideCommand(sensitiveCommand, kind: "build")
        let copyEvent = activityLog.events.last(where: {
            $0.code == "integration_guide_command_copied"
        })
        XCTAssertEqual(copyEvent?.fields, ["command_kind": "build"])
        XCTAssertFalse(activityLog.jsonLines().contains(sensitiveCommand))
        XCTAssertFalse(activityLog.jsonLines().contains("secret.example"))

        let sensitiveSigningSetup = "bootstrap --service private.service --public-key-out /private/key.pem"
        model.copyIntegrationGuideCommand(sensitiveSigningSetup, kind: "signing_setup")
        let signingCopyEvent = activityLog.events.last(where: {
            $0.code == "integration_guide_command_copied"
        })
        XCTAssertEqual(signingCopyEvent?.fields, ["command_kind": "signing_setup"])
        XCTAssertFalse(activityLog.jsonLines().contains(sensitiveSigningSetup))
        XCTAssertFalse(activityLog.jsonLines().contains("private.service"))
    }

    func testManagedMissingBindingOpensConsumerOnboarding() {
        let binding = URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent(UUID().uuidString)
            .appendingPathComponent("routing-binding.json")
        let model = MenuBarModel(
            client: nil,
            targetStore: TargetStore(nil),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            bindingURL: binding,
            startsPolling: false,
            runtimeMode: .managed,
            localization: englishLocalization()
        )

        XCTAssertEqual(model.integrationAvailability, .missing)
        XCTAssertTrue(model.shouldOpenSelfHostedOnboarding)
    }

    func testMissingBindingWithoutExecutableHelperBlocksBothDiscoveryClientsAndManualSelection() async throws {
        let appURL = try makeAppBundle()
        let root = URL(fileURLWithPath: NSTemporaryDirectory(), isDirectory: true)
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: root)
        }
        let discovery = DiscoveryClient(responses: [
            try discoveryResult(tier: .a, includesCandidate: true),
        ])
        let model = MenuBarModel(
            client: nil,
            discoveryClient: discovery,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            bindingURL: root.appendingPathComponent("missing-routing-binding.json"),
            helperURL: root.appendingPathComponent("missing-relayctl"),
            startsPolling: false,
            runtimeMode: .managed,
            localization: englishLocalization()
        )

        XCTAssertEqual(model.integrationAvailability, .missing)
        XCTAssertTrue(
            LocalOpenCodexPageMode.resolve(model.integrationAvailability).showsDiscoveryControls
        )

        model.addLocalOpenCodexBackend()
        XCTAssertEqual(model.openCodexDiscoveryState, .idle)
        XCTAssertEqual(model.message?.code, "relayctl_unavailable")

        XCTAssertFalse(model.chooseDiscoveredOpenCodexCandidate(id: "0123456789abcdef01234567"))
        model.selectOpenCodexExecutableManually()

        let blockedCalls = await discovery.calls()
        XCTAssertEqual(blockedCalls, [])
        XCTAssertNil(model.openCodexRemovalFlow)
        XCTAssertEqual(model.openCodexDiscoveryState, .idle)
    }

    func testUnsafeInvalidPreviewAndHelperUnavailableNeverDispatchStandaloneRemoval() async throws {
        let appURL = try makeAppBundle()
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: root)
        }
        let helper = root.appendingPathComponent("opencodex-relayctl")
        try Data("#!/bin/sh\nexit 0\n".utf8).write(to: helper)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o700],
            ofItemAtPath: helper.path
        )
        let unsafeBinding = root.appendingPathComponent("unsafe-binding.json")
        try Data("{}".utf8).write(to: unsafeBinding)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o644],
            ofItemAtPath: unsafeBinding.path
        )
        let invalidBinding = root.appendingPathComponent("invalid-binding.json")
        try Data("{}".utf8).write(to: invalidBinding)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o600],
            ofItemAtPath: invalidBinding.path
        )
        let validBinding = root.appendingPathComponent("valid-binding.json")
        try writeRoutingBinding(at: validBinding)

        let native = NativeRemovalClient(
            discovery: try nativeDiscoveryResult(),
            inspection: try nativeRemovalInspection()
        )
        let integrated = DiscoveryClient(responses: [
            try discoveryResult(tier: .a, includesCandidate: true),
        ])
        let cases: [(URL, URL, RelayRuntimeMode, RelayIntegrationAvailability)] = [
            (unsafeBinding, helper, .managed, .unsafe),
            (invalidBinding, helper, .managed, .invalid),
            (root.appendingPathComponent("preview-binding.json"), helper, .preview, .preview),
            (
                validBinding,
                root.appendingPathComponent("missing-helper"),
                .managed,
                .helperUnavailable
            ),
        ]

        for (binding, helperURL, runtimeMode, expectedAvailability) in cases {
            let model = MenuBarModel(
                client: nil,
                discoveryClient: integrated,
                nativeRemovalClient: native,
                targetStore: TargetStore(DesktopTarget(url: appURL)),
                desktopApplication: DesktopController(runningValues: []),
                desktopTrustPolicy: trustedPolicy,
                desktopTrustValidator: TrustValidator(),
                desktopDiscoverer: DesktopDiscoverer(),
                loginRegistration: LoginRegistration(),
                bindingURL: binding,
                helperURL: helperURL,
                startsPolling: false,
                runtimeMode: runtimeMode,
                localization: englishLocalization()
            )
            XCTAssertEqual(model.integrationAvailability, expectedAvailability)
            XCTAssertFalse(model.canDiscoverOpenCodex)
            model.addLocalOpenCodexBackend()
            XCTAssertEqual(model.openCodexDiscoveryState, .idle)
        }

        let nativeCalls = await native.calls()
        let integratedCalls = await integrated.calls()
        XCTAssertEqual(nativeCalls.discoveries, 0)
        XCTAssertEqual(integratedCalls, [])
    }

    func testExactMissingBindingUsesOnlyStandaloneNativeDiscoveryAndRemovalInspection() async throws {
        let appURL = try makeAppBundle()
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: root)
        }
        let helper = root.appendingPathComponent("opencodex-relayctl")
        try Data("#!/bin/sh\nexit 0\n".utf8).write(to: helper)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o700],
            ofItemAtPath: helper.path
        )
        let native = NativeRemovalClient(
            discovery: try nativeDiscoveryResult(),
            inspection: try nativeRemovalInspection()
        )
        let integrated = DiscoveryClient(responses: [
            try discoveryResult(tier: .a, includesCandidate: true),
        ])
        let model = MenuBarModel(
            client: nil,
            discoveryClient: integrated,
            nativeRemovalClient: native,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            bindingURL: root.appendingPathComponent("routing-binding.json"),
            helperURL: helper,
            startsPolling: false,
            runtimeMode: .managed,
            localization: englishLocalization()
        )

        XCTAssertEqual(model.integrationAvailability, .missing)
        XCTAssertTrue(model.canDiscoverOpenCodex)
        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates(.standaloneNative) = model.openCodexDiscoveryState { return true }
            return false
        }
        let integratedCalls = await integrated.calls()
        let discoveryCalls = await native.calls()
        XCTAssertEqual(integratedCalls, [])
        XCTAssertEqual(discoveryCalls.discoveries, 1)

        XCTAssertTrue(model.chooseDiscoveredOpenCodexCandidate(id: "0123456789abcdef01234567"))
        XCTAssertEqual(model.openCodexRemovalFlow?.context, .standaloneNative)
        XCTAssertNil(model.openCodexRemovalFlow?.candidate)
        XCTAssertNotNil(model.openCodexRemovalFlow?.nativeCandidate)
        model.beginOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .options && !model.isBusy }
        let removalCalls = await native.calls()
        XCTAssertEqual(removalCalls.inspections, 1)
        XCTAssertNil(model.status)
        XCTAssertEqual(model.integrationAvailability, .missing)

        let recoveryStore = RecoveryStore()
        recoveryStore.session = try OpenCodexRemovalRecoverySession(
            context: .standaloneNative,
            selection: try OpenCodexRemovalSelection(
                installationID: "0123456789abcdef01234567",
                installationFingerprint: String(repeating: "a", count: 64)
            ),
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .nativeRecoveryRequired,
            lastCode: "native_restore_unverified",
            expectedBoundaryRevision: String(repeating: "b", count: 64),
            nativeRestoreFingerprint: String(repeating: "c", count: 64)
        )
        let blockedNative = NativeRemovalClient(
            discovery: try nativeDiscoveryResult(),
            inspection: try nativeRemovalInspection()
        )
        let recoveringModel = MenuBarModel(
            client: nil,
            nativeRemovalClient: blockedNative,
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            bindingURL: root.appendingPathComponent("routing-binding.json"),
            helperURL: helper,
            startsPolling: false,
            runtimeMode: .managed,
            localization: englishLocalization()
        )
        XCTAssertTrue(recoveringModel.hasPendingOpenCodexRemovalRecovery)
        XCTAssertFalse(recoveringModel.canDiscoverOpenCodex)
        recoveringModel.addLocalOpenCodexBackend()
        XCTAssertEqual(recoveringModel.message?.code, "opencodex_recovery_context_pending")
        let blockedCalls = await blockedNative.calls()
        XCTAssertEqual(blockedCalls.discoveries, 0)
    }

    func testStandaloneNativeRemovalCompletesWithoutCreatingRelayIntegrationAssets() async throws {
        let appURL = try makeAppBundle()
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: root)
        }
        let helper = root.appendingPathComponent("opencodex-relayctl")
        let binding = root.appendingPathComponent("routing-binding.json")
        try Data("#!/bin/sh\nexit 0\n".utf8).write(to: helper)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o700],
            ofItemAtPath: helper.path
        )
        let events = RemovalEventRecorder()
        let recoveryStore = RecoveryStore(eventRecorder: events)
        let native = NativeRemovalClient(
            discovery: try nativeDiscoveryResult(),
            terminalDiscovery: try nativeCleanDiscoveryResult(),
            inspection: try nativeRemovalInspection(),
            removal: try successfulNativeRemovalReceipt(),
            eventRecorder: events
        )
        let integrated = DiscoveryClient(responses: [
            try discoveryResult(tier: .a, includesCandidate: true),
        ])
        let desktop = DesktopController(runningValues: [], eventRecorder: events)
        let model = MenuBarModel(
            client: nil,
            discoveryClient: integrated,
            nativeRemovalClient: native,
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            bindingURL: binding,
            helperURL: helper,
            startsPolling: false,
            runtimeMode: .managed,
            localization: englishLocalization()
        )

        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates(.standaloneNative) = model.openCodexDiscoveryState { return true }
            return false
        }
        XCTAssertTrue(model.chooseDiscoveredOpenCodexCandidate(id: "0123456789abcdef01234567"))
        model.beginOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .options && !model.isBusy }
        model.reviewOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .confirmRemoval && !model.isBusy }
        model.confirmOpenCodexPackageRemoval()
        try await waitUntil(timeout: 2) {
            model.openCodexRemovalFlow?.removalProgress?.phase == .completed && !model.isBusy
        }

        XCTAssertEqual(model.openCodexRemovalFlow?.context, .standaloneNative)
        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .result)
        XCTAssertTrue(model.openCodexRemovalFlow?.nativeReceipt?.isSuccessful == true)
        XCTAssertEqual(model.message?.code, "opencodex_native_removed")
        XCTAssertEqual(desktop.quitRequests, 0)
        XCTAssertEqual(desktop.relaunches, 1)
        XCTAssertEqual(recoveryStore.saveCount, 2)
        XCTAssertEqual(recoveryStore.clearCount, 1)
        XCTAssertNil(recoveryStore.session)
        XCTAssertFalse(FileManager.default.fileExists(atPath: binding.path))
        XCTAssertNil(model.status)
        XCTAssertEqual(model.integrationAvailability, .missing)

        let integratedCalls = await integrated.calls()
        let nativeCalls = await native.calls()
        let requests = await native.recordedRequests()
        let acknowledgementDigests = await native.recordedAcknowledgementDigests()
        XCTAssertEqual(integratedCalls, [])
        XCTAssertEqual(nativeCalls.discoveries, 1)
        XCTAssertEqual(nativeCalls.acknowledgements, 1)
        XCTAssertEqual(nativeCalls.inspections, 2)
        XCTAssertEqual(nativeCalls.dataInspections, 0)
        XCTAssertEqual(nativeCalls.removals, 1)
        XCTAssertEqual(acknowledgementDigests, [String(repeating: "f", count: 64)])
        XCTAssertEqual(requests.first?.mode, .preserveData)
        XCTAssertEqual(requests.first?.selection.boundaryRevision, String(repeating: "b", count: 64))
        XCTAssertEqual(
            events.events,
            [
                "native_discover_1",
                "recovery_save_in_flight",
                "recovery_save_terminal_ack_pending",
                "native_ack_1",
                "recovery_clear",
                "desktop_relaunch",
            ]
        )
    }

    func testStandaloneNativeSelectiveTrashPersistsNativeInventoryBeforeRemovalRequest() async throws {
        let appURL = try makeAppBundle()
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: root)
        }
        let helper = root.appendingPathComponent("opencodex-relayctl")
        try Data("#!/bin/sh\nexit 0\n".utf8).write(to: helper)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o700],
            ofItemAtPath: helper.path
        )
        let itemID = "ocx-data-v1:" + String(repeating: "3", count: 32)
        let inventoryRevision = String(repeating: "d", count: 64)
        let recoveryStore = RecoveryStore()
        let native = NativeRemovalClient(
            discovery: try nativeDiscoveryResult(dataCapability: "selective_trash_v1"),
            inspection: try nativeRemovalInspection(dataCapability: "selective_trash_v1"),
            inventory: try nativeDataInventoryReceipt(
                itemID: itemID,
                inventoryRevision: inventoryRevision
            ),
            removalFailuresRemaining: 1
        )
        let model = MenuBarModel(
            client: nil,
            nativeRemovalClient: native,
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            bindingURL: root.appendingPathComponent("routing-binding.json"),
            helperURL: helper,
            startsPolling: false,
            runtimeMode: .managed,
            localization: englishLocalization()
        )

        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates(.standaloneNative) = model.openCodexDiscoveryState { return true }
            return false
        }
        XCTAssertTrue(model.chooseDiscoveredOpenCodexCandidate(id: "0123456789abcdef01234567"))
        model.beginOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .options && !model.isBusy }
        model.setOpenCodexRemovalMode(.trashSelected)
        try await waitUntil {
            model.openCodexRemovalFlow?.nativeInventory?.inventoryRevision == inventoryRevision &&
                !model.isBusy
        }
        model.toggleOpenCodexDataItem(id: itemID)
        model.reviewOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .confirmRemoval && !model.isBusy }
        model.confirmOpenCodexPackageRemoval()
        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .confirmTrash)
        model.confirmOpenCodexDataTrash()
        try await waitUntil(timeout: 2) {
            model.openCodexRemovalFlow?.phase == .rebootRequired && !model.isBusy
        }

        let requests = await native.recordedRequests()
        XCTAssertEqual(requests.count, 1)
        XCTAssertEqual(requests[0].mode, .trashSelected)
        XCTAssertEqual(requests[0].dataItemIDs, [itemID])
        XCTAssertEqual(requests[0].expectedInventoryRevision, inventoryRevision)
        XCTAssertEqual(recoveryStore.session?.context, .standaloneNative)
        XCTAssertEqual(recoveryStore.session?.mode, .trashSelected)
        XCTAssertEqual(recoveryStore.session?.orderedDataItemIDs, [itemID])
        XCTAssertEqual(recoveryStore.session?.inventoryRevision, inventoryRevision)
        XCTAssertEqual(recoveryStore.session?.recoveryKind, .rebootRequired)
    }

    func testStandaloneNativeReceiptLossKeepsRecoveryAndReplaysStableOperation() async throws {
        let appURL = try makeAppBundle()
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: root)
        }
        let helper = root.appendingPathComponent("opencodex-relayctl")
        let binding = root.appendingPathComponent("routing-binding.json")
        try Data("#!/bin/sh\nexit 0\n".utf8).write(to: helper)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o700],
            ofItemAtPath: helper.path
        )
        let recoveryStore = RecoveryStore()
        let native = NativeRemovalClient(
            discovery: try nativeDiscoveryResult(),
            terminalDiscovery: try nativeCleanDiscoveryResult(),
            inspection: try nativeRemovalInspection(),
            removal: try successfulNativeRemovalReceipt(),
            removalFailuresRemaining: 1
        )
        let desktop = DesktopController(runningValues: [])
        let model = MenuBarModel(
            client: nil,
            nativeRemovalClient: native,
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            bindingURL: binding,
            helperURL: helper,
            startsPolling: false,
            runtimeMode: .managed,
            localization: englishLocalization()
        )

        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates(.standaloneNative) = model.openCodexDiscoveryState { return true }
            return false
        }
        XCTAssertTrue(model.chooseDiscoveredOpenCodexCandidate(id: "0123456789abcdef01234567"))
        model.beginOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .options && !model.isBusy }
        model.reviewOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .confirmRemoval && !model.isBusy }
        model.confirmOpenCodexPackageRemoval()
        try await waitUntil(timeout: 2) {
            model.openCodexRemovalFlow?.phase == .rebootRequired && !model.isBusy
        }

        XCTAssertTrue(model.hasPendingOpenCodexRemovalRecovery)
        XCTAssertEqual(recoveryStore.session?.context, .standaloneNative)
        XCTAssertEqual(recoveryStore.session?.recoveryKind, .rebootRequired)
        XCTAssertEqual(recoveryStore.clearCount, 0)
        XCTAssertFalse(model.canDiscoverOpenCodex)
        model.addLocalOpenCodexBackend()
        XCTAssertEqual(model.message?.code, "opencodex_recovery_context_pending")
        var requests = await native.recordedRequests()
        XCTAssertEqual(requests.count, 1)
        XCTAssertFalse(requests[0].confirmsRebootedProcessRecovery)

        let restartedDesktop = DesktopController(runningValues: [])
        let restartedModel = MenuBarModel(
            client: nil,
            nativeRemovalClient: native,
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: restartedDesktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            bindingURL: binding,
            helperURL: helper,
            startsPolling: false,
            runtimeMode: .managed,
            localization: englishLocalization()
        )
        XCTAssertTrue(restartedModel.hasPendingOpenCodexRemovalRecovery)
        XCTAssertEqual(restartedModel.openCodexRemovalFlow?.phase, .rebootRequired)
        XCTAssertFalse(restartedModel.canDiscoverOpenCodex)

        restartedModel.prepareRebootedOpenCodexRecovery()
        try await waitUntil(timeout: 2) {
            restartedModel.openCodexRemovalFlow?.nativeReceipt?.isSuccessful == true &&
                !restartedModel.isBusy
        }

        requests = await native.recordedRequests()
        XCTAssertEqual(requests.count, 2)
        XCTAssertEqual(requests[1].selection, requests[0].selection)
        XCTAssertEqual(requests[1].mode, requests[0].mode)
        XCTAssertEqual(requests[1].dataItemIDs, requests[0].dataItemIDs)
        XCTAssertEqual(requests[1].expectedInventoryRevision, requests[0].expectedInventoryRevision)
        XCTAssertTrue(requests[1].confirmsRebootedProcessRecovery)
        XCTAssertFalse(restartedModel.hasPendingOpenCodexRemovalRecovery)
        XCTAssertNil(recoveryStore.session)
        XCTAssertEqual(recoveryStore.clearCount, 1)
        XCTAssertEqual(desktop.relaunches, 0)
        XCTAssertEqual(restartedDesktop.relaunches, 1)
        XCTAssertFalse(FileManager.default.fileExists(atPath: binding.path))
        let nativeCalls = await native.calls()
        XCTAssertEqual(nativeCalls.discoveries, 1)
        XCTAssertEqual(nativeCalls.acknowledgements, 1)
        XCTAssertEqual(nativeCalls.removals, 2)
        XCTAssertEqual(restartedModel.openCodexRemovalFlow?.phase, .result)
    }

    func testStandaloneNativeRecoveryRechecksMissingBindingAfterDesktopExit() async throws {
        let appURL = try makeAppBundle()
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: root)
        }
        let helper = root.appendingPathComponent("opencodex-relayctl")
        let binding = root.appendingPathComponent("routing-binding.json")
        try Data("#!/bin/sh\nexit 0\n".utf8).write(to: helper)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o700],
            ofItemAtPath: helper.path
        )
        let recoveryStore = RecoveryStore()
        recoveryStore.session = try OpenCodexRemovalRecoverySession(
            context: .standaloneNative,
            selection: try OpenCodexRemovalSelection(
                installationID: "0123456789abcdef01234567",
                installationFingerprint: String(repeating: "a", count: 64)
            ),
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .nativeRecoveryRequired,
            lastCode: "native_recovery_required",
            expectedBoundaryRevision: String(repeating: "b", count: 64),
            nativeRestoreFingerprint: String(repeating: "c", count: 64)
        )
        let native = NativeRemovalClient(
            discovery: try nativeDiscoveryResult(),
            inspection: try nativeRemovalInspection(),
            removal: try successfulNativeRemovalReceipt()
        )
        // Model initialization consumes the first process snapshot. The next
        // two values model a running Desktop that exits after the request.
        let desktop = DesktopController(runningValues: [true, true, false])
        desktop.waitAction = { [weak self] in
            try? self?.writeRoutingBinding(at: binding)
        }
        let model = MenuBarModel(
            client: nil,
            nativeRemovalClient: native,
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            bindingURL: binding,
            helperURL: helper,
            startsPolling: false,
            runtimeMode: .managed,
            localization: englishLocalization()
        )

        model.checkOpenCodexNativeRecovery()
        try await waitUntil(timeout: 2) { !model.isBusy }

        XCTAssertEqual(desktop.quitRequests, 1)
        XCTAssertTrue(FileManager.default.fileExists(atPath: binding.path))
        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .failed)
        XCTAssertEqual(model.message?.code, "opencodex_native_request_invalid")
        XCTAssertNotNil(recoveryStore.session)
        let calls = await native.calls()
        XCTAssertEqual(calls.removals, 0)
    }

    func testStandaloneNativeTerminalAcknowledgementSurvivesCrashBeforeLocalClear() async throws {
        let appURL = try makeAppBundle()
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: root)
        }
        let helper = root.appendingPathComponent("opencodex-relayctl")
        let binding = root.appendingPathComponent("routing-binding.json")
        try Data("#!/bin/sh\nexit 0\n".utf8).write(to: helper)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o700],
            ofItemAtPath: helper.path
        )
        let recoveryStore = RecoveryStore(clearFailuresRemaining: 1)
        let native = NativeRemovalClient(
            discovery: try nativeDiscoveryResult(),
            terminalDiscovery: try nativeCleanDiscoveryResult(),
            inspection: try nativeRemovalInspection(),
            removal: try successfulNativeRemovalReceipt()
        )
        let desktop = DesktopController(runningValues: [])
        let model = MenuBarModel(
            client: nil,
            nativeRemovalClient: native,
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            bindingURL: binding,
            helperURL: helper,
            startsPolling: false,
            runtimeMode: .managed,
            localization: englishLocalization()
        )

        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates(.standaloneNative) = model.openCodexDiscoveryState { return true }
            return false
        }
        XCTAssertTrue(model.chooseDiscoveredOpenCodexCandidate(id: "0123456789abcdef01234567"))
        model.beginOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .options && !model.isBusy }
        model.reviewOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .confirmRemoval && !model.isBusy }
        model.confirmOpenCodexPackageRemoval()
        try await waitUntil(timeout: 2) {
            model.openCodexRemovalFlow?.phase == .nativeTerminalCleanupPending && !model.isBusy
        }

        XCTAssertTrue(model.openCodexRemovalFlow?.nativeReceipt?.isSuccessful == true)
        XCTAssertEqual(model.message?.code, "native_terminal_cleanup_pending")
        XCTAssertTrue(model.hasPendingOpenCodexRemovalRecovery)
        XCTAssertNotNil(recoveryStore.session)
        XCTAssertEqual(recoveryStore.session?.recoveryKind, .terminalAckPending)
        XCTAssertEqual(
            recoveryStore.session?.terminalReceiptDigest,
            String(repeating: "f", count: 64)
        )
        XCTAssertEqual(recoveryStore.saveCount, 2)
        XCTAssertEqual(recoveryStore.clearCount, 1)
        XCTAssertEqual(desktop.relaunches, 0)
        var nativeCalls = await native.calls()
        XCTAssertEqual(nativeCalls.discoveries, 1)
        XCTAssertEqual(nativeCalls.acknowledgements, 1)
        XCTAssertEqual(nativeCalls.removals, 1)

        let restartedDesktop = DesktopController(runningValues: [])
        let restartedModel = MenuBarModel(
            client: nil,
            nativeRemovalClient: native,
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: restartedDesktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            bindingURL: binding,
            helperURL: helper,
            startsPolling: false,
            runtimeMode: .managed,
            localization: englishLocalization()
        )
        XCTAssertEqual(restartedModel.openCodexRemovalFlow?.phase, .nativeTerminalCleanupPending)
        XCTAssertNil(restartedModel.openCodexRemovalFlow?.nativeReceipt)
        restartedModel.checkOpenCodexNativeTerminalCleanup()
        try await waitUntil(timeout: 2) {
            restartedModel.openCodexRemovalFlow?.phase == .result && !restartedModel.isBusy
        }

        XCTAssertEqual(restartedModel.message?.code, "opencodex_native_removed")
        XCTAssertEqual(desktop.relaunches, 0)
        XCTAssertEqual(restartedDesktop.relaunches, 1)
        XCTAssertFalse(restartedModel.hasPendingOpenCodexRemovalRecovery)
        XCTAssertNil(recoveryStore.session)
        XCTAssertEqual(recoveryStore.saveCount, 2)
        XCTAssertEqual(recoveryStore.clearCount, 2)
        nativeCalls = await native.calls()
        XCTAssertEqual(nativeCalls.discoveries, 1)
        XCTAssertEqual(nativeCalls.acknowledgements, 2)
        XCTAssertEqual(nativeCalls.removals, 1)
        XCTAssertFalse(FileManager.default.fileExists(atPath: binding.path))
    }

    func testStandaloneNativeDataRefreshTransitionsToNativeRecovery() async throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let helper = root.appendingPathComponent("opencodex-relayctl")
        try Data("#!/bin/sh\nexit 0\n".utf8).write(to: helper)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o700],
            ofItemAtPath: helper.path
        )
        let itemID = "ocx-data-v1:" + String(repeating: "3", count: 32)
        let recoveryStore = RecoveryStore()
        recoveryStore.session = try OpenCodexRemovalRecoverySession(
            context: .standaloneNative,
            selection: try OpenCodexRemovalSelection(
                installationID: "0123456789abcdef01234567",
                installationFingerprint: String(repeating: "a", count: 64)
            ),
            mode: .trashSelected,
            orderedDataItemIDs: [itemID],
            retiredDataItemIDs: [],
            recoveryKind: .dataSelectionRefreshRequired,
            lastCode: "data_selection_refresh_required",
            inventoryRevision: String(repeating: "d", count: 64),
            expectedBoundaryRevision: String(repeating: "b", count: 64),
            nativeRestoreFingerprint: String(repeating: "c", count: 64)
        )
        let inventorySource = Data("""
        {"schema_version":1,"operation":"open-codex-native-data-inventory","context":"standalone_native","status":"refused","boundary_revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","native_state":"unavailable","native_recovery_required":true,"installation_id":"0123456789abcdef01234567","installation_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","native_restore_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","inventory_revision":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","items":[]}
        """.utf8)
        let inventory = try JSONDecoder().decode(
            OpenCodexNativeDataInventoryReceipt.self,
            from: inventorySource
        )
        let native = NativeRemovalClient(
            discovery: try nativeDiscoveryResult(),
            inspection: try nativeRemovalInspection(),
            inventory: inventory
        )
        let model = MenuBarModel(
            client: nil,
            nativeRemovalClient: native,
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(nil),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            bindingURL: root.appendingPathComponent("routing-binding.json"),
            helperURL: helper,
            startsPolling: false,
            runtimeMode: .managed,
            localization: englishLocalization()
        )

        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .dataRefreshRequired)
        model.refreshInterruptedOpenCodexInventory()
        try await waitUntil {
            model.openCodexRemovalFlow?.phase == .nativeRecoveryRequired && !model.isBusy
        }

        XCTAssertEqual(recoveryStore.session?.recoveryKind, .nativeRecoveryRequired)
        XCTAssertEqual(model.message?.code, "native_recovery_required")
        XCTAssertTrue(model.hasPendingOpenCodexRemovalRecovery)
        XCTAssertFalse(model.canDiscoverOpenCodex)
        let calls = await native.calls()
        XCTAssertEqual(calls.dataInspections, 1)
        XCTAssertEqual(calls.removals, 0)
    }

    func testBindingLossBlocksBroadApprovalAndCandidateChoice() async throws {
        let appURL = try makeAppBundle()
        let fixture = try makeManagedIntegrationFixture(status: externalStatus())
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: fixture.root)
        }
        let discovery = DiscoveryClient(responses: [
            try discoveryResult(tier: .a, includesCandidate: false),
            try discoveryResult(tier: .b, includesCandidate: false),
            try discoveryResult(tier: .a, includesCandidate: true),
        ])
        let model = MenuBarModel(
            client: nil,
            discoveryClient: discovery,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            bindingURL: fixture.binding,
            helperURL: fixture.helper,
            startsPolling: false,
            runtimeMode: .managed,
            localization: englishLocalization()
        )

        await model.refreshStatusNow()
        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .broadScanApprovalRequired = model.openCodexDiscoveryState { return true }
            return false
        }

        try FileManager.default.removeItem(at: fixture.binding)
        model.approveBroadOpenCodexDiscovery()

        XCTAssertEqual(model.integrationAvailability, .missing)
        XCTAssertEqual(model.openCodexDiscoveryState, .idle)
        let callsBeforeBroadApproval = await discovery.calls()
        XCTAssertEqual(callsBeforeBroadApproval.count, 2)

        try writeRoutingBinding(at: fixture.binding)
        await model.refreshStatusNow()
        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates = model.openCodexDiscoveryState { return true }
            return false
        }

        try FileManager.default.removeItem(at: fixture.binding)
        XCTAssertFalse(model.chooseDiscoveredOpenCodexCandidate(id: "0123456789abcdef01234567"))
        model.selectOpenCodexExecutableManually()

        XCTAssertEqual(model.integrationAvailability, .missing)
        XCTAssertEqual(model.openCodexDiscoveryState, .idle)
        XCTAssertNil(model.openCodexRemovalFlow)
        let callsAfterCandidateBlock = await discovery.calls()
        XCTAssertEqual(callsAfterCandidateBlock.count, 3)
    }

    func testBindingLossWhileDiscoveryIsInFlightDiscardsTheResult() async throws {
        let appURL = try makeAppBundle()
        let fixture = try makeManagedIntegrationFixture(status: externalStatus())
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: fixture.root)
        }
        let discovery = DelayedDiscoveryClient(
            response: try discoveryResult(tier: .a, includesCandidate: true)
        )
        let model = MenuBarModel(
            client: nil,
            discoveryClient: discovery,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            bindingURL: fixture.binding,
            helperURL: fixture.helper,
            startsPolling: false,
            runtimeMode: .managed,
            localization: englishLocalization()
        )

        await model.refreshStatusNow()
        model.addLocalOpenCodexBackend()
        await discovery.waitForStart()
        try FileManager.default.removeItem(at: fixture.binding)
        await discovery.resume()
        try await waitUntil { !model.isBusy }

        let callCount = await discovery.callCount()
        XCTAssertEqual(callCount, 1)
        XCTAssertEqual(model.integrationAvailability, .missing)
        XCTAssertEqual(model.openCodexDiscoveryState, .idle)
        XCTAssertTrue(model.discoveredOpenCodexCandidates.isEmpty)
        XCTAssertEqual(model.message?.code, "routing_binding_changed")
    }

    func testInteractiveSurfaceVisibilityTracksPopoverAndControlCenterIndependently() {
        let model = MenuBarModel(
            client: RelayctlClient(response: externalStatus()),
            targetStore: TargetStore(nil),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        XCTAssertFalse(model.isInteractiveSurfaceVisible)
        model.setPopoverVisible(true)
        XCTAssertTrue(model.isInteractiveSurfaceVisible)
        model.setControlCenterVisible(true)
        model.setPopoverVisible(false)
        XCTAssertTrue(model.isInteractiveSurfaceVisible)
        model.setControlCenterVisible(false)
        XCTAssertFalse(model.isInteractiveSurfaceVisible)
    }

    func testDockReopenPublishesControlCenterPresentationRequest() {
        let reopen = expectation(
            forNotification: .relayDockReopenRequested,
            object: nil
        )
        let delegate = RelayAppDelegate()

        XCTAssertTrue(
            delegate.applicationShouldHandleReopen(
                NSApplication.shared,
                hasVisibleWindows: false
            )
        )
        wait(for: [reopen], timeout: 1)
    }

    func testMenuBarPopoverHeightIsBoundedInEnglishAndKorean() {
        for selection in [AppLanguageSelection.english, .korean] {
            let localization = LocalizationStore()
            localization.selection = selection
            let model = MenuBarModel(
                client: RelayctlClient(response: recoveryStatus()),
                targetStore: TargetStore(nil),
                desktopApplication: DesktopController(runningValues: []),
                desktopTrustPolicy: trustedPolicy,
                desktopTrustValidator: TrustValidator(),
                desktopDiscoverer: DesktopDiscoverer(),
                loginRegistration: LoginRegistration(),
                startsPolling: false,
                distributionFlavor: .localDevelopment,
                localization: localization
            )
            let hostingView = NSHostingView(
                rootView: MenuBarContentView(
                    model: model,
                    onOpenControlCenter: {}
                )
                .environmentObject(localization)
            )

            hostingView.layoutSubtreeIfNeeded()
            XCTAssertGreaterThan(hostingView.fittingSize.height, 120)
            XCTAssertLessThanOrEqual(
                hostingView.fittingSize.height,
                ControlCenterLayout.maximumPopoverHeight
            )
        }
    }

    func testControlCenterLayoutFitsA1024By640VisibleFrame() {
        let size = ControlCenterLayout.initialWindowSize(
            for: CGSize(width: 1_024, height: 640)
        )

        XCTAssertLessThanOrEqual(
            size.width + (ControlCenterLayout.visibleFrameInset * 2),
            1_024
        )
        XCTAssertLessThanOrEqual(
            size.height + (ControlCenterLayout.visibleFrameInset * 2),
            640
        )
        XCTAssertGreaterThanOrEqual(size.width, ControlCenterLayout.minimumWindowSize.width)
        XCTAssertGreaterThanOrEqual(size.height, ControlCenterLayout.minimumWindowSize.height)
        XCTAssertEqual(ControlCenterLayout.maximumPopoverHeight, 300)
    }

    func testStatusRowsAndActionButtonsAdaptToNarrowDetailWidths() {
        let localization = englishLocalization()
        let wideStatus = NSHostingView(
            rootView: StatusRow(
                "Last local update",
                value: "August 24, 2026 at 12:32:01 AM"
            )
            .environmentObject(localization)
            .frame(width: 600)
        )
        let narrowStatus = NSHostingView(
            rootView: StatusRow(
                "Last local update",
                value: "August 24, 2026 at 12:32:01 AM"
            )
            .environmentObject(localization)
            .frame(width: 260)
        )
        let wideActions = NSHostingView(
            rootView: ControlCenterActionFooter {
                Menu("More") {
                    Button("Use native Codex") {}
                }
            } primary: {
                Button("Apply") {}
            }
            .frame(width: 600)
        )
        let narrowActions = NSHostingView(
            rootView: ControlCenterActionFooter {
                Menu("More") {
                    Button("Use native Codex") {}
                }
            } primary: {
                Button("Apply pending routing change with a long title") {}
            }
            .frame(width: 260)
        )

        for view in [wideStatus, narrowStatus, wideActions, narrowActions] {
            view.layoutSubtreeIfNeeded()
        }

        XCTAssertGreaterThanOrEqual(narrowStatus.fittingSize.height, wideStatus.fittingSize.height)
        XCTAssertGreaterThanOrEqual(narrowStatus.fittingSize.height, 42)
        XCTAssertGreaterThan(narrowActions.fittingSize.height, wideActions.fittingSize.height)
        XCTAssertEqual(ControlCenterPresentationMetrics.statusLabelWidth, 180)
        XCTAssertEqual(ControlCenterPresentationMetrics.pageMaximumWidth, 800)
    }

    func testStatusMessageTonePreservesErrorsWithoutDuplicatingNotices() {
        let message = SafeStatusMessage(code: "relayctl_failed", key: .relayctlFailed)
        let otherError = SafeStatusMessage(code: "status_failed", key: .relayctlFailed)

        XCTAssertEqual(
            ControlCenterStatusTone.messageTone(for: message, statusError: nil),
            .neutral
        )
        XCTAssertEqual(
            ControlCenterStatusTone.messageTone(for: message, statusError: message),
            .error
        )
        XCTAssertEqual(
            ControlCenterStatusTone.messageTone(for: message, statusError: otherError),
            .neutral
        )
    }

    func testLocalPageHandlesOnlyTheExactMissingIntegrationNoticeInline() {
        let missing = SafeStatusMessage(code: "routing_binding_missing", key: .bindingMissing)
        let unsafe = SafeStatusMessage(code: "routing_binding_unsafe", key: .bindingUnsafe)

        XCTAssertFalse(
            ControlCenterNoticePresentation.presents(
                missing,
                integrationMessage: missing,
                integrationAvailability: .missing,
                handlesMissingIntegrationInline: true
            )
        )
        XCTAssertTrue(
            ControlCenterNoticePresentation.presents(
                missing,
                integrationMessage: missing,
                integrationAvailability: .missing,
                handlesMissingIntegrationInline: false
            )
        )
        XCTAssertTrue(
            ControlCenterNoticePresentation.presents(
                unsafe,
                integrationMessage: unsafe,
                integrationAvailability: .unsafe,
                handlesMissingIntegrationInline: true
            )
        )
        XCTAssertTrue(
            ControlCenterNoticePresentation.presents(
                unsafe,
                integrationMessage: missing,
                integrationAvailability: .missing,
                handlesMissingIntegrationInline: true
            )
        )
    }

    func testDedicatedControlCenterPagesFitCommonDetailWidthsInEnglishAndKorean() {
        for selection in [AppLanguageSelection.english, .korean] {
            let localization = LocalizationStore()
            localization.selection = selection
            let model = MenuBarModel(
                client: RelayctlClient(response: externalStatus()),
                targetStore: TargetStore(nil),
                desktopApplication: DesktopController(runningValues: []),
                desktopTrustPolicy: trustedPolicy,
                desktopTrustValidator: TrustValidator(),
                desktopDiscoverer: DesktopDiscoverer(),
                loginRegistration: LoginRegistration(),
                startsPolling: false,
                distributionFlavor: .localDevelopment,
                localization: localization
            )
            let missingIntegrationModel = MenuBarModel(
                client: nil,
                targetStore: TargetStore(nil),
                desktopApplication: DesktopController(runningValues: []),
                desktopTrustPolicy: trustedPolicy,
                desktopTrustValidator: TrustValidator(),
                desktopDiscoverer: DesktopDiscoverer(),
                loginRegistration: LoginRegistration(),
                bindingURL: FileManager.default.temporaryDirectory
                    .appendingPathComponent(UUID().uuidString)
                    .appendingPathComponent("routing-binding.json"),
                startsPolling: false,
                distributionFlavor: .localDevelopment,
                runtimeMode: .managed,
                localization: localization
            )
            XCTAssertEqual(missingIntegrationModel.integrationAvailability, .missing)
            let localizer = localization.localizer
            let controller = model.makeCodexConfigurationController()
            let pages: [AnyView] = [
                AnyView(OverviewControlCenterPage(
                    model: model,
                    localizer: localizer,
                    title: localizer.text(.controlCenterOverview),
                    systemImage: "gauge.with.dots.needle.50percent"
                )),
                AnyView(ConnectionControlCenterPage(
                    model: model,
                    localizer: localizer,
                    title: localizer.text(.controlCenterConnection),
                    systemImage: "point.3.connected.trianglepath.dotted",
                    openMaintenance: {},
                    openSettings: {}
                )),
                AnyView(DesktopControlCenterPage(
                    model: model,
                    codexConfiguration: controller,
                    localizer: localizer,
                    title: localizer.text(.controlCenterDesktop),
                    systemImage: "macwindow",
                    requestPreview: {}
                )),
                AnyView(LocalOpenCodexControlCenterPage(
                    model: model,
                    localizer: localizer,
                    title: localizer.text(.controlCenterLocalOpenCodex),
                    systemImage: "shippingbox",
                    openMaintenance: {},
                    openSettings: {}
                )),
                AnyView(LocalOpenCodexControlCenterPage(
                    model: missingIntegrationModel,
                    localizer: localizer,
                    title: localizer.text(.controlCenterLocalOpenCodex),
                    systemImage: "shippingbox",
                    openMaintenance: {},
                    openSettings: {}
                )),
                AnyView(MaintenanceControlCenterPage(
                    model: model,
                    localizer: localizer,
                    title: localizer.text(.controlCenterMaintenance),
                    systemImage: "wrench.and.screwdriver",
                    confirmNativeRepair: {},
                    openLocalOpenCodex: {}
                )),
                AnyView(ActivityLogControlCenterPage(
                    model: model,
                    localizer: localizer,
                    title: localizer.text(.controlCenterActivityLog),
                    systemImage: "list.bullet.rectangle"
                )),
                AnyView(SettingsControlCenterPage(
                    model: model,
                    gatewaySettings: model.makeGatewaySettingsController(),
                    relocation: ApplicationRelocationController(runtimeMode: .preview),
                    languageSelection: .constant(selection),
                    languageDescriptors: localization.registry.descriptors,
                    localizer: localizer,
                    title: localizer.text(.controlCenterSettings),
                    systemImage: "gearshape"
                )),
                AnyView(AppInformationControlCenterPage(
                    model: model,
                    localizer: localizer,
                    title: localizer.text(.controlCenterAppInformation),
                    systemImage: "info.circle"
                )),
            ]

            for width in [CGFloat(800), 600, 320] {
                for (index, page) in pages.enumerated() {
                    let hostingView = NSHostingView(
                        rootView: page
                            .environmentObject(localization)
                            .frame(width: width)
                    )
                    hostingView.layoutSubtreeIfNeeded()

                    XCTAssertGreaterThan(
                        hostingView.fittingSize.height,
                        80,
                        "page \(index) was empty at \(width)pt for \(selection)"
                    )
                    XCTAssertLessThanOrEqual(
                        hostingView.fittingSize.width,
                        width,
                        "page \(index) exceeded \(width)pt for \(selection)"
                    )
                }
            }
        }
    }

    func testSuccessfulDevelopmentHelperSheetFitsCommonWidthsInEnglishAndKorean() async throws {
        for selection in [AppLanguageSelection.english, .korean] {
            let localization = LocalizationStore()
            localization.selection = selection
            let guardClient = HomebrewGuardClient(
                availability: HomebrewGuardAvailability(
                    registration: .ready,
                    helperVersion: "0.3.8-rc.3",
                    protocolVersion: homebrewGuardProtocolVersion,
                    errorCode: nil,
                    operationID: nil
                )
            )
            guardClient.backendValue = .manualAdmin
            let model = MenuBarModel(
                client: RelayctlClient(response: externalStatus()),
                targetStore: TargetStore(nil),
                desktopApplication: DesktopController(runningValues: []),
                desktopTrustPolicy: trustedPolicy,
                desktopTrustValidator: TrustValidator(),
                desktopDiscoverer: DesktopDiscoverer(),
                loginRegistration: LoginRegistration(),
                homebrewGuard: guardClient,
                startsPolling: false,
                distributionFlavor: .localDevelopment,
                localization: localization
            )
            try await waitUntil {
                model.homebrewGuardAvailability.registration == .ready
            }

            for width in [CGFloat(800), 600, 320] {
                let sheet = DevelopmentHomebrewGuardSetupView(
                    command: "/usr/bin/sudo -- '/Applications/OpenCodexRelay.app/installer' update",
                    initialRegistration: .manualUpdateRequired,
                    isRecovery: false,
                    model: model,
                    localizer: localization.localizer,
                    isPresented: .constant(true)
                )
                let hostingView = NSHostingView(
                    rootView: sheet
                        .environmentObject(localization)
                        .frame(width: width)
                )
                hostingView.layoutSubtreeIfNeeded()

                XCTAssertGreaterThan(
                    hostingView.fittingSize.height,
                    80,
                    "successful helper sheet was empty at \(width)pt for \(selection)"
                )
                XCTAssertLessThanOrEqual(
                    hostingView.fittingSize.width,
                    width,
                    "successful helper sheet exceeded \(width)pt for \(selection)"
                )
            }
        }
    }

    func testHomebrewGuardPrimaryActionAndRecoveryGuidanceAreExclusive() {
        XCTAssertEqual(HomebrewGuardPrimaryAction.resolve(.notRegistered), .register)
        XCTAssertEqual(HomebrewGuardPrimaryAction.resolve(.approvalRequired), .openSettings)
        XCTAssertEqual(HomebrewGuardPrimaryAction.resolve(.manualInstallRequired), .developmentSetup)
        XCTAssertEqual(HomebrewGuardPrimaryAction.resolve(.manualUpdateRequired), .developmentSetup)
        XCTAssertEqual(HomebrewGuardPrimaryAction.resolve(.daemonLaunchFailed), .developmentSetup)
        XCTAssertEqual(HomebrewGuardPrimaryAction.resolve(.recoveryRequired), .recover)
        XCTAssertNil(HomebrewGuardPrimaryAction.resolve(.ready))

        XCTAssertEqual(
            HomebrewGuardDiagnosticGuidanceBlock.codeGroups(isRecovery: true),
            [["rollback_result=failed"]]
        )
        XCTAssertEqual(
            HomebrewGuardDiagnosticGuidanceBlock.codeGroups(isRecovery: false),
            [
                ["daemon_start_rejected"],
                ["probe_timeout", "xpc_rejected", "invalid_response"],
                ["owner_or_mode_mismatch"],
                ["rollback_result=failed"],
            ]
        )
    }

    func testLocalOpenCodexPageSeparatesNativeManagementFromRelaySetup() {
        let missing = LocalOpenCodexPageMode.resolve(.missing)
        XCTAssertEqual(missing, .standaloneNative)
        XCTAssertTrue(missing.showsDiscoveryControls)
        XCTAssertTrue(missing.showsRelaySetupCard)

        let ready = LocalOpenCodexPageMode.resolve(.ready)
        XCTAssertEqual(ready, .integrated)
        XCTAssertTrue(ready.showsDiscoveryControls)
        XCTAssertFalse(ready.showsRelaySetupCard)

        for availability in [
            RelayIntegrationAvailability.preview,
            .unsafe,
            .invalid,
            .helperUnavailable,
        ] {
            let blocked = LocalOpenCodexPageMode.resolve(availability)
            XCTAssertEqual(blocked, .blocked)
            XCTAssertFalse(blocked.showsDiscoveryControls)
            XCTAssertFalse(blocked.showsRelaySetupCard)
        }

        for availability in [
            RelayIntegrationAvailability.missing,
            .ready,
        ] {
            let recovering = LocalOpenCodexPageMode.resolve(
                availability,
                recoveryRequired: true
            )
            XCTAssertEqual(recovering, .blocked)
            XCTAssertFalse(recovering.showsDiscoveryControls)
            XCTAssertFalse(recovering.showsRelaySetupCard)
        }
    }

    func testDevelopmentSetupSheetPresentationInvalidatesStaleCommands() {
        for initial in [
            HomebrewGuardRegistrationState.manualInstallRequired,
            .manualUpdateRequired,
            .manualInstallerRecoveryRequired,
        ] {
            XCTAssertEqual(
                DevelopmentSetupSheetPresentation.resolve(
                    initial: initial,
                    current: initial,
                    didCheck: false,
                    isChecking: false
                ),
                .command
            )
            XCTAssertEqual(
                DevelopmentSetupSheetPresentation.resolve(
                    initial: initial,
                    current: initial,
                    didCheck: false,
                    isChecking: true
                ),
                .checking
            )
            XCTAssertEqual(
                DevelopmentSetupSheetPresentation.resolve(
                    initial: initial,
                    current: initial,
                    didCheck: true,
                    isChecking: false
                ),
                .unchanged
            )
            XCTAssertEqual(
                DevelopmentSetupSheetPresentation.resolve(
                    initial: initial,
                    current: .ready,
                    didCheck: true,
                    isChecking: false
                ),
                .ready
            )
        }

        let changed = DevelopmentSetupSheetPresentation.resolve(
            initial: .manualUpdateRequired,
            current: .manualInstallerRecoveryRequired,
            didCheck: true,
            isChecking: false
        )
        XCTAssertEqual(changed, .stateChanged)
        XCTAssertFalse(changed.showsCommand)
        XCTAssertFalse(DevelopmentSetupSheetPresentation.ready.showsCommand)
        XCTAssertTrue(DevelopmentSetupSheetPresentation.unchanged.showsCommand)
    }

    func testRemovalRecoveryStoreKeepsLegacyIntegratedAndRejectsSplitBrain() throws {
        let suite = "OpenCodexRelayRecoveryStoreTests.\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        defer { defaults.removePersistentDomain(forName: suite) }
        let currentKey = "openCodexRemovalRecoverySession.v2"
        let legacyKey = "openCodexRemovalRecoverySession.v1"
        let store = UserDefaultsOpenCodexRemovalRecoverySessionStore(
            defaults: defaults,
            key: currentKey,
            legacyKey: legacyKey
        )
        let selection = try OpenCodexRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64)
        )
        let legacy = try OpenCodexRemovalRecoverySession(
            schema: 2,
            selection: selection,
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .rebootRequired,
            lastCode: "process_cleanup_unverified"
        )
        let legacyData = try JSONEncoder().encode(legacy)
        defaults.set(legacyData, forKey: legacyKey)
        XCTAssertEqual(try store.load()?.context, .integrated)

        defaults.set(legacyData, forKey: currentKey)
        XCTAssertThrowsError(try store.load())

        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let helper = root.appendingPathComponent("opencodex-relayctl")
        try Data("#!/bin/sh\nexit 0\n".utf8).write(to: helper)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o700],
            ofItemAtPath: helper.path
        )
        let blockedModel = MenuBarModel(
            client: nil,
            removalRecoveryStore: store,
            targetStore: TargetStore(nil),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            bindingURL: root.appendingPathComponent("routing-binding.json"),
            helperURL: helper,
            startsPolling: false,
            runtimeMode: .managed,
            localization: englishLocalization()
        )
        XCTAssertTrue(blockedModel.hasPendingOpenCodexRemovalRecovery)
        XCTAssertFalse(blockedModel.canDiscoverOpenCodex)
        XCTAssertEqual(blockedModel.message?.code, "opencodex_recovery_context_invalid")

        defaults.removeObject(forKey: legacyKey)
        XCTAssertThrowsError(try store.load())

        let native = try OpenCodexRemovalRecoverySession(
            context: .standaloneNative,
            selection: selection,
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .nativeRecoveryRequired,
            lastCode: "native_restore_unverified",
            expectedBoundaryRevision: String(repeating: "b", count: 64),
            nativeRestoreFingerprint: String(repeating: "c", count: 64)
        )
        defaults.set(legacyData, forKey: legacyKey)
        try store.save(native)
        XCTAssertNil(defaults.data(forKey: legacyKey))
        XCTAssertEqual(try store.load(), native)
    }

    func testRemovalRecoveryStoreClearVerificationRejectsAStillPresentSession() throws {
        let store = RecoveryStore(clearSucceeds: false)
        store.session = try OpenCodexRemovalRecoverySession(
            selection: try OpenCodexRemovalSelection(
                installationID: "0123456789abcdef01234567",
                installationFingerprint: String(repeating: "a", count: 64)
            ),
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .inFlight,
            lastCode: "native_removal_started"
        )

        XCTAssertThrowsError(try store.clearAndVerify())
        XCTAssertNotNil(store.session)
        XCTAssertEqual(store.clearCount, 1)
    }

    func testIntegrationNoticeToneWinsWhenMessageIsDeduplicated() {
        let cases: [(RelayIntegrationAvailability, ControlCenterStatusTone)] = [
            (.ready, .neutral),
            (.missing, .warning),
            (.preview, .info),
            (.unsafe, .error),
            (.invalid, .error),
            (.helperUnavailable, .error),
        ]

        for (availability, expectedTone) in cases {
            XCTAssertEqual(
                ControlCenterStatusTone.integrationTone(for: availability),
                expectedTone
            )
        }

        let missingMessage = SafeStatusMessage(
            code: RelayIntegrationAvailability.missing.safeCode,
            key: .bindingMissing
        )
        XCTAssertEqual(
            ControlCenterStatusTone.messageTone(
                for: missingMessage,
                statusError: missingMessage,
                integrationMessage: missingMessage,
                integrationAvailability: .missing
            ),
            .warning
        )
    }

    func testLongDiagnosticGuidanceFitsCommonDetailWidths() {
        for selection in [AppLanguageSelection.english, .korean] {
            let localization = LocalizationStore()
            localization.selection = selection

            for isRecovery in [false, true] {
                for width in [CGFloat(800), 600, 320] {
                    let hostingView = NSHostingView(
                        rootView: HomebrewGuardDiagnosticGuidanceBlock(
                            isRecovery: isRecovery,
                            localizer: localization.localizer
                        )
                        .environmentObject(localization)
                        .frame(width: width)
                    )
                    hostingView.layoutSubtreeIfNeeded()

                    XCTAssertGreaterThan(hostingView.fittingSize.height, 0)
                    XCTAssertLessThanOrEqual(
                        hostingView.fittingSize.width,
                        width,
                        "guidance exceeded \(width)pt for \(selection) recovery=\(isRecovery)"
                    )
                }
            }
        }
    }

    func testLanguageSelectionImmediatelyRelocalizesStatusAndSafeMessages() async throws {
        let localization = englishLocalization()
        let model = MenuBarModel(
            client: RelayctlClient(response: externalStatus()),
            targetStore: TargetStore(nil),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: localization
        )
        await model.refreshStatusNow()

        XCTAssertEqual(model.statusTitle, "External gateway ready")
        XCTAssertEqual(model.localRelayDisplay, "Healthy")
        XCTAssertEqual(model.menuAccessibilityLabel, "PW OpenCodex Relay: external gateway ready")

        localization.selection = .korean

        XCTAssertEqual(model.statusTitle, "외부 게이트웨이 준비됨")
        XCTAssertEqual(model.localRelayDisplay, "정상")
        XCTAssertEqual(model.menuAccessibilityLabel, "PW OpenCodex Relay: 외부 게이트웨이가 준비되었습니다")
        let safeMessage = SafeStatusMessage(code: "relayctl_failed", key: .relayctlFailed)
        XCTAssertEqual(safeMessage.text(using: model.localizer), "Relay 제어가 완료되지 않았습니다. 상태를 새로 고치세요.")
    }

    func testOpenCodexDiscoveryEscalatesAToBAndRequiresConsentForC() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }

        let tierBClient = DiscoveryClient(responses: [
            try discoveryResult(tier: .a, includesCandidate: false),
            try discoveryResult(tier: .b, includesCandidate: true),
        ])
        let tierBModel = MenuBarModel(
            client: RelayctlClient(response: externalStatus()),
            discoveryClient: tierBClient,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )
        await tierBModel.refreshStatusNow()
        tierBModel.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates = tierBModel.openCodexDiscoveryState { return true }
            return false
        }
        let tierBCalls = await tierBClient.calls()
        XCTAssertEqual(tierBCalls, [
            .init(tier: .a, broadScanApproved: false),
            .init(tier: .b, broadScanApproved: false),
        ])
        XCTAssertEqual(tierBModel.discoveredOpenCodexCandidates.count, 1)

        let tierCClient = DiscoveryClient(responses: [
            try discoveryResult(tier: .a, includesCandidate: false),
            try discoveryResult(tier: .b, includesCandidate: false),
            try discoveryResult(tier: .c, includesCandidate: true),
        ])
        let tierCModel = MenuBarModel(
            client: RelayctlClient(response: externalStatus()),
            discoveryClient: tierCClient,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )
        await tierCModel.refreshStatusNow()
        tierCModel.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .broadScanApprovalRequired = tierCModel.openCodexDiscoveryState { return true }
            return false
        }
        let callsBeforeBroadApproval = await tierCClient.calls()
        XCTAssertEqual(callsBeforeBroadApproval.map(\.tier), [.a, .b])

        tierCModel.approveBroadOpenCodexDiscovery()
        try await waitUntil {
            if case .candidates = tierCModel.openCodexDiscoveryState { return true }
            return false
        }
        let tierCCalls = await tierCClient.calls()
        XCTAssertEqual(tierCCalls, [
            .init(tier: .a, broadScanApproved: false),
            .init(tier: .b, broadScanApproved: false),
            .init(tier: .c, broadScanApproved: true),
        ])
    }

    func testManualRefreshReplacesAStaleFailureWithTheNewStatusResult() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let client = RelayctlClient(outcomes: [
            .failure(.reported(.operationFailed)),
            .response(externalStatus(generation: 4)),
        ])
        let activityLog = RelayActivityLogStore(subsystem: "test.relay.activity")
        let model = MenuBarModel(
            client: client,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization(),
            activityLog: activityLog
        )

        await model.refreshStatusNow()
        XCTAssertEqual(model.message?.code, "operation_failed")
        XCTAssertNil(model.status)

        await model.refreshStatusNow()
        XCTAssertEqual(model.message?.code, "status_refreshed")
        XCTAssertEqual(model.status?.generation, 4)
        XCTAssertNil(model.statusError)
        XCTAssertFalse(model.isRefreshing)
        XCTAssertTrue(activityLog.events.contains {
            $0.code == "refresh_failed" &&
                $0.fields["failure_code"] == "operation_failed"
        })
        XCTAssertTrue(activityLog.events.contains {
            $0.code == "routing_snapshot_updated" &&
                $0.fields["generation"] == "4"
        })
        XCTAssertTrue(activityLog.events.contains {
            $0.code == "refresh_completed" &&
                $0.fields["changed"] == "true" &&
                $0.fields["generation"] == "4" &&
                $0.fields["phase"] == "relay_active"
        })
    }

    func testManualRefreshReportsNoChangeForIdenticalSnapshot() async {
        let status = externalStatus(generation: 8)
        let activityLog = RelayActivityLogStore(subsystem: "test.relay.activity")
        let model = MenuBarModel(
            client: RelayctlClient(responses: [status, status]),
            targetStore: TargetStore(nil),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization(),
            activityLog: activityLog
        )

        await model.refreshStatusNow()
        await model.refreshStatusNow()

        XCTAssertEqual(model.message?.code, "status_unchanged")
        XCTAssertTrue(activityLog.events.contains {
            $0.code == "refresh_completed" &&
                $0.fields["changed"] == "false" &&
                $0.fields["generation"] == "8" &&
                $0.fields["phase"] == "relay_active"
        })
    }


    func testManualRefreshQueuesBehindAnInFlightStatusPoll() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let client = DelayedFirstStatusClient(responses: [
            externalStatus(generation: 5),
            externalStatus(generation: 6),
        ])
        let activityLog = RelayActivityLogStore(subsystem: "test.relay.activity")
        let model = MenuBarModel(
            client: client,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: true,
            distributionFlavor: .localDevelopment,
            localization: englishLocalization(),
            activityLog: activityLog
        )

        await client.waitForFirstStatusToStart()
        model.refresh()
        XCTAssertTrue(model.isRefreshing)
        await client.resumeFirstStatus()

        try await waitUntil(timeout: 2) {
            model.status?.generation == 6 &&
                model.message?.code == "status_refreshed" &&
                !model.isRefreshing
        }
        let commands = await client.commands()
        XCTAssertEqual(Array(commands.prefix(2)), [.status, .status])
        XCTAssertTrue(activityLog.events.contains { $0.code == "refresh_queued" })
        XCTAssertTrue(activityLog.events.contains {
            $0.code == "refresh_completed" &&
                $0.fields["changed"] == "true" &&
                $0.fields["generation"] == "6"
        })
    }

    func testRoutingSnapshotIncludesOverviewAndConnectionDetailsAtSameGeneration() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let client = RelayctlClient(responses: [
            externalStatus(generation: 9),
            externalStatus(
                generation: 9,
                activeRequests: 3,
                localOpenCodex: .ready
            ),
        ])
        let activityLog = RelayActivityLogStore(subsystem: "test.relay.activity")
        let model = MenuBarModel(
            client: client,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization(),
            activityLog: activityLog
        )

        await model.refreshStatusNow()
        await model.refreshStatusNow()

        let snapshots = activityLog.events.filter {
            $0.code == "routing_snapshot_updated"
        }
        XCTAssertEqual(snapshots.count, 2)
        XCTAssertEqual(snapshots.last?.fields, [
            "schema": "2",
            "generation": "9",
            "phase": "relay_active",
            "desired_backend": "external",
            "applied_backend": "external",
            "local_relay": "healthy",
            "routing_sync": "acknowledged",
            "remote_gateway": "reachable",
            "local_opencodex": "ready",
            "catalog": "running",
            "active_requests": "3",
            "drain": "not_draining",
            "relay_running": "true",
            "relay_admission": "allow",
            "catalog_refresh": "run",
            "desktop_restart_required": "false",
            "desktop_target": "registered_stopped",
        ])
    }


    func testRecoveryBlocksHandoffBeforeDesktopExitOrHelperInvocation() async throws {
        let appURL = try makeAppBundle()
        let executableFixture = try makeOpenCodexExecutableFixture()
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: executableFixture.directory)
        }
        let candidateResult = try automaticDiscoveryResult(
            tier: .a,
            handoffExecutable: executableFixture.executable
        )
        let installationID = try XCTUnwrap(candidateResult.candidates.first?.id)
        let desktop = DesktopController(runningValues: [])
        let client = RelayctlClient(responses: [
            externalStatus(generation: 2),
            orphanRecoveryStatus(generation: 3),
        ])
        let activityLog = RelayActivityLogStore(subsystem: "test.relay.activity")
        let model = MenuBarModel(
            client: client,
            discoveryClient: DiscoveryClient(responses: [candidateResult]),
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization(),
            activityLog: activityLog
        )

        await model.refreshStatusNow()
        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates = model.openCodexDiscoveryState { return true }
            return false
        }
        XCTAssertTrue(model.chooseDiscoveredOpenCodexCandidate(id: installationID))
        await model.refreshStatusNow()
        model.chooseOpenCodexHandoffAction(.retainProxyRemoveShim)

        XCTAssertEqual(model.openCodexRemovalFlow?.handoffProgress?.phase, .failed)
        XCTAssertEqual(model.openCodexRemovalFlow?.handoffProgress?.failedPhase, .preflight)
        XCTAssertEqual(model.openCodexRemovalFlow?.handoffProgress?.result?.code, "routing_recovery_required")
        XCTAssertEqual(desktop.quitRequests, 0)
        let blockedCommands = await client.commands()
        XCTAssertEqual(blockedCommands, [.status, .status])
        XCTAssertTrue(activityLog.events.contains {
            $0.code == "handoff_blocked" &&
                $0.fields["failure_code"] == "routing_recovery_required" &&
                $0.fields["handoff_phase"] == "preflight"
        })
    }

    func testLocalDevelopmentNativeRepairAdvancesGenerationAndLogsBoundedResult() async throws {
        let client = RelayctlClient(responses: [
            orphanRecoveryStatus(generation: 3),
            nativeStatus(generation: 4),
            nativeStatus(generation: 4),
        ])
        let activityLog = RelayActivityLogStore(subsystem: "test.relay.activity")
        let model = MenuBarModel(
            client: client,
            targetStore: TargetStore(nil),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            distributionFlavor: .localDevelopment,
            localization: englishLocalization(),
            activityLog: activityLog
        )

        await model.refreshStatusNow()
        XCTAssertTrue(model.canRepairNative)
        model.repairNative()
        try await waitUntil(timeout: 2) {
            model.status?.generation == 4 && model.status?.phase == .nativeActive && !model.isBusy
        }

        let repairCommands = await client.commands()
        XCTAssertEqual(repairCommands, [
            .status,
            .repairNative(expectedGeneration: 3),
            .status,
        ])
        XCTAssertEqual(model.status?.desiredBackend, RoutingBackend.none)
        XCTAssertEqual(model.status?.appliedBackend, RoutingBackend.none)
        XCTAssertEqual(model.message?.code, "native_repair_completed")
        XCTAssertTrue(activityLog.events.contains { $0.code == "native_repair_started" })
        XCTAssertTrue(activityLog.events.contains {
            $0.code == "native_repair_finished" &&
                $0.fields["generation"] == "4" &&
                $0.fields["result_code"] == "native_repair_completed"
        })
    }

    func testOpaqueGenerationZeroRecoveryCannotInspectOrRepairNativeState() async throws {
        let inspection = try nativeRepairInspection(kind: .stateOnly)
        let nativeClient = NativeRepairClient(
            inspection: inspection,
            outcome: .failure(.reported(.nativeRepairUnavailable))
        )
        let desktop = DesktopController(runningValues: [true])
        let model = MenuBarModel(
            client: RelayctlClient(response: opaqueRecoveryStatus()),
            nativeRepairClient: nativeClient,
            targetStore: TargetStore(nil),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            distributionFlavor: .localDevelopment,
            localization: englishLocalization()
        )

        await model.refreshStatusNow()
        XCTAssertEqual(model.status?.generation, 0)
        XCTAssertFalse(model.canRepairNative)

        model.inspectNativeRepair()
        model.repairNative()
        try await Task.sleep(nanoseconds: 50_000_000)

        let inspectCalls = await nativeClient.inspectCalls()
        let repairCalls = await nativeClient.repairCalls()
        XCTAssertTrue(inspectCalls.isEmpty)
        XCTAssertTrue(repairCalls.isEmpty)
        XCTAssertEqual(desktop.quitRequests, 0)
    }

    func testOpenCodexCandidateWithoutRestoreProofCannotProbeOrExitDesktop() async throws {
        let appURL = try makeAppBundle()
        let executableFixture = try makeOpenCodexExecutableFixture()
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: executableFixture.directory)
        }
        let inspection = try nativeRepairInspection(kind: .openCodex)
        let nativeClient = NativeRepairClient(
            inspection: inspection,
            outcome: .failure(.reported(.nativeOwnerResultInvalid))
        )
        let discovery = DiscoveryClient(responses: [
            try automaticDiscoveryResult(
                tier: .a,
                handoffExecutable: executableFixture.executable,
                nativeRestoreVerified: false
            ),
        ])
        let desktop = DesktopController(runningValues: [true])
        let model = MenuBarModel(
            client: RelayctlClient(response: orphanRecoveryStatus(generation: 3)),
            discoveryClient: discovery,
            nativeRepairClient: nativeClient,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            distributionFlavor: .localDevelopment,
            localization: englishLocalization()
        )

        await model.refreshStatusNow()
        model.inspectNativeRepair()
        try await waitUntil {
            if case .candidates = model.nativeRepairDiscoveryState { return !model.isBusy }
            return false
        }
        let candidate = try XCTUnwrap(model.nativeRepairCandidates.first)
        XCTAssertNil(candidate.nativeRepairSelection)

        model.chooseNativeRepairOpenCodexCandidate(id: candidate.id)
        model.repairNativeRouting()
        try await Task.sleep(nanoseconds: 50_000_000)

        XCTAssertFalse(model.canRunOwnedNativeRepair)
        XCTAssertNil(model.nativeRepairOpenCodexCandidate)
        let ownerInspectionCalls = await nativeClient.ownerInspectionCalls()
        let repairCalls = await nativeClient.repairCalls()
        XCTAssertTrue(ownerInspectionCalls.isEmpty)
        XCTAssertTrue(repairCalls.isEmpty)
        XCTAssertEqual(desktop.quitRequests, 0)
    }

    func testOwnedOpenCodexNativeRepairDiscoversExactCandidateAndKeepsProgressVisible() async throws {
        let appURL = try makeAppBundle()
        let executableFixture = try makeOpenCodexExecutableFixture()
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: executableFixture.directory)
        }
        let inspection = try nativeRepairInspection(kind: .openCodex)
        let receipt = try nativeRepairReceipt(status: nativeStatus(generation: 4))
        let nativeClient = NativeRepairClient(inspection: inspection, outcome: .receipt(receipt))
        let discovery = DiscoveryClient(responses: [
            try automaticDiscoveryResult(tier: .a, handoffExecutable: executableFixture.executable),
        ])
        let routingClient = RelayctlClient(responses: [
            orphanRecoveryStatus(generation: 3),
            nativeStatus(generation: 4),
        ])
        let desktop = DesktopController(runningValues: [true, true, true, false, false, false, false])
        let activityLog = RelayActivityLogStore(subsystem: "test.relay.activity")
        let model = MenuBarModel(
            client: routingClient,
            discoveryClient: discovery,
            nativeRepairClient: nativeClient,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            distributionFlavor: .localDevelopment,
            localization: englishLocalization(),
            activityLog: activityLog
        )

        await model.refreshStatusNow()
        model.inspectNativeRepair()
        try await waitUntil {
            if case .candidates = model.nativeRepairDiscoveryState { return !model.isBusy }
            return false
        }
        guard let candidate = model.nativeRepairCandidates.first else {
            return XCTFail("expected a Tier A candidate")
        }
        let expectedSelection = try XCTUnwrap(candidate.nativeRepairSelection)
        model.chooseNativeRepairOpenCodexCandidate(id: candidate.id)
        try await waitUntil {
            model.nativeRepairOwnerInspection?.configuration == .valid &&
                model.nativeRepairOwnerInspection?.integration == .enabled &&
                !model.isBusy
        }
        XCTAssertTrue(model.canRunOwnedNativeRepair)

        model.repairNativeRouting()
        try await waitUntil(timeout: 3) {
            model.nativeRepairProgress?.result?.code == "native_owner_repair_completed" && !model.isBusy
        }

        XCTAssertEqual(desktop.quitRequests, 1)
        XCTAssertEqual(desktop.relaunches, 1)
        XCTAssertEqual(model.status?.generation, 4)
        XCTAssertEqual(model.status?.phase, .nativeActive)
        let repairCalls = await nativeClient.repairCalls()
        XCTAssertEqual(
            repairCalls,
            [.init(generation: 3, owner: .openCodex, selection: expectedSelection)]
        )
        for step in NativeRepairFlowStep.allCases {
            XCTAssertEqual(model.nativeRepairProgress?.state(for: step), .completed)
        }
        XCTAssertTrue(activityLog.events.contains {
            $0.category == .repair &&
                $0.code == "native_routing_repair_finished" &&
                $0.fields["owner"] == "opencodex" &&
                $0.fields["openai_base_url"] == "true" &&
                $0.fields["model_catalog_json"] == "true" &&
                !$0.fields.values.contains(where: { $0.contains("/") })
        })
    }

    func testInvalidOpenCodexOwnerPreflightBlocksBeforeDesktopExit() async throws {
        let appURL = try makeAppBundle()
        let executableFixture = try makeOpenCodexExecutableFixture()
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: executableFixture.directory)
        }
        let inspection = try nativeRepairInspection(kind: .openCodex)
        let nativeClient = NativeRepairClient(
            inspection: inspection,
            outcome: .failure(.reported(.nativeOwnerConfigurationInvalid)),
            ownerConfiguration: .invalid,
            ownerIntegration: .unknown,
            ownerReason: .configurationInvalid
        )
        let discovery = DiscoveryClient(responses: [
            try automaticDiscoveryResult(tier: .a, handoffExecutable: executableFixture.executable),
        ])
        let routingClient = RelayctlClient(response: orphanRecoveryStatus(generation: 3))
        let desktop = DesktopController(runningValues: [true, true, true])
        let model = MenuBarModel(
            client: routingClient,
            discoveryClient: discovery,
            nativeRepairClient: nativeClient,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            distributionFlavor: .localDevelopment,
            localization: englishLocalization()
        )

        await model.refreshStatusNow()
        model.inspectNativeRepair()
        try await waitUntil {
            if case .candidates = model.nativeRepairDiscoveryState { return !model.isBusy }
            return false
        }
        guard let candidate = model.nativeRepairCandidates.first else {
            return XCTFail("expected OpenCodex candidate")
        }
        model.chooseNativeRepairOpenCodexCandidate(id: candidate.id)
        try await waitUntil { model.nativeRepairOwnerInspection?.configuration == .invalid && !model.isBusy }

        XCTAssertFalse(model.canRunOwnedNativeRepair)
        model.repairNativeRouting()
        try await Task.sleep(nanoseconds: 50_000_000)
        XCTAssertEqual(desktop.quitRequests, 0)
        XCTAssertEqual(desktop.relaunches, 0)
        let repairCalls = await nativeClient.repairCalls()
        XCTAssertTrue(repairCalls.isEmpty)
    }

    func testOwnerBusyRelaunchesDesktopPreservesFailureAndReinspects() async throws {
        let appURL = try makeAppBundle()
        let executableFixture = try makeOpenCodexExecutableFixture()
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: executableFixture.directory)
        }
        let inspection = try nativeRepairInspection(kind: .openCodex)
        let nativeClient = NativeRepairClient(
            inspection: inspection,
            outcome: .failure(.reported(.nativeOwnerBusy))
        )
        let discovery = DiscoveryClient(responses: [
            try automaticDiscoveryResult(tier: .a, handoffExecutable: executableFixture.executable),
        ])
        let routingClient = RelayctlClient(responses: [
            orphanRecoveryStatus(generation: 3),
            orphanRecoveryStatus(generation: 3),
        ])
        let desktop = DesktopController(runningValues: [true, true, true, false, false, false, false])
        let activityLog = RelayActivityLogStore(subsystem: "test.relay.owner-busy")
        let model = MenuBarModel(
            client: routingClient,
            discoveryClient: discovery,
            nativeRepairClient: nativeClient,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            distributionFlavor: .localDevelopment,
            localization: englishLocalization(),
            activityLog: activityLog
        )

        await model.refreshStatusNow()
        model.inspectNativeRepair()
        try await waitUntil {
            if case .candidates = model.nativeRepairDiscoveryState { return !model.isBusy }
            return false
        }
        guard let candidate = model.nativeRepairCandidates.first else {
            return XCTFail("expected OpenCodex candidate")
        }
        model.chooseNativeRepairOpenCodexCandidate(id: candidate.id)
        try await waitUntil { model.canRunOwnedNativeRepair && !model.isBusy }
        model.repairNativeRouting()
        try await waitUntil(timeout: 3) { model.message?.code == "native_owner_busy" && !model.isBusy }

        XCTAssertEqual(desktop.quitRequests, 1)
        XCTAssertEqual(desktop.relaunches, 1)
        XCTAssertEqual(model.nativeRepairProgress?.failedStep, .ownerRepair)
        XCTAssertEqual(model.nativeRepairProgress?.result?.code, "native_owner_busy")
        XCTAssertEqual(model.nativeRepairOwnerInspection?.integration, .enabled)
        XCTAssertTrue(activityLog.events.contains {
            $0.code == "desktop_relaunch_after_failure" && $0.fields["result_code"] == "completed"
        })
        XCTAssertTrue(activityLog.events.contains {
            $0.code == "native_routing_repair_finished" &&
                $0.fields["failure_code"] == "native_owner_busy" &&
                $0.fields["retry_exhausted"] == "true"
        })
    }

    func testNativeStateRepairPendingReinspectsAndEnablesStateOnlyRepair() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let owned = try nativeRepairInspection(kind: .localRelay)
        let stateOnly = try nativeRepairInspection(kind: .stateOnly)
        let nativeClient = NativeRepairClient(
            inspections: [owned, stateOnly],
            outcome: .failure(.reported(.nativeStateRepairPending))
        )
        let routingClient = RelayctlClient(responses: [
            orphanRecoveryStatus(generation: 3),
            orphanRecoveryStatus(generation: 3),
        ])
        let desktop = DesktopController(runningValues: [true, true, true, false, false, false])
        let model = MenuBarModel(
            client: routingClient,
            nativeRepairClient: nativeClient,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            distributionFlavor: .localDevelopment,
            localization: englishLocalization()
        )

        await model.refreshStatusNow()
        model.inspectNativeRepair()
        try await waitUntil { model.nativeRepairInspection?.kind == .localRelay && !model.isBusy }
        XCTAssertTrue(model.canRunOwnedNativeRepair)
        model.repairNativeRouting()
        try await waitUntil(timeout: 3) {
            model.message?.code == "native_state_repair_pending" && !model.isBusy
        }

        XCTAssertEqual(model.status?.phase, .recoveryRequired)
        XCTAssertEqual(model.nativeRepairInspection?.kind, .stateOnly)
        XCTAssertTrue(model.canRepairNative)
        XCTAssertFalse(model.canRunOwnedNativeRepair)
        XCTAssertEqual(desktop.relaunches, 1)
        let inspectCalls = await nativeClient.inspectCalls()
        XCTAssertEqual(inspectCalls, [3, 3])
    }

    func testUnavailableNativeRepairDiagnosisNeverQuitsDesktopOrCallsRepair() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let inspection = try nativeRepairInspection(kind: .unavailable)
        let nativeClient = NativeRepairClient(
            inspection: inspection,
            outcome: .failure(.reported(.nativeRepairUnavailable))
        )
        let routingClient = RelayctlClient(response: orphanRecoveryStatus(generation: 3))
        let desktop = DesktopController(runningValues: [true, true, true])
        let model = MenuBarModel(
            client: routingClient,
            nativeRepairClient: nativeClient,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            distributionFlavor: .localDevelopment,
            localization: englishLocalization()
        )

        await model.refreshStatusNow()
        model.inspectNativeRepair()
        try await waitUntil { model.nativeRepairInspection?.kind == .unavailable && !model.isBusy }
        XCTAssertFalse(model.canRunOwnedNativeRepair)
        model.repairNativeRouting()
        try await Task.sleep(nanoseconds: 50_000_000)

        XCTAssertEqual(desktop.quitRequests, 0)
        let repairCalls = await nativeClient.repairCalls()
        XCTAssertTrue(repairCalls.isEmpty)
    }

    func testShimHandoffStaysVisibleAndEnablesRemovalAfterVerifiedRefresh() async throws {
        let appURL = try makeAppBundle()
        let executableFixture = try makeOpenCodexExecutableFixture()
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: executableFixture.directory)
        }
        let candidateResult = try automaticDiscoveryResult(
            tier: .a,
            handoffExecutable: executableFixture.executable
        )
        let installationID = try XCTUnwrap(candidateResult.candidates.first?.id)
        let refreshedCandidateResult = try automaticDiscoveryResult(
            tier: .a,
            handoffExecutable: executableFixture.executable,
            installationID: "fedcba9876543210fedcba98",
            fingerprint: String(repeating: "3", count: 64)
        )
        let inventory = OpenCodexDataInventoryReceipt(
            status: .verified,
            installationID: installationID,
            items: []
        )
        let removalClient = RemovalClient(inventory: inventory)
        let client = RelayctlClient(responses: [
            externalStatus(generation: 1, routingSync: .invalid),
            externalStatus(generation: 2),
            externalStatus(generation: 3),
        ])
        let desktop = DesktopController(runningValues: [])
        let activityLog = RelayActivityLogStore(subsystem: "test.relay.activity")
        let model = MenuBarModel(
            client: client,
            discoveryClient: DiscoveryClient(responses: [candidateResult, refreshedCandidateResult]),
            removalClient: removalClient,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization(),
            activityLog: activityLog
        )

        await model.refreshStatusNow()
        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates = model.openCodexDiscoveryState { return true }
            return false
        }
        model.chooseDiscoveredOpenCodexCandidate(id: installationID)
        model.chooseOpenCodexHandoffAction(.retainProxyRemoveShim)

        try await waitUntil(timeout: 2) {
            model.openCodexRemovalFlow?.handoffProgress?.phase == .completed && !model.isBusy
        }
        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .actions)
        XCTAssertEqual(
            model.openCodexRemovalFlow?.handoffProgress?.result?.code,
            "opencodex_handoff_completed"
        )
        XCTAssertTrue(model.status?.canUninstallOpenCodex == true)
        XCTAssertEqual(model.openCodexRemovalFlow?.id, installationID)
        XCTAssertEqual(
            model.openCodexRemovalFlow?.selection.installationID,
            "fedcba9876543210fedcba98"
        )
        XCTAssertEqual(
            model.openCodexRemovalFlow?.selection.installationFingerprint,
            String(repeating: "3", count: 64)
        )
        XCTAssertFalse(model.openCodexRemovalFlow?.candidateRevalidationRequired ?? true)
        XCTAssertEqual(desktop.relaunches, 1)
        let successCommands = await client.commands()
        XCTAssertEqual(successCommands, [
            .status,
            .handoff(executableFixture.executable, .retainProxyRemoveShim),
            .status,
        ])
        XCTAssertTrue(activityLog.events.contains { $0.code == "handoff_started" })
        XCTAssertTrue(activityLog.events.contains {
            $0.code == "handoff_finished" &&
                $0.fields["result_code"] == "opencodex_handoff_completed"
        })
        let handoffPhases = activityLog.events.compactMap { event in
            event.code == "removal_phase_changed" ? event.fields["handoff_phase"] : nil
        }
        XCTAssertTrue(handoffPhases.contains("preflight"))
        XCTAssertTrue(handoffPhases.contains("desktop_exit"))
        XCTAssertTrue(handoffPhases.contains("opencodex_operation"))
        XCTAssertTrue(handoffPhases.contains("desktop_relaunch"))
        XCTAssertTrue(handoffPhases.contains("status_refresh"))
        XCTAssertTrue(handoffPhases.contains("completed"))

        model.beginOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .options }
        let inspectCount = await removalClient.inspectCount()
        XCTAssertEqual(inspectCount, 0)
    }

    func testSuccessfulHandoffWithChangedCandidateKeepsRemovalLocked() async throws {
        let appURL = try makeAppBundle()
        let executableFixture = try makeOpenCodexExecutableFixture()
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: executableFixture.directory)
        }
        let candidateResult = try automaticDiscoveryResult(
            tier: .a,
            handoffExecutable: executableFixture.executable
        )
        let noCandidates = try discoveryResult(tier: .a, includesCandidate: false)
        let installationID = try XCTUnwrap(candidateResult.candidates.first?.id)
        let client = RelayctlClient(responses: [
            externalStatus(generation: 1),
            externalStatus(generation: 2),
            externalStatus(generation: 3),
        ])
        let activityLog = RelayActivityLogStore(subsystem: "test.relay.activity")
        let model = MenuBarModel(
            client: client,
            discoveryClient: DiscoveryClient(responses: [candidateResult, noCandidates]),
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization(),
            activityLog: activityLog
        )

        await model.refreshStatusNow()
        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates = model.openCodexDiscoveryState { return true }
            return false
        }
        model.chooseDiscoveredOpenCodexCandidate(id: installationID)
        model.chooseOpenCodexHandoffAction(.retainProxyRemoveShim)
        try await waitUntil(timeout: 2) {
            model.openCodexRemovalFlow?.handoffProgress?.phase == .completed && !model.isBusy
        }

        XCTAssertEqual(
            model.openCodexRemovalFlow?.handoffProgress?.result?.code,
            "opencodex_handoff_candidate_refresh_required"
        )
        XCTAssertTrue(model.openCodexRemovalFlow?.candidateRevalidationRequired == true)
        XCTAssertFalse(model.openCodexRemovalFlow?.automaticRemovalEligible ?? true)
        XCTAssertTrue(model.status?.canUninstallOpenCodex == true)
        XCTAssertTrue(activityLog.events.contains {
            $0.code == "handoff_finished" &&
                $0.fields["result_code"] == "opencodex_handoff_candidate_refresh_required"
        })
    }

    func testFailedShimHandoffRelaunchesDesktopAndReturnsCurrentRecoveryState() async throws {
        let appURL = try makeAppBundle()
        let executableFixture = try makeOpenCodexExecutableFixture()
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: executableFixture.directory)
        }
        let candidateResult = try automaticDiscoveryResult(
            tier: .a,
            handoffExecutable: executableFixture.executable
        )
        let installationID = try XCTUnwrap(candidateResult.candidates.first?.id)
        let client = RelayctlClient(outcomes: [
            .response(externalStatus(generation: 6)),
            .failure(.reported(.operationFailed)),
            .response(recoveryStatus(generation: 7)),
        ])
        let desktop = DesktopController(runningValues: [])
        let model = MenuBarModel(
            client: client,
            discoveryClient: DiscoveryClient(responses: [candidateResult]),
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        await model.refreshStatusNow()
        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates = model.openCodexDiscoveryState { return true }
            return false
        }
        model.chooseDiscoveredOpenCodexCandidate(id: installationID)
        model.chooseOpenCodexHandoffAction(.retainProxyRemoveShim)

        try await waitUntil(timeout: 2) {
            model.openCodexRemovalFlow?.handoffProgress?.phase == .failed && !model.isBusy
        }
        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .actions)
        XCTAssertEqual(model.openCodexRemovalFlow?.handoffProgress?.failedPhase, .openCodexOperation)
        XCTAssertEqual(model.openCodexRemovalFlow?.handoffProgress?.result?.code, "operation_failed")
        XCTAssertEqual(model.status?.phase, .recoveryRequired)
        XCTAssertFalse(model.status?.canUninstallOpenCodex ?? true)
        XCTAssertEqual(desktop.relaunches, 1)
        let failureCommands = await client.commands()
        XCTAssertEqual(failureCommands, [
            .status,
            .handoff(executableFixture.executable, .retainProxyRemoveShim),
            .status,
        ])
    }

    func testUnverifiedHandoffRefreshDoesNotLeaveOldRemovalStatusActionable() async throws {
        let appURL = try makeAppBundle()
        let executableFixture = try makeOpenCodexExecutableFixture()
        defer {
            try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: executableFixture.directory)
        }
        let candidateResult = try automaticDiscoveryResult(
            tier: .a,
            handoffExecutable: executableFixture.executable
        )
        let installationID = try XCTUnwrap(candidateResult.candidates.first?.id)
        let client = RelayctlClient(outcomes: [
            .response(externalStatus()),
            .failure(.reported(.operationFailed)),
            .failure(.reported(.operationFailed)),
        ])
        let model = MenuBarModel(
            client: client,
            discoveryClient: DiscoveryClient(responses: [candidateResult]),
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        await model.refreshStatusNow()
        XCTAssertTrue(model.status?.canUninstallOpenCodex == true)

        // The pre-handoff stable snapshot must disappear as soon as the
        // mutating helper and its mandatory refresh are both unverified.
        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates = model.openCodexDiscoveryState { return true }
            return false
        }
        model.chooseDiscoveredOpenCodexCandidate(id: installationID)
        model.chooseOpenCodexHandoffAction(.retainProxyRemoveShim)
        try await waitUntil(timeout: 2) {
            model.openCodexRemovalFlow?.handoffProgress?.phase == .failed && !model.isBusy
        }

        XCTAssertNil(model.status)
        XCTAssertFalse(model.status?.canUninstallOpenCodex ?? false)
    }

    func testHomebrewGuardAvailabilityPublishesRefreshChanges() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let candidateResult = try guardedHomebrewDiscoveryResult(tier: .a)
        let candidate = try XCTUnwrap(candidateResult.candidates.first)
        let guardClient = HomebrewGuardClient(
            availability: HomebrewGuardAvailability(
                registration: .ready,
                helperVersion: "dev",
                protocolVersion: homebrewGuardProtocolVersion,
                errorCode: nil,
                operationID: nil
            )
        )
        let model = MenuBarModel(
            client: RelayctlClient(response: externalStatus()),
            discoveryClient: DiscoveryClient(responses: [candidateResult]),
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            homebrewGuard: guardClient,
            startsPolling: false,
            localization: englishLocalization()
        )

        await model.refreshStatusNow()
        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates = model.openCodexDiscoveryState { return true }
            return false
        }
        model.chooseDiscoveredOpenCodexCandidate(id: candidate.id)
        await model.refreshHomebrewGuardAvailability()
        XCTAssertTrue(model.canBeginOpenCodexRemoval)

        var observedNotRegistered = false
        let cancellable = model.$homebrewGuardAvailability
            .dropFirst()
            .sink { availability in
                observedNotRegistered = availability.registration == .notRegistered
            }
        guardClient.availabilityValue = HomebrewGuardAvailability(
            registration: .notRegistered,
            helperVersion: "dev",
            protocolVersion: homebrewGuardProtocolVersion,
            errorCode: .homebrewGuardNotRegistered,
            operationID: nil
        )

        await model.refreshHomebrewGuardAvailability()

        XCTAssertTrue(observedNotRegistered)
        XCTAssertEqual(model.homebrewGuardAvailability.registration, .notRegistered)
        XCTAssertTrue(model.canBeginOpenCodexRemoval)
        cancellable.cancel()
    }

    func testHomebrewGuardSetupIsAvailableWithoutRemovalCandidateAndOpensApprovalSettings() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let notRegistered = HomebrewGuardAvailability(
            registration: .notRegistered,
            helperVersion: "dev",
            protocolVersion: homebrewGuardProtocolVersion,
            errorCode: .homebrewGuardNotRegistered,
            operationID: nil
        )
        let guardClient = HomebrewGuardClient(availability: notRegistered)
        guardClient.availabilityAfterRegister = HomebrewGuardAvailability(
            registration: .approvalRequired,
            helperVersion: "dev",
            protocolVersion: homebrewGuardProtocolVersion,
            errorCode: .approvalRequired,
            operationID: nil
        )
        let activityLog = RelayActivityLogStore(subsystem: "test.relay.helper-setup")
        let model = MenuBarModel(
            client: RelayctlClient(response: externalStatus()),
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            homebrewGuard: guardClient,
            startsPolling: false,
            distributionFlavor: .localDevelopment,
            localization: englishLocalization(),
            activityLog: activityLog
        )

        try await waitUntil {
            model.homebrewGuardAvailability.registration == .notRegistered
        }
        XCTAssertNil(model.openCodexRemovalFlow)
        XCTAssertTrue(model.canRegisterHomebrewGuard)

        model.registerHomebrewGuard()
        try await waitUntil {
            model.homebrewGuardAvailability.registration == .approvalRequired && !model.isBusy
        }
        XCTAssertTrue(model.canOpenHomebrewGuardSystemSettings)

        model.openHomebrewGuardSystemSettings()

        XCTAssertTrue(guardClient.operations.contains("register"))
        XCTAssertTrue(guardClient.operations.contains("open_settings"))
        XCTAssertEqual(
            activityLog.events.last(where: { $0.code == "homebrew_guard_settings_opened" })?.fields,
            [
                "distribution": DistributionFlavor.localDevelopment.rawValue,
                "phase": "approval",
                "result_code": "opened",
            ]
        )
    }

    func testAdHocDevelopmentUsesReviewedManualInstallerCommand() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let guardClient = HomebrewGuardClient(
            availability: HomebrewGuardAvailability(
                registration: .manualInstallRequired,
                helperVersion: "1.2.3-dev",
                protocolVersion: homebrewGuardProtocolVersion,
                errorCode: .homebrewGuardNotRegistered,
                operationID: nil
            )
        )
        guardClient.backendValue = .manualAdmin
        let activityLog = RelayActivityLogStore(subsystem: "test.relay.manual-helper-setup")
        let model = MenuBarModel(
            client: RelayctlClient(response: externalStatus()),
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            homebrewGuard: guardClient,
            startsPolling: false,
            distributionFlavor: .localDevelopment,
            localization: englishLocalization(),
            activityLog: activityLog
        )

        try await waitUntil {
            model.homebrewGuardAvailability.registration == .manualInstallRequired
        }
        XCTAssertFalse(model.canRegisterHomebrewGuard)
        XCTAssertTrue(model.canShowDevelopmentHomebrewGuardSetup)

        let command = try XCTUnwrap(model.developmentHomebrewGuardSetupCommand())
        model.copyDevelopmentHomebrewGuardSetupCommand(command)

        XCTAssertTrue(guardClient.operations.contains("setup_install"))
        XCTAssertFalse(guardClient.operations.contains("register"))
        XCTAssertEqual(
            activityLog.events.last(where: { $0.code == "homebrew_guard_setup_command_copied" })?.fields,
            [
                "backend": HomebrewGuardBackend.manualAdmin.rawValue,
                "distribution": DistributionFlavor.localDevelopment.rawValue,
                "result_code": "copied",
            ]
        )
        XCTAssertFalse(
            activityLog.jsonLines().contains("/Applications/Test.app")
        )
    }

    func testDevelopmentDaemonFailureUsesUpdateInsteadOfRegistration() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let guardClient = HomebrewGuardClient(
            availability: HomebrewGuardAvailability(
                registration: .daemonLaunchFailed,
                helperVersion: "1.2.3-dev",
                protocolVersion: homebrewGuardProtocolVersion,
                errorCode: .homebrewGuardNotRegistered,
                operationID: nil
            )
        )
        guardClient.backendValue = .manualAdmin
        let model = MenuBarModel(
            client: RelayctlClient(response: externalStatus()),
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            homebrewGuard: guardClient,
            startsPolling: false,
            distributionFlavor: .localDevelopment,
            localization: englishLocalization()
        )

        try await waitUntil {
            model.homebrewGuardAvailability.registration == .daemonLaunchFailed
        }
        let command = try XCTUnwrap(model.developmentHomebrewGuardSetupCommand())

        XCTAssertTrue(command.hasSuffix(" update"))
        XCTAssertTrue(guardClient.operations.contains("setup_update"))
        XCTAssertFalse(guardClient.operations.contains("register"))
    }

    func testInterruptedDevelopmentInstallerUsesExplicitRecoverNotGuardRecovery() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let guardClient = HomebrewGuardClient(
            availability: HomebrewGuardAvailability(
                registration: .manualInstallerRecoveryRequired,
                helperVersion: "1.2.3-dev",
                protocolVersion: homebrewGuardProtocolVersion,
                errorCode: .recoveryRequired,
                operationID: nil
            )
        )
        guardClient.backendValue = .manualAdmin
        let activityLog = RelayActivityLogStore(subsystem: "test.relay.manual-helper-recovery")
        let model = MenuBarModel(
            client: RelayctlClient(response: externalStatus()),
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            homebrewGuard: guardClient,
            startsPolling: false,
            distributionFlavor: .localDevelopment,
            localization: englishLocalization(),
            activityLog: activityLog
        )

        try await waitUntil {
            model.homebrewGuardAvailability.registration == .manualInstallerRecoveryRequired
        }
        XCTAssertTrue(model.canShowDevelopmentHomebrewGuardSetup)
        XCTAssertFalse(model.canRecoverHomebrewGuard)

        let command = try XCTUnwrap(model.developmentHomebrewGuardSetupCommand())

        XCTAssertTrue(command.hasSuffix(" recover"))
        XCTAssertTrue(guardClient.operations.contains("setup_recover"))
        XCTAssertFalse(guardClient.operations.contains("recover"))
        XCTAssertEqual(
            activityLog.events.last(where: { $0.code == "homebrew_guard_setup_presented" })?
                .fields["phase"],
            "recover"
        )

        guardClient.availabilityValue = HomebrewGuardAvailability(
            registration: .ready,
            helperVersion: "1.2.3-dev",
            protocolVersion: homebrewGuardProtocolVersion,
            errorCode: nil,
            operationID: nil
        )
        await model.refreshHomebrewGuardAvailability()

        let availabilityEvent = try XCTUnwrap(
            activityLog.events.last(where: { $0.code == "homebrew_guard_availability_changed" })
        )
        XCTAssertEqual(availabilityEvent.fields["backend"], "manual_admin")
        XCTAssertEqual(availabilityEvent.fields["phase"], "ready")
        XCTAssertEqual(availabilityEvent.fields["result_code"], "ready")
        XCTAssertNil(availabilityEvent.fields["command"])
        XCTAssertNil(availabilityEvent.fields["path"])
        XCTAssertNil(availabilityEvent.fields["hash"])
    }

    func testGuardedRemovalRechecksHelperBeforeDesktopExit() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let candidateResult = try guardedHomebrewDiscoveryResult(tier: .a)
        let candidate = try XCTUnwrap(candidateResult.candidates.first)
        let inventory = OpenCodexDataInventoryReceipt(
            status: .verified,
            installationID: candidate.id,
            items: []
        )
        let eventRecorder = RemovalEventRecorder()
        let guardClient = HomebrewGuardClient(
            availability: HomebrewGuardAvailability(
                registration: .ready,
                helperVersion: "dev",
                protocolVersion: homebrewGuardProtocolVersion,
                errorCode: nil,
                operationID: nil
            ),
            eventRecorder: eventRecorder
        )
        let removal = RemovalClient(inventory: inventory, eventRecorder: eventRecorder)
        let desktop = DesktopController(runningValues: [true, false], eventRecorder: eventRecorder)
        let model = MenuBarModel(
            client: RelayctlClient(response: externalStatus()),
            discoveryClient: DiscoveryClient(responses: [candidateResult]),
            removalClient: removal,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            homebrewGuard: guardClient,
            startsPolling: false,
            localization: englishLocalization()
        )

        await model.refreshStatusNow()
        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates = model.openCodexDiscoveryState { return true }
            return false
        }
        model.chooseDiscoveredOpenCodexCandidate(id: candidate.id)
        await model.refreshHomebrewGuardAvailability()
        XCTAssertTrue(model.canBeginOpenCodexRemoval)
        model.beginOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .options }
        model.reviewOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .confirmRemoval }

        guardClient.availabilityValue = HomebrewGuardAvailability(
            registration: .notRegistered,
            helperVersion: "dev",
            protocolVersion: homebrewGuardProtocolVersion,
            errorCode: .homebrewGuardNotRegistered,
            operationID: nil
        )
        eventRecorder.reset()
        model.confirmOpenCodexPackageRemoval()
        try await waitUntil {
            model.message?.code == "homebrew_guard_not_registered" && !model.isBusy
        }

        XCTAssertEqual(desktop.quitRequests, 0)
        let blockedRequests = await removal.recordedRequests()
        XCTAssertTrue(blockedRequests.isEmpty)
        XCTAssertFalse(eventRecorder.events.contains("guard_prepare"))
        XCTAssertFalse(eventRecorder.events.contains("remove"))
    }

    func testGuardedRemovalOrdersProtectionAroundExistingRemoval() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let candidateResult = try guardedHomebrewDiscoveryResult(tier: .a)
        let candidate = try XCTUnwrap(candidateResult.candidates.first)
        let inventory = OpenCodexDataInventoryReceipt(
            status: .verified,
            installationID: candidate.id,
            items: []
        )
        let receipt = OpenCodexRemovalReceipt(
            status: .completed,
            mode: .preserveData,
            installationID: candidate.id,
            dataScope: "preserved",
            selectedDataItems: 0,
            movedDataItems: 0,
            packageRemoved: true,
            dataMovementUnknown: false,
            routingRecoveryRequired: false,
            permanentDeleteFallback: false,
            stages: [
                OpenCodexRemovalStage(
                    stage: .cleanupJournal,
                    status: .completed,
                    code: "cleanup_resume",
                    subjectID: candidate.id
                ),
                OpenCodexRemovalStage(
                    stage: .routingFinalVerification,
                    status: .completed,
                    code: "routing_ownership_reverified"
                ),
                OpenCodexRemovalStage(
                    stage: .packageVerification,
                    status: .completed,
                    code: "package_absent",
                    subjectID: candidate.id
                ),
                OpenCodexRemovalStage(
                    stage: .relayCleanup,
                    status: .completed,
                    code: "relay_cleanup_completed"
                ),
            ]
        )
        let eventRecorder = RemovalEventRecorder()
        let guardClient = HomebrewGuardClient(
            availability: HomebrewGuardAvailability(
                registration: .ready,
                helperVersion: "dev",
                protocolVersion: homebrewGuardProtocolVersion,
                errorCode: nil,
                operationID: nil
            ),
            eventRecorder: eventRecorder
        )
        let removal = RemovalClient(
            inventory: inventory,
            removal: receipt,
            eventRecorder: eventRecorder
        )
        let desktop = DesktopController(runningValues: [], eventRecorder: eventRecorder)
        let model = MenuBarModel(
            client: RelayctlClient(response: externalStatus()),
            discoveryClient: DiscoveryClient(responses: [candidateResult, candidateResult]),
            removalClient: removal,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            homebrewGuard: guardClient,
            startsPolling: false,
            localization: englishLocalization()
        )

        await model.refreshStatusNow()
        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates = model.openCodexDiscoveryState { return true }
            return false
        }
        model.chooseDiscoveredOpenCodexCandidate(id: candidate.id)
        await model.refreshHomebrewGuardAvailability()
        XCTAssertTrue(model.canBeginOpenCodexRemoval)
        model.beginOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .options }
        model.reviewOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .confirmRemoval }

        desktop.runningValues = [true, false]
        eventRecorder.reset()
        model.confirmOpenCodexPackageRemoval()
        try await waitUntil(timeout: 2) {
            model.openCodexRemovalFlow?.removalProgress?.phase == .completed && !model.isBusy
        }

        let significantEvents = eventRecorder.events.filter {
            [
                "desktop_quit",
                "guard_prepare",
                "guard_commit",
                "remove",
                "guard_release",
                "desktop_relaunch",
            ].contains($0)
        }
        XCTAssertEqual(significantEvents, [
            "desktop_quit",
            "guard_prepare",
            "guard_commit",
            "remove",
            "guard_release",
            "desktop_relaunch",
        ])
        let completedRequests = await removal.recordedRequests()
        XCTAssertEqual(completedRequests.count, 1)
        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .result)
        XCTAssertEqual(desktop.quitRequests, 1)
        XCTAssertEqual(desktop.relaunches, 1)
    }

    func testVerifiedPreMutationRemovalFailureClearsSpeculativeRecoveryAndRelaunches() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let candidateResult = try automaticDiscoveryResult(tier: .a)
        let candidate = try XCTUnwrap(candidateResult.candidates.first)
        let receipt = OpenCodexRemovalReceipt(
            status: .failed,
            mode: .preserveData,
            installationID: candidate.id,
            dataScope: "preserved",
            selectedDataItems: 0,
            movedDataItems: 0,
            packageRemoved: false,
            dataMovementUnknown: false,
            routingRecoveryRequired: false,
            permanentDeleteFallback: false,
            stages: [
                OpenCodexRemovalStage(
                    stage: .candidateRevalidation,
                    status: .completed,
                    code: "candidate_verified",
                    subjectID: candidate.id
                ),
                OpenCodexRemovalStage(
                    stage: .teardownPreflight,
                    status: .failed,
                    code: "teardown_preflight_failed",
                    subjectID: candidate.id
                ),
            ]
        )
        let events = RemovalEventRecorder()
        let recoveryStore = RecoveryStore()
        let relay = RelayctlClient(response: externalStatus())
        let removal = RemovalClient(
            inventory: OpenCodexDataInventoryReceipt(
                status: .verified,
                installationID: candidate.id,
                items: []
            ),
            removal: receipt,
            eventRecorder: events
        )
        let desktop = DesktopController(runningValues: [true, false], eventRecorder: events)
        let model = MenuBarModel(
            client: relay,
            discoveryClient: DiscoveryClient(responses: [candidateResult]),
            removalClient: removal,
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        await model.refreshStatusNow()
        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates = model.openCodexDiscoveryState { return true }
            return false
        }
        model.chooseDiscoveredOpenCodexCandidate(id: candidate.id)
        model.beginOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .options }
        model.reviewOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .confirmRemoval }
        desktop.runningValues = [true, false]
        events.reset()
        model.confirmOpenCodexPackageRemoval()
        try await waitUntil(timeout: 2) {
            model.openCodexRemovalFlow?.removalProgress?.phase == .failed && !model.isBusy
        }

        XCTAssertNil(recoveryStore.session)
        XCTAssertEqual(recoveryStore.clearCount, 1)
        XCTAssertFalse(model.hasPendingOpenCodexRemovalRecovery)
        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .result)
        XCTAssertEqual(model.openCodexRemovalFlow?.failure?.code, "teardown_preflight_failed")
        XCTAssertEqual(model.openCodexRemovalFlow?.removalProgress?.result?.code, "teardown_preflight_failed")
        XCTAssertEqual(model.message?.code, "teardown_preflight_failed")
        XCTAssertEqual(desktop.quitRequests, 1)
        XCTAssertEqual(desktop.relaunches, 1)
        XCTAssertEqual(events.events.filter {
            ["desktop_quit", "remove", "desktop_relaunch"].contains($0)
        }, ["desktop_quit", "remove", "desktop_relaunch"])
        let commands = await relay.commands()
        XCTAssertTrue(commands.contains(.status))
    }

    func testSafeRemovalRequiresEligibleCandidateAndStableRoute() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let candidateResult = try automaticDiscoveryResult(tier: .a)
        let installationID = try XCTUnwrap(candidateResult.candidates.first?.id)
        let inventory = OpenCodexDataInventoryReceipt(
            status: .verified,
            installationID: installationID,
            items: []
        )

        let blockedRemoval = RemovalClient(inventory: inventory)
        let recoveryModel = MenuBarModel(
            client: RelayctlClient(response: recoveryStatus()),
            discoveryClient: DiscoveryClient(responses: [candidateResult]),
            removalClient: blockedRemoval,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )
        await recoveryModel.refreshStatusNow()
        recoveryModel.addLocalOpenCodexBackend()
        XCTAssertEqual(recoveryModel.openCodexDiscoveryState, .idle)
        XCTAssertNil(recoveryModel.openCodexRemovalFlow)
        XCTAssertEqual(recoveryModel.message?.code, "routing_recovery_required")
        let blockedInspectCount = await blockedRemoval.inspectCount()
        XCTAssertEqual(blockedInspectCount, 0)

        let stableRemoval = RemovalClient(inventory: inventory)
        let stableModel = MenuBarModel(
            client: RelayctlClient(response: externalStatus()),
            discoveryClient: DiscoveryClient(responses: [candidateResult]),
            removalClient: stableRemoval,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )
        await stableModel.refreshStatusNow()
        stableModel.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates = stableModel.openCodexDiscoveryState { return true }
            return false
        }
        stableModel.chooseDiscoveredOpenCodexCandidate(id: installationID)
        stableModel.beginOpenCodexRemoval()
        try await waitUntil { stableModel.openCodexRemovalFlow?.phase == .options }

        let stableInspectCount = await stableRemoval.inspectCount()
        XCTAssertEqual(stableInspectCount, 0)
        XCTAssertEqual(stableModel.openCodexRemovalFlow?.mode, .preserveData)
        XCTAssertEqual(stableModel.openCodexRemovalFlow?.selectedDataItemIDs, [])
    }

    func testSavedRebootRecoveryReviewsOnlyTheParkedDurableGeneration() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let selection = try OpenCodexRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64)
        )
        let candidateResult = try automaticDiscoveryResult(
            tier: .a,
            installationID: selection.installationID,
            fingerprint: selection.installationFingerprint
        )
        let recoveryStore = RecoveryStore()
        recoveryStore.session = try OpenCodexRemovalRecoverySession(
            selection: selection,
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .inFlight,
            lastCode: "removal_started"
        )
        let client = RelayctlClient(response: recoveryStatus(
            localRelay: .unreachable,
            routingSync: .unreachable,
            relayRunning: false,
            activeRequests: nil
        ))
        let model = MenuBarModel(
            client: client,
            discoveryClient: DiscoveryClient(responses: [candidateResult]),
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .rebootRequired)
        await model.refreshStatusNow()
        XCTAssertFalse(model.status?.canUninstallOpenCodex ?? true)
        XCTAssertTrue(model.status?.canReviewSavedOpenCodexRemovalRecovery ?? false)

        model.prepareRebootedOpenCodexRecovery()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .confirmRemoval }
        XCTAssertEqual(model.openCodexRemovalFlow?.expectedRoutingGeneration, 1)
    }

    func testSavedRoutingRecoveryRequiresMatchingGenerationAndHealthyRelay() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let selection = try OpenCodexRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64)
        )
        let candidateResult = try automaticDiscoveryResult(
            tier: .a,
            installationID: selection.installationID,
            fingerprint: selection.installationFingerprint
        )
        let recoveryStore = RecoveryStore()
        recoveryStore.session = try OpenCodexRemovalRecoverySession(
            selection: selection,
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .routingRecoveryRequired,
            lastCode: "routing_recovery_required",
            expectedRoutingGeneration: 7
        )
        let client = RelayctlClient(response: recoveryStatus(
            generation: 6,
            localRelay: .unreachable,
            routingSync: .unreachable,
            relayRunning: false,
            activeRequests: nil
        ))
        let desktop = DesktopController(runningValues: [])
        let trust = TrustValidator()
        let model = MenuBarModel(
            client: client,
            discoveryClient: DiscoveryClient(responses: [candidateResult]),
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: trust,
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .routingRecoveryRequired)
        XCTAssertEqual(model.openCodexRemovalFlow?.expectedRoutingGeneration, 7)
        await model.refreshStatusNow()
        XCTAssertFalse(model.status?.canUninstallOpenCodex ?? true)
        XCTAssertTrue(model.status?.canReviewSavedOpenCodexRemovalRecovery ?? false)
        XCTAssertTrue(model.status?.canCheckpointSavedOpenCodexRoutingRecovery ?? false)
        XCTAssertFalse(model.status?.canReviewSavedOpenCodexRoutingRecovery ?? true)

        model.checkOpenCodexRoutingRecovery()
        try await waitUntil { !model.isBusy }
        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .routingRecoveryRequired)
        XCTAssertEqual(model.openCodexRemovalFlow?.expectedRoutingGeneration, 7)
        XCTAssertEqual(recoveryStore.session?.expectedRoutingGeneration, 7)
        XCTAssertEqual(recoveryStore.session?.lastCode, "routing_recovery_required")
        XCTAssertEqual(model.message?.code, "routing_generation_changed")
        let commandsAfterMismatch = await client.commands()
        XCTAssertFalse(commandsAfterMismatch.contains(.recoverComplete))
        XCTAssertFalse(commandsAfterMismatch.contains(
            .recoverOpenCodexRemoval(
                selection: selection,
                expectedRoutingGeneration: 7
            )
        ))

        await client.setResponses([
            recoveryStatus(
                generation: 7,
                localRelay: .unreachable,
                routingSync: .unreachable,
                relayRunning: false,
                activeRequests: nil
            ),
        ])
        model.checkOpenCodexRoutingRecovery()
        try await waitUntil { !model.isBusy }
        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .routingRecoveryRequired)
        XCTAssertEqual(model.message?.code, "opencodex_removal_route_unsafe")
        let commandsAfterUnreachable = await client.commands()
        XCTAssertFalse(commandsAfterUnreachable.contains(.recoverComplete))
        XCTAssertFalse(commandsAfterUnreachable.contains(
            .recoverOpenCodexRemoval(
                selection: selection,
                expectedRoutingGeneration: 7
            )
        ))

        await client.setResponses([
            recoveryStatus(generation: 7),
            externalStatus(generation: 8),
        ])
        model.checkOpenCodexRoutingRecovery()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .confirmRemoval }
        XCTAssertEqual(model.openCodexRemovalFlow?.selection, selection)
        XCTAssertEqual(model.openCodexRemovalFlow?.expectedRoutingGeneration, 8)
        XCTAssertFalse(model.openCodexRemovalFlow?.confirmsRebootedProcessRecovery ?? true)
        let commandsAfterRecovery = await client.commands()
        XCTAssertTrue(commandsAfterRecovery.contains(
            .recoverOpenCodexRemoval(
                selection: selection,
                expectedRoutingGeneration: 7
            )
        ))
        XCTAssertFalse(commandsAfterRecovery.contains(.recoverComplete))
        XCTAssertFalse(desktop.runningTargets.isEmpty)
        XCTAssertTrue(desktop.runningTargets.allSatisfy { $0 == appURL })
        XCTAssertFalse(trust.verifiedURLs.isEmpty)
        XCTAssertTrue(trust.verifiedURLs.allSatisfy { $0 == appURL })
    }

    func testHigherGatedRoutingGenerationCheckpointsBeforeExactRecovery() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let selection = try OpenCodexRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64)
        )
        let candidateResult = try automaticDiscoveryResult(
            tier: .a,
            installationID: selection.installationID,
            fingerprint: selection.installationFingerprint
        )
        let recoveryStore = RecoveryStore()
        recoveryStore.session = try OpenCodexRemovalRecoverySession(
            selection: selection,
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .routingRecoveryRequired,
            lastCode: "routing_recovery_required",
            expectedRoutingGeneration: 7
        )
        let client = RelayctlClient(responses: [
            recoveryStatus(generation: 8),
            recoveryStatus(generation: 8),
            externalStatus(generation: 8),
        ])
        let model = MenuBarModel(
            client: client,
            discoveryClient: DiscoveryClient(responses: [candidateResult]),
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        model.checkOpenCodexRoutingRecovery()
        try await waitUntil { !model.isBusy }
        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .routingRecoveryRequired)
        XCTAssertEqual(model.openCodexRemovalFlow?.selection, selection)
        XCTAssertEqual(model.openCodexRemovalFlow?.expectedRoutingGeneration, 8)
        XCTAssertEqual(recoveryStore.session?.selection, selection)
        XCTAssertEqual(recoveryStore.session?.expectedRoutingGeneration, 8)
        XCTAssertEqual(recoveryStore.session?.lastCode, "routing_generation_rebound")
        XCTAssertEqual(model.message?.code, "routing_generation_changed")
        let commandsAfterCheckpoint = await client.commands()
        XCTAssertFalse(commandsAfterCheckpoint.contains(.recoverComplete))
        XCTAssertFalse(commandsAfterCheckpoint.contains(
            .recoverOpenCodexRemoval(
                selection: selection,
                expectedRoutingGeneration: 7
            )
        ))
        XCTAssertFalse(commandsAfterCheckpoint.contains(
            .recoverOpenCodexRemoval(
                selection: selection,
                expectedRoutingGeneration: 8
            )
        ))

        model.checkOpenCodexRoutingRecovery()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .confirmRemoval }
        XCTAssertEqual(model.openCodexRemovalFlow?.expectedRoutingGeneration, 8)
        let commandsAfterRecovery = await client.commands()
        XCTAssertTrue(commandsAfterRecovery.contains(
            .recoverOpenCodexRemoval(
                selection: selection,
                expectedRoutingGeneration: 8
            )
        ))
        XCTAssertFalse(commandsAfterRecovery.contains(.recoverComplete))
    }

    func testSameGenerationStableTokenReleaseCompletesSavedRoutingRecovery() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let selection = try OpenCodexRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64)
        )
        let candidateResult = try automaticDiscoveryResult(
            tier: .a,
            installationID: selection.installationID,
            fingerprint: selection.installationFingerprint
        )
        let recoveryStore = RecoveryStore()
        recoveryStore.session = try OpenCodexRemovalRecoverySession(
            selection: selection,
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .routingRecoveryRequired,
            lastCode: "routing_recovery_required",
            expectedRoutingGeneration: 7
        )
        let client = RelayctlClient(responses: [
            recoveryStatus(generation: 7),
            externalStatus(generation: 7),
        ])
        let model = MenuBarModel(
            client: client,
            discoveryClient: DiscoveryClient(responses: [candidateResult]),
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        model.checkOpenCodexRoutingRecovery()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .confirmRemoval }
        XCTAssertEqual(model.openCodexRemovalFlow?.expectedRoutingGeneration, 7)
        XCTAssertEqual(recoveryStore.session?.expectedRoutingGeneration, 7)
        let commands = await client.commands()
        XCTAssertTrue(commands.contains(
            .recoverOpenCodexRemoval(
                selection: selection,
                expectedRoutingGeneration: 7
            )
        ))
        XCTAssertFalse(commands.contains(.recoverComplete))
    }

    func testHigherUnsafeRoutingGenerationDoesNotCheckpointSavedRecovery() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let selection = try OpenCodexRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64)
        )
        let recoveryStore = RecoveryStore()
        recoveryStore.session = try OpenCodexRemovalRecoverySession(
            selection: selection,
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .routingRecoveryRequired,
            lastCode: "routing_recovery_required",
            expectedRoutingGeneration: 7
        )
        let client = RelayctlClient(response: recoveryStatus(
            generation: 8,
            recoveryTarget: .localOpenCodex
        ))
        let model = MenuBarModel(
            client: client,
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        model.checkOpenCodexRoutingRecovery()
        try await waitUntil { !model.isBusy }
        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .routingRecoveryRequired)
        XCTAssertEqual(model.openCodexRemovalFlow?.expectedRoutingGeneration, 7)
        XCTAssertEqual(recoveryStore.session?.expectedRoutingGeneration, 7)
        XCTAssertEqual(recoveryStore.session?.lastCode, "routing_recovery_required")
        XCTAssertEqual(model.message?.code, "routing_generation_changed")
        XCTAssertTrue(model.status?.canReviewSavedOpenCodexRemovalRecovery ?? false)
        XCTAssertFalse(model.status?.canCheckpointSavedOpenCodexRoutingRecovery ?? true)
        let commands = await client.commands()
        XCTAssertFalse(commands.contains(
            .recoverOpenCodexRemoval(
                selection: selection,
                expectedRoutingGeneration: 7
            )
        ))
    }

    func testRoutingRecoveryReceiptPersistsTheRefreshedParkedGeneration() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let candidateResult = try automaticDiscoveryResult(tier: .a)
        let candidate = try XCTUnwrap(candidateResult.candidates.first)
        let inventory = OpenCodexDataInventoryReceipt(
            status: .verified,
            installationID: candidate.id,
            items: []
        )
        let receipt = OpenCodexRemovalReceipt(
            status: .partial,
            mode: .preserveData,
            installationID: candidate.id,
            dataScope: "preserved",
            selectedDataItems: 0,
            movedDataItems: 0,
            packageRemoved: true,
            dataMovementUnknown: false,
            routingRecoveryRequired: true,
            permanentDeleteFallback: false,
            stages: [
                OpenCodexRemovalStage(
                    stage: .cleanupJournal,
                    status: .completed,
                    code: "cleanup_resume",
                    subjectID: candidate.id
                ),
                OpenCodexRemovalStage(
                    stage: .routingPostVerification,
                    status: .failed,
                    code: "routing_ownership_changed"
                ),
                OpenCodexRemovalStage(
                    stage: .routingRecovery,
                    status: .completed,
                    code: "routing_recovery_persisted"
                ),
                OpenCodexRemovalStage(
                    stage: .routingFinalVerification,
                    status: .completed,
                    code: "routing_ownership_reverified"
                ),
                OpenCodexRemovalStage(
                    stage: .packageVerification,
                    status: .completed,
                    code: "package_absent",
                    subjectID: candidate.id
                ),
            ]
        )
        let relay = RelayctlClient(responses: [
            externalStatus(generation: 1),
            externalStatus(generation: 1),
            externalStatus(generation: 1),
            recoveryStatus(generation: 2),
            recoveryStatus(generation: 2),
            externalStatus(generation: 3),
        ])
        let recoveryStore = RecoveryStore()
        let model = MenuBarModel(
            client: relay,
            discoveryClient: DiscoveryClient(responses: [candidateResult, candidateResult]),
            removalClient: RemovalClient(inventory: inventory, removal: receipt),
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        await model.refreshStatusNow()
        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates = model.openCodexDiscoveryState { return true }
            return false
        }
        model.chooseDiscoveredOpenCodexCandidate(id: candidate.id)
        model.beginOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .options }
        model.reviewOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .confirmRemoval }
        model.confirmOpenCodexPackageRemoval()
        try await waitUntil(timeout: 2) {
            model.openCodexRemovalFlow?.phase == .routingRecoveryRequired && !model.isBusy
        }

        XCTAssertEqual(recoveryStore.session?.recoveryKind, .routingRecoveryRequired)
        XCTAssertEqual(recoveryStore.session?.expectedRoutingGeneration, 2)
        let expectedSelection = try OpenCodexRemovalSelection(candidate: candidate)
        XCTAssertEqual(recoveryStore.session?.selection, expectedSelection)

        // The receipt is already persisted, so recovery must be immediately
        // actionable in this process as well as after a relaunch restores it.
        model.checkOpenCodexRoutingRecovery()
        try await waitUntil(timeout: 2) {
            model.openCodexRemovalFlow?.phase == .confirmRemoval && !model.isBusy
        }
        XCTAssertEqual(model.openCodexRemovalFlow?.expectedRoutingGeneration, 3)
        XCTAssertEqual(model.openCodexRemovalFlow?.candidate?.id, candidate.id)
        XCTAssertEqual(recoveryStore.session?.expectedRoutingGeneration, 3)
        let commands = await relay.commands()
        XCTAssertTrue(commands.contains(
            .recoverOpenCodexRemoval(
                selection: expectedSelection,
                expectedRoutingGeneration: 2
            )
        ))
        XCTAssertFalse(commands.contains(.recoverComplete))

        // If the app exits after recover succeeds but before the removal is
        // confirmed again, the saved session binds to the new stable epoch and
        // resumes directly without a second recovery command.
        let resumedRelay = RelayctlClient(response: externalStatus(generation: 3))
        let resumedModel = MenuBarModel(
            client: resumedRelay,
            discoveryClient: DiscoveryClient(responses: [candidateResult]),
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )
        XCTAssertEqual(resumedModel.openCodexRemovalFlow?.phase, .routingRecoveryRequired)
        await resumedModel.refreshStatusNow()
        resumedModel.checkOpenCodexRoutingRecovery()
        try await waitUntil(timeout: 2) {
            resumedModel.openCodexRemovalFlow?.phase == .confirmRemoval && !resumedModel.isBusy
        }
        XCTAssertEqual(resumedModel.openCodexRemovalFlow?.expectedRoutingGeneration, 3)
        let resumedCommands = await resumedRelay.commands()
        XCTAssertFalse(resumedCommands.contains(.recoverComplete))
        XCTAssertFalse(resumedCommands.contains(
            .recoverOpenCodexRemoval(
                selection: expectedSelection,
                expectedRoutingGeneration: 3
            )
        ))
    }

    func testSelectiveTrashRecoveryRequiresExplicitReconciliationBeforeRemoval() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let selection = try OpenCodexRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64)
        )
        let itemID = "ocx-data-v1:" + String(repeating: "b", count: 32)
        let recoveryStore = RecoveryStore()
        let candidateResult = try automaticDiscoveryResult(
            tier: .a,
            fingerprint: selection.installationFingerprint,
            dataCapability: "selective_trash_v1"
        )
        recoveryStore.session = try OpenCodexRemovalRecoverySession(
            selection: selection,
            mode: .trashSelected,
            orderedDataItemIDs: [itemID],
            retiredDataItemIDs: [],
            recoveryKind: .inFlight,
            lastCode: "removal_started",
            inventoryRevision: String(repeating: "d", count: 64)
        )
        let receipt = OpenCodexRemovalReceipt(
            status: .partial,
            mode: .trashSelected,
            installationID: selection.installationID,
            dataScope: "explicit_items_only",
            selectedDataItems: 1,
            movedDataItems: 0,
            packageRemoved: false,
            dataMovementUnknown: true,
            routingRecoveryRequired: false,
            permanentDeleteFallback: false,
            stages: [
                OpenCodexRemovalStage(
                    stage: .npmUninstall,
                    status: .failed,
                    code: "process_cleanup_unverified",
                    subjectID: selection.installationID
                ),
            ]
        )
        let removal = RemovalClient(
            inventory: OpenCodexDataInventoryReceipt(
                status: .verified,
                installationID: selection.installationID,
                items: []
            ),
            removal: receipt
        )
        let model = MenuBarModel(
            client: RelayctlClient(response: recoveryStatus()),
            discoveryClient: DiscoveryClient(responses: [candidateResult]),
            removalClient: removal,
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .rebootRequired)
        XCTAssertNil(model.openCodexRemovalFlow?.failure)
        XCTAssertNil(model.message)
        model.prepareRebootedOpenCodexRecovery()
        try await waitUntil(timeout: 2) {
            model.openCodexRemovalFlow?.phase == .confirmRemoval && !model.isBusy
        }
        XCTAssertEqual(model.openCodexRemovalFlow?.recoveryInventoryRevision, String(repeating: "d", count: 64))
        XCTAssertEqual(recoveryStore.session?.recoveryKind, .inFlight)
        let inspectCount = await removal.inspectCount()
        let requests = await removal.recordedRequests()
        XCTAssertEqual(inspectCount, 0)
        XCTAssertTrue(requests.isEmpty)
    }

    func testSelectiveTrashRecoveryRecordRemainsReviewableAfterResume() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let selection = try OpenCodexRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64)
        )
        let itemID = "ocx-data-v1:" + String(repeating: "c", count: 32)
        let recoveryStore = RecoveryStore()
        recoveryStore.session = try OpenCodexRemovalRecoverySession(
            selection: selection,
            mode: .trashSelected,
            orderedDataItemIDs: [itemID],
            retiredDataItemIDs: [],
            recoveryKind: .inFlight,
            lastCode: "removal_started",
            inventoryRevision: String(repeating: "d", count: 64)
        )
        let receipt = OpenCodexRemovalReceipt(
            status: .partial,
            mode: .trashSelected,
            installationID: selection.installationID,
            dataScope: "explicit_items_only",
            selectedDataItems: 1,
            movedDataItems: 0,
            packageRemoved: false,
            dataMovementUnknown: false,
            routingRecoveryRequired: true,
            permanentDeleteFallback: false,
            stages: [
                OpenCodexRemovalStage(
                    stage: .dataTrash,
                    status: .refused,
                    code: "trash_unsupported",
                    subjectID: itemID
                ),
                OpenCodexRemovalStage(
                    stage: .routingRecovery,
                    status: .completed,
                    code: "routing_recovery_persisted"
                ),
            ]
        )
        let removal = RemovalClient(
            inventory: OpenCodexDataInventoryReceipt(
                status: .verified,
                installationID: selection.installationID,
                items: []
            ),
            removal: receipt
        )
        let model = MenuBarModel(
            client: RelayctlClient(response: recoveryStatus()),
            removalClient: removal,
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .rebootRequired)
        model.dismissOpenCodexRemoval()
        XCTAssertNil(model.openCodexRemovalFlow)
        model.resumePendingOpenCodexRemoval()
        XCTAssertEqual(model.openCodexRemovalFlow?.phase, .rebootRequired)
        XCTAssertNil(model.openCodexRemovalFlow?.failure)
        XCTAssertEqual(recoveryStore.session?.recoveryKind, .inFlight)
        let inspectCount = await removal.inspectCount()
        let requests = await removal.recordedRequests()
        XCTAssertEqual(inspectCount, 0)
        XCTAssertTrue(requests.isEmpty)
    }

    func testPreserveRemovalIgnoresLegacyTrashSelectionAndUsesFrozenGeneration() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let candidateResult = try automaticDiscoveryResult(tier: .a)
        let candidate = try XCTUnwrap(candidateResult.candidates.first)
        let itemID = "ocx-data-v1:" + String(repeating: "3", count: 32)
        let inventory = OpenCodexDataInventoryReceipt(
            status: .verified,
            installationID: candidate.id,
            items: [
                OpenCodexDataInventoryItem(
                    id: itemID,
                    category: .logs,
                    scope: .owned,
                    kind: .file,
                    relativePath: "logs/relay.log",
                    exists: true,
                    sensitive: true,
                    trashable: true
                ),
            ]
        )
        let receipt = OpenCodexRemovalReceipt(
            status: .completed,
            mode: .preserveData,
            installationID: candidate.id,
            dataScope: "preserved",
            selectedDataItems: 0,
            movedDataItems: 0,
            packageRemoved: true,
            dataMovementUnknown: false,
            routingRecoveryRequired: false,
            permanentDeleteFallback: false,
            stages: [
                OpenCodexRemovalStage(
                    stage: .dataTrash,
                    status: .skipped,
                    code: "data_preserved"
                ),
                OpenCodexRemovalStage(
                    stage: .teardownPreflight,
                    status: .completed,
                    code: "teardown_preflight_verified",
                    subjectID: candidate.id
                ),
                OpenCodexRemovalStage(
                    stage: .teardown,
                    status: .completed,
                    code: "teardown_completed",
                    subjectID: candidate.id
                ),
                OpenCodexRemovalStage(
                    stage: .cleanupJournal,
                    status: .completed,
                    code: "cleanup_resume",
                    subjectID: candidate.id
                ),
                OpenCodexRemovalStage(
                    stage: .routingFinalVerification,
                    status: .completed,
                    code: "routing_ownership_reverified"
                ),
                OpenCodexRemovalStage(
                    stage: .packageVerification,
                    status: .completed,
                    code: "package_absent",
                    subjectID: candidate.id
                ),
                OpenCodexRemovalStage(
                    stage: .relayCleanup,
                    status: .completed,
                    code: "relay_cleanup_completed"
                ),
            ]
        )
        let removal = RemovalClient(inventory: inventory, removal: receipt)
        let relay = RelayctlClient(response: externalStatus())
        let desktop = DesktopController(runningValues: [])
        let recoveryStore = RecoveryStore()
        let model = MenuBarModel(
            client: relay,
            discoveryClient: DiscoveryClient(responses: [candidateResult]),
            removalClient: removal,
            removalRecoveryStore: recoveryStore,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        await model.refreshStatusNow()
        model.addLocalOpenCodexBackend()
        try await waitUntil {
            if case .candidates = model.openCodexDiscoveryState { return true }
            return false
        }
        model.chooseDiscoveredOpenCodexCandidate(id: candidate.id)
        model.beginOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .options }
        XCTAssertEqual(model.openCodexRemovalFlow?.selectedDataItemIDs, [])

        model.setOpenCodexRemovalMode(.trashSelected)
        XCTAssertEqual(model.openCodexRemovalFlow?.mode, .preserveData)
        XCTAssertTrue(model.openCodexRemovalFlow?.canContinueFromOptions ?? false)
        model.toggleOpenCodexDataItem(id: itemID)
        XCTAssertEqual(model.openCodexRemovalFlow?.selectedDataItemIDs, [])
        model.reviewOpenCodexRemoval()
        try await waitUntil { model.openCodexRemovalFlow?.phase == .confirmRemoval }
        XCTAssertEqual(model.openCodexRemovalFlow?.expectedRoutingGeneration, 1)

        model.confirmOpenCodexPackageRemoval()
        try await waitUntil(timeout: 2) { model.openCodexRemovalFlow?.phase == .result && !model.isBusy }
        let requests = await removal.recordedRequests()
        let request = try XCTUnwrap(requests.first)
        XCTAssertEqual(request.mode, .preserveData)
        XCTAssertEqual(request.dataItemIDs, [])
        XCTAssertEqual(request.expectedRoutingGeneration, 1)
        XCTAssertTrue(request.confirmsRemoval)
        XCTAssertFalse(request.confirmsTrash)
        XCTAssertTrue(request.confirmsDesktopExited)
        XCTAssertFalse(request.confirmsInterruptedDataRefresh)
        XCTAssertFalse(request.confirmsRebootedProcessRecovery)
        XCTAssertEqual(desktop.relaunches, 1)
        XCTAssertEqual(recoveryStore.clearCount, 1)
        XCTAssertFalse(model.hasPendingOpenCodexRemovalRecovery)
        let inspectCount = await removal.inspectCount()
        XCTAssertEqual(inspectCount, 0)
    }

    func testRecoveryCommandsFollowAdvertisedCapabilities() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let client = RelayctlClient(response: recoveryStatus())
        let model = MenuBarModel(
            client: client,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: [false, false, false]),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )
        await model.refreshStatusNow()

        XCTAssertTrue(model.canRecover(.complete))
        XCTAssertFalse(model.canRecover(.rollback))
        model.recover(.rollback)
        XCTAssertEqual(model.message?.code, "recovery_action_unavailable")
        let commandsAfterRollback = await client.commands()
        XCTAssertFalse(commandsAfterRollback.contains(.recoverRollback))

        model.recover(.complete)
        try await waitUntil { !model.isBusy }
        let commandsAfterComplete = await client.commands()
        XCTAssertTrue(commandsAfterComplete.contains(.recoverComplete))
    }

    func testMissingReviewedTeamIDFailsClosedWithoutDiscoveryPersistenceOrControl() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let targetStore = TargetStore(nil)
        let desktop = DesktopController(runningValues: [])
        let trust = TrustValidator()
        let discoverer = DesktopDiscoverer(candidateURLs: [appURL])
        let client = RelayctlClient(response: externalStatus())
        let model = MenuBarModel(
            client: client,
            targetStore: targetStore,
            desktopApplication: desktop,
            desktopTrustPolicy: CodexDesktopTrustPolicy(
                bundleIdentifier: "com.example.reviewed-codex",
                teamIdentifier: nil
            ),
            desktopTrustValidator: trust,
            desktopDiscoverer: discoverer,
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        XCTAssertEqual(model.desktopTargetState, .trustConfigurationMissing)
        XCTAssertNil(targetStore.desktopTarget)
        XCTAssertTrue(discoverer.bundleIdentifiers.isEmpty)
        XCTAssertTrue(trust.verifiedURLs.isEmpty)

        model.registerDesktopApplication(at: appURL)
        model.relaunchSelectedDesktop()
        model.requestMode(.external)

        XCTAssertNil(targetStore.desktopTarget)
        XCTAssertEqual(desktop.relaunches, 0)
        let commands = await client.commands()
        XCTAssertTrue(commands.isEmpty)
        XCTAssertEqual(model.message?.code, "desktop_not_selected")
    }

    func testUniqueTrustedCodexDesktopIsAutoDiscoveredButMultipleCandidatesAreNot() throws {
        let firstURL = try makeAppBundle()
        let secondURL = try makeAppBundle()
        defer {
            try? FileManager.default.removeItem(at: firstURL.deletingLastPathComponent())
            try? FileManager.default.removeItem(at: secondURL.deletingLastPathComponent())
        }

        let uniqueStore = TargetStore(nil)
        let uniqueDiscoverer = DesktopDiscoverer(candidateURLs: [firstURL])
        let uniqueModel = MenuBarModel(
            client: RelayctlClient(response: externalStatus()),
            targetStore: uniqueStore,
            desktopApplication: DesktopController(runningValues: [false]),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: uniqueDiscoverer,
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )
        XCTAssertEqual(uniqueStore.desktopTarget?.path, firstURL.resolvingSymlinksInPath().path)
        XCTAssertEqual(uniqueModel.desktopTargetState, .registered(running: false))
        XCTAssertEqual(uniqueDiscoverer.bundleIdentifiers, ["com.example.reviewed-codex"])

        let ambiguousStore = TargetStore(nil)
        let ambiguousModel = MenuBarModel(
            client: RelayctlClient(response: externalStatus()),
            targetStore: ambiguousStore,
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(candidateURLs: [firstURL, secondURL]),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )
        XCTAssertNil(ambiguousStore.desktopTarget)
        XCTAssertNil(ambiguousModel.selectedDesktopTarget)
        XCTAssertEqual(ambiguousModel.desktopTargetState, .ambiguous)
    }

    func testSignatureChangeBeforeApplyBlocksRelayctlAndRelaunch() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let client = RelayctlClient(response: pendingNativeStatus())
        let desktop = DesktopController(runningValues: [false, false, false])
        let trust = TrustValidator(failures: [nil, nil, nil, .invalidSignature])
        let model = MenuBarModel(
            client: client,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: desktop,
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: trust,
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )
        await model.refreshStatusNow()

        model.completePendingTransition()
        try await waitUntil { model.message?.code == "desktop_trust_rejected" }

        let commands = await client.commands()
        XCTAssertFalse(commands.contains(.apply))
        XCTAssertEqual(desktop.relaunches, 0)
        XCTAssertEqual(model.desktopTargetState, .untrusted)
    }

    func testDesktopApplicationPickerDefersBundleValidationUntilAfterSelection() {
        let model = MenuBarModel(
            client: RelayctlClient(response: externalStatus()),
            targetStore: TargetStore(nil),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        let panel = model.desktopApplicationPicker()

        XCTAssertTrue(panel.canChooseFiles)
        XCTAssertTrue(panel.canChooseDirectories)
        XCTAssertFalse(panel.treatsFilePackagesAsDirectories)
        XCTAssertFalse(panel.allowsMultipleSelection)
        XCTAssertTrue(panel.allowedContentTypes.isEmpty)
    }

    func testInvalidDesktopSelectionDoesNotReplaceExistingTarget() throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let existingTarget = DesktopTarget(url: appURL)
        let targetStore = TargetStore(existingTarget)
        let model = MenuBarModel(
            client: RelayctlClient(response: externalStatus()),
            targetStore: targetStore,
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: englishLocalization()
        )

        model.registerDesktopApplication(at: appURL.deletingLastPathComponent())

        XCTAssertEqual(targetStore.desktopTarget, existingTarget)
        XCTAssertEqual(model.selectedDesktopTarget, existingTarget)
        XCTAssertEqual(model.message?.code, "desktop_selection_invalid")
    }

    func testPendingRequestRelocalizesSemanticTargetWithoutChangingRelayctlCommand() async throws {
        let appURL = try makeAppBundle()
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let localization = englishLocalization()
        let client = DelayedRelayctlClient(response: externalStatus())
        let model = MenuBarModel(
            client: client,
            targetStore: TargetStore(DesktopTarget(url: appURL)),
            desktopApplication: DesktopController(runningValues: []),
            desktopTrustPolicy: trustedPolicy,
            desktopTrustValidator: TrustValidator(),
            desktopDiscoverer: DesktopDiscoverer(),
            loginRegistration: LoginRegistration(),
            startsPolling: false,
            localization: localization
        )

        await model.refreshStatusNow()
        model.requestMode(.external)
        await client.waitForRequestToStart()

        XCTAssertEqual(model.message?.code, "routing_command_running")
        XCTAssertEqual(model.message?.arguments, [.routingRequestTarget(.external)])
        XCTAssertEqual(model.message?.text(using: model.localizer), "Requesting External gateway…")
        let commandsBeforeLanguageChange = await client.commands()
        XCTAssertEqual(commandsBeforeLanguageChange, [.status, .request(.external)])

        localization.selection = .korean

        XCTAssertEqual(model.message?.code, "routing_command_running")
        XCTAssertEqual(model.message?.text(using: model.localizer), "외부 게이트웨이 요청 중…")
        let commandsAfterLanguageChange = await client.commands()
        XCTAssertEqual(commandsAfterLanguageChange, commandsBeforeLanguageChange)

        await client.resumeRequest()
        try await waitUntil(timeout: 2) { !model.isBusy }
    }

    private func makeAppBundle() throws -> URL {
        let directory = URL(fileURLWithPath: NSTemporaryDirectory(), isDirectory: true)
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        let bundle = directory.appendingPathComponent("Codex Desktop.app", isDirectory: true)
        try FileManager.default.createDirectory(at: bundle, withIntermediateDirectories: true)
        return bundle
    }

    private func makeManagedIntegrationFixture(
        status: RoutingStatus
    ) throws -> (root: URL, binding: URL, helper: URL) {
        let root = URL(fileURLWithPath: NSTemporaryDirectory(), isDirectory: true)
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)

        let binding = root.appendingPathComponent("routing-binding.json", isDirectory: false)
        try writeRoutingBinding(at: binding)

        let helper = root.appendingPathComponent("opencodex-relayctl", isDirectory: false)
        let encodedStatus = try JSONEncoder().encode(status).base64EncodedString()
        try Data("""
        #!/bin/sh
        printf '%s' '\(encodedStatus)' | /usr/bin/base64 -D
        """.utf8).write(to: helper)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o700],
            ofItemAtPath: helper.path
        )
        return (root, binding, helper)
    }

    private func writeRoutingBinding(at url: URL) throws {
        let binding = RoutingBinding(
            relayConfig: "/tmp/opencodex-relay-tests-relay.json",
            codexConfig: "/tmp/opencodex-relay-tests-codex.toml"
        )
        try JSONEncoder().encode(binding).write(to: url)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o600],
            ofItemAtPath: url.path
        )
    }

    private func externalStatus(
        generation: UInt64 = 1,
        activeRequests: Int? = 0,
        localOpenCodex: LocalOpenCodexAvailability = .unknown,
        routingSync: RoutingSync = .acknowledged
    ) -> RoutingStatus {
        RoutingStatus(
            schemaVersion: 2,
            desiredBackend: .external,
            appliedBackend: .external,
            phase: .relayActive,
            relayAdmission: .allow,
            catalogRefresh: .run,
            relayRunning: true,
            activeRequests: activeRequests,
            desktopRestartRequired: false,
            desktopEffectiveMode: .unverifiable,
            generation: generation,
            connection: RelayConnectionStatus(
                localRelay: .healthy,
                localOpenCodex: localOpenCodex,
                routingSync: routingSync,
                remoteGateway: .reachable,
                catalog: .running
            )
        )
    }

    private func englishLocalization() -> LocalizationStore {
        let suite = "OpenCodexRelayAppTests.\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        return LocalizationStore(
            defaults: defaults,
            preferenceKey: "language",
            preferredLanguages: { ["en-US"] }
        )
    }

    private func pendingNativeStatus(generation: UInt64 = 1) -> RoutingStatus {
        RoutingStatus(
            schemaVersion: 2,
            desiredBackend: .none,
            appliedBackend: .external,
            phase: .nativePendingRestart,
            relayAdmission: .allow,
            catalogRefresh: .run,
            relayRunning: true,
            activeRequests: 0,
            desktopRestartRequired: true,
            desktopEffectiveMode: .unverifiable,
            generation: generation,
            connection: RelayConnectionStatus(
                localRelay: .healthy,
                routingSync: .acknowledged,
                remoteGateway: .reachable,
                catalog: .running
            )
        )
    }

    private func pendingExternalStatus(generation: UInt64) -> RoutingStatus {
        RoutingStatus(
            schemaVersion: 3,
            desiredBackend: .external,
            appliedBackend: .none,
            phase: .relayPendingRestart,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            relayRunning: true,
            activeRequests: 0,
            desktopRestartRequired: true,
            desktopEffectiveMode: .unverifiable,
            generation: generation,
            connection: RelayConnectionStatus(
                localRelay: .healthy,
                routingSync: .acknowledged,
                remoteGateway: .notApplicable,
                catalog: .paused
            )
        )
    }

    private func orphanRecoveryStatus(generation: UInt64) -> RoutingStatus {
        RoutingStatus(
            schemaVersion: 3,
            desiredBackend: .external,
            appliedBackend: .none,
            phase: .recoveryRequired,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            relayRunning: true,
            activeRequests: 0,
            desktopRestartRequired: true,
            desktopEffectiveMode: .unverifiable,
            generation: generation,
            connection: RelayConnectionStatus(
                localRelay: .healthy,
                routingSync: .acknowledged,
                remoteGateway: .notApplicable,
                catalog: .paused
            ),
            recoveryCapabilities: RecoveryCapabilities(
                canComplete: false,
                canRollback: false,
                completeReason: "observed_state_unavailable",
                rollbackReason: "journal_missing",
                target: .unknown,
                targetConfidence: .unavailable,
                authoritativeJournal: false
            )
        )
    }

    private func opaqueRecoveryStatus() -> RoutingStatus {
        RoutingStatus(
            schemaVersion: 3,
            desiredBackend: .unknown,
            appliedBackend: .unknown,
            phase: .recoveryRequired,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            relayRunning: false,
            activeRequests: nil,
            desktopRestartRequired: true,
            desktopEffectiveMode: .unverifiable,
            generation: 0,
            connection: RelayConnectionStatus(
                localRelay: .unknown,
                routingSync: .invalid,
                remoteGateway: .unknown,
                catalog: .unknown
            ),
            recoveryCapabilities: RecoveryCapabilities(
                canComplete: false,
                canRollback: false,
                completeReason: "observed_state_unavailable",
                rollbackReason: "observed_state_unavailable",
                target: .unknown,
                targetConfidence: .unavailable,
                authoritativeJournal: false
            )
        )
    }

    private func nativeStatus(generation: UInt64) -> RoutingStatus {
        RoutingStatus(
            schemaVersion: 3,
            desiredBackend: .none,
            appliedBackend: .none,
            phase: .nativeActive,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            relayRunning: true,
            activeRequests: 0,
            desktopRestartRequired: false,
            desktopEffectiveMode: .unverifiable,
            generation: generation,
            connection: RelayConnectionStatus(
                localRelay: .healthy,
                routingSync: .acknowledged,
                remoteGateway: .notApplicable,
                catalog: .paused
            ),
            recoveryCapabilities: RecoveryCapabilities(
                canComplete: false,
                canRollback: false,
                completeReason: "recovery_not_required",
                rollbackReason: "recovery_not_required",
                target: .unknown,
                targetConfidence: .unavailable,
                authoritativeJournal: false
            )
        )
    }

    private func nativeRepairInspection(kind: NativeRepairKind) throws -> NativeRepairInspection {
        let reason: String
        let openAIBaseURL: Bool
        let modelCatalog: Bool
        switch kind {
        case .stateOnly:
            reason = "native_routing_clean"
            openAIBaseURL = false
            modelCatalog = false
        case .localRelay:
            reason = "local_relay_owned"
            openAIBaseURL = true
            modelCatalog = false
        case .openCodex:
            reason = "opencodex_owned"
            openAIBaseURL = true
            modelCatalog = true
        case .unavailable:
            reason = "unmanaged_routing_override"
            openAIBaseURL = true
            modelCatalog = false
        }
        let source = Data("""
        {"schema_version":1,"generation":3,"phase":"recovery_required","kind":"\(kind.rawValue)","openai_base_url":\(openAIBaseURL),"model_catalog_json":\(modelCatalog),"reason":"\(reason)"}
        """.utf8)
        return try JSONDecoder().decode(NativeRepairInspection.self, from: source).validated()
    }

    private func nativeRepairReceipt(
        status: RoutingStatus,
        nonRoutingCleanupIncomplete: Bool = false
    ) throws -> NativeRoutingRepairReceipt {
        let object = try JSONSerialization.jsonObject(with: JSONEncoder().encode(status))
        let data = try JSONSerialization.data(withJSONObject: [
            "schema_version": 2,
            "status": object,
            "backup_created": true,
            "nonrouting_cleanup_incomplete": nonRoutingCleanupIncomplete,
            "owner_restore_attempts": 1,
            "owner_restore_result": "applied",
        ])
        return try JSONDecoder().decode(NativeRoutingRepairReceipt.self, from: data).validated()
    }

    private func recoveryStatus(
        generation: UInt64 = 1,
        localRelay: LocalRelayConnection = .healthy,
        routingSync: RoutingSync = .invalid,
        relayRunning: Bool = true,
        activeRequests: Int? = 0,
        recoveryTarget: RoutingBackend = .external
    ) -> RoutingStatus {
        RoutingStatus(
            schemaVersion: 3,
            desiredBackend: .unknown,
            appliedBackend: .unknown,
            phase: .recoveryRequired,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            relayRunning: relayRunning,
            activeRequests: activeRequests,
            desktopRestartRequired: true,
            desktopEffectiveMode: .unverifiable,
            generation: generation,
            connection: RelayConnectionStatus(
                localRelay: localRelay,
                routingSync: routingSync,
                remoteGateway: localRelay == .unreachable ? .unknown : .notApplicable,
                catalog: localRelay == .unreachable ? .unknown : .paused
            ),
            recoveryCapabilities: RecoveryCapabilities(
                canComplete: true,
                canRollback: false,
                completeReason: "observed_state_verified",
                rollbackReason: "journal_missing",
                target: recoveryTarget,
                targetConfidence: .observed,
                authoritativeJournal: false
            )
        )
    }

    private func makeOpenCodexExecutableFixture() throws -> (directory: URL, executable: OpenCodexExecutable) {
        let directory = URL(fileURLWithPath: NSTemporaryDirectory(), isDirectory: true)
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
            .resolvingSymlinksInPath()
        let executableURL = directory
            .appendingPathComponent("lib/node_modules/@bitkyc08/opencodex/bin", isDirectory: true)
            .appendingPathComponent("ocx.mjs", isDirectory: false)
        try FileManager.default.createDirectory(
            at: executableURL.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        try Data("#!/bin/sh\nexit 0\n".utf8).write(to: executableURL)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o700],
            ofItemAtPath: executableURL.path
        )
        return (directory, try OpenCodexExecutableResolver.select(executableURL))
    }

    private func automaticDiscoveryResult(
        tier: OpenCodexDiscoveryTier,
        handoffExecutable: OpenCodexExecutable? = nil,
        installationID: String = "0123456789abcdef01234567",
        fingerprint: String = String(repeating: "2", count: 64),
        dataCapability: String = "preserve_only",
        nativeRestoreVerified: Bool = true,
        nativeRestoreFingerprint: String = String(repeating: "9", count: 64)
    ) throws -> OpenCodexDiscoveryResult {
        let executable = handoffExecutable?.path ?? "/opt/fixture/lib/node_modules/@bitkyc08/opencodex/bin/ocx.mjs"
        let executableSHA256 = handoffExecutable?.sha256 ?? String(repeating: "a", count: 64)
        let packageRoot = URL(fileURLWithPath: executable)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .path
        let prefix = URL(fileURLWithPath: packageRoot)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .path
        let restoreProof = nativeRestoreVerified
            ? ",\"native_restore_capability\":\"verified_snapshot\",\"native_restore_fingerprint\":\"\(nativeRestoreFingerprint)\""
            : ""
        let legacySource = """
        {"schema_version":2,"requested_tier":"\(tier.rawValue)","broad_scan_approved":false,"candidates":[{"id":"\(installationID)","tier":"\(tier.rawValue)","source":"fixture","manager":"npm","prefix":"\(prefix)","package_root":"\(packageRoot)","version":"2.22.0","executable":"\(executable)","executable_sha256":"\(executableSHA256)","cli_entry":"\(packageRoot)/dist/cli.js","cli_entry_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","bun_executable":"\(prefix)/bin/bun","bun_sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","package_tree_sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","npm_tree_sha256":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","launchers":["\(prefix)/bin/ocx"],"node_executable":"\(prefix)/bin/node","node_sha256":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","npm_cli":"\(prefix)/lib/node_modules/npm/bin/npm-cli.js","npm_cli_sha256":"1111111111111111111111111111111111111111111111111111111111111111","confidence":"trusted","removal_capability":"exact_npm","removal_authority":"automatic"\(restoreProof),"user_writable":true,"requires_elevation":false,"fingerprint":"\(fingerprint)","warnings":[]}],"coverage":[],"rejected":0,"truncated":false}
        """.data(using: .utf8)!
        let source = String(decoding: legacySource, as: UTF8.self)
            .replacingOccurrences(of: #""schema_version":2"#, with: #""schema_version":5"#)
            .replacingOccurrences(
                of: #""removal_authority":"automatic""#,
                with: #""removal_authority":"automatic","homebrew_guard_required":false,"teardown_capability":"relay_preserve_v1","data_capability":"\#(dataCapability)","teardown_compatibility_reason":"compatible","teardown_adapter_id":"opencodex_npm_2_22_0_preserve_v1""#
            )
            .data(using: .utf8)!
        return try JSONDecoder().decode(OpenCodexDiscoveryResult.self, from: source).validated()
    }

    private func guardedHomebrewDiscoveryResult(
        tier: OpenCodexDiscoveryTier,
        installationID: String = "0123456789abcdef01234567",
        fingerprint: String = String(repeating: "7", count: 64)
    ) throws -> OpenCodexDiscoveryResult {
        let packageRoot = "/opt/homebrew/lib/node_modules/@bitkyc08/opencodex"
        let legacySource = """
        {"schema_version":3,"requested_tier":"\(tier.rawValue)","broad_scan_approved":false,"candidates":[{"id":"\(installationID)","tier":"\(tier.rawValue)","source":"fixture","manager":"homebrew","prefix":"/opt/homebrew","package_root":"\(packageRoot)","version":"2.22.0","executable":"\(packageRoot)/bin/ocx.mjs","executable_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","cli_entry":"\(packageRoot)/src/cli/index.ts","cli_entry_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","bun_executable":"\(packageRoot)/node_modules/bun/bin/bun","bun_sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","package_tree_sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","npm_tree_sha256":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","launchers":["/opt/homebrew/bin/ocx"],"node_executable":"/opt/homebrew/bin/node","node_sha256":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","npm_cli":"/opt/homebrew/lib/node_modules/npm/bin/npm-cli.js","npm_cli_sha256":"1111111111111111111111111111111111111111111111111111111111111111","confidence":"trusted","removal_capability":"homebrew_guarded_npm","removal_authority":"automatic","homebrew_guard_required":true,"user_writable":true,"requires_elevation":false,"fingerprint":"\(fingerprint)","warnings":["homebrew_guard_required"]}],"coverage":[],"rejected":0,"truncated":false}
        """.data(using: .utf8)!
        let source = String(decoding: legacySource, as: UTF8.self)
            .replacingOccurrences(of: #""schema_version":3"#, with: #""schema_version":4"#)
            .replacingOccurrences(
                of: #""homebrew_guard_required":true"#,
                with: #""homebrew_guard_required":true,"teardown_capability":"relay_preserve_v1","data_capability":"preserve_only","teardown_compatibility_reason":"compatible","teardown_adapter_id":"opencodex_npm_2_22_0_preserve_v1""#
            )
            .data(using: .utf8)!
        return try JSONDecoder().decode(OpenCodexDiscoveryResult.self, from: source).validated()
    }

    private func nativeDiscoveryResult(
        dataCapability: String = "preserve_only"
    ) throws -> OpenCodexNativeDiscoveryResult {
        let source = Data("""
        {"schema_version":1,"operation":"discover-open-codex-native","context":"standalone_native","status":"ready","boundary_revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","native_state":"opencodex","native_recovery_required":false,"candidates":[{"installation_id":"0123456789abcdef01234567","installation_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","native_restore_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","version":"2.22.0","manager":"npm","removal_capability":"exact_npm","removal_authority":"automatic","data_capability":"\(dataCapability)","automatic_removal_eligible":true,"homebrew_guard_required":false}],"rejected":0,"truncated":false}
        """.utf8)
        return try JSONDecoder().decode(OpenCodexNativeDiscoveryResult.self, from: source).validated()
    }

    private func nativeCleanDiscoveryResult() throws -> OpenCodexNativeDiscoveryResult {
        let source = Data("""
        {"schema_version":1,"operation":"discover-open-codex-native","context":"standalone_native","status":"ready","boundary_revision":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","native_state":"native","native_recovery_required":false,"candidates":[],"rejected":0,"truncated":false}
        """.utf8)
        return try JSONDecoder().decode(OpenCodexNativeDiscoveryResult.self, from: source).validated()
    }

    private func nativeRemovalInspection(
        dataCapability: String = "preserve_only"
    ) throws -> OpenCodexNativeRemovalInspection {
        let source = Data("""
        {"schema_version":1,"operation":"inspect-open-codex-native-removal","context":"standalone_native","status":"ready","boundary_revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","native_state":"opencodex","native_recovery_required":false,"candidate":{"installation_id":"0123456789abcdef01234567","installation_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","native_restore_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","version":"2.22.0","manager":"npm","removal_capability":"exact_npm","removal_authority":"automatic","data_capability":"\(dataCapability)","automatic_removal_eligible":true,"homebrew_guard_required":false}}
        """.utf8)
        let selection = try OpenCodexNativeRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64),
            nativeRestoreFingerprint: String(repeating: "c", count: 64),
            boundaryRevision: String(repeating: "b", count: 64)
        )
        return try JSONDecoder().decode(OpenCodexNativeRemovalInspection.self, from: source)
            .validated(for: selection)
    }

    private func nativeDataInventoryReceipt(
        itemID: String,
        inventoryRevision: String
    ) throws -> OpenCodexNativeDataInventoryReceipt {
        let source = Data("""
        {"schema_version":1,"operation":"open-codex-native-data-inventory","context":"standalone_native","status":"verified","boundary_revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","native_state":"opencodex","native_recovery_required":false,"installation_id":"0123456789abcdef01234567","installation_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","native_restore_fingerprint":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","inventory_revision":"\(inventoryRevision)","items":[{"id":"\(itemID)","category":"logs","scope":"owned","kind":"file","relative_path":"logs/opencodex.log","exists":true,"sensitive":true,"trashable":true}]}
        """.utf8)
        let selection = try OpenCodexNativeRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64),
            nativeRestoreFingerprint: String(repeating: "c", count: 64),
            boundaryRevision: String(repeating: "b", count: 64)
        )
        return try JSONDecoder().decode(OpenCodexNativeDataInventoryReceipt.self, from: source)
            .validated(for: selection)
    }

    private func successfulNativeRemovalReceipt() throws -> OpenCodexNativeRemovalReceipt {
        let source = Data("""
        {"schema_version":1,"operation":"remove-open-codex-native","context":"standalone_native","status":"completed","boundary_revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","native_state":"native","native_recovery_required":false,"mode":"preserve_data","installation_id":"0123456789abcdef01234567","data_scope":"preserved","selected_data_items":0,"moved_data_items":0,"package_removed":true,"data_movement_unknown":false,"permanent_delete_fallback":false,"terminal_receipt_digest":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","stages":[{"stage":"native_boundary_final_verification","status":"completed","code":"native_ownership_reverified"},{"stage":"package_verification","status":"completed","code":"package_absent","subject_id":"0123456789abcdef01234567"},{"stage":"cleanup_journal_retained","status":"completed","code":"terminal_receipt_replayable"}]}
        """.utf8)
        return try JSONDecoder().decode(OpenCodexNativeRemovalReceipt.self, from: source)
    }

    private func discoveryResult(
        tier: OpenCodexDiscoveryTier,
        includesCandidate: Bool
    ) throws -> OpenCodexDiscoveryResult {
        let candidate = includesCandidate ? """
        [{"id":"0123456789abcdef01234567","tier":"\(tier.rawValue)","source":"fixture","manager":"npm","prefix":"/opt/fixture","package_root":"/opt/fixture/lib/node_modules/@bitkyc08/opencodex","version":"2.22.0","executable":"/opt/fixture/lib/node_modules/@bitkyc08/opencodex/bin/ocx.mjs","executable_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","launchers":[],"confidence":"trusted","removal_capability":"manual","removal_authority":"manual","user_writable":true,"requires_elevation":false,"fingerprint":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","warnings":["exact_npm_pair_unavailable"]}]
        """ : "[]"
        let source = """
        {"schema_version":2,"requested_tier":"\(tier.rawValue)","broad_scan_approved":\(tier == .c ? "true" : "false"),"candidates":\(candidate),"coverage":[],"rejected":0,"truncated":false}
        """.data(using: .utf8)!
        return try JSONDecoder().decode(OpenCodexDiscoveryResult.self, from: source).validated()
    }

    private func waitUntil(
        timeout: TimeInterval = 1,
        _ predicate: @escaping @MainActor () -> Bool
    ) async throws {
        let deadline = Date().addingTimeInterval(timeout)
        while !predicate() {
            guard Date() < deadline else {
                XCTFail("timed out waiting for MenuBar model state")
                return
            }
            try await Task.sleep(nanoseconds: 10_000_000)
        }
    }
}
