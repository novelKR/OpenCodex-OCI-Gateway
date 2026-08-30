import Foundation

public enum OpenCodexRemovalContext: String, Codable, Equatable, Sendable {
    case integrated
    case standaloneNative = "standalone_native"
}

public enum OpenCodexNativeState: String, Codable, Equatable, Sendable {
    case native
    case openCodex = "opencodex"
    case unavailable
}

public enum OpenCodexNativeReadStatus: String, Codable, Equatable, Sendable {
    case ready
    case recoveryRequired = "recovery_required"
}

public enum OpenCodexAutomaticRemovalReason: String, Codable, Equatable, Sendable {
    case eligible
    case unreviewedPackageClosure = "unreviewed_package_closure"
    case unsupportedPackageVersion = "unsupported_package_version"
    case packageModuleChanged = "package_module_changed"
    case executionEvidenceIncomplete = "execution_evidence_incomplete"
    case manualPackageManager = "manual_package_manager"
    case identityUnverified = "identity_unverified"
    // Schema 1 did not include a reason. This value is app-local and is never
    // accepted from a schema 2 relayctl response.
    case verificationUnavailable = "verification_unavailable"
}

public enum OpenCodexNativeRemovalContractError: LocalizedError, Equatable, Sendable {
    case invalidDiscovery
    case invalidSelection
    case invalidInspection
    case invalidInventory
    case invalidRequest
    case invalidReceipt

    public var safeCode: String {
        switch self {
        case .invalidDiscovery: "opencodex_native_discovery_invalid"
        case .invalidSelection: "opencodex_native_selection_invalid"
        case .invalidInspection: "opencodex_native_inspection_invalid"
        case .invalidInventory: "opencodex_native_inventory_invalid"
        case .invalidRequest: "opencodex_native_request_invalid"
        case .invalidReceipt: "opencodex_native_receipt_invalid"
        }
    }

    public var errorDescription: String? { safeCode }
}

public struct OpenCodexNativeHomebrewGuardSnapshot: Codable, Equatable, Sendable {
    public let prefix: String
    public let packageRoot: String
    public let executable: String
    public let executableSHA256: String
    public let cliEntry: String
    public let cliEntrySHA256: String
    public let bunExecutable: String
    public let bunSHA256: String
    public let nodeExecutable: String
    public let nodeSHA256: String
    public let npmCLI: String
    public let npmCLISHA256: String
    public let launchers: [String]

    enum CodingKeys: String, CodingKey {
        case prefix
        case packageRoot = "package_root"
        case executable
        case executableSHA256 = "executable_sha256"
        case cliEntry = "cli_entry"
        case cliEntrySHA256 = "cli_entry_sha256"
        case bunExecutable = "bun_executable"
        case bunSHA256 = "bun_sha256"
        case nodeExecutable = "node_executable"
        case nodeSHA256 = "node_sha256"
        case npmCLI = "npm_cli"
        case npmCLISHA256 = "npm_cli_sha256"
        case launchers
    }

    public init(
        prefix: String,
        packageRoot: String,
        executable: String,
        executableSHA256: String,
        cliEntry: String,
        cliEntrySHA256: String,
        bunExecutable: String,
        bunSHA256: String,
        nodeExecutable: String,
        nodeSHA256: String,
        npmCLI: String,
        npmCLISHA256: String,
        launchers: [String]
    ) throws {
        guard prefix == "/opt/homebrew",
              packageRoot == "/opt/homebrew/lib/node_modules/@bitkyc08/opencodex",
              Self.isCanonicalAbsolutePath(executable),
              Self.isCanonicalAbsolutePath(cliEntry),
              Self.isCanonicalAbsolutePath(bunExecutable),
              Self.isCanonicalAbsolutePath(nodeExecutable),
              Self.isCanonicalAbsolutePath(npmCLI),
              executable.hasPrefix(packageRoot + "/"),
              cliEntry.hasPrefix(packageRoot + "/"),
              bunExecutable.hasPrefix(packageRoot + "/"),
              launchers.count <= 4,
              launchers.allSatisfy(Self.isCanonicalAbsolutePath),
              [
                  executableSHA256, cliEntrySHA256, bunSHA256,
                  nodeSHA256, npmCLISHA256,
              ].allSatisfy({ NativeRemovalValidation.isLowercaseHex($0, count: 64) }) else {
            throw OpenCodexNativeRemovalContractError.invalidDiscovery
        }
        self.prefix = prefix
        self.packageRoot = packageRoot
        self.executable = executable
        self.executableSHA256 = executableSHA256
        self.cliEntry = cliEntry
        self.cliEntrySHA256 = cliEntrySHA256
        self.bunExecutable = bunExecutable
        self.bunSHA256 = bunSHA256
        self.nodeExecutable = nodeExecutable
        self.nodeSHA256 = nodeSHA256
        self.npmCLI = npmCLI
        self.npmCLISHA256 = npmCLISHA256
        self.launchers = launchers
    }

    public init(from decoder: Decoder) throws {
        try StrictNativeRemovalJSON.requireKeys(decoder, allowed: [
            "prefix", "package_root", "executable", "executable_sha256",
            "cli_entry", "cli_entry_sha256", "bun_executable", "bun_sha256",
            "node_executable", "node_sha256", "npm_cli", "npm_cli_sha256", "launchers",
        ])
        let values = try decoder.container(keyedBy: CodingKeys.self)
        try self.init(
            prefix: values.decode(String.self, forKey: .prefix),
            packageRoot: values.decode(String.self, forKey: .packageRoot),
            executable: values.decode(String.self, forKey: .executable),
            executableSHA256: values.decode(String.self, forKey: .executableSHA256),
            cliEntry: values.decode(String.self, forKey: .cliEntry),
            cliEntrySHA256: values.decode(String.self, forKey: .cliEntrySHA256),
            bunExecutable: values.decode(String.self, forKey: .bunExecutable),
            bunSHA256: values.decode(String.self, forKey: .bunSHA256),
            nodeExecutable: values.decode(String.self, forKey: .nodeExecutable),
            nodeSHA256: values.decode(String.self, forKey: .nodeSHA256),
            npmCLI: values.decode(String.self, forKey: .npmCLI),
            npmCLISHA256: values.decode(String.self, forKey: .npmCLISHA256),
            launchers: values.decode([String].self, forKey: .launchers)
        )
    }

    private static func isCanonicalAbsolutePath(_ value: String) -> Bool {
        guard !value.isEmpty, value.utf8.count <= 4_096,
              value.hasPrefix("/"), !value.contains("\0") else { return false }
        return URL(fileURLWithPath: value).standardizedFileURL.path == value
    }
}

public struct OpenCodexNativeRemovalCandidate: Codable, Equatable, Identifiable, Sendable {
    public let installationID: String
    public let installationFingerprint: String
    public let nativeRestoreFingerprint: String?
    public let version: String
    public let manager: OpenCodexPackageManager
    public let removalCapability: OpenCodexRemovalCapability
    public let removalAuthority: OpenCodexRemovalAuthority
    public let dataCapability: OpenCodexDataCapability?
    public let automaticRemovalEligible: Bool
    public let automaticRemovalReason: OpenCodexAutomaticRemovalReason?
    public let homebrewGuardRequired: Bool
    public let homebrewGuard: OpenCodexNativeHomebrewGuardSnapshot?

    public var id: String { installationID }

    enum CodingKeys: String, CodingKey {
        case installationID = "installation_id"
        case installationFingerprint = "installation_fingerprint"
        case nativeRestoreFingerprint = "native_restore_fingerprint"
        case version
        case manager
        case removalCapability = "removal_capability"
        case removalAuthority = "removal_authority"
        case dataCapability = "data_capability"
        case automaticRemovalEligible = "automatic_removal_eligible"
        case automaticRemovalReason = "automatic_removal_reason"
        case homebrewGuardRequired = "homebrew_guard_required"
        case homebrewGuard = "homebrew_guard"
    }

    public init(
        installationID: String,
        installationFingerprint: String,
        nativeRestoreFingerprint: String?,
        version: String,
        manager: OpenCodexPackageManager,
        removalCapability: OpenCodexRemovalCapability,
        removalAuthority: OpenCodexRemovalAuthority,
        dataCapability: OpenCodexDataCapability?,
        automaticRemovalEligible: Bool,
        homebrewGuardRequired: Bool,
        homebrewGuard: OpenCodexNativeHomebrewGuardSnapshot?,
        automaticRemovalReason: OpenCodexAutomaticRemovalReason? = nil
    ) throws {
        guard NativeRemovalValidation.isLowercaseHex(installationID, count: 24),
              NativeRemovalValidation.isLowercaseHex(installationFingerprint, count: 64),
              nativeRestoreFingerprint.map({ NativeRemovalValidation.isLowercaseHex($0, count: 64) }) ?? true,
              NativeRemovalValidation.isDisplayVersion(version),
              homebrewGuardRequired == (homebrewGuard != nil) else {
            throw OpenCodexNativeRemovalContractError.invalidDiscovery
        }
        if homebrewGuardRequired {
            guard manager == .homebrew,
                  removalCapability == .homebrewGuardedNPM else {
                throw OpenCodexNativeRemovalContractError.invalidDiscovery
            }
        } else if removalCapability == .homebrewGuardedNPM {
            throw OpenCodexNativeRemovalContractError.invalidDiscovery
        }
        if automaticRemovalEligible {
            guard nativeRestoreFingerprint != nil,
                  removalAuthority == .automatic,
                  removalCapability == .exactNPM || removalCapability == .homebrewGuardedNPM,
                  dataCapability == .preserveOnly || dataCapability == .selectiveTrashV1,
                  automaticRemovalReason == nil || automaticRemovalReason == .eligible else {
                throw OpenCodexNativeRemovalContractError.invalidDiscovery
            }
        } else if automaticRemovalReason == .eligible {
            throw OpenCodexNativeRemovalContractError.invalidDiscovery
        }
        self.installationID = installationID
        self.installationFingerprint = installationFingerprint
        self.nativeRestoreFingerprint = nativeRestoreFingerprint
        self.version = version
        self.manager = manager
        self.removalCapability = removalCapability
        self.removalAuthority = removalAuthority
        self.dataCapability = dataCapability
        self.automaticRemovalEligible = automaticRemovalEligible
        self.automaticRemovalReason = automaticRemovalReason
        self.homebrewGuardRequired = homebrewGuardRequired
        self.homebrewGuard = homebrewGuard
    }

    public init(from decoder: Decoder) throws {
        try StrictNativeRemovalJSON.requireKeys(
            decoder,
            allowed: [
                "installation_id", "installation_fingerprint", "native_restore_fingerprint",
                "version", "manager", "removal_capability", "removal_authority",
                "data_capability", "automatic_removal_eligible", "homebrew_guard_required",
                "homebrew_guard", "automatic_removal_reason",
            ],
            required: [
                "installation_id", "installation_fingerprint", "version", "manager",
                "removal_capability", "removal_authority", "automatic_removal_eligible",
                "homebrew_guard_required",
            ]
        )
        let values = try decoder.container(keyedBy: CodingKeys.self)
        try self.init(
            installationID: values.decode(String.self, forKey: .installationID),
            installationFingerprint: values.decode(String.self, forKey: .installationFingerprint),
            nativeRestoreFingerprint: values.decodeIfPresent(String.self, forKey: .nativeRestoreFingerprint),
            version: values.decode(String.self, forKey: .version),
            manager: values.decode(OpenCodexPackageManager.self, forKey: .manager),
            removalCapability: values.decode(OpenCodexRemovalCapability.self, forKey: .removalCapability),
            removalAuthority: values.decode(OpenCodexRemovalAuthority.self, forKey: .removalAuthority),
            dataCapability: values.decodeIfPresent(OpenCodexDataCapability.self, forKey: .dataCapability),
            automaticRemovalEligible: values.decode(Bool.self, forKey: .automaticRemovalEligible),
            homebrewGuardRequired: values.decode(Bool.self, forKey: .homebrewGuardRequired),
            homebrewGuard: values.decodeIfPresent(OpenCodexNativeHomebrewGuardSnapshot.self, forKey: .homebrewGuard),
            automaticRemovalReason: values.decodeIfPresent(OpenCodexAutomaticRemovalReason.self, forKey: .automaticRemovalReason)
        )
    }
}

public struct OpenCodexNativeDiscoveryResult: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let operation: String
    public let context: OpenCodexRemovalContext
    public let status: OpenCodexNativeReadStatus
    public let boundaryRevision: String
    public let nativeState: OpenCodexNativeState
    public let nativeRecoveryRequired: Bool
    public let candidates: [OpenCodexNativeRemovalCandidate]
    public let rejected: Int
    public let truncated: Bool

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case operation
        case context
        case status
        case boundaryRevision = "boundary_revision"
        case nativeState = "native_state"
        case nativeRecoveryRequired = "native_recovery_required"
        case candidates
        case rejected
        case truncated
    }

    public func validated() throws -> Self {
        guard schemaVersion == 1 || schemaVersion == 2,
              operation == "discover-open-codex-native",
              context == .standaloneNative,
              NativeRemovalValidation.isLowercaseHex(boundaryRevision, count: 64),
              candidates.count <= 128,
              rejected >= 0,
              Set(candidates.map(\.installationID)).count == candidates.count else {
            throw OpenCodexNativeRemovalContractError.invalidDiscovery
        }
        if schemaVersion == 1 {
            guard candidates.allSatisfy({ $0.automaticRemovalReason == nil }) else {
                throw OpenCodexNativeRemovalContractError.invalidDiscovery
            }
        } else {
            guard candidates.allSatisfy({ candidate in
                guard let reason = candidate.automaticRemovalReason,
                      reason != .verificationUnavailable else { return false }
                return candidate.automaticRemovalEligible == (reason == .eligible)
            }) else {
                throw OpenCodexNativeRemovalContractError.invalidDiscovery
            }
        }
        switch status {
        case .ready:
            guard !nativeRecoveryRequired, nativeState != .unavailable else {
                throw OpenCodexNativeRemovalContractError.invalidDiscovery
            }
        case .recoveryRequired:
            guard nativeRecoveryRequired, nativeState == .unavailable, candidates.isEmpty else {
                throw OpenCodexNativeRemovalContractError.invalidDiscovery
            }
        }
        return self
    }

    public init(from decoder: Decoder) throws {
        try StrictNativeRemovalJSON.requireKeys(decoder, allowed: [
            "schema_version", "operation", "context", "status", "boundary_revision",
            "native_state", "native_recovery_required", "candidates", "rejected", "truncated",
        ])
        let values = try decoder.container(keyedBy: CodingKeys.self)
        self.schemaVersion = try values.decode(Int.self, forKey: .schemaVersion)
        self.operation = try values.decode(String.self, forKey: .operation)
        self.context = try values.decode(OpenCodexRemovalContext.self, forKey: .context)
        self.status = try values.decode(OpenCodexNativeReadStatus.self, forKey: .status)
        self.boundaryRevision = try values.decode(String.self, forKey: .boundaryRevision)
        self.nativeState = try values.decode(OpenCodexNativeState.self, forKey: .nativeState)
        self.nativeRecoveryRequired = try values.decode(Bool.self, forKey: .nativeRecoveryRequired)
        self.candidates = try values.decode([OpenCodexNativeRemovalCandidate].self, forKey: .candidates)
        self.rejected = try values.decode(Int.self, forKey: .rejected)
        self.truncated = try values.decode(Bool.self, forKey: .truncated)
    }
}

public struct OpenCodexNativeRemovalSelection: Codable, Equatable, Sendable {
    public let installationID: String
    public let installationFingerprint: String
    public let nativeRestoreFingerprint: String
    public let boundaryRevision: String

    public init(
        installationID: String,
        installationFingerprint: String,
        nativeRestoreFingerprint: String,
        boundaryRevision: String
    ) throws {
        guard NativeRemovalValidation.isLowercaseHex(installationID, count: 24),
              NativeRemovalValidation.isLowercaseHex(installationFingerprint, count: 64),
              NativeRemovalValidation.isLowercaseHex(nativeRestoreFingerprint, count: 64),
              NativeRemovalValidation.isLowercaseHex(boundaryRevision, count: 64) else {
            throw OpenCodexNativeRemovalContractError.invalidSelection
        }
        self.installationID = installationID
        self.installationFingerprint = installationFingerprint
        self.nativeRestoreFingerprint = nativeRestoreFingerprint
        self.boundaryRevision = boundaryRevision
    }

    public init(candidate: OpenCodexNativeRemovalCandidate, boundaryRevision: String) throws {
        guard candidate.automaticRemovalEligible,
              let nativeRestoreFingerprint = candidate.nativeRestoreFingerprint else {
            throw OpenCodexNativeRemovalContractError.invalidSelection
        }
        try self.init(
            installationID: candidate.installationID,
            installationFingerprint: candidate.installationFingerprint,
            nativeRestoreFingerprint: nativeRestoreFingerprint,
            boundaryRevision: boundaryRevision
        )
    }

    var selectorArguments: [String] {
        [
            "--installation-id", installationID,
            "--installation-fingerprint", installationFingerprint,
            "--native-restore-fingerprint", nativeRestoreFingerprint,
            "--expected-boundary-revision", boundaryRevision,
        ]
    }
}

public struct OpenCodexNativeRemovalInspection: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let operation: String
    public let context: OpenCodexRemovalContext
    public let status: OpenCodexNativeReadStatus
    public let boundaryRevision: String
    public let nativeState: OpenCodexNativeState
    public let nativeRecoveryRequired: Bool
    public let candidate: OpenCodexNativeRemovalCandidate?

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case operation
        case context
        case status
        case boundaryRevision = "boundary_revision"
        case nativeState = "native_state"
        case nativeRecoveryRequired = "native_recovery_required"
        case candidate
    }

    public func validated(for selection: OpenCodexNativeRemovalSelection) throws -> Self {
        guard schemaVersion == 1 || schemaVersion == 2,
              operation == "inspect-open-codex-native-removal",
              context == .standaloneNative,
              boundaryRevision == selection.boundaryRevision else {
            throw OpenCodexNativeRemovalContractError.invalidInspection
        }
        switch status {
        case .ready:
            guard !nativeRecoveryRequired,
                  nativeState != .unavailable,
                  let candidate,
                  candidate.automaticRemovalEligible,
                  candidate.installationID == selection.installationID,
                  candidate.installationFingerprint == selection.installationFingerprint,
                  candidate.nativeRestoreFingerprint == selection.nativeRestoreFingerprint else {
                throw OpenCodexNativeRemovalContractError.invalidInspection
            }
            if schemaVersion == 1 {
                guard candidate.automaticRemovalReason == nil else {
                    throw OpenCodexNativeRemovalContractError.invalidInspection
                }
            } else {
                guard candidate.automaticRemovalReason == .eligible else {
                    throw OpenCodexNativeRemovalContractError.invalidInspection
                }
            }
        case .recoveryRequired:
            guard nativeRecoveryRequired, nativeState == .unavailable, candidate == nil else {
                throw OpenCodexNativeRemovalContractError.invalidInspection
            }
        }
        return self
    }

    public init(from decoder: Decoder) throws {
        try StrictNativeRemovalJSON.requireKeys(
            decoder,
            allowed: [
                "schema_version", "operation", "context", "status", "boundary_revision",
                "native_state", "native_recovery_required", "candidate",
            ],
            required: [
                "schema_version", "operation", "context", "status", "boundary_revision",
                "native_state", "native_recovery_required",
            ]
        )
        let values = try decoder.container(keyedBy: CodingKeys.self)
        self.schemaVersion = try values.decode(Int.self, forKey: .schemaVersion)
        self.operation = try values.decode(String.self, forKey: .operation)
        self.context = try values.decode(OpenCodexRemovalContext.self, forKey: .context)
        self.status = try values.decode(OpenCodexNativeReadStatus.self, forKey: .status)
        self.boundaryRevision = try values.decode(String.self, forKey: .boundaryRevision)
        self.nativeState = try values.decode(OpenCodexNativeState.self, forKey: .nativeState)
        self.nativeRecoveryRequired = try values.decode(Bool.self, forKey: .nativeRecoveryRequired)
        self.candidate = try values.decodeIfPresent(OpenCodexNativeRemovalCandidate.self, forKey: .candidate)
    }
}

public struct OpenCodexNativeDataInventoryReceipt: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let operation: String
    public let context: OpenCodexRemovalContext
    public let status: OpenCodexInventoryStatus
    public let boundaryRevision: String
    public let nativeState: OpenCodexNativeState
    public let nativeRecoveryRequired: Bool
    public let installationID: String
    public let installationFingerprint: String
    public let nativeRestoreFingerprint: String
    public let inventoryRevision: String
    public let items: [OpenCodexDataInventoryItem]

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case operation
        case context
        case status
        case boundaryRevision = "boundary_revision"
        case nativeState = "native_state"
        case nativeRecoveryRequired = "native_recovery_required"
        case installationID = "installation_id"
        case installationFingerprint = "installation_fingerprint"
        case nativeRestoreFingerprint = "native_restore_fingerprint"
        case inventoryRevision = "inventory_revision"
        case items
    }

    public func validated(for selection: OpenCodexNativeRemovalSelection) throws -> Self {
        guard schemaVersion == 1,
              operation == "open-codex-native-data-inventory",
              context == .standaloneNative,
              boundaryRevision == selection.boundaryRevision,
              installationID == selection.installationID,
              installationFingerprint == selection.installationFingerprint,
              nativeRestoreFingerprint == selection.nativeRestoreFingerprint,
              NativeRemovalValidation.isLowercaseHex(inventoryRevision, count: 64),
              items.count <= 512,
              Set(items.map(\.id)).count == items.count else {
            throw OpenCodexNativeRemovalContractError.invalidInventory
        }
        if nativeRecoveryRequired {
            guard status == .refused, nativeState == .unavailable, items.isEmpty else {
                throw OpenCodexNativeRemovalContractError.invalidInventory
            }
        } else {
            guard nativeState != .unavailable else {
                throw OpenCodexNativeRemovalContractError.invalidInventory
            }
            if status != .verified {
                guard items.isEmpty else { throw OpenCodexNativeRemovalContractError.invalidInventory }
            } else {
                guard items.allSatisfy(\.isValid) else {
                    throw OpenCodexNativeRemovalContractError.invalidInventory
                }
            }
        }
        return self
    }

    public init(from decoder: Decoder) throws {
        try StrictNativeRemovalJSON.requireKeys(decoder, allowed: [
            "schema_version", "operation", "context", "status", "boundary_revision",
            "native_state", "native_recovery_required", "installation_id",
            "installation_fingerprint", "native_restore_fingerprint", "inventory_revision", "items",
        ])
        let values = try decoder.container(keyedBy: CodingKeys.self)
        self.schemaVersion = try values.decode(Int.self, forKey: .schemaVersion)
        self.operation = try values.decode(String.self, forKey: .operation)
        self.context = try values.decode(OpenCodexRemovalContext.self, forKey: .context)
        self.status = try values.decode(OpenCodexInventoryStatus.self, forKey: .status)
        self.boundaryRevision = try values.decode(String.self, forKey: .boundaryRevision)
        self.nativeState = try values.decode(OpenCodexNativeState.self, forKey: .nativeState)
        self.nativeRecoveryRequired = try values.decode(Bool.self, forKey: .nativeRecoveryRequired)
        self.installationID = try values.decode(String.self, forKey: .installationID)
        self.installationFingerprint = try values.decode(String.self, forKey: .installationFingerprint)
        self.nativeRestoreFingerprint = try values.decode(String.self, forKey: .nativeRestoreFingerprint)
        self.inventoryRevision = try values.decode(String.self, forKey: .inventoryRevision)
        self.items = try values.decode([OpenCodexDataInventoryItem].self, forKey: .items)
    }
}

public struct OpenCodexNativeRemovalRequest: Equatable, Sendable {
    public let selection: OpenCodexNativeRemovalSelection
    public let mode: OpenCodexRemovalMode
    public let dataItemIDs: [String]
    public let expectedInventoryRevision: String?
    public let confirmsRemoval: Bool
    public let confirmsTrash: Bool
    public let confirmsInterruptedDataRefresh: Bool
    public let confirmsRebootedProcessRecovery: Bool
    public let confirmsDesktopExited: Bool

    public init(
        selection: OpenCodexNativeRemovalSelection,
        mode: OpenCodexRemovalMode,
        dataItemIDs: [String],
        expectedInventoryRevision: String? = nil,
        confirmsRemoval: Bool,
        confirmsTrash: Bool,
        confirmsInterruptedDataRefresh: Bool = false,
        confirmsRebootedProcessRecovery: Bool = false,
        confirmsDesktopExited: Bool
    ) throws {
        guard confirmsRemoval, confirmsDesktopExited,
              !(confirmsInterruptedDataRefresh && confirmsRebootedProcessRecovery),
              dataItemIDs.count <= 128,
              Set(dataItemIDs).count == dataItemIDs.count,
              dataItemIDs.allSatisfy(NativeRemovalValidation.isDataItemID) else {
            throw OpenCodexNativeRemovalContractError.invalidRequest
        }
        switch mode {
        case .preserveData:
            guard dataItemIDs.isEmpty, !confirmsTrash, expectedInventoryRevision == nil else {
                throw OpenCodexNativeRemovalContractError.invalidRequest
            }
        case .trashSelected:
            guard !dataItemIDs.isEmpty, confirmsTrash,
                  expectedInventoryRevision.map({ NativeRemovalValidation.isLowercaseHex($0, count: 64) }) == true else {
                throw OpenCodexNativeRemovalContractError.invalidRequest
            }
        }
        self.selection = selection
        self.mode = mode
        self.dataItemIDs = dataItemIDs
        self.expectedInventoryRevision = expectedInventoryRevision
        self.confirmsRemoval = confirmsRemoval
        self.confirmsTrash = confirmsTrash
        self.confirmsInterruptedDataRefresh = confirmsInterruptedDataRefresh
        self.confirmsRebootedProcessRecovery = confirmsRebootedProcessRecovery
        self.confirmsDesktopExited = confirmsDesktopExited
    }

    var arguments: [String] {
        var result = ["mode", "remove-open-codex-native"] + selection.selectorArguments + [
            "--removal-mode", mode.rawValue,
        ]
        for itemID in dataItemIDs {
            result.append(contentsOf: ["--data-item", itemID])
        }
        if confirmsTrash {
            result.append("--confirm-data-trash")
            result.append(contentsOf: ["--expected-inventory-revision", expectedInventoryRevision ?? ""])
        }
        if confirmsInterruptedDataRefresh { result.append("--confirm-interrupted-data-refresh") }
        if confirmsRebootedProcessRecovery { result.append("--confirm-rebooted-process-recovery") }
        result.append(contentsOf: [
            "--confirm-opencodex-native-removal", "--confirm-desktop-exited", "--json",
        ])
        return result
    }
}

public enum OpenCodexNativeRemovalStageName: String, Codable, CaseIterable, Sendable {
    case requestValidation = "request_validation"
    case candidateRevalidation = "candidate_revalidation"
    case dataPolicy = "data_policy"
    case teardownPreflight = "teardown_preflight"
    case cleanupJournal = "cleanup_journal"
    case nativeBoundaryPreTeardown = "native_boundary_pre_teardown"
    case teardown
    case nativeRestore = "native_restore"
    case nativeBoundaryVerification = "native_boundary_verification"
    case nativeBoundaryPreTrash = "native_boundary_pre_trash"
    case dataTrash = "data_trash"
    case nativeBoundaryPostTrash = "native_boundary_post_trash"
    case nativeBoundaryReverification = "native_boundary_reverification"
    case npmUninstall = "npm_uninstall"
    case packageVerification = "package_verification"
    case nativeBoundaryFinalVerification = "native_boundary_final_verification"
    case nativeRecovery = "native_recovery"
    case cleanupJournalRetained = "cleanup_journal_retained"
}

public struct OpenCodexNativeRemovalStage: Codable, Equatable, Sendable {
    public let stage: OpenCodexNativeRemovalStageName
    public let status: OpenCodexRemovalStageStatus
    public let code: String
    public let subjectID: String?

    enum CodingKeys: String, CodingKey {
        case stage
        case status
        case code
        case subjectID = "subject_id"
    }

    public init(from decoder: Decoder) throws {
        try StrictNativeRemovalJSON.requireKeys(
            decoder,
            allowed: ["stage", "status", "code", "subject_id"],
            required: ["stage", "status", "code"]
        )
        let values = try decoder.container(keyedBy: CodingKeys.self)
        self.stage = try values.decode(OpenCodexNativeRemovalStageName.self, forKey: .stage)
        self.status = try values.decode(OpenCodexRemovalStageStatus.self, forKey: .status)
        self.code = try values.decode(String.self, forKey: .code)
        self.subjectID = try values.decodeIfPresent(String.self, forKey: .subjectID)
    }

    fileprivate func validated(installationID: String) throws -> Self {
        guard NativeRemovalValidation.isValidStage(stage, status: status, code: code),
              NativeRemovalValidation.isValidSubject(subjectID, stage: stage, installationID: installationID) else {
            throw OpenCodexNativeRemovalContractError.invalidReceipt
        }
        return self
    }
}

public struct OpenCodexNativeRemovalReceipt: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let operation: String
    public let context: OpenCodexRemovalContext
    public let status: OpenCodexRemovalStatus
    public let boundaryRevision: String
    public let nativeState: OpenCodexNativeState
    public let nativeRecoveryRequired: Bool
    public let mode: OpenCodexRemovalMode
    public let installationID: String
    public let dataScope: String
    public let selectedDataItems: Int
    public let movedDataItems: Int
    public let packageRemoved: Bool
    public let dataMovementUnknown: Bool
    public let permanentDeleteFallback: Bool
    public let terminalReceiptDigest: String?
    public let stages: [OpenCodexNativeRemovalStage]

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case operation
        case context
        case status
        case boundaryRevision = "boundary_revision"
        case nativeState = "native_state"
        case nativeRecoveryRequired = "native_recovery_required"
        case mode
        case installationID = "installation_id"
        case dataScope = "data_scope"
        case selectedDataItems = "selected_data_items"
        case movedDataItems = "moved_data_items"
        case packageRemoved = "package_removed"
        case dataMovementUnknown = "data_movement_unknown"
        case permanentDeleteFallback = "permanent_delete_fallback"
        case terminalReceiptDigest = "terminal_receipt_digest"
        case stages
    }

    public var isSuccessful: Bool {
        status == .completed && packageRemoved && !dataMovementUnknown &&
            !nativeRecoveryRequired && !permanentDeleteFallback && nativeState == .native &&
            (mode == .preserveData || movedDataItems == selectedDataItems) &&
            terminalReceiptDigest.map({ NativeRemovalValidation.isLowercaseHex($0, count: 64) }) == true &&
            hasCompletedTerminalProof
    }

    public var requiresDataSelectionRefresh: Bool {
        dataMovementUnknown || stages.contains {
            $0.code == "data_selection_refresh_required" || $0.code == "trash_unsupported"
        }
    }

    public var requiresWholeMacReboot: Bool {
        stages.contains { $0.code == "process_cleanup_unverified" }
    }

    public func validated(for request: OpenCodexNativeRemovalRequest) throws -> Self {
        let expectedScope = request.mode == .preserveData ? "preserved" : "explicit_items_only"
        let identities = stages.map {
            "\($0.stage.rawValue)|\($0.status.rawValue)|\($0.code)|\($0.subjectID ?? "")"
        }
        guard schemaVersion == 1,
              operation == "remove-open-codex-native",
              context == .standaloneNative,
              boundaryRevision == request.selection.boundaryRevision,
              mode == request.mode,
              installationID == request.selection.installationID,
              dataScope == expectedScope,
              selectedDataItems == request.dataItemIDs.count,
              selectedDataItems >= 0,
              movedDataItems >= 0,
              movedDataItems <= selectedDataItems,
              stages.count > 0,
              stages.count <= 64,
              Set(identities).count == stages.count,
              !permanentDeleteFallback else {
            throw OpenCodexNativeRemovalContractError.invalidReceipt
        }
        for stage in stages { _ = try stage.validated(installationID: installationID) }
        if mode == .preserveData {
            guard selectedDataItems == 0, movedDataItems == 0, !dataMovementUnknown else {
                throw OpenCodexNativeRemovalContractError.invalidReceipt
            }
        }
        if nativeRecoveryRequired {
            guard status != .completed, nativeState == .unavailable else {
                throw OpenCodexNativeRemovalContractError.invalidReceipt
            }
        } else {
            guard nativeState != .unavailable else {
                throw OpenCodexNativeRemovalContractError.invalidReceipt
            }
        }

        let packageProofs = stageIndices(
            stage: .packageVerification,
            status: .completed,
            code: "package_absent"
        )
        let nativeProofs = stageIndices(
            stage: .nativeBoundaryFinalVerification,
            status: .completed,
            code: "native_ownership_reverified"
        )
        let retainedProofs = stageIndices(
            stage: .cleanupJournalRetained,
            status: .completed,
            code: "terminal_receipt_replayable"
        )
        if packageRemoved {
            guard status != .failed,
                  packageProofs.count == 1,
                  nativeProofs.count == 1 else {
                throw OpenCodexNativeRemovalContractError.invalidReceipt
            }
            if nativeRecoveryRequired {
                guard retainedProofs.isEmpty, terminalReceiptDigest == nil else {
                    throw OpenCodexNativeRemovalContractError.invalidReceipt
                }
            } else {
                guard status == .completed,
                      retainedProofs.count == 1,
                      terminalReceiptDigest.map({
                          NativeRemovalValidation.isLowercaseHex($0, count: 64)
                      }) == true,
                      let retained = retainedProofs.first,
                      retained == stages.indices.last,
                      packageProofs[0] < retained,
                      nativeProofs[0] < retained else {
                    throw OpenCodexNativeRemovalContractError.invalidReceipt
                }
            }
        } else {
            guard packageProofs.isEmpty,
                  nativeProofs.isEmpty,
                  retainedProofs.isEmpty,
                  terminalReceiptDigest == nil else {
                throw OpenCodexNativeRemovalContractError.invalidReceipt
            }
        }

        let recoveryStages = stages.filter { $0.stage == .nativeRecovery }
        if nativeRecoveryRequired {
            guard recoveryStages.count == 1 else {
                throw OpenCodexNativeRemovalContractError.invalidReceipt
            }
        } else if !recoveryStages.isEmpty {
            throw OpenCodexNativeRemovalContractError.invalidReceipt
        }
        if status == .completed {
            guard isSuccessful,
                  stages.allSatisfy({ $0.status == .completed || $0.status == .skipped }) else {
                throw OpenCodexNativeRemovalContractError.invalidReceipt
            }
        }
        return self
    }

    public init(from decoder: Decoder) throws {
        try StrictNativeRemovalJSON.requireKeys(
            decoder,
            allowed: [
                "schema_version", "operation", "context", "status", "boundary_revision",
                "native_state", "native_recovery_required", "mode", "installation_id",
                "data_scope", "selected_data_items", "moved_data_items", "package_removed",
                "data_movement_unknown", "permanent_delete_fallback", "terminal_receipt_digest",
                "stages",
            ],
            required: [
                "schema_version", "operation", "context", "status", "boundary_revision",
                "native_state", "native_recovery_required", "mode", "installation_id",
                "data_scope", "selected_data_items", "moved_data_items", "package_removed",
                "data_movement_unknown", "permanent_delete_fallback", "stages",
            ]
        )
        let values = try decoder.container(keyedBy: CodingKeys.self)
        if values.contains(.terminalReceiptDigest), try values.decodeNil(forKey: .terminalReceiptDigest) {
            throw OpenCodexNativeRemovalContractError.invalidReceipt
        }
        self.schemaVersion = try values.decode(Int.self, forKey: .schemaVersion)
        self.operation = try values.decode(String.self, forKey: .operation)
        self.context = try values.decode(OpenCodexRemovalContext.self, forKey: .context)
        self.status = try values.decode(OpenCodexRemovalStatus.self, forKey: .status)
        self.boundaryRevision = try values.decode(String.self, forKey: .boundaryRevision)
        self.nativeState = try values.decode(OpenCodexNativeState.self, forKey: .nativeState)
        self.nativeRecoveryRequired = try values.decode(Bool.self, forKey: .nativeRecoveryRequired)
        self.mode = try values.decode(OpenCodexRemovalMode.self, forKey: .mode)
        self.installationID = try values.decode(String.self, forKey: .installationID)
        self.dataScope = try values.decode(String.self, forKey: .dataScope)
        self.selectedDataItems = try values.decode(Int.self, forKey: .selectedDataItems)
        self.movedDataItems = try values.decode(Int.self, forKey: .movedDataItems)
        self.packageRemoved = try values.decode(Bool.self, forKey: .packageRemoved)
        self.dataMovementUnknown = try values.decode(Bool.self, forKey: .dataMovementUnknown)
        self.permanentDeleteFallback = try values.decode(Bool.self, forKey: .permanentDeleteFallback)
        self.terminalReceiptDigest = try values.decodeIfPresent(String.self, forKey: .terminalReceiptDigest)
        self.stages = try values.decode([OpenCodexNativeRemovalStage].self, forKey: .stages)
    }

    private var hasCompletedTerminalProof: Bool {
        guard let package = stages.firstIndex(where: {
            $0.stage == .packageVerification && $0.status == .completed && $0.code == "package_absent"
        }),
        let native = stages.firstIndex(where: {
            $0.stage == .nativeBoundaryFinalVerification &&
                $0.status == .completed && $0.code == "native_ownership_reverified"
        }),
        let retained = stages.firstIndex(where: {
            $0.stage == .cleanupJournalRetained &&
                $0.status == .completed && $0.code == "terminal_receipt_replayable"
        }) else { return false }
        return retained == stages.indices.last && package < retained && native < retained
    }

    private func stageIndices(
        stage: OpenCodexNativeRemovalStageName,
        status: OpenCodexRemovalStageStatus,
        code: String
    ) -> [Int] {
        stages.indices.filter {
            stages[$0].stage == stage && stages[$0].status == status && stages[$0].code == code
        }
    }
}

public protocol OpenCodexNativeRemovalExecuting: Sendable {
    func discover() async throws -> OpenCodexNativeDiscoveryResult
    func acknowledgeTerminal(receiptDigest: String) async throws -> OpenCodexNativeDiscoveryResult
    func inspect(selection: OpenCodexNativeRemovalSelection) async throws -> OpenCodexNativeRemovalInspection
    func inspectData(selection: OpenCodexNativeRemovalSelection) async throws -> OpenCodexNativeDataInventoryReceipt
    func remove(_ request: OpenCodexNativeRemovalRequest) async throws -> OpenCodexNativeRemovalReceipt
}

private enum NativeRemovalValidation {
    static func isLowercaseHex(_ value: String, count: Int) -> Bool {
        value.utf8.count == count && value.utf8.allSatisfy {
            ($0 >= 48 && $0 <= 57) || ($0 >= 97 && $0 <= 102)
        }
    }

    static func isSafeToken(_ value: String) -> Bool {
        let bytes = Array(value.utf8)
        return !bytes.isEmpty && bytes.count <= 64 && bytes.allSatisfy {
            ($0 >= 48 && $0 <= 57) || ($0 >= 97 && $0 <= 122) || $0 == 95 || $0 == 45
        }
    }

    static func isDisplayVersion(_ value: String) -> Bool {
        let bytes = Array(value.utf8)
        return !bytes.isEmpty && bytes.count <= 128 && bytes.allSatisfy { $0 >= 32 && $0 != 127 }
    }

    static func isDataItemID(_ value: String) -> Bool {
        let prefix = "ocx-data-v1:"
        return value.hasPrefix(prefix) && isLowercaseHex(String(value.dropFirst(prefix.count)), count: 32)
    }

    static func isValidSubject(
        _ subjectID: String?,
        stage: OpenCodexNativeRemovalStageName,
        installationID: String
    ) -> Bool {
        switch stage {
        case .requestValidation, .nativeBoundaryPreTeardown, .nativeBoundaryVerification,
             .nativeRestore, .nativeBoundaryPreTrash, .nativeBoundaryPostTrash,
             .nativeBoundaryReverification, .nativeBoundaryFinalVerification,
             .nativeRecovery, .cleanupJournalRetained:
            return subjectID == nil
        case .candidateRevalidation, .dataPolicy, .teardownPreflight, .teardown,
             .npmUninstall, .packageVerification:
            return subjectID == installationID
        case .cleanupJournal:
            return subjectID == nil || subjectID == installationID
        case .dataTrash:
            return subjectID == nil || subjectID == installationID ||
                subjectID.map(isDataItemID) == true
        }
    }

    static func isValidStage(
        _ stage: OpenCodexNativeRemovalStageName,
        status: OpenCodexRemovalStageStatus,
        code: String
    ) -> Bool {
        switch stage {
        case .requestValidation:
            return OpenCodexRemovalValidation.isValidStage(
                .requestValidation,
                status: status,
                code: code
            )
        case .candidateRevalidation:
            return OpenCodexRemovalValidation.isValidStage(
                .candidateRevalidation,
                status: status,
                code: code
            )
        case .dataPolicy:
            return OpenCodexRemovalValidation.isValidStage(.dataPolicy, status: status, code: code)
        case .teardownPreflight:
            return OpenCodexRemovalValidation.isValidStage(
                .teardownPreflight,
                status: status,
                code: code
            )
        case .cleanupJournal:
            if status == .failed,
               [
                   "teardown_completion_unavailable",
                   "native_restore_execution_intent_unavailable",
                   "native_restore_execution_result_unavailable",
               ].contains(code) {
                return true
            }
            return OpenCodexRemovalValidation.isValidStage(
                .cleanupJournal,
                status: status,
                code: code
            )
        case .teardown:
            if status == .skipped, code == "teardown_already_completed" {
                return true
            }
            return OpenCodexRemovalValidation.isValidStage(
                .teardown,
                status: status,
                code: integratedBoundaryCode(code)
            )
        case .dataTrash:
            return OpenCodexRemovalValidation.isValidStage(
                .dataTrash,
                status: status,
                code: integratedBoundaryCode(code)
            )
        case .npmUninstall:
            return OpenCodexRemovalValidation.isValidStage(
                .npmUninstall,
                status: status,
                code: integratedBoundaryCode(code)
            )
        case .packageVerification:
            return OpenCodexRemovalValidation.isValidStage(
                .packageVerification,
                status: status,
                code: code
            )
        case .nativeBoundaryPreTeardown:
            return matches(
                status,
                code,
                completed: ["native_ownership_reverified"],
                failed: ["native_ownership_unverified"]
            )
        case .nativeBoundaryVerification:
            return matches(
                status,
                code,
                completed: ["native_ownership_reverified"],
                failed: ["native_ownership_unverified", "native_boundary_changed"]
            )
        case .nativeBoundaryPreTrash, .nativeBoundaryPostTrash,
             .nativeBoundaryReverification:
            return matches(
                status,
                code,
                completed: ["native_ownership_reverified"],
                failed: ["native_boundary_changed"]
            )
        case .nativeBoundaryFinalVerification:
            return matches(
                status,
                code,
                completed: [
                    "native_ownership_post_package_verified",
                    "native_ownership_reverified",
                ],
                failed: ["native_boundary_changed"]
            )
        case .nativeRestore:
            return matches(
                status,
                code,
                completed: ["native_restore_applied", "native_already_active"],
                skipped: ["native_already_active"],
                failed: ["native_restore_unverified"]
            )
        case .nativeRecovery:
            return matches(
                status,
                code,
                completed: ["native_recovery_persisted"],
                failed: ["native_recovery_required", "native_recovery_persist_failed"]
            )
        case .cleanupJournalRetained:
            return matches(
                status,
                code,
                completed: ["terminal_receipt_replayable"]
            )
        }
    }

    private static func integratedBoundaryCode(_ code: String) -> String {
        code == "native_boundary_changed" ? "routing_ownership_changed" : code
    }

    private static func matches(
        _ status: OpenCodexRemovalStageStatus,
        _ code: String,
        completed: Set<String> = [],
        skipped: Set<String> = [],
        refused: Set<String> = [],
        failed: Set<String> = []
    ) -> Bool {
        switch status {
        case .completed: completed.contains(code)
        case .skipped: skipped.contains(code)
        case .refused: refused.contains(code)
        case .failed: failed.contains(code)
        }
    }
}

private struct StrictNativeRemovalJSONKey: CodingKey {
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

private enum StrictNativeRemovalJSON {
    static func requireKeys(
        _ decoder: Decoder,
        allowed: Set<String>,
        required: Set<String>? = nil
    ) throws {
        let values = try decoder.container(keyedBy: StrictNativeRemovalJSONKey.self)
        let present = Set(values.allKeys.map(\.stringValue))
        guard present.isSubset(of: allowed),
              (required ?? allowed).isSubset(of: present) else {
            throw OpenCodexNativeRemovalContractError.invalidReceipt
        }
    }
}
