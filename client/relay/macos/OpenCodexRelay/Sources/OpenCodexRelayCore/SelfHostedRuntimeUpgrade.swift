import Foundation

public enum SelfHostedRuntimeUpgradeState: String, Codable, Sendable {
    case notIntegrated = "not_integrated"
    case current
    case upgradeAvailable = "upgrade_available"
    case recoveryRequired = "recovery_required"
    case incompatible
}

public enum SelfHostedRuntimeUpgradeContractError: Error, Equatable, Sendable {
    case invalidJSON
    case duplicateField
    case unknownField
    case invalidSchema
    case invalidState
}

public struct SelfHostedRuntimeUpgradeInspection: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let state: SelfHostedRuntimeUpgradeState
    public let stateDigest: String
    public let installedRuntimeVersion: String
    public let installedRuntimeDigest: String
    public let bundledRuntimeVersion: String
    public let bundledRuntimeDigest: String
    public let integrationProtocol: Int
    public let restartRequired: Bool

    enum CodingKeys: String, CodingKey, CaseIterable {
        case schemaVersion = "schema_version"
        case state
        case stateDigest = "state_digest"
        case installedRuntimeVersion = "installed_runtime_version"
        case installedRuntimeDigest = "installed_runtime_digest"
        case bundledRuntimeVersion = "bundled_runtime_version"
        case bundledRuntimeDigest = "bundled_runtime_digest"
        case integrationProtocol = "integration_protocol"
        case restartRequired = "restart_required"
    }

    public init(
        schemaVersion: Int,
        state: SelfHostedRuntimeUpgradeState,
        stateDigest: String,
        installedRuntimeVersion: String,
        installedRuntimeDigest: String,
        bundledRuntimeVersion: String,
        bundledRuntimeDigest: String,
        integrationProtocol: Int,
        restartRequired: Bool
    ) {
        self.schemaVersion = schemaVersion
        self.state = state
        self.stateDigest = stateDigest
        self.installedRuntimeVersion = installedRuntimeVersion
        self.installedRuntimeDigest = installedRuntimeDigest
        self.bundledRuntimeVersion = bundledRuntimeVersion
        self.bundledRuntimeDigest = bundledRuntimeDigest
        self.integrationProtocol = integrationProtocol
        self.restartRequired = restartRequired
    }

    public static func decodeStrict(_ data: Data) throws -> Self {
        try SelfHostedRuntimeUpgradeJSON.decodeStrict(
            data,
            allowedKeys: Set(CodingKeys.allCases.map(\.rawValue)),
            as: Self.self
        ).validated()
    }

    func validated() throws -> Self {
        guard schemaVersion == 1,
              SelfHostedRuntimeUpgradeJSON.isSHA256(stateDigest),
              StrictReleaseVersion(bundledRuntimeVersion) != nil,
              SelfHostedRuntimeUpgradeJSON.isSHA256(bundledRuntimeDigest),
              integrationProtocol == 1 else {
            throw SelfHostedRuntimeUpgradeContractError.invalidSchema
        }
        let hasInstalledVersion = !installedRuntimeVersion.isEmpty
        let hasInstalledDigest = !installedRuntimeDigest.isEmpty
        guard hasInstalledVersion == hasInstalledDigest,
              !hasInstalledVersion || (
                StrictReleaseVersion(installedRuntimeVersion) != nil &&
                    SelfHostedRuntimeUpgradeJSON.isSHA256(installedRuntimeDigest)
              ) else {
            throw SelfHostedRuntimeUpgradeContractError.invalidState
        }
        switch state {
        case .notIntegrated:
            guard !hasInstalledVersion, !restartRequired else {
                throw SelfHostedRuntimeUpgradeContractError.invalidState
            }
        case .current:
            guard hasInstalledVersion,
                  installedRuntimeVersion == bundledRuntimeVersion,
                  installedRuntimeDigest == bundledRuntimeDigest,
                  !restartRequired else {
                throw SelfHostedRuntimeUpgradeContractError.invalidState
            }
        case .upgradeAvailable:
            guard hasInstalledVersion,
                  installedRuntimeVersion != bundledRuntimeVersion,
                  installedRuntimeDigest != bundledRuntimeDigest,
                  restartRequired else {
                throw SelfHostedRuntimeUpgradeContractError.invalidState
            }
        case .recoveryRequired:
            guard restartRequired else {
                throw SelfHostedRuntimeUpgradeContractError.invalidState
            }
        case .incompatible:
            guard !restartRequired else {
                throw SelfHostedRuntimeUpgradeContractError.invalidState
            }
        }
        return self
    }
}

public struct SelfHostedRuntimeUpgradeReceipt: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let ok: Bool
    public let state: SelfHostedRuntimeUpgradeState
    public let stateDigest: String
    public let installedRuntimeVersion: String
    public let installedRuntimeDigest: String
    public let bundledRuntimeVersion: String
    public let bundledRuntimeDigest: String
    public let integrationProtocol: Int
    public let restartRequired: Bool

    enum CodingKeys: String, CodingKey, CaseIterable {
        case schemaVersion = "schema_version"
        case ok
        case state
        case stateDigest = "state_digest"
        case installedRuntimeVersion = "installed_runtime_version"
        case installedRuntimeDigest = "installed_runtime_digest"
        case bundledRuntimeVersion = "bundled_runtime_version"
        case bundledRuntimeDigest = "bundled_runtime_digest"
        case integrationProtocol = "integration_protocol"
        case restartRequired = "restart_required"
    }

    public init(
        schemaVersion: Int,
        ok: Bool,
        state: SelfHostedRuntimeUpgradeState,
        stateDigest: String,
        installedRuntimeVersion: String,
        installedRuntimeDigest: String,
        bundledRuntimeVersion: String,
        bundledRuntimeDigest: String,
        integrationProtocol: Int,
        restartRequired: Bool
    ) {
        self.schemaVersion = schemaVersion
        self.ok = ok
        self.state = state
        self.stateDigest = stateDigest
        self.installedRuntimeVersion = installedRuntimeVersion
        self.installedRuntimeDigest = installedRuntimeDigest
        self.bundledRuntimeVersion = bundledRuntimeVersion
        self.bundledRuntimeDigest = bundledRuntimeDigest
        self.integrationProtocol = integrationProtocol
        self.restartRequired = restartRequired
    }

    public static func decodeStrict(_ data: Data) throws -> Self {
        let receipt = try SelfHostedRuntimeUpgradeJSON.decodeStrict(
            data,
            allowedKeys: Set(CodingKeys.allCases.map(\.rawValue)),
            as: Self.self
        )
        guard receipt.ok else {
            throw SelfHostedRuntimeUpgradeContractError.invalidState
        }
        _ = try receipt.inspection.validated()
        return receipt
    }

    public var inspection: SelfHostedRuntimeUpgradeInspection {
        SelfHostedRuntimeUpgradeInspection(
            schemaVersion: schemaVersion,
            state: state,
            stateDigest: stateDigest,
            installedRuntimeVersion: installedRuntimeVersion,
            installedRuntimeDigest: installedRuntimeDigest,
            bundledRuntimeVersion: bundledRuntimeVersion,
            bundledRuntimeDigest: bundledRuntimeDigest,
            integrationProtocol: integrationProtocol,
            restartRequired: restartRequired
        )
    }
}

public protocol SelfHostedRuntimeUpgrading: Sendable {
    func inspect() async throws -> SelfHostedRuntimeUpgradeInspection
    func apply(
        expectedStateDigest: String,
        confirmRelayRestart: Bool
    ) async throws -> SelfHostedRuntimeUpgradeReceipt
    func recover() async throws -> SelfHostedRuntimeUpgradeReceipt
}

public struct ProcessSelfHostedRuntimeUpgradeClient: SelfHostedRuntimeUpgrading, Sendable {
    public let executableURL: URL
    public let executionPolicy: RelayctlExecutionPolicy

    public init(
        executableURL: URL = RelayctlHelperLocation.resolve(),
        executionPolicy: RelayctlExecutionPolicy = RelayctlExecutionPolicy(
            timeout: 160,
            terminationGracePeriod: 2,
            maximumOutputBytes: 64 * 1024
        )
    ) {
        self.executableURL = executableURL
        self.executionPolicy = executionPolicy
    }

    public func inspect() async throws -> SelfHostedRuntimeUpgradeInspection {
        let data = try await execute(
            arguments: ["integration", "upgrade", "inspect", "--json"],
            timeout: 20
        )
        return try SelfHostedRuntimeUpgradeInspection.decodeStrict(data)
    }

    public func apply(
        expectedStateDigest: String,
        confirmRelayRestart: Bool
    ) async throws -> SelfHostedRuntimeUpgradeReceipt {
        guard SelfHostedRuntimeUpgradeJSON.isSHA256(expectedStateDigest),
              confirmRelayRestart else {
            throw SelfHostedRuntimeUpgradeContractError.invalidState
        }
        let data = try await execute(
            arguments: [
                "integration", "upgrade", "apply",
                "--expected-state-digest", expectedStateDigest,
                "--confirm-relay-restart",
                "--json",
            ],
            timeout: 160
        )
        return try SelfHostedRuntimeUpgradeReceipt.decodeStrict(data)
    }

    public func recover() async throws -> SelfHostedRuntimeUpgradeReceipt {
        let data = try await execute(
            arguments: ["integration", "upgrade", "recover", "--json"],
            timeout: 100
        )
        return try SelfHostedRuntimeUpgradeReceipt.decodeStrict(data)
    }

    private func execute(arguments: [String], timeout: TimeInterval) async throws -> Data {
        guard executableURL.isFileURL,
              FileManager.default.isExecutableFile(atPath: executableURL.path) else {
            throw RelayctlError.helperUnavailable
        }
        let operation = RelayctlProcessOperation(
            executableURL: executableURL,
            arguments: arguments,
            policy: RelayctlExecutionPolicy(
                timeout: max(executionPolicy.timeout, timeout),
                terminationGracePeriod: executionPolicy.terminationGracePeriod,
                maximumOutputBytes: executionPolicy.maximumOutputBytes
            )
        )
        let result = try await withTaskCancellationHandler {
            try await Task.detached(priority: .utility) { try operation.run() }.value
        } onCancel: {
            operation.cancel()
        }
        if Task.isCancelled { throw RelayctlError.cancelled }
        guard result.exitCode == 0 else {
            if let envelope = try? JSONDecoder().decode(
                RelayctlOperationErrorEnvelope.self,
                from: result.stdout
            ), let code = envelope.reportedCode() {
                throw RelayctlError.reported(code)
            }
            throw RelayctlError.invocationFailed(exitCode: result.exitCode)
        }
        return result.stdout
    }
}

private enum SelfHostedRuntimeUpgradeJSON {
    static func decodeStrict<Value: Decodable>(
        _ data: Data,
        allowedKeys: Set<String>,
        as type: Value.Type
    ) throws -> Value {
        guard data.count <= 64 * 1024 else {
            throw SelfHostedRuntimeUpgradeContractError.invalidJSON
        }
        do {
            var scanner = FlatJSONKeyScanner(data: data)
            try scanner.validateUniqueKeys()
        } catch ReleaseUpdateContractError.duplicateField {
            throw SelfHostedRuntimeUpgradeContractError.duplicateField
        } catch {
            throw SelfHostedRuntimeUpgradeContractError.invalidJSON
        }
        guard let object = try JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            throw SelfHostedRuntimeUpgradeContractError.invalidJSON
        }
        guard Set(object.keys).isSubset(of: allowedKeys),
              allowedKeys.isSubset(of: Set(object.keys)) else {
            throw SelfHostedRuntimeUpgradeContractError.unknownField
        }
        do {
            return try JSONDecoder().decode(Value.self, from: data)
        } catch {
            throw SelfHostedRuntimeUpgradeContractError.invalidJSON
        }
    }

    static func isSHA256(_ value: String) -> Bool {
        value.count == 64 && value.allSatisfy { character in
            guard let ascii = character.asciiValue else { return false }
            return (0x30...0x39).contains(ascii) || (0x61...0x66).contains(ascii)
        }
    }
}
