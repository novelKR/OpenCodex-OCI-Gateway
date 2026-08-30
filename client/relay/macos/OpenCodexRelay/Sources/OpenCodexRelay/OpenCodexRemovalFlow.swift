import Foundation
import OpenCodexRelayCore

protocol OpenCodexRemovalRecoverySessionStoring: AnyObject {
    func load() throws -> OpenCodexRemovalRecoverySession?
    func save(_ session: OpenCodexRemovalRecoverySession) throws
    func clear()
}

extension OpenCodexRemovalRecoverySessionStoring {
    func clearAndVerify() throws {
        clear()
        guard try load() == nil else {
            throw OpenCodexRemovalContractError.invalidRequest
        }
    }
}

final class UserDefaultsOpenCodexRemovalRecoverySessionStore: OpenCodexRemovalRecoverySessionStoring {
    private static let maximumBytes = 32 * 1024

    private let defaults: UserDefaults
    private let key: String
    private let legacyKey: String

    init(
        defaults: UserDefaults = .standard,
        key: String = "openCodexRemovalRecoverySession.v2",
        legacyKey: String = "openCodexRemovalRecoverySession.v1"
    ) {
        self.defaults = defaults
        self.key = key
        self.legacyKey = legacyKey
    }

    func load() throws -> OpenCodexRemovalRecoverySession? {
        let current = defaults.data(forKey: key)
        let legacy = defaults.data(forKey: legacyKey)
        guard current == nil || legacy == nil else {
            throw OpenCodexRemovalContractError.invalidRequest
        }
        guard let data = current ?? legacy else { return nil }
        guard !data.isEmpty, data.count <= Self.maximumBytes else {
            throw OpenCodexRemovalContractError.invalidRequest
        }
        let session = try JSONDecoder().decode(OpenCodexRemovalRecoverySession.self, from: data)
        if current != nil {
            guard session.schema == OpenCodexRemovalRecoverySession.schemaVersion else {
                throw OpenCodexRemovalContractError.invalidRequest
            }
        } else {
            guard session.schema == 1 || session.schema == 2,
                  session.context == .integrated else {
                throw OpenCodexRemovalContractError.invalidRequest
            }
        }
        return session
    }

    func save(_ session: OpenCodexRemovalRecoverySession) throws {
        guard session.schema == OpenCodexRemovalRecoverySession.schemaVersion else {
            throw OpenCodexRemovalContractError.invalidRequest
        }
        let data = try JSONEncoder().encode(session)
        guard !data.isEmpty, data.count <= Self.maximumBytes else {
            throw OpenCodexRemovalContractError.invalidRequest
        }
        defaults.set(data, forKey: key)
        guard defaults.data(forKey: key) == data else {
            throw OpenCodexRemovalContractError.invalidRequest
        }
        defaults.removeObject(forKey: legacyKey)
    }

    func clear() {
        defaults.removeObject(forKey: key)
        defaults.removeObject(forKey: legacyKey)
    }
}

enum OpenCodexRemovalPhase: Equatable {
    case actions
    case handoff
    case loadingInventory
    case options
    case confirmRemoval
    case confirmTrash
    case quittingDesktop
    case removing
    case dataRefreshRequired
    case rebootRequired
    case routingRecoveryRequired
    case nativeRecoveryRequired
    case nativeTerminalCleanupPending
    case result
    case failed
}

enum OpenCodexHandoffProgressPhase: Int, Equatable {
    case preflight
    case desktopExit
    case openCodexOperation
    case desktopRelaunch
    case statusRefresh
    case completed
    case failed
}

struct OpenCodexHandoffProgress: Equatable {
    let action: OpenCodexHandoffAction
    var phase: OpenCodexHandoffProgressPhase
    var failedPhase: OpenCodexHandoffProgressPhase?
    var result: SafeStatusMessage?

    init(action: OpenCodexHandoffAction) {
        self.action = action
        self.phase = .preflight
        self.failedPhase = nil
        self.result = nil
    }
}

enum OpenCodexRemovalExecutionPhase: Int, CaseIterable, Equatable {
    case preflight
    case desktopExit
    case homebrewProtection
    case candidateRevalidation
    case teardown
    case packageRemoval
    case resultVerification
    case permissionRestore
    case desktopRelaunch
    case statusRefresh
    case completed
    case failed
}

struct OpenCodexRemovalExecutionProgress: Equatable {
    var phase: OpenCodexRemovalExecutionPhase = .preflight
    var failedPhase: OpenCodexRemovalExecutionPhase?
    var result: SafeStatusMessage?
    let usesHomebrewGuard: Bool
}

struct OpenCodexRemovalFlow: Identifiable, Equatable {
    let id: String
    let context: OpenCodexRemovalContext
    var selection: OpenCodexRemovalSelection
    var candidate: OpenCodexInstallationCandidate?
    var nativeSelection: OpenCodexNativeRemovalSelection?
    var nativeCandidate: OpenCodexNativeRemovalCandidate?
    var phase: OpenCodexRemovalPhase
    var inventory: OpenCodexDataInventoryReceipt?
    var nativeInventory: OpenCodexNativeDataInventoryReceipt?
    var mode: OpenCodexRemovalMode
    var selectedDataItemIDs: [String]
    var retiredDataItemIDs: Set<String>
    var recoveryInventoryRevision: String?
    var expectedRoutingGeneration: UInt64?
    var expectedBoundaryRevision: String?
    var receipt: OpenCodexRemovalReceipt?
    var nativeReceipt: OpenCodexNativeRemovalReceipt?
    var terminalReceiptDigest: String?
    var failure: SafeStatusMessage?
    var handoffProgress: OpenCodexHandoffProgress?
    var removalProgress: OpenCodexRemovalExecutionProgress?
    var candidateRevalidationRequired: Bool
    var confirmsInterruptedDataRefresh: Bool
    var confirmsRebootedProcessRecovery: Bool
    /// The typed persisted continuation marker, when this flow was restored
    /// from a recovery session. Fresh candidate-driven uninstall flows have
    /// no marker and must use the ordinary stable-route predicate.
    var recoveryKind: OpenCodexRemovalRecoveryKind?

    init(candidate: OpenCodexInstallationCandidate, selection: OpenCodexRemovalSelection) {
        self.id = selection.installationID
        self.context = .integrated
        self.selection = selection
        self.candidate = candidate
        self.nativeSelection = nil
        self.nativeCandidate = nil
        self.phase = .actions
        self.inventory = nil
        self.nativeInventory = nil
        self.mode = .preserveData
        self.selectedDataItemIDs = []
        self.retiredDataItemIDs = []
        self.recoveryInventoryRevision = nil
        self.expectedRoutingGeneration = nil
        self.expectedBoundaryRevision = nil
        self.receipt = nil
        self.nativeReceipt = nil
        self.terminalReceiptDigest = nil
        self.failure = nil
        self.handoffProgress = nil
        self.removalProgress = nil
        self.candidateRevalidationRequired = false
        self.confirmsInterruptedDataRefresh = false
        self.confirmsRebootedProcessRecovery = false
        self.recoveryKind = nil
    }

    init(
        nativeCandidate: OpenCodexNativeRemovalCandidate,
        nativeSelection: OpenCodexNativeRemovalSelection,
        selection: OpenCodexRemovalSelection
    ) {
        self.id = selection.installationID
        self.context = .standaloneNative
        self.selection = selection
        self.candidate = nil
        self.nativeSelection = nativeSelection
        self.nativeCandidate = nativeCandidate
        self.phase = .actions
        self.inventory = nil
        self.nativeInventory = nil
        self.mode = .preserveData
        self.selectedDataItemIDs = []
        self.retiredDataItemIDs = []
        self.recoveryInventoryRevision = nil
        self.expectedRoutingGeneration = nil
        self.expectedBoundaryRevision = nativeSelection.boundaryRevision
        self.receipt = nil
        self.nativeReceipt = nil
        self.terminalReceiptDigest = nil
        self.failure = nil
        self.handoffProgress = nil
        self.removalProgress = nil
        self.candidateRevalidationRequired = false
        self.confirmsInterruptedDataRefresh = false
        self.confirmsRebootedProcessRecovery = false
        self.recoveryKind = nil
    }

    init(recoverySession: OpenCodexRemovalRecoverySession) {
        self.id = recoverySession.selection.installationID
        self.context = recoverySession.context
        self.selection = recoverySession.selection
        self.candidate = nil
        if recoverySession.context == .standaloneNative,
           let boundary = recoverySession.expectedBoundaryRevision,
           let restore = recoverySession.nativeRestoreFingerprint {
            self.nativeSelection = try? OpenCodexNativeRemovalSelection(
                installationID: recoverySession.selection.installationID,
                installationFingerprint: recoverySession.selection.installationFingerprint,
                nativeRestoreFingerprint: restore,
                boundaryRevision: boundary
            )
        } else {
            self.nativeSelection = nil
        }
        self.nativeCandidate = nil
        self.inventory = nil
        self.nativeInventory = nil
        self.mode = recoverySession.mode
        self.selectedDataItemIDs = recoverySession.orderedDataItemIDs
        self.retiredDataItemIDs = Set(recoverySession.retiredDataItemIDs)
        self.recoveryInventoryRevision = recoverySession.inventoryRevision
        self.receipt = nil
        self.nativeReceipt = nil
        self.terminalReceiptDigest = recoverySession.terminalReceiptDigest
        self.failure = nil
        self.handoffProgress = nil
        self.removalProgress = nil
        self.candidateRevalidationRequired = true
        self.confirmsInterruptedDataRefresh = recoverySession.recoveryKind == .dataSelectionRefreshRequired
        self.confirmsRebootedProcessRecovery = recoverySession.recoveryKind == .rebootRequired || recoverySession.recoveryKind == .inFlight
        self.recoveryKind = recoverySession.recoveryKind
        self.expectedRoutingGeneration = recoverySession.expectedRoutingGeneration
        self.expectedBoundaryRevision = recoverySession.expectedBoundaryRevision
        switch recoverySession.recoveryKind {
        case .dataSelectionRefreshRequired:
            self.phase = .dataRefreshRequired
        case .inFlight, .rebootRequired:
            self.phase = .rebootRequired
        case .routingRecoveryRequired:
            self.phase = .routingRecoveryRequired
        case .nativeRecoveryRequired:
            self.phase = .nativeRecoveryRequired
        case .terminalAckPending:
            self.phase = .nativeTerminalCleanupPending
        }
    }

    var automaticRemovalEligible: Bool {
        guard !candidateRevalidationRequired else { return false }
        switch context {
        case .integrated: return candidate?.isAutomaticRemovalEligible == true
        case .standaloneNative: return nativeCandidate?.automaticRemovalEligible == true
        }
    }

    var displayVersion: String? { candidate?.version ?? nativeCandidate?.version }
    var displayManager: OpenCodexPackageManager? { candidate?.manager ?? nativeCandidate?.manager }
    var displayTier: OpenCodexDiscoveryTier? { candidate?.tier }
    var requiresHomebrewGuard: Bool {
        candidate?.requiresHomebrewGuard == true || nativeCandidate?.homebrewGuardRequired == true
    }
    var teardownAdapterID: String? { candidate?.teardownAdapterID }
    var usesRelayPreservingTeardown: Bool {
        switch context {
        case .integrated:
            return candidate?.teardownCapability == .relayPreserveV1 &&
                (candidate?.dataCapability == .preserveOnly ||
                    candidate?.dataCapability == .selectiveTrashV1) &&
                candidate?.teardownCompatibilityReason == "compatible"
        case .standaloneNative:
            return nativeCandidate?.automaticRemovalEligible == true
        }
    }

    var supportsSelectiveTrash: Bool {
        guard usesRelayPreservingTeardown else { return false }
        switch context {
        case .integrated: return candidate?.dataCapability == .selectiveTrashV1
        case .standaloneNative: return nativeCandidate?.dataCapability == .selectiveTrashV1
        }
    }

    var inventoryItems: [OpenCodexDataInventoryItem] {
        inventory?.items ?? nativeInventory?.items ?? []
    }

    func isSelectable(_ item: OpenCodexDataInventoryItem) -> Bool {
        item.exists && item.trashable && !retiredDataItemIDs.contains(item.id)
    }

    var selectedItems: [OpenCodexDataInventoryItem] {
        let selected = Set(selectedDataItemIDs)
        return inventoryItems.filter { selected.contains($0.id) }
    }

    var canContinueFromOptions: Bool {
        guard usesRelayPreservingTeardown else { return false }
        switch mode {
        case .preserveData:
            return selectedDataItemIDs.isEmpty
        case .trashSelected:
            let inventoryVerified = inventory?.status == .verified || nativeInventory?.status == .verified
            return supportsSelectiveTrash && inventoryVerified &&
                !selectedDataItemIDs.isEmpty &&
                selectedItems.count == selectedDataItemIDs.count
        }
    }

    /// Only sessions restored from reboot/in-flight recovery may review the
    /// deliberately parked routing projection. New removal attempts and other
    /// recovery kinds must continue to satisfy the normal uninstall predicate.
    var isSavedRebootOrInFlightRecovery: Bool {
        candidate == nil && nativeCandidate == nil &&
            (recoveryKind == .inFlight || recoveryKind == .rebootRequired) &&
            confirmsRebootedProcessRecovery &&
            !confirmsInterruptedDataRefresh
    }

    /// A routing-recovery receipt is a separate persisted continuation. It may
    /// re-review the parked generation after an explicit routing-recovery
    /// confirmation, but it must never broaden the ordinary uninstall
    /// predicate. The combination of a nil candidate, typed
    /// `.routingRecoveryRequired` marker, and nonzero parked generation is
    /// established only after the routing-recovery session has been persisted;
    /// a fresh candidate flow can never satisfy it. Keep this independent of
    /// `phase` because the review action advances the saved flow to package
    /// confirmation.
    var isSavedRoutingRecovery: Bool {
        context == .integrated && candidate == nil &&
            recoveryKind == .routingRecoveryRequired &&
            expectedRoutingGeneration != nil &&
            !confirmsInterruptedDataRefresh &&
            !confirmsRebootedProcessRecovery
    }

    var isSavedNativeRecovery: Bool {
        context == .standaloneNative && candidate == nil && nativeCandidate == nil &&
            recoveryKind == .nativeRecoveryRequired && expectedBoundaryRevision != nil
    }

    var isSavedTerminalAcknowledgement: Bool {
        context == .standaloneNative && candidate == nil && nativeCandidate == nil &&
            recoveryKind == .terminalAckPending && terminalReceiptDigest != nil &&
            expectedBoundaryRevision != nil
    }
}
