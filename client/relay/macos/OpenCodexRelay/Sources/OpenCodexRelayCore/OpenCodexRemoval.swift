import Foundation

public protocol OpenCodexRemovalExecuting: Sendable {
    func inspect(selection: OpenCodexRemovalSelection) async throws -> OpenCodexDataInventoryReceipt
    func remove(_ request: OpenCodexRemovalRequest) async throws -> OpenCodexRemovalReceipt
}

public enum OpenCodexRemovalContractError: LocalizedError, Equatable, Sendable {
    case invalidSelection
    case invalidRequest
    case invalidInventoryReceipt
    case invalidRemovalReceipt

    public var safeCode: String {
        switch self {
        case .invalidSelection: "opencodex_removal_selection_invalid"
        case .invalidRequest: "opencodex_removal_request_invalid"
        case .invalidInventoryReceipt: "opencodex_inventory_receipt_invalid"
        case .invalidRemovalReceipt: "opencodex_removal_receipt_invalid"
        }
    }

    public var errorDescription: String? { safeCode }
}

public struct OpenCodexRemovalSelection: Codable, Equatable, Sendable {
    public let installationID: String
    public let installationFingerprint: String

    public init(installationID: String, installationFingerprint: String) throws {
        guard OpenCodexRemovalValidation.isLowercaseHex(installationID, count: 24),
              OpenCodexRemovalValidation.isLowercaseHex(installationFingerprint, count: 64) else {
            throw OpenCodexRemovalContractError.invalidSelection
        }
        self.installationID = installationID
        self.installationFingerprint = installationFingerprint
    }

    public init(candidate: OpenCodexInstallationCandidate) throws {
        try self.init(
            installationID: candidate.id,
            installationFingerprint: candidate.fingerprint
        )
    }

    enum CodingKeys: String, CodingKey {
        case installationID = "installation_id"
        case installationFingerprint = "installation_fingerprint"
    }

    public init(from decoder: Decoder) throws {
        try StrictRemovalJSON.requireKeys(
            decoder,
            allowed: ["installation_id", "installation_fingerprint"]
        )
        let values = try decoder.container(keyedBy: CodingKeys.self)
        try self.init(
            installationID: values.decode(String.self, forKey: .installationID),
            installationFingerprint: values.decode(String.self, forKey: .installationFingerprint)
        )
    }
    func inventoryArguments(relayConfig: String) -> [String] {
        [
            "mode", "inspect-open-codex-data",
            "--installation-id", installationID,
            "--installation-fingerprint", installationFingerprint,
            "--config", relayConfig,
            "--json",
        ]
    }
}

public enum OpenCodexRemovalMode: String, Codable, CaseIterable, Sendable {
    case preserveData = "preserve_data"
    case trashSelected = "trash_selected"
}

public struct OpenCodexRemovalRequest: Equatable, Sendable {
    public let selection: OpenCodexRemovalSelection
    public let mode: OpenCodexRemovalMode
    public let dataItemIDs: [String]
    public let expectedRoutingGeneration: UInt64
    public let expectedInventoryRevision: String?
    public let confirmsRemoval: Bool
    public let confirmsTrash: Bool
    public let confirmsInterruptedDataRefresh: Bool
    public let confirmsRebootedProcessRecovery: Bool
    public let confirmsDesktopExited: Bool

    public init(
        selection: OpenCodexRemovalSelection,
        mode: OpenCodexRemovalMode,
        dataItemIDs: [String],
        expectedRoutingGeneration: UInt64,
        expectedInventoryRevision: String? = nil,
        confirmsRemoval: Bool,
        confirmsTrash: Bool,
        confirmsInterruptedDataRefresh: Bool = false,
        confirmsRebootedProcessRecovery: Bool = false,
        confirmsDesktopExited: Bool
    ) throws {
        guard expectedRoutingGeneration > 0,
              confirmsRemoval,
              confirmsDesktopExited,
              !(confirmsInterruptedDataRefresh && confirmsRebootedProcessRecovery),
              dataItemIDs.count <= 128,
              Set(dataItemIDs).count == dataItemIDs.count,
              dataItemIDs.allSatisfy(OpenCodexRemovalValidation.isDataItemID) else {
            throw OpenCodexRemovalContractError.invalidRequest
        }
        switch mode {
        case .preserveData:
            guard dataItemIDs.isEmpty, !confirmsTrash, expectedInventoryRevision == nil else {
                throw OpenCodexRemovalContractError.invalidRequest
            }
        case .trashSelected:
            guard !dataItemIDs.isEmpty, confirmsTrash,
                  expectedInventoryRevision.map({ OpenCodexRemovalValidation.isLowercaseHex($0, count: 64) }) == true else {
                throw OpenCodexRemovalContractError.invalidRequest
            }
        }
        self.selection = selection
        self.mode = mode
        self.dataItemIDs = dataItemIDs
        self.expectedRoutingGeneration = expectedRoutingGeneration
        self.expectedInventoryRevision = expectedInventoryRevision
        self.confirmsRemoval = confirmsRemoval
        self.confirmsTrash = confirmsTrash
        self.confirmsInterruptedDataRefresh = confirmsInterruptedDataRefresh
        self.confirmsRebootedProcessRecovery = confirmsRebootedProcessRecovery
        self.confirmsDesktopExited = confirmsDesktopExited
    }

    func removalArguments(relayConfig: String, codexConfig: String) -> [String] {
        var arguments = [
            "mode", "remove-open-codex",
            "--installation-id", selection.installationID,
            "--installation-fingerprint", selection.installationFingerprint,
            "--removal-mode", mode.rawValue,
            "--expected-routing-generation", String(expectedRoutingGeneration),
        ]
        for itemID in dataItemIDs {
            arguments.append(contentsOf: ["--data-item", itemID])
        }
        arguments.append("--confirm-opencodex-removal")
        if confirmsTrash {
            arguments.append("--confirm-data-trash")
            arguments.append(contentsOf: [
                "--expected-inventory-revision",
                expectedInventoryRevision ?? "",
            ])
        }
        if confirmsInterruptedDataRefresh {
            arguments.append("--confirm-interrupted-data-refresh")
        }
        if confirmsRebootedProcessRecovery {
            arguments.append("--confirm-rebooted-process-recovery")
        }
        arguments.append(contentsOf: [
            "--confirm-desktop-exited",
            "--config", relayConfig,
            "--codex-config", codexConfig,
            "--json",
        ])
        return arguments
    }
}

public enum OpenCodexInventoryStatus: String, Codable, Sendable {
    case absent
    case refused
    case verified
}

public enum OpenCodexInventoryCategory: String, Codable, CaseIterable, Sendable {
    case credentials
    case configuration
    case integrationBackups = "integration-backups"
    case logs
    case runtime
    case artifacts
    case ownershipMetadata = "ownership-metadata"
    case root
    case other
}

public enum OpenCodexInventoryScope: String, Codable, CaseIterable, Sendable {
    case owned
    case ownershipMetadata = "ownership-metadata"
    case configRoot = "config-root"
}

public enum OpenCodexInventoryKind: String, Codable, CaseIterable, Sendable {
    case absent
    case file
    case directory
    case symlink
    case other
}

public struct OpenCodexDataInventoryItem: Codable, Equatable, Identifiable, Sendable {
    public let id: String
    public let category: OpenCodexInventoryCategory
    public let scope: OpenCodexInventoryScope
    public let kind: OpenCodexInventoryKind
    public let relativePath: String
    public let exists: Bool
    public let sensitive: Bool
    public let trashable: Bool

    public init(
        id: String,
        category: OpenCodexInventoryCategory,
        scope: OpenCodexInventoryScope,
        kind: OpenCodexInventoryKind,
        relativePath: String,
        exists: Bool,
        sensitive: Bool,
        trashable: Bool
    ) {
        self.id = id
        self.category = category
        self.scope = scope
        self.kind = kind
        self.relativePath = relativePath
        self.exists = exists
        self.sensitive = sensitive
        self.trashable = trashable
    }

    enum CodingKeys: String, CodingKey {
        case id
        case category
        case scope
        case kind
        case relativePath = "relative_path"
        case exists
        case sensitive
        case trashable
    }

    public init(from decoder: Decoder) throws {
        try StrictRemovalJSON.requireKeys(
            decoder,
            allowed: ["id", "category", "scope", "kind", "relative_path", "exists", "sensitive", "trashable"]
        )
        let values = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            id: try values.decode(String.self, forKey: .id),
            category: try values.decode(OpenCodexInventoryCategory.self, forKey: .category),
            scope: try values.decode(OpenCodexInventoryScope.self, forKey: .scope),
            kind: try values.decode(OpenCodexInventoryKind.self, forKey: .kind),
            relativePath: try values.decode(String.self, forKey: .relativePath),
            exists: try values.decode(Bool.self, forKey: .exists),
            sensitive: try values.decode(Bool.self, forKey: .sensitive),
            trashable: try values.decode(Bool.self, forKey: .trashable)
        )
    }

    fileprivate var isValid: Bool {
        guard OpenCodexRemovalValidation.isDataItemID(id),
              OpenCodexRemovalValidation.isSafeRelativePath(relativePath),
              exists == (kind != .absent),
              (relativePath != "." || scope == .configRoot) else {
            return false
        }
        if trashable {
            guard exists,
                  scope == .owned,
                  category != .ownershipMetadata,
                  category != .root else {
                return false
            }
        }
        switch scope {
        case .owned:
            return category != .ownershipMetadata && category != .root
        case .ownershipMetadata:
            return category == .ownershipMetadata && !trashable
        case .configRoot:
            return category == .root && !trashable
        }
    }
}

public struct OpenCodexDataInventoryReceipt: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let operation: String
    public let status: OpenCodexInventoryStatus
    public let installationID: String
    public let installationFingerprint: String
    public let inventoryRevision: String
    public let routingGeneration: UInt64
    public let items: [OpenCodexDataInventoryItem]

    public init(
        schemaVersion: Int = 2,
        operation: String = "open-codex-data-inventory",
        status: OpenCodexInventoryStatus,
        installationID: String,
        installationFingerprint: String = String(repeating: "0", count: 64),
        inventoryRevision: String = String(repeating: "0", count: 64),
        routingGeneration: UInt64 = 1,
        items: [OpenCodexDataInventoryItem]
    ) {
        self.schemaVersion = schemaVersion
        self.operation = operation
        self.status = status
        self.installationID = installationID
        self.installationFingerprint = installationFingerprint
        self.inventoryRevision = inventoryRevision
        self.routingGeneration = routingGeneration
        self.items = items
    }

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case operation
        case status
        case installationID = "installation_id"
        case installationFingerprint = "installation_fingerprint"
        case inventoryRevision = "inventory_revision"
        case routingGeneration = "routing_generation"
        case items
    }

    public init(from decoder: Decoder) throws {
        try StrictRemovalJSON.requireKeys(
            decoder,
            allowed: [
                "schema_version", "operation", "status", "installation_id",
                "installation_fingerprint", "inventory_revision", "routing_generation", "items",
            ]
        )
        let values = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            schemaVersion: try values.decode(Int.self, forKey: .schemaVersion),
            operation: try values.decode(String.self, forKey: .operation),
            status: try values.decode(OpenCodexInventoryStatus.self, forKey: .status),
            installationID: try values.decode(String.self, forKey: .installationID),
            installationFingerprint: try values.decode(String.self, forKey: .installationFingerprint),
            inventoryRevision: try values.decode(String.self, forKey: .inventoryRevision),
            routingGeneration: try values.decode(UInt64.self, forKey: .routingGeneration),
            items: try values.decode([OpenCodexDataInventoryItem].self, forKey: .items)
        )
    }

    public func validated(for selection: OpenCodexRemovalSelection) throws -> Self {
        guard schemaVersion == 2,
              operation == "open-codex-data-inventory",
              installationID == selection.installationID,
              installationFingerprint == selection.installationFingerprint,
              OpenCodexRemovalValidation.isLowercaseHex(inventoryRevision, count: 64),
              routingGeneration > 0,
              items.count <= 512,
              Set(items.map(\.id)).count == items.count else {
            throw OpenCodexRemovalContractError.invalidInventoryReceipt
        }
        if status != .verified {
            guard items.isEmpty else {
                throw OpenCodexRemovalContractError.invalidInventoryReceipt
            }
        } else {
            guard items.allSatisfy(\.isValid) else {
                throw OpenCodexRemovalContractError.invalidInventoryReceipt
            }
        }
        return self
    }
}

public enum OpenCodexRemovalStatus: String, Codable, Sendable {
    case completed
    case partial
    case failed
}

public enum OpenCodexRemovalStageStatus: String, Codable, Sendable {
    case completed
    case skipped
    case refused
    case failed
}

public enum OpenCodexRemovalStageName: String, Codable, CaseIterable, Sendable {
    case requestValidation = "request_validation"
    case candidateRevalidation = "candidate_revalidation"
    case dataPolicy = "data_policy"
    case teardownPreflight = "teardown_preflight"
    case cleanupJournal = "cleanup_journal"
    case routingPreTeardown = "routing_pre_teardown"
    case teardown
    case routingVerification = "routing_verification"
    case routingPreTrash = "routing_pre_trash"
    case dataTrash = "data_trash"
    case routingPostTrash = "routing_post_trash"
    case routingReverification = "routing_reverification"
    case npmUninstall = "npm_uninstall"
    case routingPostVerification = "routing_post_verification"
    case packageVerification = "package_verification"
    case routingFinalVerification = "routing_final_verification"
    case routingRecovery = "routing_recovery"
    case relayCleanup = "relay_cleanup"
}

public struct OpenCodexRemovalStage: Codable, Equatable, Sendable {
    public let stage: OpenCodexRemovalStageName
    public let status: OpenCodexRemovalStageStatus
    public let code: String
    public let subjectID: String?

    public init(
        stage: OpenCodexRemovalStageName,
        status: OpenCodexRemovalStageStatus,
        code: String,
        subjectID: String? = nil
    ) {
        self.stage = stage
        self.status = status
        self.code = code
        self.subjectID = subjectID
    }

    enum CodingKeys: String, CodingKey {
        case stage
        case status
        case code
        case subjectID = "subject_id"
    }

    public init(from decoder: Decoder) throws {
        try StrictRemovalJSON.requireKeys(
            decoder,
            allowed: ["stage", "status", "code", "subject_id"],
            required: ["stage", "status", "code"]
        )
        let values = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            stage: try values.decode(OpenCodexRemovalStageName.self, forKey: .stage),
            status: try values.decode(OpenCodexRemovalStageStatus.self, forKey: .status),
            code: try values.decode(String.self, forKey: .code),
            subjectID: try values.decodeIfPresent(String.self, forKey: .subjectID)
        )
    }

    fileprivate var isValid: Bool {
        guard OpenCodexRemovalValidation.isValidStage(stage, status: status, code: code) else { return false }
        guard let subjectID else { return true }
        return OpenCodexRemovalValidation.isLowercaseHex(subjectID, count: 24) ||
            OpenCodexRemovalValidation.isDataItemID(subjectID)
    }
}

public struct OpenCodexRemovalReceipt: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let operation: String
    public let status: OpenCodexRemovalStatus
    public let mode: OpenCodexRemovalMode
    public let installationID: String
    public let dataScope: String
    public let selectedDataItems: Int
    public let movedDataItems: Int
    public let packageRemoved: Bool
    public let dataMovementUnknown: Bool
    public let routingRecoveryRequired: Bool
    public let permanentDeleteFallback: Bool
    public let stages: [OpenCodexRemovalStage]

    public init(
        schemaVersion: Int = 2,
        operation: String = "remove-open-codex",
        status: OpenCodexRemovalStatus,
        mode: OpenCodexRemovalMode,
        installationID: String,
        dataScope: String,
        selectedDataItems: Int,
        movedDataItems: Int,
        packageRemoved: Bool,
        dataMovementUnknown: Bool,
        routingRecoveryRequired: Bool,
        permanentDeleteFallback: Bool,
        stages: [OpenCodexRemovalStage]
    ) {
        self.schemaVersion = schemaVersion
        self.operation = operation
        self.status = status
        self.mode = mode
        self.installationID = installationID
        self.dataScope = dataScope
        self.selectedDataItems = selectedDataItems
        self.movedDataItems = movedDataItems
        self.packageRemoved = packageRemoved
        self.dataMovementUnknown = dataMovementUnknown
        self.routingRecoveryRequired = routingRecoveryRequired
        self.permanentDeleteFallback = permanentDeleteFallback
        self.stages = stages
    }

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case operation
        case status
        case mode
        case installationID = "installation_id"
        case dataScope = "data_scope"
        case selectedDataItems = "selected_data_items"
        case movedDataItems = "moved_data_items"
        case packageRemoved = "package_removed"
        case dataMovementUnknown = "data_movement_unknown"
        case routingRecoveryRequired = "routing_recovery_required"
        case permanentDeleteFallback = "permanent_delete_fallback"
        case stages
    }

    public init(from decoder: Decoder) throws {
        try StrictRemovalJSON.requireKeys(
            decoder,
            allowed: [
                "schema_version", "operation", "status", "mode", "installation_id", "data_scope",
                "selected_data_items", "moved_data_items", "package_removed", "data_movement_unknown",
                "routing_recovery_required", "permanent_delete_fallback", "stages",
            ]
        )
        let values = try decoder.container(keyedBy: CodingKeys.self)
        self.init(
            schemaVersion: try values.decode(Int.self, forKey: .schemaVersion),
            operation: try values.decode(String.self, forKey: .operation),
            status: try values.decode(OpenCodexRemovalStatus.self, forKey: .status),
            mode: try values.decode(OpenCodexRemovalMode.self, forKey: .mode),
            installationID: try values.decode(String.self, forKey: .installationID),
            dataScope: try values.decode(String.self, forKey: .dataScope),
            selectedDataItems: try values.decode(Int.self, forKey: .selectedDataItems),
            movedDataItems: try values.decode(Int.self, forKey: .movedDataItems),
            packageRemoved: try values.decode(Bool.self, forKey: .packageRemoved),
            dataMovementUnknown: try values.decode(Bool.self, forKey: .dataMovementUnknown),
            routingRecoveryRequired: try values.decode(Bool.self, forKey: .routingRecoveryRequired),
            permanentDeleteFallback: try values.decode(Bool.self, forKey: .permanentDeleteFallback),
            stages: try values.decode([OpenCodexRemovalStage].self, forKey: .stages)
        )
    }

    public var isSuccessful: Bool {
        status == .completed &&
            packageRemoved &&
            !dataMovementUnknown &&
            !routingRecoveryRequired &&
            !permanentDeleteFallback &&
            (mode == .preserveData || movedDataItems == selectedDataItems) &&
            hasCompletedTerminalProof
    }

    public var requiresDataSelectionRefresh: Bool {
        dataMovementUnknown || stages.contains {
            $0.code == "data_selection_refresh_required" ||
                $0.code == "trash_unsupported"
        }
    }

    public var requiresWholeMacReboot: Bool {
        stages.contains { $0.code == "process_cleanup_unverified" }
    }

    /// Returns the original bounded failure code only when a validated Relay
    /// receipt proves that removal stopped before cleanup intent or any
    /// package, data, routing, process-unknown, or reboot boundary existed.
    /// Callers may then discard a speculative `.inFlight` UI checkpoint.
    public var verifiedPreMutationFailureCode: String? {
        guard status == .failed,
              mode == .preserveData,
              selectedDataItems == 0,
              movedDataItems == 0,
              !packageRemoved,
              !dataMovementUnknown,
              !routingRecoveryRequired,
              !permanentDeleteFallback,
              let terminal = stages.last,
              terminal.status == .refused || terminal.status == .failed else {
            return nil
        }

        switch stages.count {
        case 1:
            return terminal.stage == .requestValidation || terminal.stage == .candidateRevalidation
                ? terminal.code
                : nil
        case 2:
            guard stages[0].stage == .candidateRevalidation,
                  stages[0].status == .completed,
                  stages[0].code == "candidate_verified" else {
                return nil
            }
            return terminal.stage == .candidateRevalidation ||
                terminal.stage == .dataPolicy ||
                terminal.stage == .teardownPreflight
                ? terminal.code
                : nil
        case 3:
            guard stages[0].stage == .candidateRevalidation,
                  stages[0].status == .completed,
                  stages[0].code == "candidate_verified",
                  stages[1].stage == .teardownPreflight,
                  stages[1].status == .completed,
                  stages[1].code == "teardown_preflight_verified",
                  terminal.stage == .candidateRevalidation,
                  terminal.code == "teardown_candidate_changed" else {
                return nil
            }
            return terminal.code
        default:
            return nil
        }
    }

    public func validated(for request: OpenCodexRemovalRequest) throws -> Self {
        let expectedScope = request.mode == .preserveData ? "preserved" : "explicit_items_only"
        let stageIdentities = stages.map {
            "\($0.stage.rawValue)|\($0.status.rawValue)|\($0.code)|\($0.subjectID ?? "")"
        }
        guard schemaVersion == 2,
              operation == "remove-open-codex",
              mode == request.mode,
              installationID == request.selection.installationID,
              dataScope == expectedScope,
              selectedDataItems == request.dataItemIDs.count,
              selectedDataItems >= 0,
              movedDataItems >= 0,
              movedDataItems <= selectedDataItems,
              stages.count > 0,
              stages.count <= 64,
              Set(stageIdentities).count == stages.count,
              stages.allSatisfy(\.isValid),
              stages.allSatisfy({
                  OpenCodexRemovalValidation.isValidSubject(
                      $0.subjectID,
                      for: $0.stage,
                      installationID: installationID
                  )
              }),
              !permanentDeleteFallback else {
            throw OpenCodexRemovalContractError.invalidRemovalReceipt
        }
        if mode == .preserveData {
            guard selectedDataItems == 0,
                  movedDataItems == 0,
                  !dataMovementUnknown else {
                throw OpenCodexRemovalContractError.invalidRemovalReceipt
            }
        }
        if dataMovementUnknown {
            guard mode == .trashSelected, status != .completed else {
                throw OpenCodexRemovalContractError.invalidRemovalReceipt
            }
        }

        let packageProofs = stageIndices(
            stage: .packageVerification,
            status: .completed,
            code: "package_absent"
        )
        let routingProofs = stageIndices(
            stage: .routingFinalVerification,
            status: .completed,
            code: "routing_ownership_reverified"
        )
        let relayCleanupIndices = stages.indices.filter { stages[$0].stage == .relayCleanup }
        if packageRemoved {
            guard status != .failed,
                  packageProofs.count == 1,
                  routingProofs.count == 1 else {
                throw OpenCodexRemovalContractError.invalidRemovalReceipt
            }
            if routingRecoveryRequired {
                guard status == .partial, relayCleanupIndices.isEmpty else {
                    throw OpenCodexRemovalContractError.invalidRemovalReceipt
                }
            } else {
                guard relayCleanupIndices.count == 1,
                      let cleanupIndex = relayCleanupIndices.first,
                      cleanupIndex == stages.indices.last,
                      packageProofs[0] < cleanupIndex,
                      routingProofs[0] < cleanupIndex else {
                    throw OpenCodexRemovalContractError.invalidRemovalReceipt
                }
                if stages[cleanupIndex].status == .failed {
                    guard status == .partial else {
                        throw OpenCodexRemovalContractError.invalidRemovalReceipt
                    }
                }
            }
        } else {
            guard packageProofs.isEmpty,
                  routingProofs.isEmpty,
                  relayCleanupIndices.isEmpty else {
                throw OpenCodexRemovalContractError.invalidRemovalReceipt
            }
        }

        let recoveryStages = stages.filter { $0.stage == .routingRecovery }
        if routingRecoveryRequired {
            guard status != .completed, recoveryStages.count == 1 else {
                throw OpenCodexRemovalContractError.invalidRemovalReceipt
            }
        } else if !recoveryStages.isEmpty {
            throw OpenCodexRemovalContractError.invalidRemovalReceipt
        }

        if status == .completed {
            guard isSuccessful,
                  stages.allSatisfy({ $0.status == .completed || $0.status == .skipped }) else {
                throw OpenCodexRemovalContractError.invalidRemovalReceipt
            }
        }
        return self
    }

    private var hasCompletedTerminalProof: Bool {
        let packageProofs = stageIndices(
            stage: .packageVerification,
            status: .completed,
            code: "package_absent"
        )
        let routingProofs = stageIndices(
            stage: .routingFinalVerification,
            status: .completed,
            code: "routing_ownership_reverified"
        )
        let cleanupProofs = stageIndices(
            stage: .relayCleanup,
            status: .completed,
            code: "relay_cleanup_completed"
        )
        guard packageProofs.count == 1,
              routingProofs.count == 1,
              cleanupProofs.count == 1,
              let cleanupIndex = cleanupProofs.first,
              cleanupIndex == stages.indices.last else {
            return false
        }
        return packageProofs[0] < cleanupIndex && routingProofs[0] < cleanupIndex
    }

    private func stageIndices(
        stage: OpenCodexRemovalStageName,
        status: OpenCodexRemovalStageStatus,
        code: String
    ) -> [Int] {
        stages.indices.filter {
            stages[$0].stage == stage && stages[$0].status == status && stages[$0].code == code
        }
    }
}

public enum OpenCodexRemovalRecoveryKind: String, Codable, Sendable {
    case inFlight = "in_flight"
    case dataSelectionRefreshRequired = "data_selection_refresh_required"
    case rebootRequired = "reboot_required"
    case routingRecoveryRequired = "routing_recovery_required"
}

public struct OpenCodexRemovalRecoverySession: Codable, Equatable, Sendable {
    public static let schemaVersion = 2

    public let schema: Int
    public let selection: OpenCodexRemovalSelection
    public let mode: OpenCodexRemovalMode
    public let orderedDataItemIDs: [String]
    public let retiredDataItemIDs: [String]
    public let recoveryKind: OpenCodexRemovalRecoveryKind
    public let lastCode: String
    /// Opaque revision of the inventory whose selected IDs were confirmed.
    /// Selective-trash recovery requires it so a restarted app cannot bind
    /// the durable operation to a different inventory.
    public let inventoryRevision: String?
    /// The durable routing epoch that produced a routing-recovery receipt.
    /// It is optional for older reboot/data-refresh sessions, but a typed
    /// routing-recovery continuation must carry it so the next confirmation
    /// cannot silently bind to a different parked state.
    public let expectedRoutingGeneration: UInt64?

    public init(
        schema: Int = schemaVersion,
        selection: OpenCodexRemovalSelection,
        mode: OpenCodexRemovalMode,
        orderedDataItemIDs: [String],
        retiredDataItemIDs: [String],
        recoveryKind: OpenCodexRemovalRecoveryKind,
        lastCode: String,
        inventoryRevision: String? = nil,
        expectedRoutingGeneration: UInt64? = nil
    ) throws {
        guard (schema == 1 || schema == Self.schemaVersion),
              expectedRoutingGeneration.map({ $0 > 0 }) ?? true,
              orderedDataItemIDs.count <= 128,
              retiredDataItemIDs.count <= 512,
              Set(orderedDataItemIDs).count == orderedDataItemIDs.count,
              Set(retiredDataItemIDs).count == retiredDataItemIDs.count,
              Set(orderedDataItemIDs).isDisjoint(with: Set(retiredDataItemIDs)),
              orderedDataItemIDs.allSatisfy(OpenCodexRemovalValidation.isDataItemID),
              retiredDataItemIDs.allSatisfy(OpenCodexRemovalValidation.isDataItemID),
              OpenCodexRemovalValidation.isSafeToken(lastCode) else {
            throw OpenCodexRemovalContractError.invalidRequest
        }
        switch mode {
        case .preserveData:
            guard orderedDataItemIDs.isEmpty, inventoryRevision == nil else {
                throw OpenCodexRemovalContractError.invalidRequest
            }
        case .trashSelected:
            guard schema == Self.schemaVersion,
                  !orderedDataItemIDs.isEmpty,
                  inventoryRevision.map({ OpenCodexRemovalValidation.isLowercaseHex($0, count: 64) }) == true else {
                throw OpenCodexRemovalContractError.invalidRequest
            }
        }
        if recoveryKind == .routingRecoveryRequired,
           expectedRoutingGeneration == nil {
            // Legacy routing-recovery records do not prove which durable
            // generation was reviewed. Refuse them instead of upgrading a
            // stale/ambiguous record into an actionable continuation.
            throw OpenCodexRemovalContractError.invalidRequest
        }
        self.schema = schema
        self.selection = selection
        self.mode = mode
        self.orderedDataItemIDs = orderedDataItemIDs
        self.retiredDataItemIDs = retiredDataItemIDs
        self.recoveryKind = recoveryKind
        self.lastCode = lastCode
        self.inventoryRevision = inventoryRevision
        self.expectedRoutingGeneration = expectedRoutingGeneration
    }

    enum CodingKeys: String, CodingKey {
        case schema
        case selection
        case mode
        case orderedDataItemIDs = "ordered_data_item_ids"
        case retiredDataItemIDs = "retired_data_item_ids"
        case recoveryKind = "recovery_kind"
        case lastCode = "last_code"
        case inventoryRevision = "inventory_revision"
        case expectedRoutingGeneration = "expected_routing_generation"
    }

    public init(from decoder: Decoder) throws {
        try StrictRemovalJSON.requireKeys(
            decoder,
            allowed: [
                "schema", "selection", "mode", "ordered_data_item_ids", "retired_data_item_ids",
                "recovery_kind", "last_code", "inventory_revision", "expected_routing_generation",
            ],
            required: [
                "schema", "selection", "mode", "ordered_data_item_ids", "retired_data_item_ids",
                "recovery_kind", "last_code",
            ]
        )
        let values = try decoder.container(keyedBy: CodingKeys.self)
        try self.init(
            schema: values.decode(Int.self, forKey: .schema),
            selection: values.decode(OpenCodexRemovalSelection.self, forKey: .selection),
            mode: values.decode(OpenCodexRemovalMode.self, forKey: .mode),
            orderedDataItemIDs: values.decode([String].self, forKey: .orderedDataItemIDs),
            retiredDataItemIDs: values.decode([String].self, forKey: .retiredDataItemIDs),
            recoveryKind: values.decode(OpenCodexRemovalRecoveryKind.self, forKey: .recoveryKind),
            lastCode: values.decode(String.self, forKey: .lastCode),
            inventoryRevision: values.decodeIfPresent(String.self, forKey: .inventoryRevision),
            expectedRoutingGeneration: values.decodeIfPresent(
                UInt64.self,
                forKey: .expectedRoutingGeneration
            )
        )
    }
}

private enum OpenCodexRemovalValidation {
    static func isLowercaseHex(_ value: String, count: Int) -> Bool {
        value.utf8.count == count && value.utf8.allSatisfy {
            ($0 >= 48 && $0 <= 57) || ($0 >= 97 && $0 <= 102)
        }
    }

    static func isDataItemID(_ value: String) -> Bool {
        let prefix = "ocx-data-v1:"
        guard value.hasPrefix(prefix) else { return false }
        return isLowercaseHex(String(value.dropFirst(prefix.count)), count: 32)
    }

    static func isSafeToken(_ value: String) -> Bool {
        let bytes = Array(value.utf8)
        return !bytes.isEmpty && bytes.count <= 64 && bytes.allSatisfy {
            ($0 >= 48 && $0 <= 57) || ($0 >= 97 && $0 <= 122) || $0 == 95 || $0 == 45
        }
    }

    static func isSafeRelativePath(_ value: String) -> Bool {
        guard !value.isEmpty,
              value.utf8.count <= 4_096,
              !value.hasPrefix("/"),
              !value.contains("\0") else {
            return false
        }
        if value == "." { return true }
        return value.split(separator: "/", omittingEmptySubsequences: false).allSatisfy {
            !$0.isEmpty && $0 != "." && $0 != ".."
        }
    }

    static func isValidStage(
        _ stage: OpenCodexRemovalStageName,
        status: OpenCodexRemovalStageStatus,
        code: String
    ) -> Bool {
        switch stage {
        case .requestValidation:
            return matches(
                status,
                code,
                refused: ["confirmation_required", "invalid_request"],
                failed: ["coordinator_unavailable"]
            )
        case .candidateRevalidation:
            return matches(
                status,
                code,
                completed: ["candidate_verified"],
                refused: selectionFailureCodes
            )
        case .dataPolicy:
            return matches(status, code, refused: ["teardown_unsupported"])
        case .teardownPreflight:
            return matches(
                status,
                code,
                completed: ["teardown_preflight_verified"],
                refused: [
                    "teardown_preflight_failed", "teardown_result_invalid",
                    "teardown_verification_failed", "teardown_candidate_changed",
                ],
                failed: executionFailureCodes.union(["teardown_preflight_failed"])
            )
        case .cleanupJournal:
            return matches(
                status,
                code,
                completed: [
                    "cleanup_intent_persisted", "data_outcome_persisted", "cleanup_journal_persisted",
                    "package_execution_in_flight", "package_cleanup_verified",
                    "package_execution_not_started", "cleanup_resume", "cleanup_intent_reconciled",
                    "data_outcome_reconciled",
                ],
                refused: ["removal_in_flight"],
                failed: [
                    "cleanup_intent_unavailable", "data_outcome_journal_unavailable",
                    "cleanup_journal_unavailable", "package_execution_intent_unavailable",
                    "package_execution_result_unavailable", "cleanup_journal_invalid",
                    "cleanup_interrupted_before_package", "teardown_execution_intent_unavailable",
                    "trash_execution_intent_unavailable", "teardown_execution_result_unavailable",
                    "trash_execution_result_unavailable",
                ]
            )
        case .routingPreTeardown:
            return matches(
                status,
                code,
                completed: ["routing_ownership_verified"],
                failed: ["routing_ownership_unverified"]
            )
        case .teardown:
            return matches(
                status,
                code,
                completed: ["teardown_completed"],
                refused: selectionFailureCodes.union([
                    "teardown_partial", "teardown_refused", "routing_ownership_changed",
                ]),
                failed: executionFailureCodes.union([
                    "teardown_not_started", "teardown_receipt_invalid", "teardown_failed",
                    "teardown_result_invalid", "teardown_verification_failed",
                    "teardown_execution_intent_unavailable", "teardown_execution_result_unavailable",
                ])
            )
        case .routingVerification:
            return matches(
                status,
                code,
                completed: ["routing_ownership_verified", "routing_ownership_reverified"],
                failed: ["routing_ownership_unverified", "routing_ownership_changed"]
            )
        case .routingPreTrash, .routingPostTrash, .routingReverification,
             .routingPostVerification, .routingFinalVerification:
            return matches(
                status,
                code,
                completed: ["routing_ownership_reverified"],
                failed: ["routing_ownership_changed"]
            )
        case .dataTrash:
            return matches(
                status,
                code,
                completed: ["trash_completed"],
                skipped: ["data_preserved"],
                refused: selectionFailureCodes.union([
                    "trash_partial", "trash_unsupported", "data_selection_refresh_required",
                    "routing_ownership_changed",
                ]),
                failed: executionFailureCodes.union([
                    "trash_not_started", "trash_receipt_invalid", "trash_failed",
                    "trash_execution_intent_unavailable", "trash_execution_result_unavailable",
                ])
            )
        case .npmUninstall:
            return matches(
                status,
                code,
                completed: ["npm_uninstall_completed"],
                refused: selectionFailureCodes.union(["routing_ownership_changed"]),
                failed: executionFailureCodes.union([
                    "npm_uninstall_not_started", "npm_uninstall_failed",
                ])
            )
        case .packageVerification:
            return matches(
                status,
                code,
                completed: ["package_absent"],
                failed: ["package_removal_unverified"]
            )
        case .routingRecovery:
            return matches(
                status,
                code,
                completed: ["routing_recovery_persisted"],
                failed: ["routing_recovery_persist_failed"]
            )
        case .relayCleanup:
            return matches(
                status,
                code,
                completed: ["relay_cleanup_completed"],
                failed: [
                    "relay_config_cleanup_failed", "enrollment_cleanup_failed",
                    "cleanup_journal_remove_failed", "finalization_proof_unavailable",
                ]
            )
        }
    }

    static func isValidSubject(
        _ subjectID: String?,
        for stage: OpenCodexRemovalStageName,
        installationID: String
    ) -> Bool {
        switch stage {
        case .requestValidation, .routingPreTeardown, .routingVerification, .routingPreTrash,
             .routingPostTrash, .routingReverification, .routingPostVerification,
             .routingFinalVerification, .routingRecovery, .relayCleanup:
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

    private static let selectionFailureCodes: Set<String> = [
        "invalid_request", "candidate_not_found", "candidate_changed", "manual_removal_required",
        "teardown_unsupported", "teardown_candidate_changed", "teardown_refused",
        "operation_timed_out", "operation_cancelled", "operation_failed",
    ]

    private static let executionFailureCodes: Set<String> = [
        "routing_ownership_changed", "operation_timed_out", "operation_cancelled",
        "process_cleanup_unverified", "child_output_invalid", "operation_failed",
    ]
}

private struct StrictRemovalJSONKey: CodingKey {
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

private enum StrictRemovalJSON {
    static func requireKeys(
        _ decoder: Decoder,
        allowed: Set<String>,
        required: Set<String>? = nil
    ) throws {
        let values = try decoder.container(keyedBy: StrictRemovalJSONKey.self)
        let present = Set(values.allKeys.map(\.stringValue))
        guard present.isSubset(of: allowed),
              (required ?? allowed).isSubset(of: present) else {
            throw OpenCodexRemovalContractError.invalidRemovalReceipt
        }
    }
}
