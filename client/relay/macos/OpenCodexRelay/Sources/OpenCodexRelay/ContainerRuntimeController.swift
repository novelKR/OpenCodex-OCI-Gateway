import Foundation
import OpenCodexRelayCore

struct ContainerRuntimeMutationWitness: Equatable, Sendable {
    let stateDigest: String
    let routingGeneration: UInt64
}

typealias ContainerRuntimeActivationWitness = ContainerRuntimeMutationWitness
typealias ContainerRuntimeStopWitness = ContainerRuntimeMutationWitness
typealias ContainerRuntimeRecoveryWitness = ContainerRuntimeMutationWitness

/// UI state for the bounded relayctl contract. Runtime secrets remain exclusively
/// owned by the Go lifecycle manager and never enter this process.
@MainActor
final class ContainerRuntimeController: ObservableObject {
    @Published private(set) var inspection: ContainerRuntimeInspection?
    @Published private(set) var checkReceipt: ContainerRuntimeCheckReceipt?
    @Published private(set) var providers: [ContainerRuntimeOAuthProvider] = []
    @Published private(set) var oauthReceipt: ContainerRuntimeOAuthReceipt?
    @Published private(set) var isBusy = false
    @Published private(set) var isOAuthBusy = false
    @Published private(set) var lastErrorCode: String?
    @Published private(set) var oauthErrorCode: String?
    @Published private(set) var optedIn: Bool

    private let client: any ContainerRuntimeManaging
    private let defaults: UserDefaults
    private let optInKey: String
    private let lastCheckKey: String
    private let now: @Sendable () -> Date
    private var requestID = 0
    private var oauthRequestID = 0
    private var started = false
    private var automaticCheckTask: Task<Void, Never>?
    private var lastCheckAttempt: Date?

    init(
        client: any ContainerRuntimeManaging = ProcessContainerRuntimeClient(),
        defaults: UserDefaults = .standard,
        optInKey: String = "appleContainerRuntime.optIn.v1",
        lastCheckKey: String = "appleContainerRuntime.lastCheck.v1",
        now: @escaping @Sendable () -> Date = Date.init
    ) {
        self.client = client
        self.defaults = defaults
        self.optInKey = optInKey
        self.lastCheckKey = lastCheckKey
        self.now = now
        self.optedIn = defaults.bool(forKey: optInKey)
    }

    deinit { automaticCheckTask?.cancel() }

    var canCheck: Bool { optedIn && !isBusy }

    var canStage: Bool {
        guard !isBusy,
              let receipt = checkReceipt,
              receipt.status == .updateAvailable,
              receipt.compatible,
              receipt.candidate != nil,
              inspection?.state != .unavailable,
              inspection?.state != .recoveryRequired else { return false }
        return true
    }

    var canActivate: Bool {
        guard !isBusy,
              let inspection,
              inspection.capability.available,
              inspection.state != .unavailable,
              inspection.state != .recoveryRequired,
              inspection.state != .staging,
              inspection.state != .updating else { return false }
        return inspection.staged != nil ||
            (inspection.state == .stopped && inspection.active != nil)
    }

    var activationWitness: ContainerRuntimeActivationWitness? {
        guard canActivate, let inspection else { return nil }
        return ContainerRuntimeActivationWitness(
            stateDigest: inspection.stateDigest,
            routingGeneration: inspection.routingGeneration
        )
    }

    var canStop: Bool {
        !isBusy && inspection?.active != nil && inspection?.state == .healthy
    }

    var canRecover: Bool { !isBusy && inspection?.state == .recoveryRequired }

    var stopWitness: ContainerRuntimeStopWitness? {
        guard canStop, let inspection else { return nil }
        return ContainerRuntimeStopWitness(
            stateDigest: inspection.stateDigest,
            routingGeneration: inspection.routingGeneration
        )
    }

    var recoveryWitness: ContainerRuntimeRecoveryWitness? {
        guard canRecover, let inspection else { return nil }
        return ContainerRuntimeRecoveryWitness(
            stateDigest: inspection.stateDigest,
            routingGeneration: inspection.routingGeneration
        )
    }

    var canManageOAuth: Bool { !isBusy && inspection?.state == .healthy }

    func start() {
        guard !started else { return }
        started = true
        if optedIn, automaticCheckIsDue {
            checkNow()
        } else {
            refresh()
        }
        scheduleAutomaticChecks()
    }

    func setOptedIn(_ value: Bool) {
        guard optedIn != value else { return }
        optedIn = value
        defaults.set(value, forKey: optInKey)
        checkReceipt = nil
        automaticCheckTask?.cancel()
        automaticCheckTask = nil
        if value {
            checkNow()
            scheduleAutomaticChecks()
        }
    }

    func refresh() {
        requestID += 1
        let current = requestID
        isBusy = true
        lastErrorCode = nil
        let client = self.client
        Task {
            do {
                let inspection = try await client.inspect()
                guard requestID == current else { return }
                self.inspection = inspection
                self.isBusy = false
            } catch {
                guard requestID == current else { return }
                lastErrorCode = Self.safeCode(error)
                isBusy = false
            }
        }
    }

    func checkNow() {
        guard canCheck else { return }
        lastCheckAttempt = now()
        perform { client in
            let result = try await client.check()
            return (result.inspection, result)
        }
    }

    func stageConfirmed() {
        guard canStage,
              let state = inspection,
              let candidate = checkReceipt?.candidate else { return }
        performMutation { client in
            try await client.stage(
                expectedManifestSHA256: candidate.manifestSHA256,
                expectedStateDigest: state.stateDigest,
                expectedRoutingGeneration: state.routingGeneration
            )
        }
    }

    /// Called only from MenuBarModel while it holds the exact Desktop-exit
    /// operation lease. Rechecking the captured UI witness here prevents a
    /// status refresh or another runtime operation during the graceful-quit
    /// interval from being silently applied with a different CAS identity.
    func activateAfterVerifiedDesktopExit(
        expected witness: ContainerRuntimeActivationWitness
    ) async -> Bool {
        guard !isBusy, activationWitness == witness else {
            lastErrorCode = "container_runtime_activation_witness_changed"
            return false
        }
        requestID += 1
        let current = requestID
        isBusy = true
        lastErrorCode = nil
        do {
            let receipt = try await client.activate(
                expectedStateDigest: witness.stateDigest,
                expectedRoutingGeneration: witness.routingGeneration,
                confirmDesktopExited: true
            )
            guard requestID == current else { return false }
            inspection = receipt.inspection
            isBusy = false
            return true
        } catch {
            guard requestID == current else { return false }
            lastErrorCode = Self.safeCode(error)
            isBusy = false
            return false
        }
    }

    func stopAfterVerifiedDesktopExit(
        expected witness: ContainerRuntimeStopWitness
    ) async -> Bool {
        await performVerifiedDesktopMutation(
            expected: witness,
            current: stopWitness,
            witnessChangedCode: "container_runtime_stop_witness_changed"
        ) { client in
            try await client.stop(
                expectedStateDigest: witness.stateDigest,
                expectedRoutingGeneration: witness.routingGeneration,
                confirmDesktopExited: true
            )
        }
    }

    func recoverAfterVerifiedDesktopExit(
        expected witness: ContainerRuntimeRecoveryWitness
    ) async -> Bool {
        await performVerifiedDesktopMutation(
            expected: witness,
            current: recoveryWitness,
            witnessChangedCode: "container_runtime_recovery_witness_changed"
        ) { client in
            try await client.recover(
                expectedStateDigest: witness.stateDigest,
                confirmDesktopExited: true
            )
        }
    }

    func loadOAuthProviders() {
        guard canManageOAuth, !isOAuthBusy else { return }
        performOAuth { client in
            let receipt = try await client.oauthProviders()
            return (receipt.providers, nil)
        }
    }

    func startOAuth(provider: ContainerRuntimeOAuthProvider) {
        guard canManageOAuth, !isOAuthBusy else { return }
        performOAuth { client in
            let receipt = try await client.oauthStart(provider: provider.id, kind: provider.kind)
            return (nil, receipt)
        }
    }

    func refreshOAuth() {
        guard !isOAuthBusy, let operationID = oauthReceipt?.operationID else { return }
        performOAuth { client in
            (nil, try await client.oauthStatus(operationID: operationID))
        }
    }

    func submitOAuth(_ value: String) {
        guard !isOAuthBusy, let operationID = oauthReceipt?.operationID else { return }
        let normalized = value.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !normalized.isEmpty, normalized.utf8.count <= 4 * 1_024 else {
            oauthErrorCode = "container_runtime_oauth_invalid_input"
            return
        }
        let scheme = URL(string: normalized)?.scheme?.lowercased()
        let isURL = scheme == "http" || scheme == "https"
        performOAuth { client in
            (nil, try await client.oauthSubmit(
                operationID: operationID,
                redirectURL: isURL ? normalized : nil,
                code: isURL ? nil : normalized
            ))
        }
    }

    func cancelOAuth() {
        guard !isOAuthBusy, let operationID = oauthReceipt?.operationID else { return }
        performOAuth { client in
            (nil, try await client.oauthCancel(operationID: operationID))
        }
    }

    private func performVerifiedDesktopMutation(
        expected witness: ContainerRuntimeMutationWitness,
        current currentWitness: ContainerRuntimeMutationWitness?,
        witnessChangedCode: String,
        operation: @escaping @Sendable (any ContainerRuntimeManaging) async throws ->
            ContainerRuntimeMutationReceipt
    ) async -> Bool {
        guard !isBusy, currentWitness == witness else {
            lastErrorCode = witnessChangedCode
            return false
        }
        requestID += 1
        let current = requestID
        isBusy = true
        lastErrorCode = nil
        do {
            let receipt = try await operation(client)
            guard requestID == current else { return false }
            inspection = receipt.inspection
            isBusy = false
            return true
        } catch {
            guard requestID == current else { return false }
            lastErrorCode = Self.safeCode(error)
            isBusy = false
            return false
        }
    }

    private func perform(
        _ operation: @escaping @Sendable (any ContainerRuntimeManaging) async throws ->
            (ContainerRuntimeInspection, ContainerRuntimeCheckReceipt?)
    ) {
        requestID += 1
        let current = requestID
        isBusy = true
        lastErrorCode = nil
        let client = self.client
        Task {
            do {
                let (inspection, check) = try await operation(client)
                guard requestID == current else { return }
                self.inspection = inspection
                self.checkReceipt = check
                self.defaults.set(self.now(), forKey: self.lastCheckKey)
                self.isBusy = false
            } catch {
                guard requestID == current else { return }
                lastErrorCode = Self.safeCode(error)
                isBusy = false
            }
        }
    }

    private func performMutation(
        _ operation: @escaping @Sendable (any ContainerRuntimeManaging) async throws ->
            ContainerRuntimeMutationReceipt
    ) {
        requestID += 1
        let current = requestID
        isBusy = true
        lastErrorCode = nil
        let client = self.client
        Task {
            do {
                let receipt = try await operation(client)
                guard requestID == current else { return }
                inspection = receipt.inspection
                isBusy = false
            } catch {
                guard requestID == current else { return }
                lastErrorCode = Self.safeCode(error)
                isBusy = false
            }
        }
    }

    private func performOAuth(
        _ operation: @escaping @Sendable (any ContainerRuntimeManaging) async throws ->
            ([ContainerRuntimeOAuthProvider]?, ContainerRuntimeOAuthReceipt?)
    ) {
        oauthRequestID += 1
        let current = oauthRequestID
        isOAuthBusy = true
        oauthErrorCode = nil
        let client = self.client
        Task {
            do {
                let (providers, receipt) = try await operation(client)
                guard oauthRequestID == current else { return }
                if let providers { self.providers = providers }
                if let receipt { self.oauthReceipt = receipt }
                isOAuthBusy = false
            } catch {
                guard oauthRequestID == current else { return }
                oauthErrorCode = Self.safeCode(error)
                isOAuthBusy = false
            }
        }
    }

    private func scheduleAutomaticChecks() {
        guard optedIn, automaticCheckTask == nil else { return }
        automaticCheckTask = Task { [weak self] in
            while !Task.isCancelled {
                guard let self, self.optedIn else { return }
                let remaining = UInt64(self.automaticCheckDelaySeconds.rounded(.up))
                let jitter = UInt64.random(in: 0...(60 * 60))
                try? await Task.sleep(for: .seconds(remaining + jitter))
                guard !Task.isCancelled, self.optedIn else { return }
                // A manual check may have completed while this task was
                // sleeping. Recalculate instead of issuing a second check on
                // the old deadline.
                guard self.automaticCheckIsDue else { continue }
                self.checkNow()
            }
        }
    }

    private var automaticCheckIsDue: Bool {
        automaticCheckDelaySeconds == 0
    }

    var automaticCheckDelaySeconds: TimeInterval {
        let persisted = defaults.object(forKey: lastCheckKey) as? Date
        let reference = [persisted, lastCheckAttempt].compactMap { $0 }.max()
        guard let reference else { return 0 }
        let elapsed = max(0, now().timeIntervalSince(reference))
        return max(0, 24 * 60 * 60 - elapsed)
    }

    private static func safeCode(_ error: Error) -> String {
        if let relayctl = error as? RelayctlError { return relayctl.safeCode }
        if let binding = error as? RoutingBindingError { return binding.safeCode }
        if let client = error as? ContainerRuntimeClientError { return client.safeCode }
        if let contract = error as? ContainerRuntimeContractError {
            switch contract {
            case .invalidJSON: return "container_runtime_invalid_json"
            case .duplicateField: return "container_runtime_duplicate_field"
            case .unknownField: return "container_runtime_unknown_field"
            case .invalidSchema: return "container_runtime_invalid_schema"
            case .invalidState: return "container_runtime_invalid_state"
            case .invalidArgument: return "container_runtime_invalid_argument"
            }
        }
        return "container_runtime_failed"
    }
}
