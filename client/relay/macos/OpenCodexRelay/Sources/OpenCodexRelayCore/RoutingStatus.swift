import Foundation

/// A durable routing destination. `none` means the relay-owned Codex routing
/// block is absent and native ChatGPT Codex owns the connection.
public enum RoutingBackend: String, Codable, CaseIterable, Sendable {
	case unknown
	case external
    case localOpenCodex = "local_opencodex"
    case none

    public var displayName: String {
        switch self {
		case .unknown:
			return "Unknown"
        case .external:
            return "External gateway"
        case .localOpenCodex:
            return "Local OpenCodex (10100)"
        case .none:
            return "Native ChatGPT Codex"
        }
    }

    public var isRelayBacked: Bool {
        switch self {
		case .external, .localOpenCodex:
			return true
		case .none, .unknown:
            return false
        }
    }
}

/// The compatibility labels retained in the relayctl JSON contract. They are
/// redundant with v2 backends on purpose: decoding both lets the MenuBar
/// reject a helper that mixes legacy and v2 state instead of guessing.
public enum RoutingMode: String, Codable, Sendable {
    case unknown
    case relay
    case native

    public static func forBackend(_ backend: RoutingBackend) -> RoutingMode {
        switch backend {
        case .external, .localOpenCodex:
            return .relay
        case .none:
            return .native
        case .unknown:
            return .unknown
        }
    }
}

/// CLI request targets remain user-facing: native maps to the durable `none`
/// backend, while the two relay profiles map directly to their backend names.
public enum RoutingRequestTarget: String, Codable, CaseIterable, Sendable {
    case native
    case external
    case localOpenCodex = "local_opencodex"

    public var displayName: String {
        switch self {
        case .native:
            return "Native ChatGPT Codex"
        case .external:
            return "External gateway"
        case .localOpenCodex:
            return "Local OpenCodex (10100)"
        }
    }

    public var backend: RoutingBackend {
        switch self {
        case .native:
            return .none
        case .external:
            return .external
        case .localOpenCodex:
            return .localOpenCodex
        }
    }
}

public enum RoutingPhase: String, Codable, Sendable {
    case relayActive = "relay_active"
    case nativePendingRestart = "native_pending_restart"
    case relayPendingRestart = "relay_pending_restart"
    /// An External <-> Local OpenCodex profile swap. Both sides still use the
    /// relay listener, but Desktop must restart to reload its selected catalog.
    case backendPendingRestart = "backend_pending_restart"
    case applying
    case nativeActive = "native_active"
    case recoveryRequired = "recovery_required"

    public var isTransitioning: Bool {
        switch self {
        case .nativePendingRestart, .relayPendingRestart, .backendPendingRestart, .applying:
            return true
        case .relayActive, .nativeActive, .recoveryRequired:
            return false
        }
    }

    public var requiresDesktopApply: Bool {
        switch self {
        case .nativePendingRestart, .relayPendingRestart, .backendPendingRestart:
            return true
        case .relayActive, .applying, .nativeActive, .recoveryRequired:
            return false
        }
    }
}

public enum RelayAdmission: String, Codable, Sendable {
    case allow
    case deny
}

public enum CatalogRefresh: String, Codable, Sendable {
    case run
    case pause
}

public enum DesktopEffectiveMode: String, Codable, Sendable {
    case unverifiable
}

/// Connection values are deliberately finite and redacted. They are a UI
/// projection, not a transport diagnostic: URLs, listener addresses, headers,
/// credential names, and relayctl stderr must never cross this contract.
public enum LocalRelayConnection: String, Codable, Sendable {
    case healthy
    case degraded
    case unreachable
    case unknown

    public var displayName: String {
        switch self {
        case .healthy:
            return "Healthy"
        case .degraded:
            return "Degraded"
        case .unreachable:
            return "Unavailable"
        case .unknown:
            return "Unknown"
        }
    }
}

/// The bounded result of the relay-owned 10100 identity and catalog preflight.
/// It intentionally does not expose a URL, a model list, or raw HTTP errors.
public enum LocalOpenCodexAvailability: String, Codable, Sendable {
    case ready
    case unavailable
    case foreign
    case invalid
    case unknown

    public var displayName: String {
        switch self {
        case .ready:
            return "Ready"
        case .unavailable:
            return "Unavailable"
        case .foreign:
            return "Foreign listener"
        case .invalid:
            return "Invalid catalog"
        case .unknown:
            return "Unknown"
        }
    }

    public var isReady: Bool { self == .ready }
}

public enum RoutingSync: String, Codable, Sendable {
    case acknowledged
    case pending
    case unreachable
    case invalid

    public var displayName: String {
        switch self {
        case .acknowledged:
            return "Acknowledged"
        case .pending:
            return "Pending"
        case .unreachable:
            return "Unavailable"
        case .invalid:
            return "Invalid"
        }
    }
}

public enum RemoteGatewayConnection: String, Codable, Sendable {
    case reachable
    case unreachable
    case unknown
    case notApplicable = "not_applicable"

    public var displayName: String {
        switch self {
        case .reachable:
            return "Reachable"
        case .unreachable:
            return "Unavailable"
        case .unknown:
            return "Unknown"
        case .notApplicable:
            return "Not applicable"
        }
    }
}

public enum CatalogConnection: String, Codable, Sendable {
    case running
    case paused
    case unknown

    public var displayName: String {
        switch self {
        case .running:
            return "Running"
        case .paused:
            return "Paused"
        case .unknown:
            return "Unknown"
        }
    }
}

public struct RelayConnectionStatus: Codable, Equatable, Sendable {
    public let localRelay: LocalRelayConnection
    public let localOpenCodex: LocalOpenCodexAvailability
    public let routingSync: RoutingSync
    public let remoteGateway: RemoteGatewayConnection
    public let catalog: CatalogConnection

    public init(
        localRelay: LocalRelayConnection,
        localOpenCodex: LocalOpenCodexAvailability = .unknown,
        routingSync: RoutingSync,
        remoteGateway: RemoteGatewayConnection,
        catalog: CatalogConnection
    ) {
        self.localRelay = localRelay
        self.localOpenCodex = localOpenCodex
        self.routingSync = routingSync
        self.remoteGateway = remoteGateway
        self.catalog = catalog
    }

	enum CodingKeys: String, CodingKey {
        case localRelay = "local_relay"
        case localOpenCodex = "local_opencodex"
        case routingSync = "routing_sync"
        case remoteGateway = "remote_gateway"
        case catalog
    }
}

/// The deliberately small, non-secret JSON contract emitted by `relayctl mode`.
/// Do not add upstream URLs, credential source names, listener addresses, raw
/// errors, or Codex configuration contents here.
public enum RoutingRecoveryAction: String, Codable, CaseIterable, Sendable {
    case complete
    case rollback
}

public enum RecoveryTargetConfidence: String, Codable, Sendable {
    case unavailable
    case observed
    case journal
}

/// Advisory recovery evidence emitted by relayctl schema v3. The helper
/// revalidates every predicate under the routing writer lock before mutating
/// state; the MenuBar uses this projection only to avoid offering impossible
/// actions.
public struct RecoveryCapabilities: Codable, Equatable, Sendable {
    public let canComplete: Bool
    public let canRollback: Bool
    public let completeReason: String
    public let rollbackReason: String
    public let target: RoutingBackend
    public let targetConfidence: RecoveryTargetConfidence
    public let authoritativeJournal: Bool

    public init(
        canComplete: Bool,
        canRollback: Bool,
        completeReason: String,
        rollbackReason: String,
        target: RoutingBackend,
        targetConfidence: RecoveryTargetConfidence,
        authoritativeJournal: Bool
    ) {
        self.canComplete = canComplete
        self.canRollback = canRollback
        self.completeReason = completeReason
        self.rollbackReason = rollbackReason
        self.target = target
        self.targetConfidence = targetConfidence
        self.authoritativeJournal = authoritativeJournal
    }

    enum CodingKeys: String, CodingKey {
        case canComplete = "can_complete"
        case canRollback = "can_rollback"
        case completeReason = "complete_reason"
        case rollbackReason = "rollback_reason"
        case target
        case targetConfidence = "target_confidence"
        case authoritativeJournal = "authoritative_journal"
    }

    public func allows(_ action: RoutingRecoveryAction) -> Bool {
        switch action {
        case .complete: canComplete
        case .rollback: canRollback
        }
    }
}

public struct RoutingStatus: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let desiredMode: RoutingMode
    public let appliedMode: RoutingMode
    public let desiredBackend: RoutingBackend
    public let appliedBackend: RoutingBackend
    public let phase: RoutingPhase
    public let relayAdmission: RelayAdmission
    public let catalogRefresh: CatalogRefresh
    public let relayRunning: Bool
    /// Nil means relayctl could not safely observe the local process. It is not
    /// equivalent to zero and must render as unavailable in the status surface.
    public let activeRequests: Int?
    public let desktopRestartRequired: Bool
    public let desktopEffectiveMode: DesktopEffectiveMode
    public let generation: UInt64
    public let connection: RelayConnectionStatus
    /// Nil is accepted only from legacy schema v2 helpers. It yields no
    /// actionable recovery buttons; schema v3 must always provide evidence.
    public let recoveryCapabilities: RecoveryCapabilities?

    public init(
        schemaVersion: Int,
        desiredMode: RoutingMode? = nil,
        appliedMode: RoutingMode? = nil,
        desiredBackend: RoutingBackend,
        appliedBackend: RoutingBackend,
        phase: RoutingPhase,
        relayAdmission: RelayAdmission,
        catalogRefresh: CatalogRefresh,
        relayRunning: Bool,
        activeRequests: Int?,
        desktopRestartRequired: Bool,
        desktopEffectiveMode: DesktopEffectiveMode,
        generation: UInt64,
        connection: RelayConnectionStatus,
        recoveryCapabilities: RecoveryCapabilities? = nil
    ) {
        self.schemaVersion = schemaVersion
        self.desiredMode = desiredMode ?? RoutingMode.forBackend(desiredBackend)
        self.appliedMode = appliedMode ?? RoutingMode.forBackend(appliedBackend)
        self.desiredBackend = desiredBackend
        self.appliedBackend = appliedBackend
        self.phase = phase
        self.relayAdmission = relayAdmission
        self.catalogRefresh = catalogRefresh
        self.relayRunning = relayRunning
        self.activeRequests = activeRequests
        self.desktopRestartRequired = desktopRestartRequired
        self.desktopEffectiveMode = desktopEffectiveMode
        self.generation = generation
		self.connection = connection
        self.recoveryCapabilities = recoveryCapabilities
	}

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case desiredMode = "desired_mode"
        case appliedMode = "applied_mode"
        case desiredBackend = "desired_backend"
        case appliedBackend = "applied_backend"
        case phase
        case relayAdmission = "relay_admission"
        case catalogRefresh = "catalog_refresh"
        case relayRunning = "relay_running"
        case activeRequests = "active_requests"
        case desktopRestartRequired = "desktop_restart_required"
        case desktopEffectiveMode = "desktop_effective_mode"
        case generation
        case connection
        case recoveryCapabilities = "recovery_capabilities"
    }

    /// Enforce the complete v2/v3 state machine before UI code consumes it.
    /// Schema v2 remains readable but cannot authorize recovery actions because
    /// it carries no evidence. An unexpected status is a fail-closed local
    /// control error, never a hint to guess a backend or perform a fallback.
    public func validated() throws -> RoutingStatus {
        guard (schemaVersion == 2 || schemaVersion == 3),
              activeRequests.map({ $0 >= 0 }) ?? true,
              generation > 0 || allowsOpaqueZeroGeneration,
              desiredMode == RoutingMode.forBackend(desiredBackend),
              appliedMode == RoutingMode.forBackend(appliedBackend),
              desktopEffectiveMode == .unverifiable else {
            throw RelayctlError.invalidStatus
        }
        if schemaVersion == 2 {
            guard recoveryCapabilities == nil else {
                throw RelayctlError.invalidStatus
            }
        } else {
            guard let capabilities = recoveryCapabilities,
                  Self.isSafeRecoveryReason(capabilities.completeReason),
                  Self.isSafeRecoveryReason(capabilities.rollbackReason) else {
                throw RelayctlError.invalidStatus
            }
            if phase == .recoveryRequired {
                if capabilities.canComplete {
                    guard capabilities.target != .unknown,
                          capabilities.targetConfidence != .unavailable else {
                        throw RelayctlError.invalidStatus
                    }
                }
                if capabilities.canRollback {
                    guard capabilities.canComplete,
                          capabilities.authoritativeJournal,
                          capabilities.target != .unknown,
                          capabilities.targetConfidence == .journal else {
                        throw RelayctlError.invalidStatus
                    }
                }
                if capabilities.targetConfidence == .observed,
                   capabilities.authoritativeJournal {
                    throw RelayctlError.invalidStatus
                }
            } else {
                guard !capabilities.canComplete,
                      !capabilities.canRollback,
                      capabilities.target == .unknown,
                      capabilities.targetConfidence == .unavailable,
                      !capabilities.authoritativeJournal else {
                    throw RelayctlError.invalidStatus
                }
            }
        }
        if connection.localRelay == .unreachable, activeRequests != nil {
            throw RelayctlError.invalidStatus
        }
        if connection.localRelay == .healthy, !relayRunning {
            throw RelayctlError.invalidStatus
        }
        if connection.localRelay == .unreachable, relayRunning {
            throw RelayctlError.invalidStatus
        }
        if connection.routingSync == .acknowledged, !relayRunning {
            throw RelayctlError.invalidStatus
        }

        switch phase {
        case .relayActive:
            guard desiredBackend == appliedBackend,
                  appliedBackend.isRelayBacked,
                  !desktopRestartRequired,
                  relayStateMatchesAppliedBackend else {
                throw RelayctlError.invalidStatus
            }
        case .nativeActive:
            guard desiredBackend == .none,
                  appliedBackend == .none,
                  relayAdmission == .deny,
                  catalogRefresh == .pause,
                  !desktopRestartRequired else {
                throw RelayctlError.invalidStatus
            }
        case .nativePendingRestart:
            guard desiredBackend == .none,
                  appliedBackend.isRelayBacked,
                  desktopRestartRequired,
                  relayStateMatchesAppliedBackend else {
                throw RelayctlError.invalidStatus
            }
        case .relayPendingRestart:
            guard desiredBackend.isRelayBacked,
                  appliedBackend == .none,
                  relayAdmission == .deny,
                  catalogRefresh == .pause,
                  desktopRestartRequired else {
                throw RelayctlError.invalidStatus
            }
        case .backendPendingRestart:
            guard desiredBackend.isRelayBacked,
                  appliedBackend.isRelayBacked,
                  desiredBackend != appliedBackend,
                  desktopRestartRequired,
                  relayStateMatchesAppliedBackend else {
                throw RelayctlError.invalidStatus
            }
        case .applying:
            guard desiredBackend != appliedBackend,
                  relayAdmission == .deny,
                  catalogRefresh == .pause,
                  desktopRestartRequired else {
                throw RelayctlError.invalidStatus
            }
        case .recoveryRequired:
            guard relayAdmission == .deny,
                  catalogRefresh == .pause,
                  desktopRestartRequired else {
                throw RelayctlError.invalidStatus
            }
        }
        return self
    }

    private static func isSafeRecoveryReason(_ value: String) -> Bool {
        let bytes = Array(value.utf8)
        guard !bytes.isEmpty, bytes.count <= 64 else { return false }
        return bytes.allSatisfy { byte in
            (byte >= 97 && byte <= 122) || (byte >= 48 && byte <= 57) || byte == 95
        }
    }

    /// A gated status may have no trusted durable generation when the
    /// underlying routing state is malformed, legacy, or otherwise
    /// unvalidated. Go uses zero as an explicit non-witness sentinel in that
    /// narrow projection. Accepting it here lets the UI render a bounded
    /// recovery/unavailable state, but the projection remains permanently
    /// non-actionable: no recovery capability or saved-removal predicate may
    /// use it as a generation witness.
    private var allowsOpaqueZeroGeneration: Bool {
        guard schemaVersion == 3,
              generation == 0,
              phase == .recoveryRequired,
              desiredBackend == .unknown,
              appliedBackend == .unknown,
              relayAdmission == .deny,
              catalogRefresh == .pause,
              (connection.routingSync == .invalid ||
                  (connection.localRelay == .unreachable &&
                      connection.routingSync == .unreachable)),
              let capabilities = recoveryCapabilities else {
            return false
        }
        return !capabilities.canComplete &&
            !capabilities.canRollback &&
            capabilities.completeReason == "observed_state_unavailable" &&
            capabilities.rollbackReason == "observed_state_unavailable" &&
            capabilities.target == .unknown &&
            capabilities.targetConfidence == .unavailable &&
            !capabilities.authoritativeJournal
    }

    public func canRecover(_ action: RoutingRecoveryAction) -> Bool {
        guard schemaVersion == 3,
              phase == .recoveryRequired,
              let recoveryCapabilities else {
            return false
        }
        return recoveryCapabilities.allows(action)
    }

    public func recoveryReason(for action: RoutingRecoveryAction) -> String? {
        guard let recoveryCapabilities else { return nil }
        switch action {
        case .complete: return recoveryCapabilities.completeReason
        case .rollback: return recoveryCapabilities.rollbackReason
        }
    }

    private var relayStateMatchesAppliedBackend: Bool {
        switch appliedBackend {
        case .external:
            return relayAdmission == .allow && catalogRefresh == .run
        case .localOpenCodex:
            if connection.localOpenCodex.isReady {
                return relayAdmission == .allow && catalogRefresh == .run
            }
            // A lost/foreign/invalid local backend must remain selected in
            // durable state but be parked. It is never silently changed to
            // External and it must not admit or replay a request.
            return relayAdmission == .deny && catalogRefresh == .pause
        case .none:
			return false
		case .unknown:
			return false
        }
    }

    public var presentation: RoutingPresentation {
        // A local relay process failure has priority over every durable route.
        if connection.localRelay == .unreachable || connection.routingSync == .unreachable {
            return .relayUnavailable
        }
        // The local profile is explicitly fail-closed. This comes before the
        // generic relay-active label so an apparently active durable state is
        // never mistaken for a live 10100 backend.
        if appliedBackend == .localOpenCodex, !connection.localOpenCodex.isReady {
            return .localOpenCodexUnavailable
        }
        if connection.routingSync == .invalid || phase == .recoveryRequired {
            return .recoveryRequired
        }
        if connection.routingSync == .pending {
            return .routingSyncPending
        }
        if connection.localRelay == .degraded {
            return .relayDegraded
        }
        switch phase {
        case .relayActive:
            return appliedBackend == .localOpenCodex ? .localOpenCodexReady : .externalReady
        case .nativePendingRestart:
            return .nativePending
        case .relayPendingRestart, .backendPendingRestart:
            return desiredBackend == .localOpenCodex ? .localOpenCodexPending : .externalPending
        case .applying:
            return .switching
        case .nativeActive:
            return .nativeParked
        case .recoveryRequired:
            return .recoveryRequired
        }
    }

    public var menuTitle: String { presentation.title }

    public var isDraining: Bool {
        phase == .applying && (activeRequests ?? 0) > 0
    }

    public var needsDesktopApply: Bool {
        phase.requiresDesktopApply && desktopRestartRequired
    }

    /// Local may be requested only after the bounded 10100 identity and model
    /// preflight reports ready. Recovery states remain fail-closed.
    public var canRequestLocalOpenCodex: Bool {
        connection.localOpenCodex.isReady &&
            connection.localRelay == .healthy &&
            connection.routingSync == .acknowledged &&
            relayRunning &&
            phase != .recoveryRequired
    }

    /// Native mode intentionally suppresses automatic 10100 probes so it has
    /// zero local/remote diagnostic egress while parked.  The user may still
    /// explicitly choose Local from that stable Native state: relayctl then
    /// performs the same bounded identity/catalog preflight before it records
    /// any transition.  A known unavailable/foreign/invalid result remains
    /// disabled; only the deliberately unprobed `unknown` Native state may
    /// request that explicit check.
    public var canAttemptLocalOpenCodex: Bool {
        if canRequestLocalOpenCodex {
            return true
        }
        return phase == .nativeActive &&
            connection.localOpenCodex == .unknown &&
            connection.localRelay == .healthy &&
            connection.routingSync == .acknowledged &&
            relayRunning
    }

    /// An irreversible OpenCodex uninstall is permitted only after the UI has
    /// observed a stable, acknowledged route that is definitely not Local.
    /// Unknown/recovery/unreachable status is intentionally not enough.
    public var canUninstallOpenCodex: Bool {
        guard connection.localRelay == .healthy,
              connection.routingSync == .acknowledged,
              relayRunning,
              phase == .relayActive || phase == .nativeActive else {
            return false
        }
        return desiredBackend != .localOpenCodex &&
            appliedBackend != .localOpenCodex &&
            desiredBackend != .unknown &&
            appliedBackend != .unknown
    }

    /// A saved reboot/in-flight removal continuation cannot reuse the ordinary
    /// uninstall predicate: its own durable journal intentionally parks the
    /// routing surface as recovery/unknown/deny. It may use only a fresh,
    /// schema-validated durable generation from that exact fail-closed
    /// projection. The helper independently verifies the saved recovery
    /// witness before it performs any mutation.
    public var canReviewSavedOpenCodexRemovalRecovery: Bool {
        schemaVersion == 3 &&
            generation > 0 &&
            phase == .recoveryRequired &&
            desiredBackend == .unknown &&
            appliedBackend == .unknown &&
            relayAdmission == .deny &&
            catalogRefresh == .pause &&
            (connection.routingSync == .invalid ||
                (connection.localRelay == .unreachable &&
                    connection.routingSync == .unreachable))
    }

    /// A newer gated epoch may be checkpointed for a later explicit routing
    /// recovery action only when it remains a positive saved-removal
    /// projection with a Complete authority targeting a non-Local route.
    /// Health may still be invalid or unreachable at this point; execution
    /// remains subject to the narrower predicate below.
    public var canCheckpointSavedOpenCodexRoutingRecovery: Bool {
        guard canReviewSavedOpenCodexRemovalRecovery,
              let recoveryCapabilities,
              recoveryCapabilities.canComplete,
              (recoveryCapabilities.target == .external ||
                  recoveryCapabilities.target == .none) else {
            return false
        }
        return true
    }

    /// Routing recovery changes the durable routing state itself, so it has a
    /// narrower execution predicate than reboot/in-flight removal recovery.
    /// An unreachable relay may still report a persisted generation for the
    /// latter, but cannot safely acknowledge the recovery transition here.
    public var canReviewSavedOpenCodexRoutingRecovery: Bool {
        guard canCheckpointSavedOpenCodexRoutingRecovery,
              connection.localRelay == .healthy,
              relayRunning,
              connection.routingSync == .invalid else {
            return false
        }
        return true
    }
}

public enum RoutingPresentation: Equatable, Sendable {
    case externalReady
    case localOpenCodexReady
    case nativePending
    case externalPending
    case localOpenCodexPending
    case routingSyncPending
    case relayDegraded
    case switching
    case nativeParked
    case recoveryRequired
    case localOpenCodexUnavailable
    case relayUnavailable

    public var title: String {
        switch self {
        case .externalReady:
            return "External gateway ready"
        case .localOpenCodexReady:
            return "Local OpenCodex ready"
        case .nativePending:
            return "Native pending — restart Codex"
        case .externalPending:
            return "External gateway pending — restart Codex"
        case .localOpenCodexPending:
            return "Local OpenCodex pending — restart Codex"
        case .routingSyncPending:
            return "Routing sync pending"
        case .relayDegraded:
            return "Relay degraded"
        case .switching:
            return "Switching / draining"
        case .nativeParked:
            return "Native Codex active"
        case .recoveryRequired:
            return "Recovery required"
        case .localOpenCodexUnavailable:
            return "Local unavailable — external only"
        case .relayUnavailable:
            return "Relay unavailable"
        }
    }

    public var symbolName: String {
        switch self {
        case .externalReady:
            return "arrow.trianglehead.2.clockwise.rotate.90"
        case .localOpenCodexReady:
            return "desktopcomputer.and.macbook"
        case .nativePending:
            return "arrow.counterclockwise.circle"
        case .externalPending:
            return "arrow.clockwise.circle"
        case .localOpenCodexPending:
            return "arrow.clockwise.circle"
        case .routingSyncPending, .switching:
            return "arrow.triangle.2.circlepath"
        case .relayDegraded:
            return "exclamationmark.circle"
        case .nativeParked:
            return "pause.circle"
        case .recoveryRequired:
            return "exclamationmark.triangle"
        case .localOpenCodexUnavailable:
            return "network.slash"
        case .relayUnavailable:
            return "network.slash"
        }
    }

    public var accessibilityLabel: String {
        switch self {
        case .externalReady:
            return "PW OpenCodex Relay: external gateway ready"
        case .localOpenCodexReady:
            return "PW OpenCodex Relay: local OpenCodex on port 10100 ready"
        case .nativePending:
            return "PW OpenCodex Relay: native mode pending; restart Codex is required"
        case .externalPending:
            return "PW OpenCodex Relay: external gateway pending; restart Codex is required"
        case .localOpenCodexPending:
            return "PW OpenCodex Relay: local OpenCodex pending; restart Codex is required"
        case .routingSyncPending:
            return "PW OpenCodex Relay: routing synchronization is pending; no backend state is confirmed"
        case .relayDegraded:
            return "PW OpenCodex Relay: local relay is degraded"
        case .switching:
            return "PW OpenCodex Relay: switching routing and draining requests"
        case .nativeParked:
            return "PW OpenCodex Relay: native Codex is active; relay is parked"
        case .recoveryRequired:
            return "PW OpenCodex Relay: recovery is required before routing can resume"
        case .localOpenCodexUnavailable:
            return "PW OpenCodex Relay: local OpenCodex is unavailable; only external gateway or native Codex may be selected"
        case .relayUnavailable:
            return "PW OpenCodex Relay: local relay is unavailable"
        }
    }

    public var compactLabel: String {
        switch self {
        case .externalReady:
            return "External"
        case .localOpenCodexReady:
            return "Local"
        case .nativePending:
            return "Native pending"
        case .externalPending:
            return "External pending"
        case .localOpenCodexPending:
            return "Local pending"
        case .routingSyncPending:
            return "Sync pending"
        case .relayDegraded:
            return "Degraded"
        case .switching:
            return "Switching"
        case .nativeParked:
            return "Native"
        case .recoveryRequired:
            return "Recovery"
        case .localOpenCodexUnavailable:
            return "Local offline"
        case .relayUnavailable:
            return "Offline"
        }
    }
}

public enum RoutingStatusPolling {
    public static func intervalSeconds(status: RoutingStatus?, isPopoverVisible: Bool) -> TimeInterval {
        if isPopoverVisible || status?.phase.isTransitioning == true {
            return 2
        }
        guard let status else {
            return 30
        }
        if status.appliedBackend == .localOpenCodex,
           !status.connection.localOpenCodex.isReady {
            return 2
        }
        if status.connection.localRelay == .unreachable || status.connection.routingSync == .unreachable {
            return 30
        }
        return 15
    }
}

public enum RelayctlCommand: Equatable, Sendable {
    case status
    case request(RoutingRequestTarget)
    case requestExternalMigratingKnownLegacy(
        expectedConfigDigest: String,
        expectedRoutingGeneration: UInt64
    )
    case apply
    case cancel
    case recoverComplete
    case recoverOpenCodexRemoval(
        selection: OpenCodexRemovalSelection,
        expectedRoutingGeneration: UInt64
    )
    case recoverRollback
    case repairNative(expectedGeneration: UInt64)
    case handoff(OpenCodexExecutable, OpenCodexHandoffAction)

    /// Lower bounds prevent a short MenuBar helper deadline from interrupting
    /// the relay's documented listener observation, drain, or handoff window.
    public var minimumHelperTimeout: TimeInterval {
        switch self {
        case .status, .request, .requestExternalMigratingKnownLegacy, .cancel, .repairNative:
            return 20
        case .apply, .recoverComplete, .recoverOpenCodexRemoval, .recoverRollback:
            // A Local apply may spend 30 seconds draining, up to 12 seconds
            // synchronously materializing the verified Local catalog, up to
            // 30 seconds in the owner-only runtime-control swap, then a
            // second bounded final watcher/lifecycle acknowledgement. Keep
            // the helper alive through that atomic Desktop restart boundary.
            return 110
        case .handoff:
            return 85
        }
    }

    public var arguments: [String] {
        switch self {
        case .status:
            return ["mode", "status", "--json"]
        case let .request(target):
            return ["mode", "request", target.rawValue, "--json"]
        case let .requestExternalMigratingKnownLegacy(
            expectedConfigDigest,
            expectedRoutingGeneration
        ):
            return [
                "mode", "request", RoutingRequestTarget.external.rawValue,
                "--known-legacy-backup-and-migrate",
                "--expected-config-digest", expectedConfigDigest,
                "--expected-routing-generation", String(expectedRoutingGeneration),
                "--json",
            ]
        case .apply:
            return ["mode", "apply", "--confirm-desktop-exited", "--json"]
        case .cancel:
            return ["mode", "cancel", "--json"]
        case .recoverComplete:
            return ["mode", "recover", "--complete", "--confirm-desktop-exited", "--json"]
        case let .recoverOpenCodexRemoval(selection, expectedRoutingGeneration):
            return [
                "mode", "recover",
                "--installation-id", selection.installationID,
                "--installation-fingerprint", selection.installationFingerprint,
                "--expected-routing-generation", String(expectedRoutingGeneration),
                "--complete",
                "--confirm-desktop-exited",
                "--json",
            ]
        case .recoverRollback:
            return ["mode", "recover", "--rollback", "--confirm-desktop-exited", "--json"]
        case let .repairNative(expectedGeneration):
            return [
                "mode", "repair-native",
                "--expected-routing-generation", String(expectedGeneration),
                "--confirm-local-development-native-repair",
                "--json",
            ]
        case let .handoff(executable, action):
            return [
                "mode", "handoff",
                "--ocx-executable", executable.path,
				"--ocx-sha256", executable.sha256,
                "--action", action.rawValue,
                "--confirm-opencodex-handoff",
                "--confirm-desktop-exited",
                "--json",
            ]
        }
    }
}

/// Relayctl errors intentionally retain no stderr, stdout, path, or other raw
/// process diagnostics. The MenuBar application may render only this bounded
/// code/message pair.
public enum RelayctlReportedErrorCode: String, Codable, Equatable, Sendable {
    case operationFailed = "operation_failed"
    case invalidRequest = "invalid_request"
    case routingRecoveryRequired = "routing_recovery_required"
    case routingGenerationChanged = "routing_generation_changed"
    case nativeRoutingUnverified = "native_routing_unverified"
    case nativeRepairUnavailable = "native_repair_unavailable"
    case nativeRepairOwnerChanged = "native_repair_owner_changed"
    case nativeOwnerRepairFailed = "native_owner_repair_failed"
    case nativeOwnerBusy = "native_owner_busy"
    case nativeOwnerConfigurationInvalid = "native_owner_configuration_invalid"
    case nativeOwnerRestoreFailed = "native_owner_restore_failed"
    case nativeOwnerResultInvalid = "native_owner_result_invalid"
    case nativeStateRepairPending = "native_state_repair_pending"
    case desktopExitConfirmationRequired = "desktop_exit_confirmation_required"
    case operationTimedOut = "operation_timed_out"
    case operationCancelled = "operation_cancelled"
    case permissionRequired = "permission_required"
    case openCodexCandidateChanged = "opencodex_candidate_changed"
    case openCodexManualRemovalRequired = "opencodex_manual_removal_required"
    case openCodexCleanupJournalUnsafe = "opencodex_cleanup_journal_unsafe"
    case nativeRemovalBoundaryUnsafe = "native_removal_boundary_unsafe"
    case nativeRemovalBoundaryChanged = "native_removal_boundary_changed"
    case nativeRecoveryRequired = "native_recovery_required"
    case customCodexHomeUnsupported = "custom_codex_home_unsupported"
    case teardownUnsupported = "teardown_unsupported"
    case teardownCandidateChanged = "teardown_candidate_changed"
    case teardownPreflightFailed = "teardown_preflight_failed"
    case teardownRefused = "teardown_refused"
    case teardownResultInvalid = "teardown_result_invalid"
    case teardownVerificationFailed = "teardown_verification_failed"
    case invalidAddress = "invalid_address"
    case credentialUnavailable = "credential_unavailable"
    case authenticationFailed = "authentication_failed"
    case gatewayUnreachable = "gateway_unreachable"
    case catalogInvalid = "catalog_invalid"
    case configChanged = "config_changed"
    case routingChanged = "routing_changed"
    case transitionPending = "transition_pending"
    case runtimeSwapFailed = "runtime_swap_failed"
    case gatewayUnsupported = "gateway_unsupported"
    case integrationAppLocationInvalid = "integration_app_location_invalid"
    case integrationArtifactInvalid = "integration_artifact_invalid"
    case integrationStateChanged = "integration_state_changed"
    case integrationStateUnsafe = "integration_state_unsafe"
    case integrationRecoveryRequired = "integration_recovery_required"
    case integrationActivationFailed = "integration_activation_failed"
    case releaseStageInvalidRequest = "release_stage_invalid_request"
    case releaseStageBusy = "release_stage_busy"
    case releaseStageVerificationFailed = "release_stage_verification_failed"
    case releaseStageRecoveryRequired = "release_stage_recovery_required"
    case lifecycleWriterBusy = "lifecycle_writer_busy"
    case lifecycleStateUnsafe = "lifecycle_state_unsafe"
    case runtimeUpgradeIncompatible = "runtime_upgrade_incompatible"
    case relayRestartConfirmationRequired = "relay_restart_confirmation_required"

    fileprivate var expectedRetryable: Bool {
        switch self {
        case .operationFailed, .routingRecoveryRequired, .routingGenerationChanged,
             .nativeRepairOwnerChanged, .nativeOwnerRepairFailed, .nativeOwnerBusy, .nativeOwnerRestoreFailed, .nativeStateRepairPending,
             .desktopExitConfirmationRequired, .operationTimedOut, .openCodexCandidateChanged,
             .nativeRemovalBoundaryChanged, .nativeRecoveryRequired,
             .teardownCandidateChanged, .teardownPreflightFailed,
             .authenticationFailed, .gatewayUnreachable, .configChanged,
             .routingChanged, .transitionPending, .runtimeSwapFailed,
             .integrationStateChanged, .integrationRecoveryRequired,
             .integrationActivationFailed, .releaseStageBusy, .lifecycleWriterBusy:
            true
        case .invalidRequest, .operationCancelled, .permissionRequired,
             .nativeRoutingUnverified, .nativeRepairUnavailable, .nativeOwnerConfigurationInvalid, .nativeOwnerResultInvalid,
             .openCodexManualRemovalRequired, .openCodexCleanupJournalUnsafe,
             .nativeRemovalBoundaryUnsafe, .customCodexHomeUnsupported,
             .teardownUnsupported, .teardownRefused, .teardownResultInvalid,
             .teardownVerificationFailed, .invalidAddress, .credentialUnavailable,
             .catalogInvalid, .gatewayUnsupported, .integrationAppLocationInvalid,
             .integrationArtifactInvalid, .integrationStateUnsafe,
             .releaseStageInvalidRequest, .releaseStageVerificationFailed,
             .releaseStageRecoveryRequired, .lifecycleStateUnsafe,
             .runtimeUpgradeIncompatible, .relayRestartConfirmationRequired:
            false
        }
    }

    fileprivate var expectedRecommendedAction: String {
        switch self {
        case .operationFailed, .operationTimedOut, .routingGenerationChanged,
             .nativeRepairOwnerChanged, .nativeOwnerRepairFailed, .nativeOwnerRestoreFailed: "refresh_status"
        case .nativeOwnerBusy: "retry_owner_repair"
        case .invalidRequest: "review_request"
        case .routingRecoveryRequired, .nativeStateRepairPending: "open_recovery"
        case .nativeRecoveryRequired: "open_recovery"
        case .nativeRemovalBoundaryChanged: "refresh_native_removal"
        case .desktopExitConfirmationRequired: "retry_after_desktop_exit"
        case .operationCancelled: "none"
        case .permissionRequired, .nativeRoutingUnverified, .nativeRepairUnavailable,
             .nativeOwnerConfigurationInvalid, .nativeOwnerResultInvalid, .openCodexManualRemovalRequired, .openCodexCleanupJournalUnsafe,
             .nativeRemovalBoundaryUnsafe:
            "manual_remediation"
        case .customCodexHomeUnsupported: "review_request"
        case .openCodexCandidateChanged: "rediscover_opencodex"
        case .teardownCandidateChanged: "rediscover_opencodex"
        case .teardownPreflightFailed: "refresh_status"
        case .teardownVerificationFailed: "open_recovery"
        case .invalidAddress: "review_request"
        case .credentialUnavailable, .authenticationFailed: "update_credentials"
        case .gatewayUnsupported: "manual_remediation"
        case .gatewayUnreachable: "retry"
        case .catalogInvalid: "review_gateway"
        case .configChanged: "refresh_gateway"
        case .routingChanged, .transitionPending, .runtimeSwapFailed: "refresh_status"
        case .teardownUnsupported, .teardownRefused, .teardownResultInvalid: "manual_remediation"
        case .integrationAppLocationInvalid: "move_application"
        case .integrationArtifactInvalid: "verify_download"
        case .integrationStateChanged: "refresh_status"
        case .integrationStateUnsafe: "manual_remediation"
        case .integrationRecoveryRequired: "open_recovery"
        case .integrationActivationFailed: "retry"
        case .releaseStageInvalidRequest: "check_for_updates"
        case .releaseStageBusy, .lifecycleWriterBusy: "retry"
        case .releaseStageVerificationFailed: "open_release"
        case .releaseStageRecoveryRequired, .lifecycleStateUnsafe: "manual_remediation"
        case .runtimeUpgradeIncompatible: "manual_remediation"
        case .relayRestartConfirmationRequired: "confirm_restart"
        }
    }
}

private struct RelayctlOperationErrorCodingKey: CodingKey {
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

struct RelayctlOperationErrorEnvelope: Decodable {
    struct Payload: Decodable {
        let code: String
        let messageKey: String
        let retryable: Bool
        let recommendedAction: String

        enum CodingKeys: String, CodingKey {
            case code
            case messageKey = "message_key"
            case retryable
            case recommendedAction = "recommended_action"
        }

        init(from decoder: Decoder) throws {
            try RelayctlOperationErrorEnvelope.requireExactKeys(
                decoder,
                ["code", "message_key", "retryable", "recommended_action"]
            )
            let values = try decoder.container(keyedBy: CodingKeys.self)
            code = try values.decode(String.self, forKey: .code)
            messageKey = try values.decode(String.self, forKey: .messageKey)
            retryable = try values.decode(Bool.self, forKey: .retryable)
            recommendedAction = try values.decode(String.self, forKey: .recommendedAction)
        }
    }

    let schemaVersion: Int
    let ok: Bool
    let error: Payload

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case ok
        case error
    }

    init(from decoder: Decoder) throws {
        try Self.requireExactKeys(decoder, ["schema_version", "ok", "error"])
        let values = try decoder.container(keyedBy: CodingKeys.self)
        schemaVersion = try values.decode(Int.self, forKey: .schemaVersion)
        ok = try values.decode(Bool.self, forKey: .ok)
        error = try values.decode(Payload.self, forKey: .error)
    }

    private static func requireExactKeys(_ decoder: Decoder, _ expected: Set<String>) throws {
        let values = try decoder.container(keyedBy: RelayctlOperationErrorCodingKey.self)
        guard Set(values.allKeys.map(\.stringValue)) == expected else {
            throw DecodingError.dataCorrupted(.init(
                codingPath: decoder.codingPath,
                debugDescription: "invalid operation error envelope"
            ))
        }
    }

    func reportedCode() -> RelayctlReportedErrorCode? {
        guard schemaVersion == 1,
              !ok,
              error.messageKey == error.code,
              error.code.utf8.count <= 64,
              error.recommendedAction.utf8.count <= 64,
              let code = RelayctlReportedErrorCode(rawValue: error.code),
              error.retryable == code.expectedRetryable,
              error.recommendedAction == code.expectedRecommendedAction else {
            return nil
        }
        return code
    }
}

public enum RelayctlError: LocalizedError, Equatable, Sendable {
    case helperUnavailable
    case invocationFailed(exitCode: Int32)
    case reported(RelayctlReportedErrorCode)
    case invalidJSON
    case invalidStatus
    case launchFailed
    case timedOut
    case cancelled
    case outputTooLarge

    public var safeCode: String {
        switch self {
        case .helperUnavailable:
            return "relayctl_unavailable"
        case .invocationFailed:
            return "relayctl_failed"
        case let .reported(code):
            return code.rawValue
        case .invalidJSON:
            return "relayctl_invalid_json"
        case .invalidStatus:
            return "relayctl_invalid_status"
        case .launchFailed:
            return "relayctl_launch_failed"
        case .timedOut:
            return "relayctl_timeout"
        case .cancelled:
            return "relayctl_cancelled"
        case .outputTooLarge:
            return "relayctl_output_too_large"
        }
    }

    public var safeMessage: String {
        switch self {
        case .helperUnavailable:
            return "Relay control helper is unavailable."
        case .invocationFailed:
            return "Relay control did not complete. Try refreshing the status."
        case let .reported(code):
            switch code {
            case .routingRecoveryRequired:
                return "Routing recovery is required before this operation can continue."
            case .routingGenerationChanged:
                return "Routing state changed. Refresh it before attempting native repair again."
            case .nativeRoutingUnverified:
                return "Native Codex routing could not be verified without changing user configuration."
            case .nativeRepairUnavailable:
                return "Local-development native repair is unavailable for the current state."
            case .nativeRepairOwnerChanged:
                return "Codex routing ownership changed. Refresh the repair inspection before retrying."
            case .nativeOwnerRepairFailed:
                return "The verified routing owner did not complete native configuration repair."
            case .nativeOwnerBusy:
                return "OpenCodex remained busy without changing routing settings. Retry the owner repair."
            case .nativeOwnerConfigurationInvalid:
                return "OpenCodex configuration is invalid, so automatic repair is unavailable."
            case .nativeOwnerRestoreFailed:
                return "OpenCodex reported that its Codex integration restore failed."
            case .nativeOwnerResultInvalid:
                return "OpenCodex returned an invalid bounded repair result. No native state was confirmed."
            case .nativeStateRepairPending:
                return "Codex routing is native, but the routing state still requires repair."
            case .desktopExitConfirmationRequired:
                return "The selected Codex Desktop app must exit before this operation can continue."
            case .operationTimedOut:
                return "Relay control timed out before it could complete safely."
            case .operationCancelled:
                return "Relay control was cancelled."
            case .permissionRequired, .openCodexManualRemovalRequired:
                return "This installation requires a separate manual permission step."
            case .openCodexCandidateChanged:
                return "The selected OpenCodex installation changed. Discover it again before continuing."
            case .openCodexCleanupJournalUnsafe:
                return "The OpenCodex cleanup journal cannot be verified safely. Manual recovery is required."
            case .nativeRemovalBoundaryUnsafe:
                return "The standalone Native Codex removal boundary could not be verified safely."
            case .nativeRemovalBoundaryChanged:
                return "The standalone Native Codex removal boundary changed. Refresh removal discovery before continuing."
            case .nativeRecoveryRequired:
                return "Recover the interrupted standalone Native Codex removal before continuing."
            case .customCodexHomeUnsupported:
                return "Standalone Native removal supports only the fixed ~/.codex configuration location."
            case .teardownUnsupported:
                return "This OpenCodex installation does not have a verified Relay preserving teardown adapter."
            case .teardownCandidateChanged:
                return "The OpenCodex installation changed after compatibility verification. Discover it again before continuing."
            case .teardownPreflightFailed:
                return "Relay could not verify that the preserving teardown can start without changing OpenCodex data."
            case .teardownRefused:
                return "The preserving teardown refused to change an unverified OpenCodex integration state."
            case .teardownResultInvalid:
                return "The preserving teardown returned an invalid bounded result. No package removal was confirmed."
            case .teardownVerificationFailed:
                return "Relay could not verify the preserving teardown result. Recovery is required before retrying."
            case .invalidAddress:
                return "Enter an absolute HTTPS gateway address ending in /v1."
            case .credentialUnavailable:
                return "One or more gateway credentials are unavailable."
            case .authenticationFailed:
                return "The gateway rejected the current address and credential combination."
            case .gatewayUnreachable:
                return "The gateway could not be reached."
            case .catalogInvalid:
                return "The gateway returned an invalid Codex model catalog."
            case .configChanged:
                return "The gateway configuration changed. Refresh it before applying."
            case .routingChanged:
                return "Routing changed. Refresh the gateway settings before applying."
            case .transitionPending:
                return "Finish or cancel the pending routing change before editing the gateway."
            case .runtimeSwapFailed:
                return "The resident Relay could not safely reload the gateway runtime."
            case .gatewayUnsupported:
                return "This Relay installation does not support Desktop gateway settings."
            case .integrationAppLocationInvalid:
                return "Move the app to Applications before preparing the user Relay integration."
            case .integrationArtifactInvalid:
                return "The app bundle did not pass the integration artifact checks."
            case .integrationStateChanged:
                return "Integration state changed. Refresh before trying again."
            case .integrationStateUnsafe:
                return "The existing user integration state is unsafe and requires review."
            case .integrationRecoveryRequired:
                return "Recover the interrupted user integration before trying again."
            case .integrationActivationFailed:
                return "The user Relay could not be started and verified."
            case .releaseStageInvalidRequest:
                return "The selected update request is no longer valid. Check for updates again."
            case .releaseStageBusy, .lifecycleWriterBusy:
                return "Another Relay lifecycle operation is active. Try again after it finishes."
            case .releaseStageVerificationFailed:
                return "The selected release failed verification. Open the exact release for review."
            case .releaseStageRecoveryRequired, .lifecycleStateUnsafe:
                return "Resolve the existing Relay recovery state before staging an update."
            case .runtimeUpgradeIncompatible:
                return "The installed and bundled Relay runtimes are not safely upgrade-compatible."
            case .relayRestartConfirmationRequired:
                return "Confirm the Relay restart before finishing the runtime update."
            case .invalidRequest, .operationFailed:
                return "Relay control did not complete. Refresh the status and try again."
            }
        case .invalidJSON, .invalidStatus:
            return "Relay control returned an unrecognized routing status."
        case .launchFailed:
            return "Relay control could not be started."
        case .timedOut:
            return "Relay control timed out and was stopped."
        case .cancelled:
            return "Relay control was cancelled."
        case .outputTooLarge:
            return "Relay control returned an oversized status response."
        }
    }

    public var errorDescription: String? { safeMessage }
}
