import Foundation

public enum ContainerRuntimeState: String, Codable, CaseIterable, Sendable {
    case unavailable
    case stopped
    case staging
    case healthy
    case updating
    case recoveryRequired = "recovery_required"
}

public enum ContainerRuntimeCheckStatus: String, Codable, Sendable {
    case current
    case updateAvailable = "update_available"
    case unavailable
    case incompatible
    case offline
    case rateLimited = "rate_limited"
    case invalidRelease = "invalid_release"
}

public enum ContainerRuntimeOAuthKind: String, Codable, CaseIterable, Hashable, Sendable {
    case generic
    case codex
}

public enum ContainerRuntimeOAuthStatus: String, Codable, Sendable {
    case pending
    case awaitingUser = "awaiting_user"
    case complete
    case cancelled
    case failed
}

public enum ContainerRuntimeContractError: Error, Equatable, Sendable {
    case invalidJSON
    case duplicateField
    case unknownField
    case invalidSchema
    case invalidState
    case invalidArgument
}

/// The Apple Container lifecycle is a single per-user production resource:
/// its durable state root, container name, host port, and Keychain services
/// are fixed. Until those resources are independently namespaced, preview and
/// local-development bundles must not mutate the production singleton.
public enum ContainerRuntimeClientError: LocalizedError, Equatable, Sendable {
    case previewMode
    case unsupportedDistribution

    public var safeCode: String {
        switch self {
        case .previewMode:
            return "container_runtime_preview_mode"
        case .unsupportedDistribution:
            return "container_runtime_distribution_unsupported"
        }
    }

    public var errorDescription: String? { safeCode }
}

public struct ContainerRuntimeCapability: Codable, Equatable, Sendable {
    public let available: Bool
    public let reason: String
    public let macOSVersion: String
    public let appleContainerVersion: String
    public let systemServiceState: String

    enum CodingKeys: String, CodingKey, CaseIterable {
        case available
        case reason
        case macOSVersion = "macos_version"
        case appleContainerVersion = "apple_container_version"
        case systemServiceState = "system_service_state"
    }

    public init(
        available: Bool,
        reason: String,
        macOSVersion: String,
        appleContainerVersion: String,
        systemServiceState: String
    ) {
        self.available = available
        self.reason = reason
        self.macOSVersion = macOSVersion
        self.appleContainerVersion = appleContainerVersion
        self.systemServiceState = systemServiceState
    }

    fileprivate func validated() throws -> Self {
        guard Self.isBounded(reason, maximumBytes: 256),
              Self.isBounded(macOSVersion, maximumBytes: 64),
              Self.isBounded(appleContainerVersion, maximumBytes: 64),
              Self.isIdentifier(systemServiceState, maximumBytes: 64) else {
            throw ContainerRuntimeContractError.invalidState
        }
        if available {
            guard !macOSVersion.isEmpty,
                  !appleContainerVersion.isEmpty,
                  reason.isEmpty else {
                throw ContainerRuntimeContractError.invalidState
            }
        } else {
            guard !reason.isEmpty else {
                throw ContainerRuntimeContractError.invalidState
            }
        }
        return self
    }

    fileprivate static func isBounded(_ value: String, maximumBytes: Int) -> Bool {
        !value.contains("\0") && value.utf8.count <= maximumBytes
    }

    fileprivate static func isIdentifier(_ value: String, maximumBytes: Int) -> Bool {
        !value.isEmpty && value.utf8.count <= maximumBytes && value.utf8.allSatisfy {
            (0x30...0x39).contains($0) || (0x41...0x5a).contains($0) ||
                (0x61...0x7a).contains($0) || $0 == 0x5f || $0 == 0x2d || $0 == 0x2e
        }
    }
}

public struct ContainerRuntimeArtifactSummary: Codable, Equatable, Sendable {
    public let artifactVersion: String
    public let releaseSequence: UInt64
    public let manifestSHA256: String
    public let indexDigest: String
    public let arm64Digest: String

    enum CodingKeys: String, CodingKey, CaseIterable {
        case artifactVersion = "artifact_version"
        case releaseSequence = "release_sequence"
        case manifestSHA256 = "manifest_sha256"
        case indexDigest = "index_digest"
        case arm64Digest = "arm64_digest"
    }

    public init(
        artifactVersion: String,
        releaseSequence: UInt64,
        manifestSHA256: String,
        indexDigest: String,
        arm64Digest: String
    ) {
        self.artifactVersion = artifactVersion
        self.releaseSequence = releaseSequence
        self.manifestSHA256 = manifestSHA256
        self.indexDigest = indexDigest
        self.arm64Digest = arm64Digest
    }

    fileprivate func validated() throws -> Self {
        guard Self.isArtifactVersion(artifactVersion),
              releaseSequence > 0,
              ContainerRuntimeJSON.isSHA256(manifestSHA256),
              ContainerRuntimeJSON.isOCIDigest(indexDigest),
              ContainerRuntimeJSON.isOCIDigest(arm64Digest),
              indexDigest != arm64Digest else {
            throw ContainerRuntimeContractError.invalidState
        }
        return self
    }

    private static func isArtifactVersion(_ value: String) -> Bool {
        guard let separator = value.range(of: "-r", options: .backwards),
              separator.lowerBound != value.startIndex,
              separator.upperBound != value.endIndex else {
            return false
        }
        let version = String(value[..<separator.lowerBound])
        let revisionText = String(value[separator.upperBound...])
        let components = version.split(separator: ".", omittingEmptySubsequences: false)
        return components.count == 3 && components.allSatisfy { component in
            guard component == "0" || component.first != "0" else { return false }
            return !component.isEmpty && component.allSatisfy { $0.isASCII && $0.isNumber } &&
                UInt32(component) != nil
        } &&
            UInt64(revisionText).map { $0 > 0 && String($0) == revisionText } == true
    }
}

public struct ContainerRuntimeInspection: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let ok: Bool
    public let state: ContainerRuntimeState
    public let capability: ContainerRuntimeCapability
    public let staged: ContainerRuntimeArtifactSummary?
    public let active: ContainerRuntimeArtifactSummary?
    public let stateDigest: String
    public let routingGeneration: UInt64
    public let recoveryRequired: Bool

    enum CodingKeys: String, CodingKey, CaseIterable {
        case schemaVersion = "schema_version"
        case ok
        case state
        case capability
        case staged
        case active
        case stateDigest = "state_digest"
        case routingGeneration = "routing_generation"
        case recoveryRequired = "recovery_required"
    }

    public init(
        schemaVersion: Int,
        ok: Bool,
        state: ContainerRuntimeState,
        capability: ContainerRuntimeCapability,
        staged: ContainerRuntimeArtifactSummary?,
        active: ContainerRuntimeArtifactSummary?,
        stateDigest: String,
        routingGeneration: UInt64,
        recoveryRequired: Bool
    ) {
        self.schemaVersion = schemaVersion
        self.ok = ok
        self.state = state
        self.capability = capability
        self.staged = staged
        self.active = active
        self.stateDigest = stateDigest
        self.routingGeneration = routingGeneration
        self.recoveryRequired = recoveryRequired
    }

    public static func decodeStrict(_ data: Data) throws -> Self {
        let value: Self = try ContainerRuntimeJSON.decodeStrict(
            data,
            allowedKeys: Set(CodingKeys.allCases.map(\.rawValue)),
            requiredKeys: Set(CodingKeys.allCases.map(\.rawValue)).subtracting(["staged", "active"]),
            nestedKeys: [
                "capability": Set(ContainerRuntimeCapability.CodingKeys.allCases.map(\.rawValue)),
                "staged": Set(ContainerRuntimeArtifactSummary.CodingKeys.allCases.map(\.rawValue)),
                "active": Set(ContainerRuntimeArtifactSummary.CodingKeys.allCases.map(\.rawValue)),
            ],
            as: Self.self
        )
        return try value.validated()
    }

    fileprivate func validated() throws -> Self {
        guard schemaVersion == 1, ok,
              ContainerRuntimeJSON.isSHA256(stateDigest) else {
            throw ContainerRuntimeContractError.invalidSchema
        }
        _ = try capability.validated()
        _ = try staged?.validated()
        _ = try active?.validated()
        guard recoveryRequired == (state == .recoveryRequired) else {
            throw ContainerRuntimeContractError.invalidState
        }
        switch state {
        case .unavailable:
            // Capability loss must not erase the durable active artifact
            // identity. The Go lifecycle manager keeps that exact manifest,
            // digest, and home so a restored installation can be re-probed.
            // A check receipt may also project the absence of every signed
            // stable runtime as unavailable while the host capability itself
            // remains healthy; that projection cannot carry an artifact.
            guard !capability.available || staged == nil && active == nil else {
                throw ContainerRuntimeContractError.invalidState
            }
        case .healthy:
            guard capability.available, active != nil else {
                throw ContainerRuntimeContractError.invalidState
            }
        case .staging, .updating:
            guard capability.available else {
                throw ContainerRuntimeContractError.invalidState
            }
        case .stopped, .recoveryRequired:
            break
        }
        return self
    }
}

public struct ContainerRuntimeCheckReceipt: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let ok: Bool
    public let state: ContainerRuntimeState
    public let capability: ContainerRuntimeCapability
    public let staged: ContainerRuntimeArtifactSummary?
    public let active: ContainerRuntimeArtifactSummary?
    public let stateDigest: String
    public let routingGeneration: UInt64
    public let recoveryRequired: Bool
    public let status: ContainerRuntimeCheckStatus
    public let candidate: ContainerRuntimeArtifactSummary?
    public let compatible: Bool
    public let reason: String

    enum CodingKeys: String, CodingKey, CaseIterable {
        case schemaVersion = "schema_version"
        case ok
        case state
        case capability
        case staged
        case active
        case stateDigest = "state_digest"
        case routingGeneration = "routing_generation"
        case recoveryRequired = "recovery_required"
        case status
        case candidate
        case compatible
        case reason
    }

    public static func decodeStrict(_ data: Data) throws -> Self {
        let value: Self = try ContainerRuntimeJSON.decodeStrict(
            data,
            allowedKeys: Set(CodingKeys.allCases.map(\.rawValue)),
            requiredKeys: Set(CodingKeys.allCases.map(\.rawValue)).subtracting([
                "staged", "active", "candidate",
            ]),
            nestedKeys: [
                "capability": Set(ContainerRuntimeCapability.CodingKeys.allCases.map(\.rawValue)),
                "staged": Set(ContainerRuntimeArtifactSummary.CodingKeys.allCases.map(\.rawValue)),
                "active": Set(ContainerRuntimeArtifactSummary.CodingKeys.allCases.map(\.rawValue)),
                "candidate": Set(ContainerRuntimeArtifactSummary.CodingKeys.allCases.map(\.rawValue)),
            ],
            as: Self.self
        )
        return try value.validated()
    }

    public var inspection: ContainerRuntimeInspection {
        ContainerRuntimeInspection(
            schemaVersion: schemaVersion,
            ok: ok,
            state: state,
            capability: capability,
            staged: staged,
            active: active,
            stateDigest: stateDigest,
            routingGeneration: routingGeneration,
            recoveryRequired: recoveryRequired
        )
    }

    private func validated() throws -> Self {
        _ = try inspection.validated()
        _ = try candidate?.validated()
        guard ContainerRuntimeCapability.isBounded(reason, maximumBytes: 256) else {
            throw ContainerRuntimeContractError.invalidState
        }
        switch status {
        case .updateAvailable:
            guard compatible, candidate != nil else {
                throw ContainerRuntimeContractError.invalidState
            }
        case .current:
            guard compatible else { throw ContainerRuntimeContractError.invalidState }
        case .unavailable:
            let hasRetainedSignedRuntime = staged != nil || active != nil
            let isFreshInstallUnavailable = state == .unavailable && !hasRetainedSignedRuntime
            guard !compatible,
                  candidate == nil,
                  isFreshInstallUnavailable || hasRetainedSignedRuntime,
                  reason == "stable_runtime_manifest_unavailable" else {
                throw ContainerRuntimeContractError.invalidState
            }
        case .incompatible:
            guard !compatible, candidate != nil, !reason.isEmpty else {
                throw ContainerRuntimeContractError.invalidState
            }
        case .offline, .rateLimited, .invalidRelease:
            guard candidate == nil, !reason.isEmpty else {
                throw ContainerRuntimeContractError.invalidState
            }
        }
        return self
    }
}

public struct ContainerRuntimeMutationReceipt: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let ok: Bool
    public let state: ContainerRuntimeState
    public let capability: ContainerRuntimeCapability
    public let staged: ContainerRuntimeArtifactSummary?
    public let active: ContainerRuntimeArtifactSummary?
    public let stateDigest: String
    public let routingGeneration: UInt64
    public let recoveryRequired: Bool

    enum CodingKeys: String, CodingKey, CaseIterable {
        case schemaVersion = "schema_version"
        case ok
        case state
        case capability
        case staged
        case active
        case stateDigest = "state_digest"
        case routingGeneration = "routing_generation"
        case recoveryRequired = "recovery_required"
    }

    public static func decodeStrict(_ data: Data) throws -> Self {
        let value: Self = try ContainerRuntimeJSON.decodeStrict(
            data,
            allowedKeys: Set(CodingKeys.allCases.map(\.rawValue)),
            requiredKeys: Set(CodingKeys.allCases.map(\.rawValue)).subtracting(["staged", "active"]),
            nestedKeys: [
                "capability": Set(ContainerRuntimeCapability.CodingKeys.allCases.map(\.rawValue)),
                "staged": Set(ContainerRuntimeArtifactSummary.CodingKeys.allCases.map(\.rawValue)),
                "active": Set(ContainerRuntimeArtifactSummary.CodingKeys.allCases.map(\.rawValue)),
            ],
            as: Self.self
        )
        _ = try value.inspection.validated()
        return value
    }

    public var inspection: ContainerRuntimeInspection {
        ContainerRuntimeInspection(
            schemaVersion: schemaVersion,
            ok: ok,
            state: state,
            capability: capability,
            staged: staged,
            active: active,
            stateDigest: stateDigest,
            routingGeneration: routingGeneration,
            recoveryRequired: recoveryRequired
        )
    }
}

public struct ContainerRuntimeOAuthProvider: Codable, Equatable, Identifiable, Sendable {
    public let id: String
    public let name: String
    public let kind: ContainerRuntimeOAuthKind
    public let supportsDeviceFlow: Bool

    enum CodingKeys: String, CodingKey, CaseIterable {
        case id
        case name
        case kind
        case supportsDeviceFlow = "supports_device_flow"
    }

    fileprivate func validated() throws -> Self {
        guard ContainerRuntimeCapability.isIdentifier(id, maximumBytes: 64),
              id == id.lowercased(),
              !name.isEmpty,
              ContainerRuntimeCapability.isBounded(name, maximumBytes: 128),
              (kind == .codex) == (id == "chatgpt") else {
            throw ContainerRuntimeContractError.invalidState
        }
        return self
    }
}

public struct ContainerRuntimeOAuthProvidersReceipt: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let ok: Bool
    public let providers: [ContainerRuntimeOAuthProvider]

    enum CodingKeys: String, CodingKey, CaseIterable {
        case schemaVersion = "schema_version"
        case ok
        case providers
    }

    public static func decodeStrict(_ data: Data) throws -> Self {
        let value: Self = try ContainerRuntimeJSON.decodeStrict(
            data,
            allowedKeys: Set(CodingKeys.allCases.map(\.rawValue)),
            requiredKeys: Set(CodingKeys.allCases.map(\.rawValue)),
            arrayObjectKeys: [
                "providers": Set(ContainerRuntimeOAuthProvider.CodingKeys.allCases.map(\.rawValue)),
            ],
            as: Self.self
        )
        guard value.schemaVersion == 1, value.ok,
              (1...64).contains(value.providers.count),
              Set(value.providers.map(\.id)).count == value.providers.count else {
            throw ContainerRuntimeContractError.invalidSchema
        }
        for provider in value.providers { _ = try provider.validated() }
        return value
    }
}

public struct ContainerRuntimeOAuthReceipt: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let ok: Bool
    public let operationID: String
    public let provider: String
    public let kind: ContainerRuntimeOAuthKind
    public let status: ContainerRuntimeOAuthStatus
    public let authorizationURL: String?
    public let instructions: String?
    public let userCode: String?

    enum CodingKeys: String, CodingKey, CaseIterable {
        case schemaVersion = "schema_version"
        case ok
        case operationID = "operation_id"
        case provider
        case kind
        case status
        case authorizationURL = "authorization_url"
        case instructions
        case userCode = "user_code"
    }

    public static func decodeStrict(_ data: Data) throws -> Self {
        let value: Self = try ContainerRuntimeJSON.decodeStrict(
            data,
            allowedKeys: Set(CodingKeys.allCases.map(\.rawValue)),
            requiredKeys: Set(CodingKeys.allCases.map(\.rawValue)).subtracting([
                "authorization_url", "instructions", "user_code",
            ]),
            as: Self.self
        )
        guard value.schemaVersion == 1, value.ok,
              ContainerRuntimeJSON.isSHA256(value.operationID),
              ContainerRuntimeCapability.isIdentifier(value.provider, maximumBytes: 64),
              value.provider == value.provider.lowercased(),
              (value.kind == .codex) == (value.provider == "chatgpt"),
              ContainerRuntimeCapability.isBounded(value.instructions ?? "", maximumBytes: 2_048),
              ContainerRuntimeCapability.isBounded(value.userCode ?? "", maximumBytes: 128) else {
            throw ContainerRuntimeContractError.invalidSchema
        }
        if let rawURL = value.authorizationURL {
            guard rawURL.utf8.count <= 4_096,
                  let url = URL(string: rawURL),
                  let scheme = url.scheme?.lowercased(),
                  scheme == "https" || scheme == "http",
                  url.host?.isEmpty == false,
                  url.user == nil,
                  url.password == nil,
                  url.fragment == nil,
                  !rawURL.contains(where: { $0 == "\0" || $0 == "\r" || $0 == "\n" || $0 == "\u{7f}" }) else {
                throw ContainerRuntimeContractError.invalidState
            }
        }
        if value.status == .complete || value.status == .cancelled || value.status == .failed {
            guard value.authorizationURL == nil, value.userCode == nil else {
                throw ContainerRuntimeContractError.invalidState
            }
        }
        return value
    }
}

public protocol ContainerRuntimeManaging: Sendable {
    func inspect() async throws -> ContainerRuntimeInspection
    func check() async throws -> ContainerRuntimeCheckReceipt
    func stage(
        expectedManifestSHA256: String,
        expectedStateDigest: String,
        expectedRoutingGeneration: UInt64
    ) async throws -> ContainerRuntimeMutationReceipt
    func activate(
        expectedStateDigest: String,
        expectedRoutingGeneration: UInt64,
        confirmDesktopExited: Bool
    ) async throws -> ContainerRuntimeMutationReceipt
    func stop(
        expectedStateDigest: String,
        expectedRoutingGeneration: UInt64,
        confirmDesktopExited: Bool
    ) async throws -> ContainerRuntimeMutationReceipt
    func recover(
        expectedStateDigest: String,
        confirmDesktopExited: Bool
    ) async throws -> ContainerRuntimeMutationReceipt
    func oauthProviders() async throws -> ContainerRuntimeOAuthProvidersReceipt
    func oauthStart(provider: String, kind: ContainerRuntimeOAuthKind) async throws -> ContainerRuntimeOAuthReceipt
    func oauthStatus(operationID: String) async throws -> ContainerRuntimeOAuthReceipt
    func oauthSubmit(operationID: String, redirectURL: String?, code: String?) async throws -> ContainerRuntimeOAuthReceipt
    func oauthCancel(operationID: String) async throws -> ContainerRuntimeOAuthReceipt
}

public struct ProcessContainerRuntimeClient: ContainerRuntimeManaging, Sendable {
    public let executableURL: URL
    public let bindingURL: URL
    public let runtimeMode: RelayRuntimeMode
    public let distributionFlavor: DistributionFlavor
    public let executionPolicy: RelayctlExecutionPolicy

    public init(
        executableURL: URL = RelayctlHelperLocation.resolve(),
        bindingURL: URL = RoutingBindingReader.defaultURL(),
        runtimeMode: RelayRuntimeMode = .current,
        distributionFlavor: DistributionFlavor = .current,
        executionPolicy: RelayctlExecutionPolicy = RelayctlExecutionPolicy(
            timeout: 300,
            terminationGracePeriod: 2,
            maximumOutputBytes: 64 * 1_024
        )
    ) {
        self.executableURL = executableURL
        self.bindingURL = bindingURL
        self.runtimeMode = runtimeMode
        self.distributionFlavor = distributionFlavor
        self.executionPolicy = executionPolicy
    }

    public func inspect() async throws -> ContainerRuntimeInspection {
        try ContainerRuntimeInspection.decodeStrict(
            await execute(arguments: ["container-runtime", "inspect", "--json"], timeout: 20)
        )
    }

    public func check() async throws -> ContainerRuntimeCheckReceipt {
        try ContainerRuntimeCheckReceipt.decodeStrict(
            await execute(arguments: ["container-runtime", "check", "--json"], timeout: 45)
        )
    }

    public func stage(
        expectedManifestSHA256: String,
        expectedStateDigest: String,
        expectedRoutingGeneration: UInt64
    ) async throws -> ContainerRuntimeMutationReceipt {
        try validateDigest(expectedManifestSHA256)
        try validateDigest(expectedStateDigest)
        return try ContainerRuntimeMutationReceipt.decodeStrict(await execute(arguments: [
            "container-runtime", "stage",
            "--expected-manifest-sha256", expectedManifestSHA256,
            "--expected-state-digest", expectedStateDigest,
            "--expected-routing-generation", String(expectedRoutingGeneration),
            "--json",
        ], timeout: 300))
    }

    public func activate(
        expectedStateDigest: String,
        expectedRoutingGeneration: UInt64,
        confirmDesktopExited: Bool
    ) async throws -> ContainerRuntimeMutationReceipt {
        try validateDigest(expectedStateDigest)
        guard confirmDesktopExited else { throw ContainerRuntimeContractError.invalidArgument }
        return try ContainerRuntimeMutationReceipt.decodeStrict(await execute(arguments: [
            "container-runtime", "activate",
            "--expected-state-digest", expectedStateDigest,
            "--expected-routing-generation", String(expectedRoutingGeneration),
            "--confirm-desktop-exited",
            "--json",
        ], timeout: 300))
    }

    public func stop(
        expectedStateDigest: String,
        expectedRoutingGeneration: UInt64,
        confirmDesktopExited: Bool
    ) async throws -> ContainerRuntimeMutationReceipt {
        try validateDigest(expectedStateDigest)
        guard confirmDesktopExited else { throw ContainerRuntimeContractError.invalidArgument }
        return try ContainerRuntimeMutationReceipt.decodeStrict(await execute(arguments: [
            "container-runtime", "stop",
            "--expected-state-digest", expectedStateDigest,
            "--expected-routing-generation", String(expectedRoutingGeneration),
            "--confirm-desktop-exited",
            "--json",
        ], timeout: 120))
    }

    public func recover(
        expectedStateDigest: String,
        confirmDesktopExited: Bool
    ) async throws -> ContainerRuntimeMutationReceipt {
        try validateDigest(expectedStateDigest)
        guard confirmDesktopExited else { throw ContainerRuntimeContractError.invalidArgument }
        return try ContainerRuntimeMutationReceipt.decodeStrict(await execute(arguments: [
            "container-runtime", "recover",
            "--expected-state-digest", expectedStateDigest,
            "--confirm-desktop-exited",
            "--json",
        ], timeout: 300))
    }

    public func oauthProviders() async throws -> ContainerRuntimeOAuthProvidersReceipt {
        try ContainerRuntimeOAuthProvidersReceipt.decodeStrict(await execute(
            arguments: ["container-runtime", "oauth", "providers", "--json"],
            timeout: 20
        ))
    }

    public func oauthStart(
        provider: String,
        kind: ContainerRuntimeOAuthKind
    ) async throws -> ContainerRuntimeOAuthReceipt {
        try validateIdentifier(provider)
        return try ContainerRuntimeOAuthReceipt.decodeStrict(await execute(arguments: [
            "container-runtime", "oauth", "start",
            "--provider", provider,
            "--kind", kind.rawValue,
            "--json",
        ], timeout: 30))
    }

    public func oauthStatus(operationID: String) async throws -> ContainerRuntimeOAuthReceipt {
        try validateDigest(operationID)
        return try ContainerRuntimeOAuthReceipt.decodeStrict(await execute(arguments: [
            "container-runtime", "oauth", "status",
            "--operation-id", operationID,
            "--json",
        ], timeout: 20))
    }

    public func oauthSubmit(
        operationID: String,
        redirectURL: String?,
        code: String?
    ) async throws -> ContainerRuntimeOAuthReceipt {
        try validateDigest(operationID)
        let hasURL = !(redirectURL ?? "").isEmpty
        let hasCode = !(code ?? "").isEmpty
        guard hasURL != hasCode else { throw ContainerRuntimeContractError.invalidArgument }
        let submitted = hasURL ? redirectURL! : code!
        guard submitted.utf8.count <= 4 * 1_024,
              !submitted.contains("\0"),
              !submitted.contains("\r"),
              !submitted.contains("\n") else {
            throw ContainerRuntimeContractError.invalidArgument
        }
        let object: [String: Any] = if hasURL {
            ["schema_version": 1, "redirect_url": redirectURL!]
        } else {
            ["schema_version": 1, "code": code!]
        }
        let input = try JSONSerialization.data(withJSONObject: object)
        guard input.count <= 16 * 1_024 else { throw ContainerRuntimeContractError.invalidArgument }
        return try ContainerRuntimeOAuthReceipt.decodeStrict(await execute(arguments: [
            "container-runtime", "oauth", "submit",
            "--operation-id", operationID,
            "--json",
        ], standardInput: input, timeout: 30))
    }

    public func oauthCancel(operationID: String) async throws -> ContainerRuntimeOAuthReceipt {
        try validateDigest(operationID)
        return try ContainerRuntimeOAuthReceipt.decodeStrict(await execute(arguments: [
            "container-runtime", "oauth", "cancel",
            "--operation-id", operationID,
            "--json",
        ], timeout: 20))
    }

    private func validateDigest(_ value: String) throws {
        guard ContainerRuntimeJSON.isSHA256(value) else {
            throw ContainerRuntimeContractError.invalidArgument
        }
    }

    private func validateIdentifier(_ value: String) throws {
        guard ContainerRuntimeCapability.isIdentifier(value, maximumBytes: 64) else {
            throw ContainerRuntimeContractError.invalidArgument
        }
    }

    private func execute(
        arguments: [String],
        standardInput: Data? = nil,
        timeout: TimeInterval
    ) async throws -> Data {
        guard runtimeMode == .managed else {
            throw ContainerRuntimeClientError.previewMode
        }
        guard distributionFlavor == .production else {
            throw ContainerRuntimeClientError.unsupportedDistribution
        }
        // Match the other installer-bound relayctl clients: re-read the
        // owner-only binding for every operation so an atomic installer
        // replacement cannot leave this long-lived UI targeting stale paths.
        let binding = try RoutingBindingReader.load(at: bindingURL)
        guard executableURL.isFileURL,
              FileManager.default.isExecutableFile(atPath: executableURL.path) else {
            throw RelayctlError.helperUnavailable
        }
        let operation = RelayctlProcessOperation(
            executableURL: executableURL,
            arguments: arguments + binding.relayctlArguments,
            standardInput: standardInput,
            policy: RelayctlExecutionPolicy(
                timeout: min(executionPolicy.timeout, timeout),
                terminationGracePeriod: executionPolicy.terminationGracePeriod,
                maximumOutputBytes: min(executionPolicy.maximumOutputBytes, 64 * 1_024)
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

private enum ContainerRuntimeJSON {
    static func decodeStrict<Value: Decodable>(
        _ data: Data,
        allowedKeys: Set<String>,
        requiredKeys: Set<String>? = nil,
        nestedKeys: [String: Set<String>] = [:],
        arrayObjectKeys: [String: Set<String>] = [:],
        as type: Value.Type
    ) throws -> Value {
        guard data.count <= 64 * 1_024 else {
            throw ContainerRuntimeContractError.invalidJSON
        }
        do {
            var scanner = ContainerRuntimeJSONKeyScanner(data: data)
            try scanner.validateUniqueKeys()
        } catch ContainerRuntimeContractError.duplicateField {
            throw ContainerRuntimeContractError.duplicateField
        } catch {
            throw ContainerRuntimeContractError.invalidJSON
        }
        let raw: Any
        do {
            raw = try JSONSerialization.jsonObject(with: data)
        } catch {
            throw ContainerRuntimeContractError.invalidJSON
        }
        guard let object = raw as? [String: Any],
              Set(object.keys).isSubset(of: allowedKeys),
              (requiredKeys ?? allowedKeys).isSubset(of: Set(object.keys)) else {
            throw ContainerRuntimeContractError.unknownField
        }
        for (key, expected) in nestedKeys {
            guard let value = object[key] else { continue }
            if value is NSNull { continue }
            guard let nested = value as? [String: Any], Set(nested.keys) == expected else {
                throw ContainerRuntimeContractError.unknownField
            }
        }
        for (key, expected) in arrayObjectKeys {
            guard let values = object[key] as? [Any] else {
                throw ContainerRuntimeContractError.invalidJSON
            }
            for value in values {
                guard let nested = value as? [String: Any], Set(nested.keys) == expected else {
                    throw ContainerRuntimeContractError.unknownField
                }
            }
        }
        do {
            return try JSONDecoder().decode(Value.self, from: data)
        } catch {
            throw ContainerRuntimeContractError.invalidJSON
        }
    }

    static func isSHA256(_ value: String) -> Bool {
        value.count == 64 && value.allSatisfy { character in
            guard let ascii = character.asciiValue else { return false }
            return (0x30...0x39).contains(ascii) || (0x61...0x66).contains(ascii)
        }
    }

    static func isOCIDigest(_ value: String) -> Bool {
        value.hasPrefix("sha256:") && isSHA256(String(value.dropFirst(7)))
    }
}

private struct ContainerRuntimeJSONKeyScanner {
    private let bytes: [UInt8]
    private var index = 0

    init(data: Data) { bytes = Array(data) }

    mutating func validateUniqueKeys() throws {
        skipWhitespace()
        try scanValue()
        skipWhitespace()
        guard index == bytes.count else { throw ContainerRuntimeContractError.invalidJSON }
    }

    private mutating func scanValue() throws {
        guard index < bytes.count else { throw ContainerRuntimeContractError.invalidJSON }
        switch bytes[index] {
        case 0x7B: try scanObject()
        case 0x5B: try scanArray()
        case 0x22: _ = try scanString()
        default: try scanPrimitive()
        }
    }

    private mutating func scanObject() throws {
        try consume(0x7B)
        skipWhitespace()
        if consumeIf(0x7D) { return }
        var keys = Set<String>()
        while true {
            let key = try scanString()
            guard keys.insert(key).inserted else {
                throw ContainerRuntimeContractError.duplicateField
            }
            skipWhitespace()
            try consume(0x3A)
            skipWhitespace()
            try scanValue()
            skipWhitespace()
            if consumeIf(0x7D) { return }
            try consume(0x2C)
            skipWhitespace()
        }
    }

    private mutating func scanArray() throws {
        try consume(0x5B)
        skipWhitespace()
        if consumeIf(0x5D) { return }
        while true {
            try scanValue()
            skipWhitespace()
            if consumeIf(0x5D) { return }
            try consume(0x2C)
            skipWhitespace()
        }
    }

    private mutating func scanString() throws -> String {
        guard index < bytes.count, bytes[index] == 0x22 else {
            throw ContainerRuntimeContractError.invalidJSON
        }
        let start = index
        index += 1
        var escaped = false
        while index < bytes.count {
            let byte = bytes[index]
            index += 1
            if escaped {
                escaped = false
            } else if byte == 0x5C {
                escaped = true
            } else if byte == 0x22 {
                let encoded = Data(bytes[start..<index])
                guard let value = try? JSONDecoder().decode(String.self, from: encoded) else {
                    throw ContainerRuntimeContractError.invalidJSON
                }
                return value
            } else if byte < 0x20 {
                throw ContainerRuntimeContractError.invalidJSON
            }
        }
        throw ContainerRuntimeContractError.invalidJSON
    }

    private mutating func scanPrimitive() throws {
        let start = index
        while index < bytes.count,
              !Self.isWhitespace(bytes[index]),
              bytes[index] != 0x2C,
              bytes[index] != 0x5D,
              bytes[index] != 0x7D {
            index += 1
        }
        guard index > start else { throw ContainerRuntimeContractError.invalidJSON }
    }

    private mutating func skipWhitespace() {
        while index < bytes.count, Self.isWhitespace(bytes[index]) { index += 1 }
    }

    private mutating func consume(_ byte: UInt8) throws {
        guard consumeIf(byte) else { throw ContainerRuntimeContractError.invalidJSON }
    }

    private mutating func consumeIf(_ byte: UInt8) -> Bool {
        guard index < bytes.count, bytes[index] == byte else { return false }
        index += 1
        return true
    }

    private static func isWhitespace(_ byte: UInt8) -> Bool {
        byte == 0x20 || byte == 0x09 || byte == 0x0A || byte == 0x0D
    }
}
