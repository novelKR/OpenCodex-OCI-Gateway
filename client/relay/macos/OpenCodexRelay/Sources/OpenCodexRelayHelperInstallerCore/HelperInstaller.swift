import CryptoKit
import Darwin
import Foundation

public enum HelperInstallerDistribution: String, Sendable {
    case production
    case localDevelopment = "local_development"
}

public struct HelperInstallerProfile: Equatable, Sendable {
    public let distribution: HelperInstallerDistribution
    public let appIdentifier: String
    public let helperIdentifier: String
    public let installerIdentifier: String
    public let serviceName: String
    public let helperRelativePath: String
    public let plistRelativePath: String
    public let transactionDirectoryName: String

    public static let production = make(
        distribution: .production,
        appIdentifier: "io.github.novelkr.opencodex-relay",
        helperIdentifier: "io.github.novelkr.opencodex-relay.homebrew-guard.helper",
        installerIdentifier: "io.github.novelkr.opencodex-relay.homebrew-guard.installer",
        serviceName: "io.github.novelkr.opencodex-relay.homebrew-guard",
        transactionDirectoryName: "homebrew-guard-installer-transaction-production"
    )

    public static let localDevelopment = make(
        distribution: .localDevelopment,
        appIdentifier: "io.github.novelkr.opencodex-relay.dev",
        helperIdentifier: "io.github.novelkr.opencodex-relay.homebrew-guard.helper.dev",
        installerIdentifier: "io.github.novelkr.opencodex-relay.homebrew-guard.installer.dev",
        serviceName: "io.github.novelkr.opencodex-relay.homebrew-guard.manual.dev",
        transactionDirectoryName: "homebrew-guard-installer-transaction"
    )

    public static func forDistribution(_ distribution: HelperInstallerDistribution) -> Self {
        distribution == .production ? .production : .localDevelopment
    }

    private static func make(
        distribution: HelperInstallerDistribution,
        appIdentifier: String,
        helperIdentifier: String,
        installerIdentifier: String,
        serviceName: String,
        transactionDirectoryName: String
    ) -> Self {
        Self(
            distribution: distribution,
            appIdentifier: appIdentifier,
            helperIdentifier: helperIdentifier,
            installerIdentifier: installerIdentifier,
            serviceName: serviceName,
            helperRelativePath: "Library/PrivilegedHelperTools/" + serviceName,
            plistRelativePath: "Library/LaunchDaemons/" + serviceName + ".plist",
            transactionDirectoryName: transactionDirectoryName
        )
    }
}

public enum HelperInstallerConstants {
    public static let installerExecutableName = "OpenCodexRelayHelperInstaller"
    public static let helperExecutableName = "OpenCodexRelayPrivilegedHelper"
    public static let stateDirectoryRelativePath = "var/db/io.github.novelkr.opencodex-relay"
    public static let protectionJournalName = "homebrew-guard.json"
    public static let protectionLockName = "homebrew-guard.lock"
    // Local-development aliases used by its isolated tests. Production always
    // resolves the explicit production profile.
    public static let manualServiceName = HelperInstallerProfile.localDevelopment.serviceName
    public static let helperRelativePath = HelperInstallerProfile.localDevelopment.helperRelativePath
    public static let plistRelativePath = HelperInstallerProfile.localDevelopment.plistRelativePath
    public static let transactionDirectoryName = HelperInstallerProfile.localDevelopment.transactionDirectoryName
}

public enum HelperInstallerCommand: String, CaseIterable, Sendable {
    case status
    case install
    case update
    case uninstall
    case recover
}

public enum HelperInstallerState: String, Codable, Sendable {
    case installRequired = "install_required"
    case updateRequired = "update_required"
    case ready
    case recoveryRequired = "recovery_required"
}

public enum HelperInstallerErrorCode: String, Codable, Error, Sendable {
    case invalidInvocation = "invalid_invocation"
    case rootRequired = "root_required"
    case artifactInvalid = "artifact_invalid"
    case installRequired = "install_required"
    case updateRequired = "update_required"
    case busy
    case recoveryRequired = "recovery_required"
    case protectionActive = "protection_active"
    case installationFailed = "installation_failed"
    case daemonLaunchFailed = "daemon_launch_failed"
    case rollbackFailed = "rollback_failed"
    case recoveryUnavailable = "recovery_unavailable"
    case recoveryVerificationFailed = "recovery_verification_failed"
}

public enum HelperInstallerFailurePhase: String, Codable, Sendable {
    case preflight
    case statePrepare = "state_prepare"
    case helperPublish = "helper_publish"
    case plistPublish = "plist_publish"
    case signatureCheck = "signature_check"
    case daemonStop = "daemon_stop"
    case daemonStart = "daemon_start"
    case readiness
    case finalize
    case rollback
}

public enum HelperInstallerFailureReason: String, Codable, Error, Sendable {
    case permissionDenied = "permission_denied"
    case unsafeParent = "unsafe_parent"
    case sourceUnreadable = "source_unreadable"
    case publishFailed = "publish_failed"
    case durabilityFailed = "durability_failed"
    case ownerOrModeMismatch = "owner_or_mode_mismatch"
    case signatureInvalid = "signature_invalid"
    case daemonStopFailed = "daemon_stop_failed"
    case daemonStartRejected = "daemon_start_rejected"
    case daemonNotLoaded = "daemon_not_loaded"
    case probeSpawnFailed = "probe_spawn_failed"
    case probeTimeout = "probe_timeout"
    case xpcRejected = "xpc_rejected"
    case invalidResponse = "invalid_response"
    case unknown
}

public enum HelperInstallerRollbackResult: String, Codable, Sendable {
    case notNeeded = "not_needed"
    case completed
    case failed
}

public enum HelperInstallerProbeTimeoutCleanup {
    @discardableResult
    public static func stop(
        isRunning: () -> Bool,
        terminate: () -> Void,
        waitForExit: (_ seconds: TimeInterval) -> Bool,
        forceKill: () -> Void
    ) -> Bool {
        terminate()
        if waitForExit(1) { return true }
        guard isRunning() else { return true }
        forceKill()
        return waitForExit(1)
    }
}

public struct HelperInstallerFailure: Error, Equatable, Sendable {
    public let errorCode: HelperInstallerErrorCode
    public let phase: HelperInstallerFailurePhase
    public let reason: HelperInstallerFailureReason
    public let rollbackResult: HelperInstallerRollbackResult

    public init(
        errorCode: HelperInstallerErrorCode,
        phase: HelperInstallerFailurePhase,
        reason: HelperInstallerFailureReason,
        rollbackResult: HelperInstallerRollbackResult = .notNeeded
    ) {
        self.errorCode = errorCode
        self.phase = phase
        self.reason = reason
        self.rollbackResult = rollbackResult
    }

    func changing(
        errorCode: HelperInstallerErrorCode? = nil,
        rollbackResult: HelperInstallerRollbackResult? = nil
    ) -> HelperInstallerFailure {
        HelperInstallerFailure(
            errorCode: errorCode ?? self.errorCode,
            phase: phase,
            reason: reason,
            rollbackResult: rollbackResult ?? self.rollbackResult
        )
    }
}

public struct HelperInstallerReceipt: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let command: String
    public let state: HelperInstallerState
    public let resultCode: String
    public let helperVersion: String?
    public let errorCode: HelperInstallerErrorCode?
    public let failurePhase: HelperInstallerFailurePhase?
    public let failureReason: HelperInstallerFailureReason?
    public let rollbackResult: HelperInstallerRollbackResult?

    public init(
        command: HelperInstallerCommand,
        state: HelperInstallerState,
        resultCode: String,
        helperVersion: String?,
        errorCode: HelperInstallerErrorCode? = nil,
        failurePhase: HelperInstallerFailurePhase? = nil,
        failureReason: HelperInstallerFailureReason? = nil,
        rollbackResult: HelperInstallerRollbackResult? = nil
    ) {
        self.schemaVersion = 1
        self.command = command.rawValue
        self.state = state
        self.resultCode = resultCode
        self.helperVersion = helperVersion
        self.errorCode = errorCode
        self.failurePhase = failurePhase
        self.failureReason = failureReason
        self.rollbackResult = rollbackResult
    }

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case command
        case state
        case resultCode = "result_code"
        case helperVersion = "helper_version"
        case errorCode = "error_code"
        case failurePhase = "failure_phase"
        case failureReason = "failure_reason"
        case rollbackResult = "rollback_result"
    }
}

public enum HelperInstallerDiagnostics {
    public static func failure(
        _ error: any Error,
        phase: HelperInstallerFailurePhase,
        fallbackReason: HelperInstallerFailureReason,
        fallbackCode: HelperInstallerErrorCode = .installationFailed
    ) -> HelperInstallerFailure {
        if let failure = error as? HelperInstallerFailure {
            return failure
        }
        if let reason = error as? HelperInstallerFailureReason {
            let code: HelperInstallerErrorCode = switch reason {
            case .signatureInvalid: .artifactInvalid
            case .daemonStartRejected, .daemonNotLoaded, .probeSpawnFailed,
                 .probeTimeout, .xpcRejected, .invalidResponse:
                .daemonLaunchFailed
            default: fallbackCode
            }
            return HelperInstallerFailure(
                errorCode: code,
                phase: phase,
                reason: reason
            )
        }
        if let code = error as? HelperInstallerErrorCode {
            return HelperInstallerFailure(
                errorCode: code,
                phase: phase,
                reason: code == .artifactInvalid && phase == .signatureCheck
                    ? .signatureInvalid
                    : fallbackReason
            )
        }
        return HelperInstallerFailure(
            errorCode: fallbackCode,
            phase: phase,
            reason: fallbackReason
        )
    }

    public static func receipt(
        command: HelperInstallerCommand,
        fallbackState: HelperInstallerState,
        helperVersion: String?,
        error: any Error
    ) -> HelperInstallerReceipt {
        let failure = error as? HelperInstallerFailure
        let code = failure?.errorCode ??
            (error as? HelperInstallerErrorCode) ?? .installationFailed
        let state: HelperInstallerState = switch code {
        case .installRequired: .installRequired
        case .updateRequired: .updateRequired
        case .recoveryRequired, .protectionActive, .rollbackFailed,
             .recoveryUnavailable, .recoveryVerificationFailed:
            .recoveryRequired
        default: fallbackState
        }
        return HelperInstallerReceipt(
            command: command,
            state: state,
            resultCode: code.rawValue,
            helperVersion: helperVersion,
            errorCode: code,
            failurePhase: failure?.phase,
            failureReason: failure?.reason,
            rollbackResult: failure?.rollbackResult
        )
    }
}

public struct HelperInstallerArtifacts: Sendable {
    public let sourceHelperURL: URL
    public let helperVersion: String
    public let daemonClientRequirement: String

    public init(
        sourceHelperURL: URL,
        helperVersion: String,
        daemonClientRequirement: String
    ) {
        self.sourceHelperURL = sourceHelperURL.standardizedFileURL
        self.helperVersion = helperVersion
        self.daemonClientRequirement = daemonClientRequirement
    }
}

public protocol HelperInstallerRuntime: Sendable {
    func bootout(serviceName: String) throws
    func bootstrap(plistURL: URL) throws
    func serviceIsLoaded(serviceName: String) -> Bool
    func validateInstalledHelper(at helperURL: URL) throws
    func probeReadiness() throws
}

public struct HelperInstallerConfiguration: Sendable {
    public let systemRootURL: URL
    public let requireRoot: Bool
    public let expectedOwnerUID: uid_t
    public let expectedOwnerGID: gid_t
    public let profile: HelperInstallerProfile

    public init(
        systemRootURL: URL = URL(fileURLWithPath: "/"),
        requireRoot: Bool = true,
        expectedOwnerUID: uid_t = 0,
        expectedOwnerGID: gid_t = 0,
        distribution: HelperInstallerDistribution = .localDevelopment
    ) {
        self.systemRootURL = systemRootURL.standardizedFileURL
        self.requireRoot = requireRoot
        self.expectedOwnerUID = expectedOwnerUID
        self.expectedOwnerGID = expectedOwnerGID
        self.profile = .forDistribution(distribution)
    }
}

public final class HelperInstallerController: @unchecked Sendable {
    private enum TransactionPhase: String, Codable {
        case preparing
        case backupsReady = "backups_ready"
        case mutationStarted = "mutation_started"
        case helperChanged = "helper_changed"
        case plistChanged = "plist_changed"
        case serviceStopped = "service_stopped"
        case serviceStarted = "service_started"
        case activationVerified = "activation_verified"
    }

    private struct FileWitness: Codable, Equatable {
        let size: UInt64
        let sha256: String

        func validated() -> Bool {
            size <= 64 * 1024 * 1024 &&
                sha256.range(of: #"^[0-9a-f]{64}$"#, options: .regularExpression) != nil
        }
    }

    private struct TransactionJournal: Codable {
        let schemaVersion: Int
        let transactionID: String
        let command: String
        let phase: TransactionPhase
        let hadHelper: Bool
        let hadPlist: Bool
        let wasServiceLoaded: Bool
        let helperWitness: FileWitness?
        let plistWitness: FileWitness?

        enum CodingKeys: String, CodingKey {
            case schemaVersion = "schema_version"
            case transactionID = "transaction_id"
            case command
            case phase
            case hadHelper = "had_helper"
            case hadPlist = "had_plist"
            case wasServiceLoaded = "was_service_loaded"
            case helperWitness = "helper_witness"
            case plistWitness = "plist_witness"
        }

        func changingPhase(to nextPhase: TransactionPhase) -> TransactionJournal {
            TransactionJournal(
                schemaVersion: schemaVersion,
                transactionID: transactionID,
                command: command,
                phase: nextPhase,
                hadHelper: hadHelper,
                hadPlist: hadPlist,
                wasServiceLoaded: wasServiceLoaded,
                helperWitness: helperWitness,
                plistWitness: plistWitness
            )
        }
    }

    private let configuration: HelperInstallerConfiguration
    private let artifacts: HelperInstallerArtifacts
    private let runtime: any HelperInstallerRuntime
    private let fileManager: FileManager

    public init(
        configuration: HelperInstallerConfiguration,
        artifacts: HelperInstallerArtifacts,
        runtime: any HelperInstallerRuntime,
        fileManager: FileManager = .default
    ) {
        self.configuration = configuration
        self.artifacts = artifacts
        self.runtime = runtime
        self.fileManager = fileManager
    }

    public func perform(_ command: HelperInstallerCommand) throws -> HelperInstallerReceipt {
        try diagnosed(phase: .preflight, fallbackReason: .unknown) {
            try validateArtifacts()
        }
        if command == .status {
            return statusReceipt(command: command)
        }
        if configuration.requireRoot && geteuid() != 0 {
            throw HelperInstallerErrorCode.rootRequired
        }

        let lock = try diagnosed(phase: .statePrepare, fallbackReason: .unknown) {
            try ensureStateDirectory()
            return try acquireProtectionLock()
        }
        defer {
            _ = flock(lock, LOCK_UN)
            Darwin.close(lock)
        }
        guard !pathExists(protectionJournalURL) else {
            throw HelperInstallerErrorCode.protectionActive
        }
        if command == .recover {
            guard pathExists(transactionDirectoryURL) else {
                throw HelperInstallerErrorCode.recoveryUnavailable
            }
        } else {
            guard !pathExists(transactionDirectoryURL) else {
                throw HelperInstallerErrorCode.recoveryRequired
            }
        }

        switch command {
        case .status:
            return statusReceipt(command: command)
        case .install:
            let current = installationState()
            if current == .ready {
                return readyReceipt(command: command, resultCode: "already_ready")
            }
            if current == .recoveryRequired {
                throw HelperInstallerErrorCode.recoveryRequired
            }
            guard current == .installRequired else {
                throw HelperInstallerErrorCode.updateRequired
            }
            try replaceInstallation(command: command)
            return readyReceipt(command: command, resultCode: "installed")
        case .update:
            let current = installationState()
            guard current != .installRequired else {
                throw HelperInstallerErrorCode.installRequired
            }
            guard current != .recoveryRequired else {
                throw HelperInstallerErrorCode.recoveryRequired
            }
            try replaceInstallation(command: command)
            return readyReceipt(command: command, resultCode: "updated")
        case .uninstall:
            let current = installationState()
            if current == .installRequired {
                return HelperInstallerReceipt(
                    command: command,
                    state: .installRequired,
                    resultCode: "already_absent",
                    helperVersion: nil
                )
            }
            guard current != .recoveryRequired else {
                throw HelperInstallerErrorCode.recoveryRequired
            }
            try removeInstallation(command: command)
            return HelperInstallerReceipt(
                command: command,
                state: .installRequired,
                resultCode: "uninstalled",
                helperVersion: nil
            )
        case .recover:
            return try recoverInterruptedTransaction()
        }
    }

    public func failureReceipt(
        command: HelperInstallerCommand,
        error: any Error
    ) -> HelperInstallerReceipt {
        HelperInstallerDiagnostics.receipt(
            command: command,
            fallbackState: installationState(),
            helperVersion: artifacts.helperVersion,
            error: error
        )
    }

    private var helperURL: URL {
        configuration.systemRootURL.appending(path: configuration.profile.helperRelativePath)
    }

    private var plistURL: URL {
        configuration.systemRootURL.appending(path: configuration.profile.plistRelativePath)
    }

    private var stateDirectoryURL: URL {
        configuration.systemRootURL.appending(path: HelperInstallerConstants.stateDirectoryRelativePath)
    }

    private var protectionJournalURL: URL {
        stateDirectoryURL.appending(path: HelperInstallerConstants.protectionJournalName)
    }

    private var protectionLockURL: URL {
        stateDirectoryURL.appending(path: HelperInstallerConstants.protectionLockName)
    }

    private var transactionDirectoryURL: URL {
        stateDirectoryURL.appending(path: configuration.profile.transactionDirectoryName)
    }

    private var transactionJournalURL: URL {
        transactionDirectoryURL.appending(path: "journal.json")
    }

    private var helperBackupURL: URL {
        transactionDirectoryURL.appending(path: "helper.backup")
    }

    private var plistBackupURL: URL {
        transactionDirectoryURL.appending(path: "daemon.backup.plist")
    }

    private func statusReceipt(command: HelperInstallerCommand) -> HelperInstallerReceipt {
        let state = installationState()
        return HelperInstallerReceipt(
            command: command,
            state: state,
            resultCode: state.rawValue,
            helperVersion: state == .installRequired ? nil : artifacts.helperVersion,
            errorCode: state == .recoveryRequired ? .recoveryRequired : nil
        )
    }

    private func readyReceipt(
        command: HelperInstallerCommand,
        resultCode: String
    ) -> HelperInstallerReceipt {
        HelperInstallerReceipt(
            command: command,
            state: .ready,
            resultCode: resultCode,
            helperVersion: artifacts.helperVersion
        )
    }

    private func installationState() -> HelperInstallerState {
        if pathExists(transactionDirectoryURL) || pathExists(protectionJournalURL) {
            return .recoveryRequired
        }
        return artifactInstallationState()
    }

    private func artifactInstallationState() -> HelperInstallerState {
        let helperExists = pathExists(helperURL)
        let plistExists = pathExists(plistURL)
        if !helperExists && !plistExists { return .installRequired }
        guard helperExists, plistExists,
              regularFile(helperURL, requiredMode: 0o755),
              regularFile(plistURL, requiredMode: 0o644) else {
            return .recoveryRequired
        }
        guard filesEqual(helperURL, artifacts.sourceHelperURL),
              installedPlistMatches() else {
            return .updateRequired
        }
        return .ready
    }

    private func targetInstallationMatches() -> Bool {
        guard artifactInstallationState() == .ready else { return false }
        do {
            try runtime.validateInstalledHelper(at: helperURL)
            return true
        } catch {
            return false
        }
    }

    private func validateArtifacts() throws {
        guard Self.validVersion(artifacts.helperVersion),
              Self.validRequirement(artifacts.daemonClientRequirement),
              regularFile(artifacts.sourceHelperURL, requiredMode: nil),
              access(artifacts.sourceHelperURL.path, X_OK) == 0 else {
            throw HelperInstallerErrorCode.artifactInvalid
        }
    }

    private func ensureStateDirectory() throws {
        try ensureDirectory(stateDirectoryURL, mode: 0o700)
    }

    private func acquireProtectionLock() throws -> Int32 {
        let descriptor = Darwin.open(
            protectionLockURL.path,
            O_CREAT | O_RDWR | O_NOFOLLOW | O_CLOEXEC,
            mode_t(0o600)
        )
        guard descriptor >= 0 else { throw HelperInstallerErrorCode.busy }
        var info = stat()
        guard fstat(descriptor, &info) == 0,
              (info.st_mode & S_IFMT) == S_IFREG,
              info.st_uid == configuration.expectedOwnerUID,
              info.st_mode & mode_t(0o077) == 0,
              flock(descriptor, LOCK_EX | LOCK_NB) == 0 else {
            Darwin.close(descriptor)
            throw HelperInstallerErrorCode.busy
        }
        return descriptor
    }

    private func persistTransactionJournal(_ journal: TransactionJournal) throws {
        guard validTransactionJournal(journal) else {
            throw HelperInstallerErrorCode.recoveryUnavailable
        }
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        try atomicWrite(
            try encoder.encode(journal),
            to: transactionJournalURL,
            mode: 0o600
        )
    }

    private func loadTransactionJournal() throws -> TransactionJournal {
        guard secureDirectory(transactionDirectoryURL, exactMode: 0o700) else {
            throw HelperInstallerErrorCode.recoveryUnavailable
        }
        let data = try secureFileData(
            transactionJournalURL,
            requiredMode: 0o600,
            maximumBytes: 64 * 1024
        )
        guard let journal = try? JSONDecoder().decode(TransactionJournal.self, from: data),
              validTransactionJournal(journal) else {
            throw HelperInstallerErrorCode.recoveryUnavailable
        }
        return journal
    }

    private func validTransactionJournal(_ journal: TransactionJournal) -> Bool {
        guard journal.schemaVersion == 2,
              UUID(uuidString: journal.transactionID)?.uuidString.lowercased() == journal.transactionID,
              let command = HelperInstallerCommand(rawValue: journal.command),
              command == .install || command == .update || command == .uninstall,
              journal.hadHelper == (journal.helperWitness != nil),
              journal.hadPlist == (journal.plistWitness != nil),
              journal.helperWitness?.validated() ?? true,
              journal.plistWitness?.validated() ?? true else {
            return false
        }
        return !journal.wasServiceLoaded || (journal.hadHelper && journal.hadPlist)
    }

    private func updateTransactionPhase(_ phase: TransactionPhase) throws {
        let journal = try loadTransactionJournal()
        let validTransition: Bool
        switch HelperInstallerCommand(rawValue: journal.command) {
        case .install, .update:
            validTransition = switch (journal.phase, phase) {
            case (.backupsReady, .mutationStarted),
                 (.mutationStarted, .helperChanged),
                 (.helperChanged, .plistChanged),
                 (.plistChanged, .serviceStopped),
                 (.serviceStopped, .serviceStarted),
                 (.serviceStarted, .activationVerified):
                true
            default:
                false
            }
        case .uninstall:
            validTransition = switch (journal.phase, phase) {
            case (.backupsReady, .mutationStarted),
                 (.mutationStarted, .serviceStopped),
                 (.serviceStopped, .helperChanged),
                 (.helperChanged, .plistChanged),
                 (.plistChanged, .activationVerified):
                true
            default:
                false
            }
        case .status, .recover, nil:
            validTransition = false
        }
        guard validTransition else {
            throw HelperInstallerErrorCode.recoveryUnavailable
        }
        try persistTransactionJournal(journal.changingPhase(to: phase))
    }

    private func replaceInstallation(command: HelperInstallerCommand) throws {
        let hadHelper = pathExists(helperURL)
        let hadPlist = pathExists(plistURL)
        do {
            _ = try diagnosed(phase: .statePrepare, fallbackReason: .publishFailed) {
                try beginTransaction(command: command, hadHelper: hadHelper, hadPlist: hadPlist)
            }
        } catch {
            let failure = diagnosedFailure(
                error,
                phase: .statePrepare,
                fallbackReason: .publishFailed
            )
            if pathExists(transactionDirectoryURL) {
                throw failure.changing(
                    errorCode: .rollbackFailed,
                    rollbackResult: .failed
                )
            }
            throw failure
        }
        do {
            try diagnosed(phase: .statePrepare, fallbackReason: .publishFailed) {
                try updateTransactionPhase(.mutationStarted)
                // Preserve a sticky bit already present on a root-owned macOS
                // system publishing directory. App-owned state remains exact-mode.
                try ensureDirectory(
                    helperURL.deletingLastPathComponent(),
                    mode: 0o755,
                    allowExistingStickyBit: true
                )
                try ensureDirectory(
                    plistURL.deletingLastPathComponent(),
                    mode: 0o755,
                    allowExistingStickyBit: true
                )
            }
            try diagnosed(phase: .helperPublish, fallbackReason: .publishFailed) {
                try atomicCopy(artifacts.sourceHelperURL, to: helperURL, mode: 0o755)
                try updateTransactionPhase(.helperChanged)
            }
            try diagnosed(phase: .plistPublish, fallbackReason: .publishFailed) {
                try atomicWrite(try daemonPlistData(), to: plistURL, mode: 0o644)
                try updateTransactionPhase(.plistChanged)
            }
            try diagnosed(phase: .signatureCheck, fallbackReason: .signatureInvalid) {
                try runtime.validateInstalledHelper(at: helperURL)
            }
            try diagnosed(phase: .daemonStop, fallbackReason: .daemonStopFailed) {
                try runtime.bootout(serviceName: configuration.profile.serviceName)
                try updateTransactionPhase(.serviceStopped)
            }
            try diagnosed(
                phase: .daemonStart,
                fallbackReason: .daemonStartRejected,
                fallbackCode: .daemonLaunchFailed
            ) {
                try runtime.bootstrap(plistURL: plistURL)
                try updateTransactionPhase(.serviceStarted)
            }
            try diagnosed(
                phase: .readiness,
                fallbackReason: .daemonNotLoaded,
                fallbackCode: .daemonLaunchFailed
            ) {
                guard runtime.serviceIsLoaded(
                    serviceName: configuration.profile.serviceName
                ) else {
                    throw HelperInstallerFailureReason.daemonNotLoaded
                }
                try runtime.probeReadiness()
                try updateTransactionPhase(.activationVerified)
            }
            try diagnosed(phase: .finalize, fallbackReason: .durabilityFailed) {
                try finishTransaction()
            }
        } catch {
            let failure = diagnosedFailure(error, phase: .statePrepare, fallbackReason: .unknown)
            try rollbackAndThrow(failure)
        }
    }

    private func removeInstallation(command: HelperInstallerCommand) throws {
        let hadHelper = pathExists(helperURL)
        let hadPlist = pathExists(plistURL)
        do {
            _ = try diagnosed(phase: .statePrepare, fallbackReason: .publishFailed) {
                try beginTransaction(command: command, hadHelper: hadHelper, hadPlist: hadPlist)
            }
        } catch {
            let failure = diagnosedFailure(
                error,
                phase: .statePrepare,
                fallbackReason: .publishFailed
            )
            if pathExists(transactionDirectoryURL) {
                throw failure.changing(
                    errorCode: .rollbackFailed,
                    rollbackResult: .failed
                )
            }
            throw failure
        }
        do {
            try diagnosed(phase: .statePrepare, fallbackReason: .publishFailed) {
                try updateTransactionPhase(.mutationStarted)
            }
            try diagnosed(phase: .daemonStop, fallbackReason: .daemonStopFailed) {
                try runtime.bootout(serviceName: configuration.profile.serviceName)
                try updateTransactionPhase(.serviceStopped)
            }
            try diagnosed(phase: .helperPublish, fallbackReason: .publishFailed) {
                if pathExists(helperURL) { try fileManager.removeItem(at: helperURL) }
                try updateTransactionPhase(.helperChanged)
            }
            try diagnosed(phase: .plistPublish, fallbackReason: .publishFailed) {
                if pathExists(plistURL) { try fileManager.removeItem(at: plistURL) }
                try updateTransactionPhase(.plistChanged)
                guard !pathExists(helperURL), !pathExists(plistURL) else {
                    throw HelperInstallerFailureReason.publishFailed
                }
                try updateTransactionPhase(.activationVerified)
            }
            try diagnosed(phase: .finalize, fallbackReason: .durabilityFailed) {
                try finishTransaction()
            }
        } catch {
            let failure = diagnosedFailure(error, phase: .statePrepare, fallbackReason: .unknown)
            try rollbackAndThrow(failure)
        }
    }

    private func beginTransaction(
        command: HelperInstallerCommand,
        hadHelper: Bool,
        hadPlist: Bool
    ) throws -> TransactionJournal {
        let helperWitness = try hadHelper
            ? fileWitness(helperURL, requiredMode: 0o755, maximumBytes: 64 * 1024 * 1024)
            : nil
        let plistWitness = try hadPlist
            ? fileWitness(plistURL, requiredMode: 0o644, maximumBytes: 64 * 1024)
            : nil
        let wasServiceLoaded = runtime.serviceIsLoaded(
            serviceName: configuration.profile.serviceName
        )
        try ensureDirectory(transactionDirectoryURL, mode: 0o700)
        var journal = TransactionJournal(
            schemaVersion: 2,
            transactionID: UUID().uuidString.lowercased(),
            command: command.rawValue,
            phase: .preparing,
            hadHelper: hadHelper,
            hadPlist: hadPlist,
            wasServiceLoaded: wasServiceLoaded,
            helperWitness: helperWitness,
            plistWitness: plistWitness
        )
        try persistTransactionJournal(journal)
        if hadHelper {
            try atomicCopy(helperURL, to: helperBackupURL, mode: 0o600)
            guard try fileWitness(
                helperBackupURL,
                requiredMode: 0o600,
                maximumBytes: 64 * 1024 * 1024
            ) == helperWitness else {
                throw HelperInstallerErrorCode.recoveryVerificationFailed
            }
        }
        if hadPlist {
            try atomicCopy(plistURL, to: plistBackupURL, mode: 0o600)
            guard try fileWitness(
                plistBackupURL,
                requiredMode: 0o600,
                maximumBytes: 64 * 1024
            ) == plistWitness else {
                throw HelperInstallerErrorCode.recoveryVerificationFailed
            }
        }
        journal = journal.changingPhase(to: .backupsReady)
        try persistTransactionJournal(journal)
        return journal
    }

    private func finishTransaction() throws {
        try synchronizeDirectory(transactionDirectoryURL)
        try synchronizeDirectory(stateDirectoryURL)
        try fileManager.removeItem(at: transactionDirectoryURL)
        // The durable pre-removal sync ensures recovery evidence is never
        // deleted before verification is persisted. If this final sync fails,
        // a crash may resurrect the transaction and safely request recovery.
        try? synchronizeDirectory(stateDirectoryURL)
    }

    private func rollbackOrEscalate() throws {
        do {
            let journal = try loadTransactionJournal()
            _ = try restoreOriginalState(journal)
        } catch {
            throw HelperInstallerErrorCode.rollbackFailed
        }
    }

    private func rollbackAndThrow(_ failure: HelperInstallerFailure) throws -> Never {
        do {
            try rollbackOrEscalate()
        } catch {
            throw failure.changing(
                errorCode: .rollbackFailed,
                rollbackResult: .failed
            )
        }
        throw failure.changing(rollbackResult: .completed)
    }

    private func recoverInterruptedTransaction() throws -> HelperInstallerReceipt {
        let journal = try loadTransactionJournal()
        switch journal.phase {
        case .preparing:
            guard try originalStateMatches(journal),
                  runtime.serviceIsLoaded(
                    serviceName: configuration.profile.serviceName
                  ) == journal.wasServiceLoaded else {
                throw HelperInstallerErrorCode.recoveryVerificationFailed
            }
            try finishTransaction()
            return try recoveredReceipt()
        case .backupsReady:
            guard try backupStateMatches(journal),
                  try originalStateMatches(journal),
                  runtime.serviceIsLoaded(
                    serviceName: configuration.profile.serviceName
                  ) == journal.wasServiceLoaded else {
                throw HelperInstallerErrorCode.recoveryVerificationFailed
            }
            try finishTransaction()
            return try recoveredReceipt()
        case .mutationStarted, .helperChanged, .plistChanged,
             .serviceStopped, .serviceStarted, .activationVerified:
            if let receipt = try completeTargetStateIfPossible(journal) {
                return receipt
            }
            do {
                return try restoreOriginalState(journal)
            } catch HelperInstallerErrorCode.recoveryUnavailable {
                throw HelperInstallerErrorCode.recoveryUnavailable
            } catch {
                throw HelperInstallerErrorCode.recoveryVerificationFailed
            }
        }
    }

    private func completeTargetStateIfPossible(
        _ journal: TransactionJournal
    ) throws -> HelperInstallerReceipt? {
        guard let command = HelperInstallerCommand(rawValue: journal.command) else {
            throw HelperInstallerErrorCode.recoveryUnavailable
        }
        switch command {
        case .install, .update:
            guard targetInstallationMatches() else { return nil }
            do {
                try runtime.bootout(serviceName: configuration.profile.serviceName)
                try runtime.bootstrap(plistURL: plistURL)
                guard runtime.serviceIsLoaded(
                    serviceName: configuration.profile.serviceName
                ) else {
                    return nil
                }
                try runtime.probeReadiness()
                try finishTransaction()
                return readyReceipt(command: .recover, resultCode: "recovered")
            } catch {
                return nil
            }
        case .uninstall:
            guard !pathExists(helperURL), !pathExists(plistURL) else { return nil }
            do {
                try runtime.bootout(serviceName: configuration.profile.serviceName)
                guard !runtime.serviceIsLoaded(
                    serviceName: configuration.profile.serviceName
                ) else {
                    return nil
                }
                try finishTransaction()
                return HelperInstallerReceipt(
                    command: .recover,
                    state: .installRequired,
                    resultCode: "recovered",
                    helperVersion: nil
                )
            } catch {
                return nil
            }
        case .status, .recover:
            throw HelperInstallerErrorCode.recoveryUnavailable
        }
    }

    private func restoreOriginalState(
        _ journal: TransactionJournal
    ) throws -> HelperInstallerReceipt {
        guard try backupStateMatches(journal) else {
            throw HelperInstallerErrorCode.recoveryUnavailable
        }
        try runtime.bootout(serviceName: configuration.profile.serviceName)
        if journal.hadHelper {
            try atomicCopy(helperBackupURL, to: helperURL, mode: 0o755)
        } else if pathExists(helperURL) {
            try fileManager.removeItem(at: helperURL)
        }
        if journal.hadPlist {
            try atomicCopy(plistBackupURL, to: plistURL, mode: 0o644)
        } else if pathExists(plistURL) {
            try fileManager.removeItem(at: plistURL)
        }
        guard try originalStateMatches(journal) else {
            throw HelperInstallerErrorCode.recoveryVerificationFailed
        }
        if journal.wasServiceLoaded {
            guard journal.hadHelper, journal.hadPlist else {
                throw HelperInstallerErrorCode.recoveryUnavailable
            }
            try runtime.bootstrap(plistURL: plistURL)
            guard runtime.serviceIsLoaded(
                serviceName: configuration.profile.serviceName
            ) else {
                throw HelperInstallerErrorCode.recoveryVerificationFailed
            }
            if targetInstallationMatches() {
                do {
                    try runtime.probeReadiness()
                } catch {
                    throw HelperInstallerErrorCode.recoveryVerificationFailed
                }
            }
        } else if runtime.serviceIsLoaded(
            serviceName: configuration.profile.serviceName
        ) {
            throw HelperInstallerErrorCode.recoveryVerificationFailed
        }
        try finishTransaction()
        return try recoveredReceipt()
    }

    private func recoveredReceipt() throws -> HelperInstallerReceipt {
        switch artifactInstallationState() {
        case .ready:
            return readyReceipt(command: .recover, resultCode: "recovered")
        case .updateRequired:
            return HelperInstallerReceipt(
                command: .recover,
                state: .updateRequired,
                resultCode: "rollback_completed_update_required",
                helperVersion: artifacts.helperVersion
            )
        case .installRequired:
            return HelperInstallerReceipt(
                command: .recover,
                state: .installRequired,
                resultCode: "recovered",
                helperVersion: nil
            )
        case .recoveryRequired:
            throw HelperInstallerErrorCode.recoveryVerificationFailed
        }
    }

    private func daemonPlistData() throws -> Data {
        let dictionary: [String: Any] = [
            "Label": configuration.profile.serviceName,
            "Program": helperURL.path,
            "ProgramArguments": [
                helperURL.path,
                "--daemon",
                "--distribution", configuration.profile.distribution.rawValue,
                "--mach-service", configuration.profile.serviceName,
                "--client-requirement", artifacts.daemonClientRequirement,
                "--helper-version", artifacts.helperVersion,
            ],
            "MachServices": [configuration.profile.serviceName: true],
            "ProcessType": "Adaptive",
            "RunAtLoad": false,
        ]
        return try PropertyListSerialization.data(
            fromPropertyList: dictionary,
            format: .xml,
            options: 0
        )
    }

    private func installedPlistMatches() -> Bool {
        guard let data = try? Data(contentsOf: plistURL),
              data.count <= 64 * 1024,
              let value = try? PropertyListSerialization.propertyList(
                  from: data,
                  options: [],
                  format: nil
              ),
              let dictionary = value as? [String: Any],
              dictionary["Label"] as? String == configuration.profile.serviceName,
              dictionary["Program"] as? String == helperURL.path,
              let arguments = dictionary["ProgramArguments"] as? [String],
              arguments == [
                  helperURL.path,
                  "--daemon",
                  "--distribution", configuration.profile.distribution.rawValue,
                  "--mach-service", configuration.profile.serviceName,
                  "--client-requirement", artifacts.daemonClientRequirement,
                  "--helper-version", artifacts.helperVersion,
              ],
              let services = dictionary["MachServices"] as? [String: Bool],
              services == [configuration.profile.serviceName: true] else {
            return false
        }
        return true
    }

    private func backupStateMatches(_ journal: TransactionJournal) throws -> Bool {
        let helperMatches: Bool
        if let witness = journal.helperWitness {
            helperMatches = try fileWitness(
                helperBackupURL,
                requiredMode: 0o600,
                maximumBytes: 64 * 1024 * 1024
            ) == witness
        } else {
            helperMatches = !pathExists(helperBackupURL)
        }
        let plistMatches: Bool
        if let witness = journal.plistWitness {
            plistMatches = try fileWitness(
                plistBackupURL,
                requiredMode: 0o600,
                maximumBytes: 64 * 1024
            ) == witness
        } else {
            plistMatches = !pathExists(plistBackupURL)
        }
        return helperMatches && plistMatches
    }

    private func originalStateMatches(_ journal: TransactionJournal) throws -> Bool {
        let helperMatches: Bool
        if let witness = journal.helperWitness {
            helperMatches = try fileWitness(
                helperURL,
                requiredMode: 0o755,
                maximumBytes: 64 * 1024 * 1024
            ) == witness
        } else {
            helperMatches = !pathExists(helperURL)
        }
        let plistMatches: Bool
        if let witness = journal.plistWitness {
            plistMatches = try fileWitness(
                plistURL,
                requiredMode: 0o644,
                maximumBytes: 64 * 1024
            ) == witness
        } else {
            plistMatches = !pathExists(plistURL)
        }
        return helperMatches && plistMatches
    }

    private func fileWitness(
        _ url: URL,
        requiredMode: mode_t,
        maximumBytes: Int
    ) throws -> FileWitness {
        let data = try secureFileData(
            url,
            requiredMode: requiredMode,
            maximumBytes: maximumBytes
        )
        return FileWitness(
            size: UInt64(data.count),
            sha256: SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
        )
    }

    private func secureFileData(
        _ url: URL,
        requiredMode: mode_t,
        maximumBytes: Int
    ) throws -> Data {
        var linkInfo = stat()
        guard lstat(url.path, &linkInfo) == 0,
              (linkInfo.st_mode & S_IFMT) == S_IFREG,
              linkInfo.st_uid == configuration.expectedOwnerUID,
              linkInfo.st_mode & mode_t(0o7777) == requiredMode,
              linkInfo.st_size >= 0,
              UInt64(linkInfo.st_size) <= UInt64(maximumBytes) else {
            throw HelperInstallerErrorCode.recoveryUnavailable
        }
        let descriptor = Darwin.open(url.path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        guard descriptor >= 0 else {
            throw HelperInstallerErrorCode.recoveryUnavailable
        }
        defer { Darwin.close(descriptor) }
        var openedInfo = stat()
        guard fstat(descriptor, &openedInfo) == 0,
              openedInfo.st_dev == linkInfo.st_dev,
              openedInfo.st_ino == linkInfo.st_ino,
              openedInfo.st_size == linkInfo.st_size else {
            throw HelperInstallerErrorCode.recoveryUnavailable
        }
        let handle = FileHandle(fileDescriptor: descriptor, closeOnDealloc: false)
        guard let data = try handle.readToEnd(), data.count == Int(openedInfo.st_size) else {
            throw HelperInstallerErrorCode.recoveryUnavailable
        }
        return data
    }

    private func synchronizeDirectory(_ url: URL) throws {
        let descriptor = Darwin.open(
            url.path,
            O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC
        )
        guard descriptor >= 0 else {
            throw HelperInstallerErrorCode.recoveryVerificationFailed
        }
        defer { Darwin.close(descriptor) }
        guard fsync(descriptor) == 0 else {
            throw HelperInstallerErrorCode.recoveryVerificationFailed
        }
    }

    private func diagnosed<T>(
        phase: HelperInstallerFailurePhase,
        fallbackReason: HelperInstallerFailureReason,
        fallbackCode: HelperInstallerErrorCode = .installationFailed,
        _ operation: () throws -> T
    ) throws -> T {
        do {
            return try operation()
        } catch {
            throw diagnosedFailure(
                error,
                phase: phase,
                fallbackReason: fallbackReason,
                fallbackCode: fallbackCode
            )
        }
    }

    private func diagnosedFailure(
        _ error: any Error,
        phase: HelperInstallerFailurePhase,
        fallbackReason: HelperInstallerFailureReason,
        fallbackCode: HelperInstallerErrorCode = .installationFailed
    ) -> HelperInstallerFailure {
        HelperInstallerDiagnostics.failure(
            error,
            phase: phase,
            fallbackReason: fallbackReason,
            fallbackCode: fallbackCode
        )
    }

    private func systemFailureReason(
        _ errorNumber: Int32,
        fallback: HelperInstallerFailureReason
    ) -> HelperInstallerFailureReason {
        switch errorNumber {
        case EACCES, EPERM: .permissionDenied
        case ELOOP, ENOTDIR: .unsafeParent
        default: fallback
        }
    }

    private func ensureDirectory(
        _ url: URL,
        mode: mode_t,
        allowExistingStickyBit: Bool = false
    ) throws {
        let parent = url.deletingLastPathComponent()
        guard secureDirectory(parent, exactMode: nil) else {
            throw HelperInstallerFailureReason.unsafeParent
        }
        var info = stat()
        var created = false
        if lstat(url.path, &info) != 0 {
            let lookupError = errno
            guard lookupError == ENOENT else {
                throw systemFailureReason(lookupError, fallback: .publishFailed)
            }
            guard mkdir(url.path, mode) == 0 else {
                throw systemFailureReason(errno, fallback: .publishFailed)
            }
            guard chmod(url.path, mode) == 0 else {
                throw systemFailureReason(errno, fallback: .ownerOrModeMismatch)
            }
            if configuration.requireRoot {
                guard chown(url.path, configuration.expectedOwnerUID, configuration.expectedOwnerGID) == 0 else {
                    throw systemFailureReason(errno, fallback: .ownerOrModeMismatch)
                }
            }
            guard lstat(url.path, &info) == 0 else {
                throw systemFailureReason(errno, fallback: .publishFailed)
            }
            created = true
        }
        let observedMode = info.st_mode & mode_t(0o7777)
        let acceptedExistingMode = mode | mode_t(S_ISVTX)
        let modeAccepted = observedMode == mode ||
            (!created && allowExistingStickyBit && observedMode == acceptedExistingMode)
        guard (info.st_mode & S_IFMT) == S_IFDIR,
              info.st_uid == configuration.expectedOwnerUID,
              modeAccepted else {
            throw HelperInstallerFailureReason.ownerOrModeMismatch
        }
    }

    private func secureDirectory(_ url: URL, exactMode: mode_t?) -> Bool {
        var info = stat()
        guard lstat(url.path, &info) == 0,
              (info.st_mode & S_IFMT) == S_IFDIR,
              info.st_uid == configuration.expectedOwnerUID,
              info.st_mode & mode_t(0o022) == 0 else {
            return false
        }
        if let exactMode {
            return info.st_mode & mode_t(0o7777) == exactMode
        }
        return true
    }

    private func atomicCopy(_ source: URL, to destination: URL, mode: mode_t) throws {
        guard regularFile(source, requiredMode: nil) else {
            throw HelperInstallerFailureReason.sourceUnreadable
        }
        let data: Data
        do {
            data = try Data(contentsOf: source, options: .mappedIfSafe)
        } catch {
            throw HelperInstallerFailureReason.sourceUnreadable
        }
        try atomicWrite(data, to: destination, mode: mode)
    }

    private func atomicWrite(_ data: Data, to destination: URL, mode: mode_t) throws {
        guard data.count <= 64 * 1024 * 1024 else {
            throw HelperInstallerErrorCode.artifactInvalid
        }
        let parent = destination.deletingLastPathComponent()
        let temporary = parent.appending(path: ".\(destination.lastPathComponent).\(UUID().uuidString).tmp")
        let descriptor = Darwin.open(
            temporary.path,
            O_CREAT | O_EXCL | O_WRONLY | O_NOFOLLOW | O_CLOEXEC,
            mode
        )
        guard descriptor >= 0 else {
            throw systemFailureReason(errno, fallback: .publishFailed)
        }
        var succeeded = false
        defer {
            Darwin.close(descriptor)
            if !succeeded { _ = unlink(temporary.path) }
        }
        try data.withUnsafeBytes { bytes in
            guard let base = bytes.baseAddress else { return }
            var offset = 0
            while offset < bytes.count {
                let count = Darwin.write(descriptor, base.advanced(by: offset), bytes.count - offset)
                if count < 0 && errno == EINTR { continue }
                guard count > 0 else {
                    throw systemFailureReason(errno, fallback: .publishFailed)
                }
                offset += count
            }
        }
        guard fchmod(descriptor, mode) == 0 else {
            throw systemFailureReason(errno, fallback: .ownerOrModeMismatch)
        }
        if configuration.requireRoot,
           fchown(descriptor, configuration.expectedOwnerUID, configuration.expectedOwnerGID) != 0 {
            throw systemFailureReason(errno, fallback: .ownerOrModeMismatch)
        }
        guard fsync(descriptor) == 0 else {
            throw HelperInstallerFailureReason.durabilityFailed
        }
        guard rename(temporary.path, destination.path) == 0 else {
            throw systemFailureReason(errno, fallback: .publishFailed)
        }
        let directoryDescriptor = Darwin.open(
            parent.path,
            O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC
        )
        guard directoryDescriptor >= 0 else {
            throw systemFailureReason(errno, fallback: .durabilityFailed)
        }
        defer { Darwin.close(directoryDescriptor) }
        guard fsync(directoryDescriptor) == 0 else {
            throw HelperInstallerFailureReason.durabilityFailed
        }
        succeeded = true
    }

    private func regularFile(_ url: URL, requiredMode: mode_t?) -> Bool {
        var linkInfo = stat()
        guard lstat(url.path, &linkInfo) == 0,
              (linkInfo.st_mode & S_IFMT) == S_IFREG,
              linkInfo.st_uid == configuration.expectedOwnerUID || url == artifacts.sourceHelperURL,
              requiredMode.map({ linkInfo.st_mode & mode_t(0o7777) == $0 }) ?? true else {
            return false
        }
        let descriptor = Darwin.open(url.path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        guard descriptor >= 0 else { return false }
        defer { Darwin.close(descriptor) }
        var openedInfo = stat()
        return fstat(descriptor, &openedInfo) == 0 &&
            openedInfo.st_dev == linkInfo.st_dev &&
            openedInfo.st_ino == linkInfo.st_ino
    }

    private func pathExists(_ url: URL) -> Bool {
        var info = stat()
        return lstat(url.path, &info) == 0
    }

    private func filesEqual(_ lhs: URL, _ rhs: URL) -> Bool {
        guard let left = try? Data(contentsOf: lhs, options: .mappedIfSafe),
              let right = try? Data(contentsOf: rhs, options: .mappedIfSafe),
              left.count <= 64 * 1024 * 1024,
              right.count <= 64 * 1024 * 1024 else {
            return false
        }
        return left == right
    }

    private static func validVersion(_ value: String) -> Bool {
        value == "dev" || value.range(
            of: #"^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$"#,
            options: .regularExpression
        ) != nil
    }

    private static func validRequirement(_ value: String) -> Bool {
        !value.isEmpty && value.utf8.count <= 2_048 &&
            !value.contains("\0") && !value.contains("\n") && !value.contains("\r")
    }
}
