import Foundation
import OpenCodexRelayCore

protocol OpenCodexRemovalRecoverySessionStoring: AnyObject {
    func load() throws -> OpenCodexRemovalRecoverySession?
    func save(_ session: OpenCodexRemovalRecoverySession) throws
    func clear()
}

final class UserDefaultsOpenCodexRemovalRecoverySessionStore: OpenCodexRemovalRecoverySessionStoring {
    private static let maximumBytes = 32 * 1024

    private let defaults: UserDefaults
    private let key: String

    init(
        defaults: UserDefaults = .standard,
        key: String = "openCodexRemovalRecoverySession.v1"
    ) {
        self.defaults = defaults
        self.key = key
    }

    func load() throws -> OpenCodexRemovalRecoverySession? {
        guard let data = defaults.data(forKey: key) else { return nil }
        guard !data.isEmpty, data.count <= Self.maximumBytes else {
            throw OpenCodexRemovalContractError.invalidRequest
        }
        return try JSONDecoder().decode(OpenCodexRemovalRecoverySession.self, from: data)
    }

    func save(_ session: OpenCodexRemovalRecoverySession) throws {
        let data = try JSONEncoder().encode(session)
        guard !data.isEmpty, data.count <= Self.maximumBytes else {
            throw OpenCodexRemovalContractError.invalidRequest
        }
        defaults.set(data, forKey: key)
    }

    func clear() {
        defaults.removeObject(forKey: key)
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
    var selection: OpenCodexRemovalSelection
    var candidate: OpenCodexInstallationCandidate?
    var phase: OpenCodexRemovalPhase
    var inventory: OpenCodexDataInventoryReceipt?
    var mode: OpenCodexRemovalMode
    var selectedDataItemIDs: [String]
    var retiredDataItemIDs: Set<String>
    var recoveryInventoryRevision: String?
    var expectedRoutingGeneration: UInt64?
    var receipt: OpenCodexRemovalReceipt?
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
        self.selection = selection
        self.candidate = candidate
        self.phase = .actions
        self.inventory = nil
        self.mode = .preserveData
        self.selectedDataItemIDs = []
        self.retiredDataItemIDs = []
        self.recoveryInventoryRevision = nil
        self.expectedRoutingGeneration = nil
        self.receipt = nil
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
        self.selection = recoverySession.selection
        self.candidate = nil
        self.inventory = nil
        self.mode = recoverySession.mode
        self.selectedDataItemIDs = recoverySession.orderedDataItemIDs
        self.retiredDataItemIDs = Set(recoverySession.retiredDataItemIDs)
        self.recoveryInventoryRevision = recoverySession.inventoryRevision
        self.receipt = nil
        self.failure = nil
        self.handoffProgress = nil
        self.removalProgress = nil
        self.candidateRevalidationRequired = true
        self.confirmsInterruptedDataRefresh = recoverySession.recoveryKind == .dataSelectionRefreshRequired
        self.confirmsRebootedProcessRecovery = recoverySession.recoveryKind == .rebootRequired || recoverySession.recoveryKind == .inFlight
        self.recoveryKind = recoverySession.recoveryKind
        self.expectedRoutingGeneration = recoverySession.expectedRoutingGeneration
        switch recoverySession.recoveryKind {
        case .dataSelectionRefreshRequired:
            self.phase = .dataRefreshRequired
        case .inFlight, .rebootRequired:
            self.phase = .rebootRequired
        case .routingRecoveryRequired:
            self.phase = .routingRecoveryRequired
        }
    }

    var automaticRemovalEligible: Bool {
        !candidateRevalidationRequired && candidate?.isAutomaticRemovalEligible == true
    }

    var displayVersion: String? { candidate?.version }
    var displayManager: OpenCodexPackageManager? { candidate?.manager }
    var displayTier: OpenCodexDiscoveryTier? { candidate?.tier }
    var requiresHomebrewGuard: Bool { candidate?.requiresHomebrewGuard == true }
    var teardownAdapterID: String? { candidate?.teardownAdapterID }
    var usesRelayPreservingTeardown: Bool {
        candidate?.teardownCapability == .relayPreserveV1 &&
            (candidate?.dataCapability == .preserveOnly ||
                candidate?.dataCapability == .selectiveTrashV1) &&
            candidate?.teardownCompatibilityReason == "compatible"
    }

    var supportsSelectiveTrash: Bool {
        usesRelayPreservingTeardown && candidate?.dataCapability == .selectiveTrashV1
    }

    var inventoryItems: [OpenCodexDataInventoryItem] {
        inventory?.items ?? []
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
            return supportsSelectiveTrash && inventory?.status == .verified &&
                !selectedDataItemIDs.isEmpty &&
                selectedItems.count == selectedDataItemIDs.count
        }
    }

    /// Only sessions restored from reboot/in-flight recovery may review the
    /// deliberately parked routing projection. New removal attempts and other
    /// recovery kinds must continue to satisfy the normal uninstall predicate.
    var isSavedRebootOrInFlightRecovery: Bool {
        candidate == nil &&
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
        candidate == nil &&
            recoveryKind == .routingRecoveryRequired &&
            expectedRoutingGeneration != nil &&
            !confirmsInterruptedDataRefresh &&
            !confirmsRebootedProcessRecovery
    }
}
