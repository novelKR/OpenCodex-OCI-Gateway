import Foundation

public enum NativeRepairKind: String, Codable, Equatable, Sendable {
    case stateOnly = "state_only"
    case localRelay = "local_relay"
    case openCodex = "opencodex"
    case unavailable
}

public enum NativeRepairReason: String, Codable, Equatable, Sendable {
    case nativeRoutingClean = "native_routing_clean"
    case localRelayOwned = "local_relay_owned"
    case openCodexOwned = "opencodex_owned"
    case relayMarkerIncomplete = "relay_marker_incomplete"
    case foreignRelayOwner = "foreign_relay_owner"
    case managedArtifactInvalid = "managed_artifact_invalid"
    case mixedRoutingOwners = "mixed_routing_owners"
    case unmanagedRoutingOverride = "unmanaged_routing_override"
}

public enum NativeOwnerConfiguration: String, Codable, Equatable, Sendable {
    case valid
    case invalid
    case unavailable
}

public enum NativeOwnerIntegration: String, Codable, Equatable, Sendable {
    case enabled
    case disabled
    case unknown
}

public enum NativeOwnerInspectionReason: String, Codable, Equatable, Sendable {
    case ready = "owner_ready"
    case configurationInvalid = "owner_configuration_invalid"
    case probeUnavailable = "owner_probe_unavailable"
}

public enum NativeOwnerRestoreOutcome: String, Codable, Equatable, Sendable {
    case applied
    case alreadyNative = "already_native"
    case retryableNoMutation = "retryable_no_mutation"
    case notApplicable = "not_applicable"
}

private struct NativeRepairCodingKey: CodingKey {
    let stringValue: String
    let intValue: Int?

    init?(stringValue: String) {
        self.stringValue = stringValue
        self.intValue = nil
    }

    init?(intValue: Int) {
        self.stringValue = String(intValue)
        self.intValue = intValue
    }
}

private func nativeRepairKeys(_ decoder: Decoder) throws -> Set<String> {
    let values = try decoder.container(keyedBy: NativeRepairCodingKey.self)
    return Set(values.allKeys.map(\.stringValue))
}

private func requireNativeRepairKeys(_ decoder: Decoder, _ expected: Set<String>) throws {
    guard try nativeRepairKeys(decoder) == expected else {
        throw DecodingError.dataCorrupted(.init(
            codingPath: decoder.codingPath,
            debugDescription: "invalid native repair envelope"
        ))
    }
}

public struct NativeRepairInspection: Decodable, Equatable, Sendable {
    public let schemaVersion: Int
    public let generation: UInt64
    public let phase: RoutingPhase
    public let kind: NativeRepairKind
    public let openAIBaseURL: Bool
    public let modelCatalogJSON: Bool
    public let reason: NativeRepairReason

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case generation, phase, kind
        case openAIBaseURL = "openai_base_url"
        case modelCatalogJSON = "model_catalog_json"
        case reason
    }

    public init(from decoder: Decoder) throws {
        try requireNativeRepairKeys(decoder, ["schema_version", "generation", "phase", "kind", "openai_base_url", "model_catalog_json", "reason"])
        let values = try decoder.container(keyedBy: CodingKeys.self)
        schemaVersion = try values.decode(Int.self, forKey: .schemaVersion)
        generation = try values.decode(UInt64.self, forKey: .generation)
        phase = try values.decode(RoutingPhase.self, forKey: .phase)
        kind = try values.decode(NativeRepairKind.self, forKey: .kind)
        openAIBaseURL = try values.decode(Bool.self, forKey: .openAIBaseURL)
        modelCatalogJSON = try values.decode(Bool.self, forKey: .modelCatalogJSON)
        reason = try values.decode(NativeRepairReason.self, forKey: .reason)
    }

    public func validated() throws -> NativeRepairInspection {
        guard schemaVersion == 1, generation > 0, phase == .recoveryRequired else { throw RelayctlError.invalidStatus }
        switch kind {
        case .stateOnly:
            guard reason == .nativeRoutingClean, !openAIBaseURL, !modelCatalogJSON else { throw RelayctlError.invalidStatus }
        case .localRelay:
            guard reason == .localRelayOwned else { throw RelayctlError.invalidStatus }
        case .openCodex:
            guard reason == .openCodexOwned else { throw RelayctlError.invalidStatus }
        case .unavailable:
            guard reason != .nativeRoutingClean, reason != .localRelayOwned, reason != .openCodexOwned else { throw RelayctlError.invalidStatus }
        }
        return self
    }
}

public struct NativeRepairOwnerInspection: Decodable, Equatable, Sendable {
    public let schemaVersion: Int
    public let generation: UInt64
    public let owner: NativeRepairKind
    public let configuration: NativeOwnerConfiguration
    public let integration: NativeOwnerIntegration
    public let reason: NativeOwnerInspectionReason

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case generation, owner, configuration, integration, reason
    }

    public init(from decoder: Decoder) throws {
        try requireNativeRepairKeys(decoder, ["schema_version", "generation", "owner", "configuration", "integration", "reason"])
        let values = try decoder.container(keyedBy: CodingKeys.self)
        schemaVersion = try values.decode(Int.self, forKey: .schemaVersion)
        generation = try values.decode(UInt64.self, forKey: .generation)
        owner = try values.decode(NativeRepairKind.self, forKey: .owner)
        configuration = try values.decode(NativeOwnerConfiguration.self, forKey: .configuration)
        integration = try values.decode(NativeOwnerIntegration.self, forKey: .integration)
        reason = try values.decode(NativeOwnerInspectionReason.self, forKey: .reason)
    }

    public func validated() throws -> NativeRepairOwnerInspection {
        guard schemaVersion == 1, generation > 0, owner == .openCodex else { throw RelayctlError.invalidStatus }
        switch configuration {
        case .valid:
            guard reason == .ready, integration == .enabled || integration == .disabled else { throw RelayctlError.invalidStatus }
        case .invalid:
            guard reason == .configurationInvalid, integration == .unknown else { throw RelayctlError.invalidStatus }
        case .unavailable:
            guard reason == .probeUnavailable, integration == .unknown else { throw RelayctlError.invalidStatus }
        }
        return self
    }
}

public struct NativeRoutingRepairReceipt: Decodable, Equatable, Sendable {
    public let schemaVersion: Int
    public let status: RoutingStatus
    public let backupCreated: Bool
    public let nonRoutingCleanupIncomplete: Bool
    public let ownerRestoreAttempts: Int
    public let ownerRestoreResult: NativeOwnerRestoreOutcome

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case status
        case backupCreated = "backup_created"
        case nonRoutingCleanupIncomplete = "nonrouting_cleanup_incomplete"
        case ownerRestoreAttempts = "owner_restore_attempts"
        case ownerRestoreResult = "owner_restore_result"
    }

    public init(from decoder: Decoder) throws {
        let keys = try nativeRepairKeys(decoder)
        let v1: Set<String> = ["schema_version", "status", "backup_created", "nonrouting_cleanup_incomplete"]
        let v2 = v1.union(["owner_restore_attempts", "owner_restore_result"])
        guard keys == v1 || keys == v2 else { throw RelayctlError.invalidStatus }
        let values = try decoder.container(keyedBy: CodingKeys.self)
        schemaVersion = try values.decode(Int.self, forKey: .schemaVersion)
        status = try values.decode(RoutingStatus.self, forKey: .status)
        backupCreated = try values.decode(Bool.self, forKey: .backupCreated)
        nonRoutingCleanupIncomplete = try values.decode(Bool.self, forKey: .nonRoutingCleanupIncomplete)
        if schemaVersion == 2 {
            guard keys == v2 else { throw RelayctlError.invalidStatus }
            ownerRestoreAttempts = try values.decode(Int.self, forKey: .ownerRestoreAttempts)
            ownerRestoreResult = try values.decode(NativeOwnerRestoreOutcome.self, forKey: .ownerRestoreResult)
        } else {
            guard schemaVersion == 1, keys == v1 else { throw RelayctlError.invalidStatus }
            ownerRestoreAttempts = 0
            ownerRestoreResult = .notApplicable
        }
    }

    public func validated() throws -> NativeRoutingRepairReceipt {
        _ = try status.validated()
        guard schemaVersion == 1 || schemaVersion == 2,
              ownerRestoreAttempts >= 0 && ownerRestoreAttempts <= 4,
              status.phase == .nativeActive,
              status.desiredBackend == .none,
              status.appliedBackend == .none else { throw RelayctlError.invalidStatus }
        if schemaVersion == 2 {
            guard (ownerRestoreAttempts == 0 && ownerRestoreResult == .notApplicable) ||
                    (ownerRestoreAttempts > 0 && (ownerRestoreResult == .applied || ownerRestoreResult == .alreadyNative)) else {
                throw RelayctlError.invalidStatus
            }
        }
        return self
    }
}

public struct OpenCodexNativeRepairSelection: Equatable, Sendable {
    public let installationID: String
    public let installationFingerprint: String
    public let nativeRestoreFingerprint: String
    public let executable: OpenCodexExecutable

    public init(
        installationID: String,
        installationFingerprint: String,
        nativeRestoreFingerprint: String,
        executable: OpenCodexExecutable
    ) throws {
        guard Self.isLowercaseHex(installationID, count: 24),
              Self.isLowercaseHex(installationFingerprint, count: 64),
              Self.isLowercaseHex(nativeRestoreFingerprint, count: 64) else {
            throw RelayctlError.invalidStatus
        }
        self.installationID = installationID
        self.installationFingerprint = installationFingerprint
        self.nativeRestoreFingerprint = nativeRestoreFingerprint
        self.executable = executable
    }

    public init(candidate: OpenCodexInstallationCandidate) throws {
        guard candidate.nativeRestoreCapability == .verifiedSnapshot,
              let nativeRestoreFingerprint = candidate.nativeRestoreFingerprint,
              let executable = candidate.handoffExecutable else {
            throw RelayctlError.invalidStatus
        }
        try self.init(
            installationID: candidate.id,
            installationFingerprint: candidate.fingerprint,
            nativeRestoreFingerprint: nativeRestoreFingerprint,
            executable: executable
        )
    }

    private static func isLowercaseHex(_ value: String, count: Int) -> Bool {
        value.utf8.count == count && value.utf8.allSatisfy {
            ($0 >= 48 && $0 <= 57) || ($0 >= 97 && $0 <= 102)
        }
    }
}

public protocol NativeRepairExecuting: Sendable {
    func inspect(expectedGeneration: UInt64) async throws -> NativeRepairInspection
    func inspectOwner(
        expectedGeneration: UInt64,
        owner: NativeRepairKind,
        openCodexSelection: OpenCodexNativeRepairSelection
    ) async throws -> NativeRepairOwnerInspection
    func repair(
        expectedGeneration: UInt64,
        owner: NativeRepairKind,
        openCodexSelection: OpenCodexNativeRepairSelection?
    ) async throws -> NativeRoutingRepairReceipt
}
