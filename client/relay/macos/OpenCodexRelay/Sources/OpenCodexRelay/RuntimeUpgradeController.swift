import Foundation
import OpenCodexRelayCore

@MainActor
final class RuntimeUpgradeController: ObservableObject {
    @Published private(set) var inspection: SelfHostedRuntimeUpgradeInspection?
    @Published private(set) var isBusy = false
    @Published private(set) var lastErrorCode: String?

    private let client: any SelfHostedRuntimeUpgrading
    private var requestID = 0
    private var started = false

    init(
        client: any SelfHostedRuntimeUpgrading = ProcessSelfHostedRuntimeUpgradeClient()
    ) {
        self.client = client
    }

    var canApply: Bool {
        !isBusy && inspection?.state == .upgradeAvailable
    }

    var canRecover: Bool {
        !isBusy && inspection?.state == .recoveryRequired
    }

    func start() {
        guard !started else { return }
        started = true
        refresh()
    }

    func refresh() {
        perform { client in
            try await client.inspect()
        }
    }

    func applyConfirmed() {
        guard canApply, let expected = inspection?.stateDigest else { return }
        perform { client in
            try await client.apply(
                expectedStateDigest: expected,
                confirmRelayRestart: true
            ).inspection
        }
    }

    func recoverConfirmed() {
        guard canRecover else { return }
        perform { client in
            try await client.recover().inspection
        }
    }

    private func perform(
        _ operation: @escaping @Sendable (
            any SelfHostedRuntimeUpgrading
        ) async throws -> SelfHostedRuntimeUpgradeInspection
    ) {
        requestID += 1
        let currentRequest = requestID
        isBusy = true
        lastErrorCode = nil
        let client = self.client
        Task {
            do {
                let result = try await operation(client)
                guard currentRequest == requestID else { return }
                inspection = result
                isBusy = false
            } catch {
                guard currentRequest == requestID else { return }
                lastErrorCode = Self.safeCode(error)
                isBusy = false
            }
        }
    }

    private static func safeCode(_ error: Error) -> String {
        if let relayctl = error as? RelayctlError {
            return relayctl.safeCode
        }
        if let contract = error as? SelfHostedRuntimeUpgradeContractError {
            switch contract {
            case .invalidJSON: return "runtime_upgrade_invalid_json"
            case .duplicateField: return "runtime_upgrade_duplicate_field"
            case .unknownField: return "runtime_upgrade_unknown_field"
            case .invalidSchema: return "runtime_upgrade_invalid_schema"
            case .invalidState: return "runtime_upgrade_invalid_state"
            }
        }
        return "runtime_upgrade_failed"
    }
}
