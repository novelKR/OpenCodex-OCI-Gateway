import Foundation

public enum SelfHostedIntegrationState: String, Codable, Sendable {
    case integrationRequired = "integration_required"
    case ready
    case recoveryRequired = "recovery_required"
}

public struct SelfHostedIntegrationInspection: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let state: SelfHostedIntegrationState
    public let stateDigest: String
    public let credentialAccount: String

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case state
        case stateDigest = "state_digest"
        case credentialAccount = "credential_account"
    }

    public init(
        schemaVersion: Int,
        state: SelfHostedIntegrationState,
        stateDigest: String,
        credentialAccount: String
    ) {
        self.schemaVersion = schemaVersion
        self.state = state
        self.stateDigest = stateDigest
        self.credentialAccount = credentialAccount
    }

    public func validated() throws -> Self {
        guard schemaVersion == 1,
              GatewayInspection.isDigest(stateDigest),
              !credentialAccount.isEmpty,
              credentialAccount.utf8.count <= 256 else {
            throw RelayctlError.invalidStatus
        }
        return self
    }
}

public struct SelfHostedIntegrationReceipt: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let ok: Bool
    public let state: SelfHostedIntegrationState
    public let configDigest: String?
    public let routingGeneration: UInt64?

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case ok
        case state
        case configDigest = "config_digest"
        case routingGeneration = "routing_generation"
    }

    public init(
        schemaVersion: Int,
        ok: Bool,
        state: SelfHostedIntegrationState,
        configDigest: String?,
        routingGeneration: UInt64?
    ) {
        self.schemaVersion = schemaVersion
        self.ok = ok
        self.state = state
        self.configDigest = configDigest
        self.routingGeneration = routingGeneration
    }

    public func validatedApply() throws -> Self {
        guard schemaVersion == 1, ok, state == .ready,
              let configDigest,
              GatewayInspection.isDigest(configDigest),
              let routingGeneration,
              routingGeneration > 0 else {
            throw RelayctlError.invalidStatus
        }
        return self
    }

    public func validatedRecovery() throws -> Self {
        guard schemaVersion == 1, ok, state == .integrationRequired,
              configDigest == nil, routingGeneration == nil else {
            throw RelayctlError.invalidStatus
        }
        return self
    }
}

public protocol SelfHostedIntegrationManaging: Sendable {
    func inspect() async throws -> SelfHostedIntegrationInspection
    func apply(
        candidate: GatewayCandidate,
        expectedStateDigest: String
    ) async throws -> SelfHostedIntegrationReceipt
    func recover() async throws -> SelfHostedIntegrationReceipt
}

public struct ProcessSelfHostedIntegrationClient: SelfHostedIntegrationManaging, Sendable {
    public let executableURL: URL
    public let executionPolicy: RelayctlExecutionPolicy

    public init(
        executableURL: URL,
        executionPolicy: RelayctlExecutionPolicy = RelayctlExecutionPolicy(
            timeout: 130,
            terminationGracePeriod: 2,
            maximumOutputBytes: 64 * 1024
        )
    ) {
        self.executableURL = executableURL
        self.executionPolicy = executionPolicy
    }

    public func inspect() async throws -> SelfHostedIntegrationInspection {
        let data = try await execute(
            arguments: ["integration", "inspect", "--json"],
            input: nil,
            timeout: 20
        )
        do {
            return try JSONDecoder().decode(
                SelfHostedIntegrationInspection.self,
                from: data
            ).validated()
        } catch let error as RelayctlError {
            throw error
        } catch {
            throw RelayctlError.invalidJSON
        }
    }

    public func apply(
        candidate: GatewayCandidate,
        expectedStateDigest: String
    ) async throws -> SelfHostedIntegrationReceipt {
        guard GatewayInspection.isDigest(expectedStateDigest),
              GatewayInspection.isCandidateInput(candidate.upstreamBaseURL) else {
            throw RelayctlError.invalidStatus
        }
        let input = try JSONEncoder().encode(candidate)
        let data = try await execute(
            arguments: [
                "integration", "apply",
                "--expected-state-digest", expectedStateDigest,
                "--json",
            ],
            input: input,
            timeout: 130
        )
        do {
            return try JSONDecoder().decode(
                SelfHostedIntegrationReceipt.self,
                from: data
            ).validatedApply()
        } catch let error as RelayctlError {
            throw error
        } catch {
            throw RelayctlError.invalidJSON
        }
    }

    public func recover() async throws -> SelfHostedIntegrationReceipt {
        let data = try await execute(
            arguments: ["integration", "recover", "--json"],
            input: nil,
            timeout: 70
        )
        do {
            return try JSONDecoder().decode(
                SelfHostedIntegrationReceipt.self,
                from: data
            ).validatedRecovery()
        } catch let error as RelayctlError {
            throw error
        } catch {
            throw RelayctlError.invalidJSON
        }
    }

    private func execute(
        arguments: [String],
        input: Data?,
        timeout: TimeInterval
    ) async throws -> Data {
        guard executableURL.isFileURL,
              FileManager.default.isExecutableFile(atPath: executableURL.path) else {
            throw RelayctlError.helperUnavailable
        }
        let operation = RelayctlProcessOperation(
            executableURL: executableURL,
            arguments: arguments,
            standardInput: input,
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
        if result.exitCode != 0 {
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
