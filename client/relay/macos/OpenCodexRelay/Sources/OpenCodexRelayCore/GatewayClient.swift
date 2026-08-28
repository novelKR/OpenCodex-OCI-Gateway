import Foundation

public protocol GatewayManaging: Sendable {
    func inspect() async throws -> GatewayInspection
    func test(upstreamBaseURL: String) async throws -> GatewayValidation
    func test(candidate: GatewayCandidate) async throws -> GatewayValidation
    func apply(
        upstreamBaseURL: String,
        expectedConfigDigest: String,
        expectedRoutingGeneration: UInt64
    ) async throws -> GatewayApplyReceipt
    func apply(
        candidate: GatewayCandidate,
        expectedConfigDigest: String,
        expectedRoutingGeneration: UInt64
    ) async throws -> GatewayApplyReceipt
}

public extension GatewayManaging {
    func test(candidate: GatewayCandidate) async throws -> GatewayValidation {
        try await test(upstreamBaseURL: candidate.upstreamBaseURL)
    }

    func apply(
        candidate: GatewayCandidate,
        expectedConfigDigest: String,
        expectedRoutingGeneration: UInt64
    ) async throws -> GatewayApplyReceipt {
        try await apply(
            upstreamBaseURL: candidate.upstreamBaseURL,
            expectedConfigDigest: expectedConfigDigest,
            expectedRoutingGeneration: expectedRoutingGeneration
        )
    }
}

public enum RemoteAuthenticationProfile: String, CaseIterable, Codable, Sendable {
    case none
    case gatewayAPIKey = "gateway_api_key"
    case cloudflareAccessAndGatewayAPIKey = "cloudflare_access_and_gateway_api_key"
}

public struct GatewayCandidate: Codable, Equatable, Sendable {
    public let upstreamBaseURL: String
    public let authenticationProfile: RemoteAuthenticationProfile
    public let allowInsecurePrivateIP: Bool

    public init(
        upstreamBaseURL: String,
        authenticationProfile: RemoteAuthenticationProfile,
        allowInsecurePrivateIP: Bool = false
    ) {
        self.upstreamBaseURL = upstreamBaseURL
        self.authenticationProfile = authenticationProfile
        self.allowInsecurePrivateIP = allowInsecurePrivateIP
    }

    enum CodingKeys: String, CodingKey {
        case upstreamBaseURL = "upstream_base_url"
        case authenticationProfile = "authentication_profile"
        case allowInsecurePrivateIP = "allow_insecure_private_ip"
    }
}

public struct GatewayInspection: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let upstreamBaseURL: String
    public let configDigest: String
    public let routingGeneration: UInt64
    public let credentialSource: String
    public let credentialAccount: String?
    public let credentialsEditable: Bool
    public let authenticationProfile: RemoteAuthenticationProfile
    public let requiredCredentials: [String]
    public let allowInsecurePrivateIP: Bool

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case upstreamBaseURL = "upstream_base_url"
        case configDigest = "config_digest"
        case routingGeneration = "routing_generation"
        case credentialSource = "credential_source"
        case credentialAccount = "credential_account"
        case credentialsEditable = "credentials_editable"
        case authenticationProfile = "authentication_profile"
        case requiredCredentials = "required_credentials"
        case allowInsecurePrivateIP = "allow_insecure_private_ip"
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        schemaVersion = try container.decode(Int.self, forKey: .schemaVersion)
        upstreamBaseURL = try container.decode(String.self, forKey: .upstreamBaseURL)
        configDigest = try container.decode(String.self, forKey: .configDigest)
        routingGeneration = try container.decode(UInt64.self, forKey: .routingGeneration)
        credentialSource = try container.decode(String.self, forKey: .credentialSource)
        credentialAccount = try container.decodeIfPresent(String.self, forKey: .credentialAccount)
        credentialsEditable = try container.decode(Bool.self, forKey: .credentialsEditable)
        authenticationProfile = try container.decodeIfPresent(
            RemoteAuthenticationProfile.self,
            forKey: .authenticationProfile
        ) ?? (credentialSource == "none" ? .none : .cloudflareAccessAndGatewayAPIKey)
        requiredCredentials = try container.decodeIfPresent(
            [String].self,
            forKey: .requiredCredentials
        ) ?? Self.requiredCredentialKinds(for: authenticationProfile)
        allowInsecurePrivateIP = try container.decodeIfPresent(
            Bool.self,
            forKey: .allowInsecurePrivateIP
        ) ?? false
    }

    public func validated() throws -> Self {
        guard schemaVersion == 1 || schemaVersion == 2,
              Self.isCandidateInput(upstreamBaseURL),
              Self.isDigest(configDigest),
              routingGeneration > 0,
              credentialSource == "keychain" || credentialSource == "file" || credentialSource == "none",
              credentialsEditable == (credentialSource == "keychain"),
              !credentialsEditable || !(credentialAccount ?? "").isEmpty,
              Set(requiredCredentials) == Set(Self.requiredCredentialKinds(for: authenticationProfile)),
              schemaVersion == 2 || (!allowInsecurePrivateIP && authenticationProfile == .cloudflareAccessAndGatewayAPIKey) else {
            throw RelayctlError.invalidStatus
        }
        return self
    }

    public static func requiredCredentialKinds(
        for profile: RemoteAuthenticationProfile
    ) -> [String] {
        switch profile {
        case .none:
            []
        case .gatewayAPIKey:
            ["gateway_api_key"]
        case .cloudflareAccessAndGatewayAPIKey:
            [
                "cloudflare_access_client_id",
                "cloudflare_access_client_secret",
                "gateway_api_key",
            ]
        }
    }

    public static func isDigest(_ value: String) -> Bool {
        value.utf8.count == 64 && value.allSatisfy {
            $0.isNumber || ("a"..."f").contains($0)
        }
    }

    public static func isCandidateInput(_ value: String) -> Bool {
        !value.isEmpty && value.utf8.count <= 4_096 && !value.contains("\0")
    }
}

public struct GatewayValidation: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let ok: Bool
    public let configDigest: String
    public let routingGeneration: UInt64
    public let modelCount: Int

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case ok
        case configDigest = "config_digest"
        case routingGeneration = "routing_generation"
        case modelCount = "model_count"
    }

    public func validated() throws -> Self {
        guard (schemaVersion == 1 || schemaVersion == 2), ok,
              GatewayInspection.isDigest(configDigest),
              routingGeneration > 0,
              modelCount > 0 else {
            throw RelayctlError.invalidStatus
        }
        return self
    }
}

public struct GatewayApplyReceipt: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let ok: Bool
    public let configDigest: String
    public let routingGeneration: UInt64
    public let runtimeReloaded: Bool

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case ok
        case configDigest = "config_digest"
        case routingGeneration = "routing_generation"
        case runtimeReloaded = "runtime_reloaded"
    }

    public func validated() throws -> Self {
        guard (schemaVersion == 1 || schemaVersion == 2), ok,
              GatewayInspection.isDigest(configDigest),
              routingGeneration > 0 else {
            throw RelayctlError.invalidStatus
        }
        return self
    }
}

public struct ProcessGatewayClient: GatewayManaging, Sendable {
    public let executableURL: URL
    public let additionalArguments: [String]
    public let executionPolicy: RelayctlExecutionPolicy

    public init(
        executableURL: URL,
        additionalArguments: [String] = [],
        executionPolicy: RelayctlExecutionPolicy = RelayctlExecutionPolicy()
    ) {
        self.executableURL = executableURL
        self.additionalArguments = additionalArguments
        self.executionPolicy = executionPolicy
    }

    public func inspect() async throws -> GatewayInspection {
        let output = try await execute(
            arguments: ["gateway", "inspect", "--json"],
            input: nil,
            timeout: 20
        )
        do {
            return try JSONDecoder().decode(GatewayInspection.self, from: output).validated()
        } catch let error as RelayctlError {
            throw error
        } catch {
            throw RelayctlError.invalidJSON
        }
    }

    public func test(upstreamBaseURL: String) async throws -> GatewayValidation {
        try await test(candidate: GatewayCandidate(
            upstreamBaseURL: upstreamBaseURL,
            authenticationProfile: .cloudflareAccessAndGatewayAPIKey
        ))
    }

    public func test(candidate: GatewayCandidate) async throws -> GatewayValidation {
        let input = try candidateData(candidate)
        let output = try await execute(
            arguments: ["gateway", "test", "--json"],
            input: input,
            timeout: 12
        )
        do {
            return try JSONDecoder().decode(GatewayValidation.self, from: output).validated()
        } catch let error as RelayctlError {
            throw error
        } catch {
            throw RelayctlError.invalidJSON
        }
    }

    public func apply(
        upstreamBaseURL: String,
        expectedConfigDigest: String,
        expectedRoutingGeneration: UInt64
    ) async throws -> GatewayApplyReceipt {
        guard GatewayInspection.isDigest(expectedConfigDigest),
              expectedRoutingGeneration > 0 else {
            throw RelayctlError.invalidStatus
        }
        return try await apply(
            candidate: GatewayCandidate(
                upstreamBaseURL: upstreamBaseURL,
                authenticationProfile: .cloudflareAccessAndGatewayAPIKey
            ),
            expectedConfigDigest: expectedConfigDigest,
            expectedRoutingGeneration: expectedRoutingGeneration
        )
    }

    public func apply(
        candidate: GatewayCandidate,
        expectedConfigDigest: String,
        expectedRoutingGeneration: UInt64
    ) async throws -> GatewayApplyReceipt {
        guard GatewayInspection.isDigest(expectedConfigDigest),
              expectedRoutingGeneration > 0 else {
            throw RelayctlError.invalidStatus
        }
        let input = try candidateData(candidate)
        let output = try await execute(
            arguments: [
                "gateway", "apply",
                "--expected-config-digest", expectedConfigDigest,
                "--expected-routing-generation", String(expectedRoutingGeneration),
                "--json",
            ],
            input: input,
            timeout: 100
        )
        do {
            return try JSONDecoder().decode(GatewayApplyReceipt.self, from: output).validated()
        } catch let error as RelayctlError {
            throw error
        } catch {
            throw RelayctlError.invalidJSON
        }
    }

    private func candidateData(_ candidate: GatewayCandidate) throws -> Data {
        guard GatewayInspection.isCandidateInput(candidate.upstreamBaseURL) else {
            throw RelayctlError.reported(.invalidAddress)
        }
        let data = try JSONEncoder().encode(candidate)
        guard data.count <= 16 * 1_024 else {
            throw RelayctlError.reported(.invalidAddress)
        }
        return data
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
        let policy = RelayctlExecutionPolicy(
            timeout: max(executionPolicy.timeout, timeout),
            terminationGracePeriod: executionPolicy.terminationGracePeriod,
            maximumOutputBytes: executionPolicy.maximumOutputBytes
        )
        let operation = RelayctlProcessOperation(
            executableURL: executableURL,
            arguments: arguments + additionalArguments,
            standardInput: input,
            policy: policy
        )
        let result = try await withTaskCancellationHandler {
            try await Task.detached(priority: .utility) {
                try operation.run()
            }.value
        } onCancel: {
            operation.cancel()
        }
        if Task.isCancelled {
            throw RelayctlError.cancelled
        }
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
