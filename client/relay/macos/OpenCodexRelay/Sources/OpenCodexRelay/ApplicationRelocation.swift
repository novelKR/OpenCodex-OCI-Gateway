import AppKit
import CryptoKit
import Darwin
import Foundation
import OpenCodexRelayCore
import Security

enum ApplicationDestinationScope: String, Codable, Sendable {
    case system
    case user
}

enum ApplicationRelocationStage: String, Codable, Sendable {
    case prepared
    case staged
    case swapped
    case destinationStarted = "destination_started"
    case sourceExited = "source_exited"
    case backupCleanupPending = "backup_cleanup_pending"
    case cleanupPending = "cleanup_pending"
    case sourceCleanupPending = "source_cleanup_pending"
    case recoveryRequired = "recovery_required"
}

enum ApplicationRelocationFailure: String, Error, Equatable, Sendable {
    case previewReadOnly = "preview_read_only"
    case sourceBundleInvalid = "source_bundle_invalid"
    case sourceProcessInvalid = "source_process_invalid"
    case sourceLocationInvalid = "source_location_invalid"
    case destinationUnavailable = "destination_unavailable"
    case artifactInvalid = "artifact_invalid"
    case copyFailed = "copy_failed"
    case swapFailed = "swap_failed"
    case launchFailed = "launch_failed"
    case launchTimedOut = "launch_timed_out"
    case sourceExitTimedOut = "source_exit_timed_out"
    case backupCleanupFailed = "backup_cleanup_failed"
    case sourceChanged = "source_changed"
    case trashFailed = "trash_failed"
    case journalInvalid = "journal_invalid"
}

enum ApplicationRelocationState: Equatable, Sendable {
    case unavailable
    case available
    case preview
    case preparing
    case fallbackConfirmationRequired
    case replacementConfirmationRequired(ApplicationDestinationScope)
    case waitingForDestination
    case sourceExitRequired
    case backupCleanupFailed
    case sourceCleanupRequired
    case completed
    case failed(ApplicationRelocationFailure)
    case recoveryRequired
}

struct ApplicationRelocationFileIdentity: Codable, Equatable, Sendable {
    let device: UInt64
    let inode: UInt64

    static func regularDirectory(at url: URL) -> Self? {
        var info = stat()
        guard Darwin.lstat(url.path, &info) == 0,
              (info.st_mode & S_IFMT) == S_IFDIR else {
            return nil
        }
        return Self(device: UInt64(info.st_dev), inode: UInt64(info.st_ino))
    }
}

struct ApplicationProcessWitness: Codable, Equatable, Sendable {
    let processIdentifier: Int32
    let launchDate: TimeInterval
    let bundlePath: String

    var isValid: Bool {
        processIdentifier > 0 && launchDate.isFinite && launchDate > 0 &&
            bundlePath.hasPrefix("/") &&
            URL(fileURLWithPath: bundlePath, isDirectory: true).pathExtension == "app"
    }
}

enum ApplicationRelocationSourceDisposition: String, Codable, Sendable {
    case keep
    case trash
}

struct ApplicationBundleInspection: Equatable, Sendable {
    let fingerprint: String
    let fileIdentity: ApplicationRelocationFileIdentity
    let runtimeMode: RelayRuntimeMode
}

struct ApplicationRelocationJournal: Codable, Equatable, Sendable {
    static let schemaVersion = 2

    let schema: Int
    var stage: ApplicationRelocationStage
    let nonce: String
    let sourcePath: String
    let destinationPath: String
    let destinationScope: ApplicationDestinationScope
    let stagingPath: String?
    var backupPath: String?
    let backupFingerprint: String?
    let backupIdentity: ApplicationRelocationFileIdentity?
    let destinationWasReused: Bool
    let sourceIdentity: ApplicationRelocationFileIdentity
    let sourceFingerprint: String
    let sourceProcessWitness: ApplicationProcessWitness?
    var destinationFingerprint: String?
    var sourceDisposition: ApplicationRelocationSourceDisposition?
}

struct ApplicationRelocationLaunchRequest: Equatable, Sendable {
    static let flag = "--complete-application-relocation"
    let nonce: String

    static func parse(_ arguments: [String]) -> Self? {
        let values = arguments.dropFirst()
        guard let index = values.firstIndex(of: flag),
              values.index(after: index) < values.endIndex,
              values.filter({ $0 == flag }).count == 1 else {
            return nil
        }
        let nonce = values[values.index(after: index)]
        guard isValidNonce(nonce) else { return nil }
        return Self(nonce: nonce)
    }

    static func isValidNonce(_ value: String) -> Bool {
        guard (32...64).contains(value.utf8.count) else { return false }
        return value.unicodeScalars.allSatisfy {
            CharacterSet.lowercaseLetters.contains($0) ||
                CharacterSet.decimalDigits.contains($0) || $0 == "-"
        }
    }
}

protocol ApplicationBundleValidating: Sendable {
    func inspect(bundleAt url: URL) throws -> ApplicationBundleInspection
}

@MainActor
protocol ApplicationProcessObserving: AnyObject {
    func currentProcessWitness(bundleAt url: URL) -> ApplicationProcessWitness?
    func isProcessRunning(_ witness: ApplicationProcessWitness) -> Bool
}

@MainActor
final class WorkspaceApplicationProcessObserver: ApplicationProcessObserving {
    func currentProcessWitness(bundleAt url: URL) -> ApplicationProcessWitness? {
        let application = NSRunningApplication.current
        guard application.processIdentifier > 0,
              let launchDate = application.launchDate,
              let bundleURL = application.bundleURL,
              Self.normalized(bundleURL) == Self.normalized(url) else {
            return nil
        }
        return ApplicationProcessWitness(
            processIdentifier: application.processIdentifier,
            launchDate: launchDate.timeIntervalSince1970,
            bundlePath: Self.normalized(bundleURL).path
        )
    }

    func isProcessRunning(_ witness: ApplicationProcessWitness) -> Bool {
        guard witness.isValid,
              let application = NSRunningApplication(
                processIdentifier: witness.processIdentifier
              ),
              !application.isTerminated,
              let launchDate = application.launchDate,
              let bundleURL = application.bundleURL else {
            return false
        }
        return Self.matches(
            witness,
            processIdentifier: application.processIdentifier,
            launchDate: launchDate.timeIntervalSince1970,
            bundleURL: bundleURL
        )
    }

    static func matches(
        _ witness: ApplicationProcessWitness,
        processIdentifier: Int32,
        launchDate: TimeInterval,
        bundleURL: URL
    ) -> Bool {
        witness.isValid &&
            processIdentifier == witness.processIdentifier &&
            abs(launchDate - witness.launchDate) < 0.001 &&
            Self.normalized(bundleURL) == Self.normalized(
                URL(fileURLWithPath: witness.bundlePath, isDirectory: true)
            )
    }

    private static func normalized(_ url: URL) -> URL {
        url.resolvingSymlinksInPath().standardizedFileURL
    }
}

@MainActor
protocol ApplicationOpening: AnyObject {
    func openApplication(at url: URL, nonce: String) async -> Bool
}

@MainActor
protocol ApplicationTrashMoving: AnyObject {
    func moveToTrash(_ url: URL) async -> Bool
}

struct SecurityApplicationBundleValidator: ApplicationBundleValidating {
    private static let nestedRelativePaths = [
        "Contents/Library/Helpers/opencodex-relay",
        "Contents/Library/Helpers/opencodex-relayctl",
        "Contents/Library/Helpers/OpenCodexRelayHelperInstaller",
        "Contents/Library/HelperTools/OpenCodexRelayPrivilegedHelper",
    ]
    private static let requiredResourceRelativePaths = [
        "Contents/Resources/RuntimeTrust/opencodex-runtime-release-ed25519.pub",
    ]

    func inspect(bundleAt url: URL) throws -> ApplicationBundleInspection {
        guard url.isFileURL,
              url.pathExtension == "app",
              let fileIdentity = ApplicationRelocationFileIdentity.regularDirectory(at: url),
              let bundle = Bundle(url: url),
              bundle.bundleIdentifier != nil else {
            throw ApplicationRelocationFailure.sourceBundleInvalid
        }

        let mode = RelayRuntimeMode.from(
            declaredMode: bundle.object(forInfoDictionaryKey: "OpenCodexRuntimeMode") as? String
        )
        var codeHashes = [try Self.validatedCodeHash(at: url)]
        for relativePath in Self.nestedRelativePaths {
            let nested = url.appendingPathComponent(relativePath, isDirectory: false)
            guard Self.executableRegularFile(at: nested) else {
                throw ApplicationRelocationFailure.artifactInvalid
            }
            codeHashes.append(try Self.validatedCodeHash(at: nested))
        }
        for relativePath in Self.requiredResourceRelativePaths {
            let resource = url.appendingPathComponent(relativePath, isDirectory: false)
            guard Self.regularFile(at: resource) else {
                throw ApplicationRelocationFailure.artifactInvalid
            }
        }

        let identityMaterial = ([
            bundle.bundleIdentifier ?? "",
            bundle.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "",
            bundle.object(forInfoDictionaryKey: "CFBundleVersion") as? String ?? "",
            mode.rawValue,
        ] + codeHashes).joined(separator: "\n")
        let fingerprint = SHA256.hash(data: Data(identityMaterial.utf8))
            .map { String(format: "%02x", $0) }
            .joined()
        return ApplicationBundleInspection(
            fingerprint: fingerprint,
            fileIdentity: fileIdentity,
            runtimeMode: mode
        )
    }

    private static func executableRegularFile(at url: URL) -> Bool {
        var info = stat()
        return Darwin.lstat(url.path, &info) == 0 &&
            (info.st_mode & S_IFMT) == S_IFREG &&
            (info.st_mode & mode_t(0o111)) != 0
    }

    private static func regularFile(at url: URL) -> Bool {
        var info = stat()
        return Darwin.lstat(url.path, &info) == 0 &&
            (info.st_mode & S_IFMT) == S_IFREG
    }

    private static func validatedCodeHash(at url: URL) throws -> String {
        var code: SecStaticCode?
        guard SecStaticCodeCreateWithPath(url as CFURL, SecCSFlags(), &code) == errSecSuccess,
              let code,
              SecStaticCodeCheckValidity(
                code,
                SecCSFlags(rawValue: kSecCSStrictValidate | kSecCSCheckAllArchitectures),
                nil
              ) == errSecSuccess else {
            throw ApplicationRelocationFailure.artifactInvalid
        }
        var information: CFDictionary?
        guard SecCodeCopySigningInformation(
            code,
            SecCSFlags(rawValue: kSecCSSigningInformation),
            &information
        ) == errSecSuccess,
            let values = information as? [String: Any],
            let unique = values[kSecCodeInfoUnique as String] as? Data,
            (20...64).contains(unique.count) else {
            throw ApplicationRelocationFailure.artifactInvalid
        }
        return unique.base64EncodedString()
    }
}

@MainActor
final class WorkspaceApplicationOpener: ApplicationOpening {
    func openApplication(at url: URL, nonce: String) async -> Bool {
        let configuration = NSWorkspace.OpenConfiguration()
        configuration.arguments = [ApplicationRelocationLaunchRequest.flag, nonce]
        configuration.activates = true
        configuration.createsNewApplicationInstance = true
        do {
            _ = try await NSWorkspace.shared.openApplication(
                at: url,
                configuration: configuration
            )
            return true
        } catch {
            return false
        }
    }
}

@MainActor
final class WorkspaceApplicationTrashMover: ApplicationTrashMoving {
    func moveToTrash(_ url: URL) async -> Bool {
        await withCheckedContinuation { continuation in
            NSWorkspace.shared.recycle([url]) { _, error in
                continuation.resume(returning: error == nil)
            }
        }
    }
}

enum PreparedApplicationRelocation: Sendable {
    case fallbackRequired
    case replacementRequired(ApplicationDestinationScope)
    case launch(ApplicationRelocationJournal)
}

enum PreparedApplicationBackupCleanup: Sendable {
    case complete(ApplicationRelocationJournal)
    case trash(URL, ApplicationRelocationJournal)
}

enum PreparedApplicationSourceCleanup: Sendable {
    case complete(ApplicationRelocationJournal)
    case trash(URL, ApplicationRelocationJournal)
}

final class SystemApplicationRelocator: @unchecked Sendable {
    static let applicationName = "OpenCodexRelay.app"
    static let maximumJournalBytes = 1 << 20

    private let fileManager: FileManager
    private let validator: any ApplicationBundleValidating
    private let sourceURL: URL
    private let homeURL: URL
    private let systemApplicationsURL: URL
    private let userApplicationsURL: URL
    private let journalURL: URL
    private let lockURL: URL

    init(
        sourceURL: URL = Bundle.main.bundleURL,
        homeURL: URL = FileManager.default.homeDirectoryForCurrentUser,
        systemApplicationsURL: URL = URL(fileURLWithPath: "/Applications", isDirectory: true),
        userApplicationsURL: URL? = nil,
        flavor: DistributionFlavor = .current,
        fileManager: FileManager = .default,
        validator: any ApplicationBundleValidating = SecurityApplicationBundleValidator()
    ) {
        self.sourceURL = sourceURL.resolvingSymlinksInPath().standardizedFileURL
        self.homeURL = homeURL.standardizedFileURL
        self.systemApplicationsURL = systemApplicationsURL.standardizedFileURL
        self.userApplicationsURL = (userApplicationsURL ?? homeURL.appendingPathComponent(
            "Applications",
            isDirectory: true
        )).standardizedFileURL
        self.fileManager = fileManager
        self.validator = validator
        let supportName = flavor == .localDevelopment ? "OpenCodexRelayDev" : "OpenCodexRelay"
        self.journalURL = homeURL
            .appendingPathComponent("Library/Application Support", isDirectory: true)
            .appendingPathComponent(supportName, isDirectory: true)
            .appendingPathComponent("application-relocation.json", isDirectory: false)
        self.lockURL = self.journalURL.deletingLastPathComponent()
            .appendingPathComponent("application-relocation.lock", isDirectory: false)
    }

    var sourceBundleURL: URL { sourceURL }

    var isRunningFromStandardLocation: Bool {
        isStandardApplicationURL(sourceURL)
    }

    var hasJournal: Bool {
        var info = stat()
        return Darwin.lstat(journalURL.path, &info) == 0
    }

    func prepare(
        scope: ApplicationDestinationScope,
        allowReplacement: Bool,
        sourceProcessWitness: ApplicationProcessWitness
    ) throws -> PreparedApplicationRelocation {
        try withJournalLock {
            try prepareLocked(
                scope: scope,
                allowReplacement: allowReplacement,
                sourceProcessWitness: sourceProcessWitness
            )
        }
    }

    private func prepareLocked(
        scope: ApplicationDestinationScope,
        allowReplacement: Bool,
        sourceProcessWitness: ApplicationProcessWitness
    ) throws -> PreparedApplicationRelocation {
        let source: ApplicationBundleInspection
        do {
            source = try validator.inspect(bundleAt: sourceURL)
        } catch {
            throw ApplicationRelocationFailure.sourceBundleInvalid
        }
        guard source.runtimeMode == .managed else {
            throw ApplicationRelocationFailure.previewReadOnly
        }
        let witnessedURL = URL(
            fileURLWithPath: sourceProcessWitness.bundlePath,
            isDirectory: true
        ).resolvingSymlinksInPath().standardizedFileURL
        guard sourceProcessWitness.isValid,
              sourceProcessWitness.processIdentifier == getpid(),
              witnessedURL == sourceURL,
              ApplicationRelocationFileIdentity.regularDirectory(at: witnessedURL) ==
                source.fileIdentity else {
            throw ApplicationRelocationFailure.sourceProcessInvalid
        }
        guard !isRunningFromStandardLocation else {
            throw ApplicationRelocationFailure.sourceLocationInvalid
        }
        let verifiedSourceProcessWitness = ApplicationProcessWitness(
            processIdentifier: sourceProcessWitness.processIdentifier,
            launchDate: sourceProcessWitness.launchDate,
            bundlePath: witnessedURL.path
        )
        let destination = destinationURL(for: scope)
        let parent = destination.deletingLastPathComponent()
        do {
            try ensureDestinationDirectory(parent, scope: scope)
        } catch {
            if scope == .system && Self.isPermissionFailure(error) {
                return .fallbackRequired
            }
            throw ApplicationRelocationFailure.destinationUnavailable
        }

        var backupInspection: ApplicationBundleInspection?
        if fileManager.fileExists(atPath: destination.path) {
            guard let existing = try? validator.inspect(bundleAt: destination) else {
                throw ApplicationRelocationFailure.artifactInvalid
            }
            if existing.fingerprint == source.fingerprint {
                var journal = makeJournal(
                    scope: scope,
                    source: source,
                    destination: destination,
                    staging: nil,
                    sourceProcessWitness: verifiedSourceProcessWitness,
                    destinationWasReused: true
                )
                journal.stage = .swapped
                journal.destinationFingerprint = existing.fingerprint
                try writeJournal(journal)
                return .launch(journal)
            }
            guard allowReplacement else { return .replacementRequired(scope) }
            backupInspection = existing
        }

        let staging = parent.appendingPathComponent(
            ".\(Self.applicationName).staging-\(UUID().uuidString.lowercased()).app",
            isDirectory: true
        )
        var journal = makeJournal(
            scope: scope,
            source: source,
            destination: destination,
            staging: staging,
            sourceProcessWitness: verifiedSourceProcessWitness,
            backupInspection: backupInspection
        )
        try writeJournal(journal)
        do {
            try fileManager.copyItem(at: sourceURL, to: staging)
        } catch {
            try? fileManager.removeItem(at: staging)
            if scope == .system && Self.isPermissionFailure(error) {
                try? removeJournal()
                return .fallbackRequired
            }
            try? removeJournal()
            throw ApplicationRelocationFailure.copyFailed
        }

        do {
            let staged: ApplicationBundleInspection
            do {
                staged = try validator.inspect(bundleAt: staging)
            } catch {
                throw ApplicationRelocationFailure.artifactInvalid
            }
            guard staged.fingerprint == source.fingerprint else {
                throw ApplicationRelocationFailure.artifactInvalid
            }
            journal.stage = .staged
            journal.destinationFingerprint = staged.fingerprint
            try writeJournal(journal)
            try swap(
                journal: &journal,
                destination: destination,
                staging: staging,
                allowReplacement: allowReplacement
            )
            try writeJournal(journal)
            return .launch(journal)
        } catch let error as ApplicationRelocationFailure {
            try? fileManager.removeItem(at: staging)
            _ = try? recoverBeforeDestinationStartLocked()
            throw error
        } catch {
            try? fileManager.removeItem(at: staging)
            _ = try? recoverBeforeDestinationStartLocked()
            throw ApplicationRelocationFailure.swapFailed
        }
    }

    func completeDestinationStart(
        nonce: String,
        currentBundleURL: URL
    ) throws -> ApplicationRelocationJournal {
        try withJournalLock {
            var journal = try readJournal()
            guard journal.schema == ApplicationRelocationJournal.schemaVersion,
                  journal.nonce == nonce,
                  journal.stage == .swapped else {
                throw ApplicationRelocationFailure.journalInvalid
            }
            try validateDestination(journal, currentBundleURL: currentBundleURL)
            journal.stage = .destinationStarted
            try writeJournal(journal)
            return journal
        }
    }

    func markSourceExited(
        nonce: String,
        currentBundleURL: URL
    ) throws -> ApplicationRelocationJournal {
        try withJournalLock {
            var journal = try readJournal()
            guard journal.schema == ApplicationRelocationJournal.schemaVersion,
                  journal.nonce == nonce,
                  journal.stage == .destinationStarted else {
                throw ApplicationRelocationFailure.journalInvalid
            }
            try validateDestination(journal, currentBundleURL: currentBundleURL)
            journal.stage = .sourceExited
            try writeJournal(journal)
            return journal
        }
    }

    func sourceIsStillOriginal(_ journal: ApplicationRelocationJournal) -> Bool {
        guard let inspection = try? validator.inspect(
            bundleAt: URL(fileURLWithPath: journal.sourcePath, isDirectory: true)
        ) else { return false }
        return inspection.fileIdentity == journal.sourceIdentity &&
            inspection.fingerprint == journal.sourceFingerprint
    }

    func backupIsStillOriginal(_ journal: ApplicationRelocationJournal) -> Bool {
        guard let backupPath = journal.backupPath,
              let expected = journal.backupFingerprint,
              let inspection = try? validator.inspect(
                bundleAt: URL(fileURLWithPath: backupPath, isDirectory: true)
              ) else { return false }
        if journal.schema == 1 {
            return inspection.fingerprint == expected
        }
        guard let expectedIdentity = journal.backupIdentity else { return false }
        return inspection.fingerprint == expected && inspection.fileIdentity == expectedIdentity
    }

    func prepareBackupCleanup(
        nonce: String,
        currentBundleURL: URL
    ) throws -> PreparedApplicationBackupCleanup {
        try withJournalLock {
            var journal = try readJournal()
            guard journal.schema == ApplicationRelocationJournal.schemaVersion,
                  journal.nonce == nonce,
                  journal.stage == .sourceExited || journal.stage == .backupCleanupPending else {
                throw ApplicationRelocationFailure.journalInvalid
            }
            try validateDestination(journal, currentBundleURL: currentBundleURL)
            guard let backupPath = journal.backupPath else {
                journal.stage = .cleanupPending
                try writeJournal(journal)
                return .complete(journal)
            }
            let backup = URL(fileURLWithPath: backupPath, isDirectory: true)
            if journal.stage == .backupCleanupPending,
               !fileManager.fileExists(atPath: backup.path) {
                journal.stage = .cleanupPending
                try writeJournal(journal)
                return .complete(journal)
            }
            guard backupIsStillOriginal(journal) else {
                throw ApplicationRelocationFailure.journalInvalid
            }
            if journal.stage == .sourceExited {
                journal.stage = .backupCleanupPending
                try writeJournal(journal)
            }
            return .trash(backup, journal)
        }
    }

    func completeBackupCleanup(
        nonce: String,
        currentBundleURL: URL
    ) throws -> ApplicationRelocationJournal {
        try withJournalLock {
            var journal = try readJournal()
            guard journal.schema == ApplicationRelocationJournal.schemaVersion,
                  journal.nonce == nonce,
                  journal.stage == .backupCleanupPending,
                  let backupPath = journal.backupPath,
                  !fileManager.fileExists(atPath: backupPath) else {
                throw ApplicationRelocationFailure.journalInvalid
            }
            try validateDestination(journal, currentBundleURL: currentBundleURL)
            journal.stage = .cleanupPending
            try writeJournal(journal)
            return journal
        }
    }

    func resumeDestinationJournal(
        currentBundleURL: URL
    ) throws -> ApplicationRelocationJournal {
        try withJournalLock {
            let journal = try readJournal()
            guard journal.schema == ApplicationRelocationJournal.schemaVersion,
                  [.destinationStarted, .sourceExited, .backupCleanupPending,
                   .cleanupPending, .sourceCleanupPending].contains(journal.stage) else {
                throw ApplicationRelocationFailure.journalInvalid
            }
            try validateDestination(journal, currentBundleURL: currentBundleURL)
            return journal
        }
    }

    func markRecoveryRequired() {
        try? withJournalLock {
            guard var journal = try? readJournal() else { return }
            journal.stage = .recoveryRequired
            try writeJournal(journal)
        }
    }

    func prepareSourceCleanup(
        disposition: ApplicationRelocationSourceDisposition,
        currentBundleURL: URL
    ) throws -> PreparedApplicationSourceCleanup {
        try withJournalLock {
            var journal = try readJournal()
            guard journal.schema == ApplicationRelocationJournal.schemaVersion,
                  journal.stage == .cleanupPending ||
                    (journal.stage == .sourceCleanupPending && disposition == .keep) else {
                throw ApplicationRelocationFailure.journalInvalid
            }
            try validateDestination(journal, currentBundleURL: currentBundleURL)
            if disposition == .trash && !sourceIsStillOriginal(journal) {
                throw ApplicationRelocationFailure.sourceChanged
            }
            journal.stage = .sourceCleanupPending
            journal.sourceDisposition = disposition
            try writeJournal(journal)
            switch disposition {
            case .keep:
                return .complete(journal)
            case .trash:
                return .trash(
                    URL(fileURLWithPath: journal.sourcePath, isDirectory: true),
                    journal
                )
            }
        }
    }

    func resumeSourceCleanup(
        currentBundleURL: URL
    ) throws -> PreparedApplicationSourceCleanup {
        try withJournalLock {
            let journal = try readJournal()
            guard journal.schema == ApplicationRelocationJournal.schemaVersion,
                  journal.stage == .sourceCleanupPending,
                  let disposition = journal.sourceDisposition else {
                throw ApplicationRelocationFailure.journalInvalid
            }
            try validateDestination(journal, currentBundleURL: currentBundleURL)
            switch disposition {
            case .keep:
                return .complete(journal)
            case .trash:
                let source = URL(fileURLWithPath: journal.sourcePath, isDirectory: true)
                guard fileManager.fileExists(atPath: source.path) else {
                    return .complete(journal)
                }
                guard sourceIsStillOriginal(journal) else {
                    throw ApplicationRelocationFailure.sourceChanged
                }
                return .trash(source, journal)
            }
        }
    }

    func finishSourceCleanup(
        nonce: String,
        currentBundleURL: URL
    ) throws {
        try withJournalLock {
            let journal = try readJournal()
            guard journal.schema == ApplicationRelocationJournal.schemaVersion,
                  journal.nonce == nonce,
                  journal.stage == .sourceCleanupPending,
                  let disposition = journal.sourceDisposition else {
                throw ApplicationRelocationFailure.journalInvalid
            }
            try validateDestination(journal, currentBundleURL: currentBundleURL)
            if disposition == .trash && fileManager.fileExists(atPath: journal.sourcePath) {
                throw ApplicationRelocationFailure.trashFailed
            }
            try removeJournal()
        }
    }

    func currentJournal() throws -> ApplicationRelocationJournal? {
        try withJournalLock {
            guard fileManager.fileExists(atPath: journalURL.path) else { return nil }
            return try readJournal()
        }
    }

    @discardableResult
    func recoverBeforeDestinationStart(expectedNonce: String? = nil) throws -> Bool {
        try withJournalLock {
            try recoverBeforeDestinationStartLocked(expectedNonce: expectedNonce)
        }
    }

    @discardableResult
    private func recoverBeforeDestinationStartLocked(expectedNonce: String? = nil) throws -> Bool {
        var journal = try readJournal()
        if let expectedNonce, journal.nonce != expectedNonce {
            throw ApplicationRelocationFailure.journalInvalid
        }
        guard journal.stage == .prepared || journal.stage == .staged || journal.stage == .swapped else {
            return false
        }
        let destination = URL(fileURLWithPath: journal.destinationPath, isDirectory: true)
        let staging = journal.stagingPath.map {
            URL(fileURLWithPath: $0, isDirectory: true)
        }
        let destinationMatches = (try? validator.inspect(bundleAt: destination).fingerprint) ==
            journal.destinationFingerprint
        if let backupPath = journal.backupPath {
            let backup = URL(fileURLWithPath: backupPath, isDirectory: true)
            if !fileManager.fileExists(atPath: backup.path),
               journal.stage == .staged,
               let backupFingerprint = journal.backupFingerprint,
               (try? validator.inspect(bundleAt: destination).fingerprint) == backupFingerprint {
                if let staging { try? fileManager.removeItem(at: staging) }
                try removeJournal()
                return true
            }
            guard backupIsStillOriginal(journal) else {
                journal.stage = .recoveryRequired
                try writeJournal(journal)
                throw ApplicationRelocationFailure.journalInvalid
            }
            if fileManager.fileExists(atPath: destination.path) {
                guard destinationMatches else {
                    journal.stage = .recoveryRequired
                    try writeJournal(journal)
                    throw ApplicationRelocationFailure.journalInvalid
                }
                try fileManager.removeItem(at: destination)
            }
            try fileManager.moveItem(at: backup, to: destination)
        } else if journal.stage == .staged,
                  fileManager.fileExists(atPath: destination.path) {
            let stagingExists = staging.map { fileManager.fileExists(atPath: $0.path) } ?? false
            if stagingExists,
               let backupFingerprint = journal.backupFingerprint,
               (try? validator.inspect(bundleAt: destination).fingerprint) == backupFingerprint {
                if let staging { try? fileManager.removeItem(at: staging) }
                try removeJournal()
                return true
            }
            guard !stagingExists, destinationMatches, sourceIsStillOriginal(journal) else {
                journal.stage = .recoveryRequired
                try writeJournal(journal)
                throw ApplicationRelocationFailure.journalInvalid
            }
            try fileManager.removeItem(at: destination)
        } else if journal.stage == .swapped && !journal.destinationWasReused {
            guard destinationMatches, sourceIsStillOriginal(journal) else {
                journal.stage = .recoveryRequired
                try writeJournal(journal)
                throw ApplicationRelocationFailure.journalInvalid
            }
            try fileManager.removeItem(at: destination)
        } else if journal.stage == .swapped {
            guard destinationMatches else {
                journal.stage = .recoveryRequired
                try writeJournal(journal)
                throw ApplicationRelocationFailure.journalInvalid
            }
        }
        if let staging {
            try? fileManager.removeItem(at: staging)
        }
        try removeJournal()
        return true
    }

    private func validateDestination(
        _ journal: ApplicationRelocationJournal,
        currentBundleURL: URL
    ) throws {
        guard currentBundleURL.standardizedFileURL.path == journal.destinationPath else {
            throw ApplicationRelocationFailure.journalInvalid
        }
        let current = try validator.inspect(bundleAt: currentBundleURL)
        guard current.fingerprint == journal.destinationFingerprint else {
            throw ApplicationRelocationFailure.artifactInvalid
        }
    }

    private func swap(
        journal: inout ApplicationRelocationJournal,
        destination: URL,
        staging: URL,
        allowReplacement: Bool
    ) throws {
        var backup: URL?
        if fileManager.fileExists(atPath: destination.path) {
            guard allowReplacement else {
                throw ApplicationRelocationFailure.swapFailed
            }
            let candidate = destination.deletingLastPathComponent().appendingPathComponent(
                ".\(Self.applicationName).backup-\(UUID().uuidString.lowercased()).app",
                isDirectory: true
            )
            journal.backupPath = candidate.path
            try writeJournal(journal)
            try fileManager.moveItem(at: destination, to: candidate)
            backup = candidate
        }
        do {
            try fileManager.moveItem(at: staging, to: destination)
            journal.stage = .swapped
        } catch {
            if let backup,
               !fileManager.fileExists(atPath: destination.path),
               fileManager.fileExists(atPath: backup.path) {
                do {
                    try fileManager.moveItem(at: backup, to: destination)
                    journal.backupPath = nil
                    journal.stage = .prepared
                    try writeJournal(journal)
                } catch {
                    // Keep the backup witness in the journal. Recovery will
                    // fail closed instead of guessing which bundle is current.
                }
            }
            throw ApplicationRelocationFailure.swapFailed
        }
    }

    private func makeJournal(
        scope: ApplicationDestinationScope,
        source: ApplicationBundleInspection,
        destination: URL,
        staging: URL?,
        sourceProcessWitness: ApplicationProcessWitness,
        backupInspection: ApplicationBundleInspection? = nil,
        destinationWasReused: Bool = false
    ) -> ApplicationRelocationJournal {
        ApplicationRelocationJournal(
            schema: ApplicationRelocationJournal.schemaVersion,
            stage: .prepared,
            nonce: UUID().uuidString.lowercased(),
            sourcePath: sourceURL.path,
            destinationPath: destination.path,
            destinationScope: scope,
            stagingPath: staging?.path,
            backupPath: nil,
            backupFingerprint: backupInspection?.fingerprint,
            backupIdentity: backupInspection?.fileIdentity,
            destinationWasReused: destinationWasReused,
            sourceIdentity: source.fileIdentity,
            sourceFingerprint: source.fingerprint,
            sourceProcessWitness: sourceProcessWitness,
            destinationFingerprint: nil,
            sourceDisposition: nil
        )
    }

    private func destinationURL(for scope: ApplicationDestinationScope) -> URL {
        let parent: URL
        switch scope {
        case .system:
            parent = systemApplicationsURL
        case .user:
            parent = userApplicationsURL
        }
        return parent.appendingPathComponent(Self.applicationName, isDirectory: true)
    }

    private func ensureDestinationDirectory(
        _ url: URL,
        scope: ApplicationDestinationScope
    ) throws {
        if !fileManager.fileExists(atPath: url.path) {
            guard scope == .user else {
                throw CocoaError(.fileNoSuchFile)
            }
            try fileManager.createDirectory(at: url, withIntermediateDirectories: true)
            try fileManager.setAttributes([.posixPermissions: 0o700], ofItemAtPath: url.path)
        }
        var info = stat()
        guard Darwin.lstat(url.path, &info) == 0,
              (info.st_mode & S_IFMT) == S_IFDIR,
              scope == .system
                ? (info.st_uid == 0 || info.st_uid == getuid())
                : info.st_uid == getuid() else {
            throw ApplicationRelocationFailure.destinationUnavailable
        }
    }

    private func withJournalLock<T>(_ body: () throws -> T) throws -> T {
        let directory = journalURL.deletingLastPathComponent()
        try ensureSecureJournalDirectory(directory)
        let descriptor = Darwin.open(
            lockURL.path,
            O_CREAT | O_RDWR | O_NOFOLLOW | O_CLOEXEC,
            mode_t(0o600)
        )
        guard descriptor >= 0 else { throw ApplicationRelocationFailure.journalInvalid }
        defer { Darwin.close(descriptor) }
        var info = stat()
        guard fstat(descriptor, &info) == 0,
              (info.st_mode & S_IFMT) == S_IFREG,
              info.st_uid == getuid(),
              fchmod(descriptor, mode_t(0o600)) == 0,
              flock(descriptor, LOCK_EX) == 0 else {
            throw ApplicationRelocationFailure.journalInvalid
        }
        defer { _ = flock(descriptor, LOCK_UN) }
        return try body()
    }

    private func writeJournal(_ journal: ApplicationRelocationJournal) throws {
        let directory = journalURL.deletingLastPathComponent()
        try ensureSecureJournalDirectory(directory)
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        let data = try encoder.encode(journal)
        guard data.count <= Self.maximumJournalBytes else {
            throw ApplicationRelocationFailure.journalInvalid
        }
        let temporary = directory.appendingPathComponent(
            ".application-relocation.\(UUID().uuidString.lowercased()).tmp",
            isDirectory: false
        )
        let descriptor = Darwin.open(
            temporary.path,
            O_CREAT | O_EXCL | O_WRONLY | O_NOFOLLOW | O_CLOEXEC,
            mode_t(0o600)
        )
        guard descriptor >= 0 else { throw ApplicationRelocationFailure.journalInvalid }
        var succeeded = false
        defer {
            Darwin.close(descriptor)
            if !succeeded { _ = Darwin.unlink(temporary.path) }
        }
        try data.withUnsafeBytes { bytes in
            guard let base = bytes.baseAddress else { return }
            var offset = 0
            while offset < bytes.count {
                let written = Darwin.write(
                    descriptor,
                    base.advanced(by: offset),
                    bytes.count - offset
                )
                if written < 0 && errno == EINTR { continue }
                guard written > 0 else { throw ApplicationRelocationFailure.journalInvalid }
                offset += written
            }
        }
        guard fchmod(descriptor, mode_t(0o600)) == 0,
              fsync(descriptor) == 0,
              Darwin.rename(temporary.path, journalURL.path) == 0 else {
            throw ApplicationRelocationFailure.journalInvalid
        }
        try syncJournalDirectory(directory)
        succeeded = true
    }

    private func readJournal() throws -> ApplicationRelocationJournal {
        var linkInfo = stat()
        guard Darwin.lstat(journalURL.path, &linkInfo) == 0,
              (linkInfo.st_mode & S_IFMT) == S_IFREG else {
            throw ApplicationRelocationFailure.journalInvalid
        }
        let descriptor = Darwin.open(journalURL.path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        guard descriptor >= 0 else { throw ApplicationRelocationFailure.journalInvalid }
        defer { Darwin.close(descriptor) }
        var info = stat()
        guard fstat(descriptor, &info) == 0,
              info.st_dev == linkInfo.st_dev,
              info.st_ino == linkInfo.st_ino,
              info.st_uid == getuid(),
              info.st_mode & mode_t(0o077) == 0,
              info.st_size >= 0,
              info.st_size <= Self.maximumJournalBytes else {
            throw ApplicationRelocationFailure.journalInvalid
        }
        var data = Data(count: Int(info.st_size))
        try data.withUnsafeMutableBytes { bytes in
            guard let base = bytes.baseAddress else { return }
            var offset = 0
            while offset < bytes.count {
                let count = Darwin.read(
                    descriptor,
                    base.advanced(by: offset),
                    bytes.count - offset
                )
                if count < 0 && errno == EINTR { continue }
                guard count > 0 else { throw ApplicationRelocationFailure.journalInvalid }
                offset += count
            }
        }
        guard data.count <= Self.maximumJournalBytes,
              let journal = try? JSONDecoder().decode(ApplicationRelocationJournal.self, from: data),
              journal.schema == 1 || journal.schema == ApplicationRelocationJournal.schemaVersion,
              ApplicationRelocationLaunchRequest.isValidNonce(journal.nonce),
              validateJournalPaths(journal),
              validateJournalShape(journal) else {
            throw ApplicationRelocationFailure.journalInvalid
        }
        return journal
    }

    private func removeJournal() throws {
        if Darwin.unlink(journalURL.path) != 0 && errno != ENOENT {
            throw ApplicationRelocationFailure.journalInvalid
        }
        try syncJournalDirectory(journalURL.deletingLastPathComponent())
    }

    private func isStandardApplicationURL(_ url: URL) -> Bool {
        let parent = url.standardizedFileURL.deletingLastPathComponent().path
        return parent == systemApplicationsURL.path || parent == userApplicationsURL.path
    }

    private func ensureSecureJournalDirectory(_ directory: URL) throws {
        if !fileManager.fileExists(atPath: directory.path) {
            try fileManager.createDirectory(at: directory, withIntermediateDirectories: true)
        }
        var info = stat()
        guard Darwin.lstat(directory.path, &info) == 0,
              (info.st_mode & S_IFMT) == S_IFDIR,
              info.st_uid == getuid(),
              directory.resolvingSymlinksInPath().standardizedFileURL ==
                directory.standardizedFileURL else {
            throw ApplicationRelocationFailure.journalInvalid
        }
        try fileManager.setAttributes([.posixPermissions: 0o700], ofItemAtPath: directory.path)
    }

    private func syncJournalDirectory(_ directory: URL) throws {
        let descriptor = Darwin.open(
            directory.path,
            O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC
        )
        guard descriptor >= 0 else { throw ApplicationRelocationFailure.journalInvalid }
        defer { Darwin.close(descriptor) }
        guard fsync(descriptor) == 0 else { throw ApplicationRelocationFailure.journalInvalid }
    }

    private func validateJournalPaths(_ journal: ApplicationRelocationJournal) -> Bool {
        let destination = destinationURL(for: journal.destinationScope).standardizedFileURL
        guard URL(fileURLWithPath: journal.destinationPath, isDirectory: true).standardizedFileURL == destination,
              journal.sourcePath.hasPrefix("/"),
              URL(fileURLWithPath: journal.sourcePath, isDirectory: true).pathExtension == "app",
              !isStandardApplicationURL(URL(fileURLWithPath: journal.sourcePath, isDirectory: true)),
              Self.isHexFingerprint(journal.sourceFingerprint),
              journal.backupFingerprint.map(Self.isHexFingerprint) ?? true,
              journal.destinationFingerprint.map(Self.isHexFingerprint) ?? true else {
            return false
        }
        let parent = destination.deletingLastPathComponent().path
        for candidate in [journal.stagingPath, journal.backupPath].compactMap({ $0 }) {
            let url = URL(fileURLWithPath: candidate, isDirectory: true).standardizedFileURL
            guard url.deletingLastPathComponent().path == parent,
                  url.lastPathComponent.hasPrefix(".\(Self.applicationName).") else {
                return false
            }
        }
        return true
    }

    private func validateJournalShape(_ journal: ApplicationRelocationJournal) -> Bool {
        if journal.schema == 1 {
            return journal.sourceProcessWitness == nil &&
                journal.backupIdentity == nil &&
                journal.sourceDisposition == nil
        }
        guard let witness = journal.sourceProcessWitness,
              witness.isValid,
              witness.bundlePath == journal.sourcePath,
              (journal.backupFingerprint == nil) == (journal.backupIdentity == nil) else {
            return false
        }
        if journal.stage == .backupCleanupPending &&
            (journal.backupPath == nil || journal.backupFingerprint == nil) {
            return false
        }
        if [.swapped, .destinationStarted, .sourceExited, .backupCleanupPending,
            .cleanupPending, .sourceCleanupPending].contains(journal.stage),
           journal.destinationFingerprint == nil {
            return false
        }
        switch journal.stage {
        case .sourceCleanupPending:
            return journal.sourceDisposition != nil
        case .recoveryRequired:
            return true
        case .prepared, .staged, .swapped, .destinationStarted, .sourceExited,
             .backupCleanupPending, .cleanupPending:
            return journal.sourceDisposition == nil
        }
    }

    private static func isHexFingerprint(_ value: String) -> Bool {
        value.utf8.count == 64 && value.unicodeScalars.allSatisfy {
            CharacterSet(charactersIn: "0123456789abcdef").contains($0)
        }
    }

    private static func isPermissionFailure(_ error: Error) -> Bool {
        let nsError = error as NSError
        if nsError.domain == NSPOSIXErrorDomain &&
            [EACCES, EPERM].contains(Int32(nsError.code)) {
            return true
        }
        if nsError.domain == NSCocoaErrorDomain && [
            CocoaError.fileWriteNoPermission.rawValue,
            CocoaError.fileReadNoPermission.rawValue,
        ].contains(nsError.code) {
            return true
        }
        if let underlying = nsError.userInfo[NSUnderlyingErrorKey] as? Error {
            return isPermissionFailure(underlying)
        }
        return false
    }
}

@MainActor
final class ApplicationRelocationController: ObservableObject {
    @Published private(set) var state: ApplicationRelocationState
    @Published private(set) var handoffVerified: Bool

    private let runtimeMode: RelayRuntimeMode
    private let relocator: SystemApplicationRelocator
    private let opener: any ApplicationOpening
    private let trashMover: any ApplicationTrashMoving
    private let processObserver: any ApplicationProcessObserving
    private let activityLog: RelayActivityLogStore?
    private let sourceExitCheckCount: Int
    private let sourceExitCheckInterval: Duration
    private var pendingScope: ApplicationDestinationScope?
    private var activeJournal: ApplicationRelocationJournal?

    init(
        runtimeMode: RelayRuntimeMode = .current,
        resumesDestinationLaunch: Bool = false,
        relocator: SystemApplicationRelocator? = nil,
        opener: any ApplicationOpening = WorkspaceApplicationOpener(),
        trashMover: any ApplicationTrashMoving = WorkspaceApplicationTrashMover(),
        processObserver: any ApplicationProcessObserving = WorkspaceApplicationProcessObserver(),
        activityLog: RelayActivityLogStore? = nil,
        sourceExitCheckCount: Int = 50,
        sourceExitCheckInterval: Duration = .milliseconds(200)
    ) {
        self.runtimeMode = runtimeMode
        let resolvedRelocator = relocator ?? SystemApplicationRelocator()
        self.relocator = resolvedRelocator
        self.opener = opener
        self.trashMover = trashMover
        self.processObserver = processObserver
        self.activityLog = activityLog
        self.sourceExitCheckCount = max(1, sourceExitCheckCount)
        self.sourceExitCheckInterval = sourceExitCheckInterval
        self.state = runtimeMode == .preview ? .preview : .unavailable
        self.handoffVerified = false
        restoreInitialState(resumesDestinationLaunch: resumesDestinationLaunch)
    }

    var canStart: Bool {
        switch state {
        case .available, .failed(.copyFailed), .failed(.swapFailed),
             .failed(.launchFailed), .failed(.launchTimedOut),
             .failed(.sourceProcessInvalid):
            true
        default:
            false
        }
    }
    var permitsGatewayConfiguration: Bool {
        runtimeMode == .managed && relocator.isRunningFromStandardLocation && handoffVerified
    }

    func begin() {
        guard runtimeMode == .managed, canStart else { return }
        prepare(scope: .system, allowReplacement: false)
    }

    func approveUserApplicationsFallback() {
        guard state == .fallbackConfirmationRequired else { return }
        prepare(scope: .user, allowReplacement: false)
    }

    func approveReplacement() {
        guard case let .replacementConfirmationRequired(scope) = state else { return }
        prepare(scope: scope, allowReplacement: true)
    }

    func cancelConfirmation() {
        pendingScope = nil
        state = .available
    }

    func completeLaunchRequest(_ request: ApplicationRelocationLaunchRequest) async {
        guard runtimeMode == .managed else { return }
        do {
            let journal = try relocator.completeDestinationStart(
                nonce: request.nonce,
                currentBundleURL: relocator.sourceBundleURL
            )
            activeJournal = journal
            state = .waitingForDestination
            record(
                stage: .destinationStarted,
                result: "destination_started",
                scope: journal.destinationScope
            )
            await continueHandoff()
        } catch {
            enterRecovery(result: "destination_start_invalid", scope: nil)
        }
    }

    func keepOriginal() {
        guard state == .sourceCleanupRequired || state == .failed(.sourceChanged) ||
                state == .failed(.trashFailed) else { return }
        do {
            let action = try relocator.prepareSourceCleanup(
                disposition: .keep,
                currentBundleURL: relocator.sourceBundleURL
            )
            guard case let .complete(journal) = action else {
                throw ApplicationRelocationFailure.journalInvalid
            }
            try relocator.finishSourceCleanup(
                nonce: journal.nonce,
                currentBundleURL: relocator.sourceBundleURL
            )
            finishSourceDisposition(journal: journal, result: "source_kept")
        } catch let failure as ApplicationRelocationFailure where failure == .sourceChanged {
            state = .failed(.sourceChanged)
        } catch {
            enterRecovery(result: "source_keep_failed", scope: activeJournal?.destinationScope)
        }
    }

    func moveOriginalToTrash() async {
        guard state == .sourceCleanupRequired else { return }
        do {
            let action = try relocator.prepareSourceCleanup(
                disposition: .trash,
                currentBundleURL: relocator.sourceBundleURL
            )
            await performSourceCleanup(action)
        } catch let failure as ApplicationRelocationFailure where failure == .sourceChanged {
            state = .failed(.sourceChanged)
            record(stage: .sourceCleanupPending, result: "source_changed", scope: activeJournal?.destinationScope)
        } catch {
            enterRecovery(result: "source_cleanup_prepare_failed", scope: activeJournal?.destinationScope)
        }
    }

    func retryHandoff() {
        guard state == .sourceExitRequired || state == .backupCleanupFailed else { return }
        state = .waitingForDestination
        Task { await continueHandoff() }
    }

    private func prepare(scope: ApplicationDestinationScope, allowReplacement: Bool) {
        guard let witness = processObserver.currentProcessWitness(
            bundleAt: relocator.sourceBundleURL
        ) else {
            state = .failed(.sourceProcessInvalid)
            record(
                stage: .prepared,
                result: ApplicationRelocationFailure.sourceProcessInvalid.rawValue,
                scope: scope
            )
            return
        }
        pendingScope = scope
        state = .preparing
        let relocator = self.relocator
        Task {
            let result: Result<PreparedApplicationRelocation, Error> = await Task.detached {
                Result {
                    try relocator.prepare(
                        scope: scope,
                        allowReplacement: allowReplacement,
                        sourceProcessWitness: witness
                    )
                }
            }.value
            handle(result)
        }
    }

    private func handle(_ result: Result<PreparedApplicationRelocation, Error>) {
        switch result {
        case let .success(.replacementRequired(scope)):
            state = .replacementConfirmationRequired(scope)
        case .success(.fallbackRequired):
            state = .fallbackConfirmationRequired
        case let .success(.launch(journal)):
            activeJournal = journal
            state = .waitingForDestination
            record(stage: .swapped, result: "launch_requested", scope: journal.destinationScope)
            Task { await launch(journal) }
        case let .failure(error):
            let failure = error as? ApplicationRelocationFailure ?? .copyFailed
            if failure == .previewReadOnly {
                state = .preview
            } else {
                state = .failed(failure)
            }
            record(stage: .prepared, result: failure.rawValue, scope: pendingScope)
        }
    }

    private func launch(_ journal: ApplicationRelocationJournal) async {
        let destination = URL(fileURLWithPath: journal.destinationPath, isDirectory: true)
        guard await opener.openApplication(at: destination, nonce: journal.nonce) else {
            handleLaunchFailure(.launchFailed, journal: journal)
            return
        }
        for _ in 0..<50 {
            try? await Task.sleep(for: .milliseconds(200))
            if let observed = try? relocator.currentJournal(),
               observed.nonce == journal.nonce,
               [.destinationStarted, .sourceExited, .backupCleanupPending,
                .cleanupPending, .sourceCleanupPending].contains(observed.stage) {
                NSApplication.shared.terminate(nil)
                return
            }
        }
        handleLaunchFailure(.launchTimedOut, journal: journal)
    }

    private func handleLaunchFailure(
        _ failure: ApplicationRelocationFailure,
        journal: ApplicationRelocationJournal
    ) {
        do {
            if try relocator.recoverBeforeDestinationStart(expectedNonce: journal.nonce) {
                state = .failed(failure)
                record(stage: .swapped, result: failure.rawValue, scope: journal.destinationScope)
            } else {
                NSApplication.shared.terminate(nil)
            }
        } catch {
            enterRecovery(result: "rollback_failed", scope: journal.destinationScope)
        }
    }

    private func restoreInitialState(resumesDestinationLaunch: Bool) {
        guard runtimeMode == .managed else {
            state = .preview
            return
        }
        if resumesDestinationLaunch {
            state = .waitingForDestination
            return
        }
        guard relocator.hasJournal else {
            handoffVerified = relocator.isRunningFromStandardLocation
            state = relocator.isRunningFromStandardLocation ? .unavailable : .available
            return
        }
        do {
            guard let journal = try relocator.currentJournal() else {
                throw ApplicationRelocationFailure.journalInvalid
            }
            activeJournal = journal
            if journal.schema == 1 {
                restoreLegacyJournal(journal)
                return
            }
            switch journal.stage {
            case .prepared, .staged, .swapped:
                guard !relocator.isRunningFromStandardLocation else {
                    enterRecovery(result: "prestart_destination_restart", scope: journal.destinationScope)
                    return
                }
                _ = try relocator.recoverBeforeDestinationStart(expectedNonce: journal.nonce)
                activeJournal = nil
                state = .available
            case .destinationStarted, .sourceExited, .backupCleanupPending:
                try requireStandardDestination(journal)
                state = .waitingForDestination
                Task { await continueHandoff() }
            case .cleanupPending:
                try requireStandardDestination(journal)
                markHandoffVerified(journal)
            case .sourceCleanupPending:
                try requireStandardDestination(journal)
                handoffVerified = true
                state = .sourceCleanupRequired
                Task { await resumeSourceCleanup() }
            case .recoveryRequired:
                state = .recoveryRequired
            }
        } catch {
            enterRecovery(result: "journal_resume_failed", scope: activeJournal?.destinationScope)
        }
    }

    private func restoreLegacyJournal(_ journal: ApplicationRelocationJournal) {
        switch journal.stage {
        case .prepared, .staged, .swapped:
            if !relocator.isRunningFromStandardLocation {
                do {
                    _ = try relocator.recoverBeforeDestinationStart(expectedNonce: journal.nonce)
                    activeJournal = nil
                    state = .available
                } catch {
                    enterRecovery(result: "legacy_rollback_failed", scope: journal.destinationScope)
                }
            } else {
                enterRecovery(result: "legacy_poststart_unverifiable", scope: journal.destinationScope)
            }
        case .destinationStarted, .sourceExited, .backupCleanupPending, .cleanupPending,
             .sourceCleanupPending, .recoveryRequired:
            enterRecovery(result: "legacy_poststart_unverifiable", scope: journal.destinationScope)
        }
    }

    private func requireStandardDestination(
        _ journal: ApplicationRelocationJournal
    ) throws {
        guard relocator.isRunningFromStandardLocation else {
            throw ApplicationRelocationFailure.journalInvalid
        }
        activeJournal = try relocator.resumeDestinationJournal(
            currentBundleURL: relocator.sourceBundleURL
        )
        guard activeJournal?.nonce == journal.nonce else {
            throw ApplicationRelocationFailure.journalInvalid
        }
    }

    private func continueHandoff() async {
        do {
            guard var journal = try relocator.currentJournal(),
                  journal.schema == ApplicationRelocationJournal.schemaVersion,
                  let witness = journal.sourceProcessWitness else {
                throw ApplicationRelocationFailure.journalInvalid
            }
            try requireStandardDestination(journal)
            if journal.stage == .destinationStarted {
                for attempt in 0..<sourceExitCheckCount {
                    if !processObserver.isProcessRunning(witness) { break }
                    if attempt + 1 < sourceExitCheckCount {
                        try? await Task.sleep(for: sourceExitCheckInterval)
                    }
                }
                guard !processObserver.isProcessRunning(witness) else {
                    state = .sourceExitRequired
                    handoffVerified = false
                    record(
                        stage: .destinationStarted,
                        result: ApplicationRelocationFailure.sourceExitTimedOut.rawValue,
                        scope: journal.destinationScope
                    )
                    return
                }
                journal = try relocator.markSourceExited(
                    nonce: journal.nonce,
                    currentBundleURL: relocator.sourceBundleURL
                )
                activeJournal = journal
                record(stage: .sourceExited, result: "source_exited", scope: journal.destinationScope)
            }
            if journal.stage == .sourceExited || journal.stage == .backupCleanupPending {
                let cleanup = try relocator.prepareBackupCleanup(
                    nonce: journal.nonce,
                    currentBundleURL: relocator.sourceBundleURL
                )
                switch cleanup {
                case let .complete(completed):
                    markHandoffVerified(completed)
                case let .trash(backup, pending):
                    activeJournal = pending
                    guard await trashMover.moveToTrash(backup) else {
                        state = .backupCleanupFailed
                        handoffVerified = false
                        record(
                            stage: .backupCleanupPending,
                            result: ApplicationRelocationFailure.backupCleanupFailed.rawValue,
                            scope: pending.destinationScope
                        )
                        return
                    }
                    let completed = try relocator.completeBackupCleanup(
                        nonce: pending.nonce,
                        currentBundleURL: relocator.sourceBundleURL
                    )
                    markHandoffVerified(completed)
                }
            } else if journal.stage == .cleanupPending {
                markHandoffVerified(journal)
            }
        } catch {
            enterRecovery(result: "handoff_resume_failed", scope: activeJournal?.destinationScope)
        }
    }

    private func markHandoffVerified(_ journal: ApplicationRelocationJournal) {
        activeJournal = journal
        handoffVerified = true
        state = .sourceCleanupRequired
        record(stage: .cleanupPending, result: "handoff_verified", scope: journal.destinationScope)
    }

    private func resumeSourceCleanup() async {
        do {
            let action = try relocator.resumeSourceCleanup(
                currentBundleURL: relocator.sourceBundleURL
            )
            await performSourceCleanup(action)
        } catch let failure as ApplicationRelocationFailure where failure == .sourceChanged {
            state = .failed(.sourceChanged)
            record(stage: .sourceCleanupPending, result: "source_changed", scope: activeJournal?.destinationScope)
        } catch {
            enterRecovery(result: "source_cleanup_resume_failed", scope: activeJournal?.destinationScope)
        }
    }

    private func performSourceCleanup(_ action: PreparedApplicationSourceCleanup) async {
        switch action {
        case let .complete(journal):
            do {
                try relocator.finishSourceCleanup(
                    nonce: journal.nonce,
                    currentBundleURL: relocator.sourceBundleURL
                )
                finishSourceDisposition(journal: journal, result: "source_kept")
            } catch {
                enterRecovery(result: "source_cleanup_finish_failed", scope: journal.destinationScope)
            }
        case let .trash(source, journal):
            guard await trashMover.moveToTrash(source) else {
                state = .failed(.trashFailed)
                record(
                    stage: .sourceCleanupPending,
                    result: "source_trash_failed",
                    scope: journal.destinationScope
                )
                return
            }
            do {
                try relocator.finishSourceCleanup(
                    nonce: journal.nonce,
                    currentBundleURL: relocator.sourceBundleURL
                )
                finishSourceDisposition(journal: journal, result: "source_trashed")
            } catch {
                enterRecovery(result: "source_cleanup_finish_failed", scope: journal.destinationScope)
            }
        }
    }

    private func finishSourceDisposition(
        journal: ApplicationRelocationJournal,
        result: String
    ) {
        handoffVerified = true
        state = .completed
        record(stage: .sourceCleanupPending, result: result, scope: journal.destinationScope)
        activeJournal = nil
    }

    private func enterRecovery(result: String, scope: ApplicationDestinationScope?) {
        relocator.markRecoveryRequired()
        handoffVerified = false
        state = .recoveryRequired
        record(stage: .recoveryRequired, result: result, scope: scope)
    }

    private func record(
        stage: ApplicationRelocationStage,
        result: String,
        scope: ApplicationDestinationScope?
    ) {
        var fields = ["result_code": result, "phase": stage.rawValue]
        if let scope { fields["destination"] = scope.rawValue }
        activityLog?.record(category: .operation, code: "application_relocation", fields: fields)
    }
}
