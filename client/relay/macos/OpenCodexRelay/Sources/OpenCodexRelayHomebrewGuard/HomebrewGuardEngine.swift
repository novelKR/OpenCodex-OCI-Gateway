import Darwin
import Foundation

public struct HomebrewGuardEngineConfiguration: Sendable {
    public let allowedRoot: String
    public let journalURL: URL
    public let lockURL: URL
    public let distribution: HomebrewGuardDistribution
    public let helperVersion: String
    public let requireRoot: Bool

    public init(
        allowedRoot: String = "/opt/homebrew",
        journalURL: URL = URL(fileURLWithPath: "/var/db/io.github.novelkr.opencodex-relay/homebrew-guard.json"),
        lockURL: URL = URL(fileURLWithPath: "/var/db/io.github.novelkr.opencodex-relay/homebrew-guard.lock"),
        distribution: HomebrewGuardDistribution,
        helperVersion: String,
        requireRoot: Bool = true
    ) {
        self.allowedRoot = URL(fileURLWithPath: allowedRoot).standardizedFileURL.path
        self.journalURL = journalURL.standardizedFileURL
        self.lockURL = lockURL.standardizedFileURL
        self.distribution = distribution
        self.helperVersion = helperVersion
        self.requireRoot = requireRoot
    }
}

public final class HomebrewGuardEngine: @unchecked Sendable {
    private enum JournalPhase: String, Codable {
        case prepared
        case committed
    }

    private struct JournalEntry: Codable, Equatable {
        let relativePath: String
        let originalMode: UInt16
        let device: UInt64
        let inode: UInt64
        let removableByPackageOperation: Bool?

        enum CodingKeys: String, CodingKey {
            case relativePath = "relative_path"
            case originalMode = "original_mode"
            case device
            case inode
            case removableByPackageOperation = "removable_by_package_operation"
        }
    }

    private struct Journal: Codable {
        let schemaVersion: Int
        let distribution: HomebrewGuardDistribution
        let operationID: String
        let installationID: String
        let installationFingerprint: String
        let ownerUID: UInt32
        var phase: JournalPhase
        let entries: [JournalEntry]

        enum CodingKeys: String, CodingKey {
            case schemaVersion = "schema_version"
            case distribution
            case operationID = "operation_id"
            case installationID = "installation_id"
            case installationFingerprint = "installation_fingerprint"
            case ownerUID = "owner_uid"
            case phase
            case entries
        }
    }

    private let configuration: HomebrewGuardEngineConfiguration
    private let stateLock = NSLock()
    private var systemLockFD: Int32 = -1
    private var activeOperationID: String?
    private var activeLeaseID: UUID?

    public init(configuration: HomebrewGuardEngineConfiguration) {
        self.configuration = configuration
    }

    deinit {
        releaseSystemLock()
    }

    public func perform(
        operation: HomebrewGuardOperation,
        requestData: Data,
        peerUID: uid_t,
        leaseID: UUID? = nil
    ) -> Data {
        stateLock.lock()
        defer { stateLock.unlock() }
        do {
            try validateRuntimeAuthority(peerUID: peerUID)
            let request = try HomebrewGuardCodec.decodeRequest(
                requestData,
                operation: operation,
                allowedRoot: configuration.allowedRoot
            )
            guard request.distribution == configuration.distribution else {
                throw HomebrewGuardErrorCode.candidateChanged
            }
            let response: HomebrewGuardResponse
            switch operation {
            case .status:
                response = try status(request: request, peerUID: peerUID)
            case .prepare:
                response = try prepare(request: request, peerUID: peerUID, leaseID: leaseID)
            case .commit:
                response = try commit(request: request, peerUID: peerUID, leaseID: leaseID)
            case .release:
                response = try release(
                    request: request,
                    peerUID: peerUID,
                    leaseID: leaseID,
                    recovery: false
                )
            case .recover:
                response = try release(
                    request: request,
                    peerUID: peerUID,
                    leaseID: leaseID,
                    recovery: true
                )
            }
            return try HomebrewGuardCodec.encode(response)
        } catch let code as HomebrewGuardErrorCode {
            return failureResponse(code, operationID: safeOperationID(from: requestData))
        } catch {
            let code: HomebrewGuardErrorCode = operation == .release || operation == .recover
                ? .restoreFailed
                : .protectionFailed
            return failureResponse(code, operationID: safeOperationID(from: requestData))
        }
    }

    public func connectionInvalidated(leaseID: UUID, peerUID: uid_t) {
        stateLock.lock()
        defer { stateLock.unlock() }
        guard activeLeaseID == leaseID,
              let operationID = activeOperationID else {
            return
        }
        activeLeaseID = nil
        do {
            guard let journal = try loadJournal() else {
                activeOperationID = nil
                releaseSystemLock()
                return
            }
            guard journal.operationID == operationID,
                  journal.distribution == configuration.distribution,
                  journal.ownerUID == UInt32(peerUID) else {
                return
            }
            guard journal.phase == .prepared else {
                return
            }
            try restore(journal)
            try removeJournal()
            activeOperationID = nil
            releaseSystemLock()
        } catch {
            // Keep the journal and process-wide lock fail-closed. A later status
            // request has no live lease and will retry restoration.
        }
    }

    private func status(
        request: HomebrewGuardRequest,
        peerUID: uid_t
    ) throws -> HomebrewGuardResponse {
        try ensureStateDirectory()
        if let journal = try loadJournal() {
            try acquireSystemLock()
            let leaseIsActive = hasActiveLease(for: journal)
            guard journal.distribution == configuration.distribution,
                  journal.ownerUID == UInt32(peerUID) else {
                if !leaseIsActive {
                    releaseSystemLock()
                }
                throw journal.phase == .committed
                    ? HomebrewGuardErrorCode.recoveryRequired
                    : HomebrewGuardErrorCode.busy
            }
            if journal.phase == .prepared && !leaseIsActive {
                do {
                    try restore(journal)
                    try removeJournal()
                    activeOperationID = nil
                    activeLeaseID = nil
                    releaseSystemLock()
                } catch {
                    activeOperationID = journal.operationID
                    activeLeaseID = nil
                    throw HomebrewGuardErrorCode.restoreFailed
                }
            } else {
                activeOperationID = journal.operationID
                let state: HomebrewGuardState = journal.phase == .committed ? .recoveryRequired : .prepared
                return response(
                    state: state,
                    result: .statusReady,
                    error: journal.phase == .committed ? .recoveryRequired : nil,
                    operationID: journal.operationID
                )
            }
        }
        if activeLeaseID == nil {
            activeOperationID = nil
            releaseSystemLock()
        }
        if let candidate = request.candidate {
            _ = try inspect(candidate: candidate, peerUID: peerUID)
            return response(state: .ready, result: .candidateReady)
        }
        return response(state: .ready, result: .statusReady)
    }

    private func prepare(
        request: HomebrewGuardRequest,
        peerUID: uid_t,
        leaseID: UUID?
    ) throws -> HomebrewGuardResponse {
        guard let operationID = request.operationID,
              let candidate = request.candidate,
              let leaseID else {
            throw HomebrewGuardErrorCode.candidateChanged
        }
        try ensureStateDirectory()
        try acquireSystemLock()
        var retainsSystemLock = false
        defer {
            if !retainsSystemLock {
                releaseSystemLock()
            }
        }
        if let existing = try loadJournal() {
            activeOperationID = existing.operationID
            retainsSystemLock = true
            if existing.phase == .committed {
                throw HomebrewGuardErrorCode.recoveryRequired
            }
            throw HomebrewGuardErrorCode.busy
        }
        let entries = try inspect(candidate: candidate, peerUID: peerUID)
        let journal = Journal(
            schemaVersion: 1,
            distribution: request.distribution,
            operationID: operationID,
            installationID: candidate.installationID,
            installationFingerprint: candidate.installationFingerprint,
            ownerUID: UInt32(peerUID),
            phase: .prepared,
            entries: entries
        )
        do {
            try writeJournal(journal)
        } catch {
            do {
                try removeFailedPreparedJournalIfPresent(journal)
            } catch {
                activeOperationID = journal.operationID
                activeLeaseID = nil
                retainsSystemLock = true
                throw HomebrewGuardErrorCode.restoreFailed
            }
            throw error
        }
        activeOperationID = operationID
        activeLeaseID = leaseID
        retainsSystemLock = true
        do {
            for entry in entries {
                try setProtectedMode(entry)
            }
        } catch {
            do {
                try restore(journal)
                try removeJournal()
                activeOperationID = nil
                activeLeaseID = nil
                retainsSystemLock = false
            } catch {
                throw HomebrewGuardErrorCode.restoreFailed
            }
            throw HomebrewGuardErrorCode.protectionFailed
        }
        return response(state: .prepared, result: .prepared, operationID: operationID)
    }

    private func commit(
        request: HomebrewGuardRequest,
        peerUID: uid_t,
        leaseID: UUID?
    ) throws -> HomebrewGuardResponse {
        guard let operationID = request.operationID,
              let leaseID,
              activeOperationID == operationID,
              activeLeaseID == leaseID else {
            throw HomebrewGuardErrorCode.busy
        }
        let journal = try matchingJournal(request, peerUID: peerUID)
        guard journal.phase == .prepared else {
            throw HomebrewGuardErrorCode.recoveryRequired
        }
        for entry in journal.entries {
            try verifyProtectedEntry(entry)
        }
        var committed = journal
        committed.phase = .committed
        try writeJournal(committed)
        activeOperationID = committed.operationID
        return response(state: .committed, result: .committed, operationID: committed.operationID)
    }

    private func release(
        request: HomebrewGuardRequest,
        peerUID: uid_t,
        leaseID: UUID?,
        recovery: Bool
    ) throws -> HomebrewGuardResponse {
        if !recovery {
            guard let operationID = request.operationID,
                  let leaseID,
                  activeOperationID == operationID,
                  activeLeaseID == leaseID else {
                throw HomebrewGuardErrorCode.busy
            }
        }
        let journal = try matchingJournal(request, peerUID: peerUID)
        if recovery && journal.phase != .committed {
            throw HomebrewGuardErrorCode.busy
        }
        do {
            try restore(journal)
            try removeJournal()
        } catch {
            activeOperationID = journal.operationID
            throw HomebrewGuardErrorCode.restoreFailed
        }
        activeOperationID = nil
        activeLeaseID = nil
        releaseSystemLock()
        return response(
            state: .ready,
            result: recovery ? .recovered : .released,
            operationID: journal.operationID
        )
    }

    private func matchingJournal(
        _ request: HomebrewGuardRequest,
        peerUID: uid_t
    ) throws -> Journal {
        try ensureStateDirectory()
        try acquireSystemLock()
        guard let operationID = request.operationID,
              let journal = try loadJournal() else {
            releaseSystemLock()
            throw HomebrewGuardErrorCode.candidateChanged
        }
        guard journal.operationID == operationID,
              journal.distribution == request.distribution,
              journal.ownerUID == UInt32(peerUID) else {
            if !hasActiveLease(for: journal) {
                releaseSystemLock()
            }
            throw journal.phase == .committed
                ? HomebrewGuardErrorCode.recoveryRequired
                : HomebrewGuardErrorCode.busy
        }
        activeOperationID = operationID
        return journal
    }

    private func hasActiveLease(for journal: Journal) -> Bool {
        activeOperationID == journal.operationID && activeLeaseID != nil
    }

    private func removeFailedPreparedJournalIfPresent(_ expected: Journal) throws {
        guard let persisted = try loadJournal() else {
            return
        }
        guard persisted.schemaVersion == expected.schemaVersion,
              persisted.distribution == expected.distribution,
              persisted.operationID == expected.operationID,
              persisted.installationID == expected.installationID,
              persisted.installationFingerprint == expected.installationFingerprint,
              persisted.ownerUID == expected.ownerUID,
              persisted.phase == .prepared,
              persisted.entries == expected.entries else {
            throw HomebrewGuardErrorCode.restoreFailed
        }
        try removeJournal()
    }

    private func inspect(candidate: HomebrewGuardCandidate, peerUID: uid_t) throws -> [JournalEntry] {
        _ = try candidate.validated(allowedRoot: configuration.allowedRoot)
        var directories = Set<String>()
        let directorySeeds = [candidate.packageRoot] + candidate.criticalFiles.map {
            URL(fileURLWithPath: $0).deletingLastPathComponent().path
        }
        for seed in directorySeeds {
            var current = seed
            while true {
                guard isContained(current) else { throw HomebrewGuardErrorCode.candidateChanged }
                directories.insert(current)
                if current == configuration.allowedRoot { break }
                let parent = URL(fileURLWithPath: current).deletingLastPathComponent().path
                guard parent != current else { throw HomebrewGuardErrorCode.candidateChanged }
                current = parent
            }
        }

        var entries: [JournalEntry] = []
        for path in directories.sorted() {
            let descriptor = try openDirectory(path, peerUID: peerUID)
            defer { Darwin.close(descriptor) }
            var info = stat()
            guard fstat(descriptor, &info) == 0 else { throw HomebrewGuardErrorCode.candidateChanged }
            let mode = info.st_mode & mode_t(0o7777)
            guard mode & mode_t(0o002) == 0 else { throw HomebrewGuardErrorCode.protectionFailed }
            if mode & mode_t(0o020) != 0 {
                entries.append(JournalEntry(
                    relativePath: relativePath(path),
                    originalMode: UInt16(mode),
                    device: UInt64(info.st_dev),
                    inode: UInt64(info.st_ino),
                    removableByPackageOperation: path == candidate.packageRoot ||
                        path.hasPrefix(candidate.packageRoot + "/")
                ))
            }
        }
        for path in candidate.criticalFiles {
            try verifyCriticalFile(path, peerUID: peerUID)
        }
        return entries.sorted { $0.relativePath < $1.relativePath }
    }

    private func openDirectory(_ path: String, peerUID: uid_t) throws -> Int32 {
        guard isContained(path) else { throw HomebrewGuardErrorCode.candidateChanged }
        var descriptor = Darwin.open(
            configuration.allowedRoot,
            O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC
        )
        guard descriptor >= 0 else { throw HomebrewGuardErrorCode.candidateChanged }
        do {
            try verifyDirectory(descriptor, path: configuration.allowedRoot, peerUID: peerUID)
            let relative = relativePath(path)
            if !relative.isEmpty {
                var traversedPath = configuration.allowedRoot
                for component in relative.split(separator: "/") {
                    let next = component.withCString {
                        openat(descriptor, $0, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
                    }
                    guard next >= 0 else { throw HomebrewGuardErrorCode.candidateChanged }
                    Darwin.close(descriptor)
                    descriptor = next
                    traversedPath += "/" + component
                    try verifyDirectory(descriptor, path: traversedPath, peerUID: peerUID)
                }
            }
            return descriptor
        } catch {
            Darwin.close(descriptor)
            throw error
        }
    }

    private func verifyDirectory(_ descriptor: Int32, path: String, peerUID: uid_t) throws {
        var info = stat()
        guard fstat(descriptor, &info) == 0,
              info.st_uid == peerUID,
              (info.st_mode & S_IFMT) == S_IFDIR,
              info.st_mode & mode_t(0o002) == 0,
              try !hasExtendedACL(path) else {
            throw HomebrewGuardErrorCode.protectionFailed
        }
    }

    private func verifyCriticalFile(_ path: String, peerUID: uid_t) throws {
        guard isContained(path) else { throw HomebrewGuardErrorCode.candidateChanged }
        let parent = URL(fileURLWithPath: path).deletingLastPathComponent().path
        let name = URL(fileURLWithPath: path).lastPathComponent
        let parentDescriptor = try openDirectory(parent, peerUID: peerUID)
        defer { Darwin.close(parentDescriptor) }
        let descriptor = name.withCString {
            openat(parentDescriptor, $0, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        }
        guard descriptor >= 0 else { throw HomebrewGuardErrorCode.candidateChanged }
        defer { Darwin.close(descriptor) }
        var info = stat()
        guard fstat(descriptor, &info) == 0,
              info.st_uid == peerUID,
              (info.st_mode & S_IFMT) == S_IFREG,
              info.st_mode & mode_t(0o022) == 0,
              try !hasExtendedACL(path) else {
            throw HomebrewGuardErrorCode.protectionFailed
        }
    }

    private func hasExtendedACL(_ path: String) throws -> Bool {
        errno = 0
        guard let acl = acl_get_file(path, ACL_TYPE_EXTENDED) else {
            if errno == ENOENT { return false }
            throw HomebrewGuardErrorCode.protectionFailed
        }
        defer { acl_free(UnsafeMutableRawPointer(acl)) }
        var entry: acl_entry_t?
        let result = acl_get_entry(acl, Int32(ACL_FIRST_ENTRY.rawValue), &entry)
        guard result >= 0 else {
            throw HomebrewGuardErrorCode.protectionFailed
        }
        return entry != nil
    }

    private func setProtectedMode(_ entry: JournalEntry) throws {
        let descriptor = try openJournalEntry(entry)
        defer { Darwin.close(descriptor) }
        let protectedMode = mode_t(entry.originalMode) & ~mode_t(0o020)
        guard fchmod(descriptor, protectedMode) == 0 else {
            throw HomebrewGuardErrorCode.protectionFailed
        }
        try verifyEntryIdentity(descriptor, entry: entry, allowedModes: [protectedMode])
    }

    private func verifyProtectedEntry(_ entry: JournalEntry) throws {
        let descriptor = try openJournalEntry(entry)
        defer { Darwin.close(descriptor) }
        let protectedMode = mode_t(entry.originalMode) & ~mode_t(0o020)
        try verifyEntryIdentity(descriptor, entry: entry, allowedModes: [protectedMode])
    }

    private func restore(_ journal: Journal) throws {
        for entry in journal.entries.reversed() {
            let descriptor = try openJournalEntry(
                entry,
                allowMissing: journal.phase == .committed &&
                    entry.removableByPackageOperation == true
            )
            guard let descriptor else { continue }
            defer { Darwin.close(descriptor) }
            let originalMode = mode_t(entry.originalMode)
            let protectedMode = originalMode & ~mode_t(0o020)
            try verifyEntryIdentity(
                descriptor,
                entry: entry,
                allowedModes: [originalMode, protectedMode]
            )
            guard fchmod(descriptor, originalMode) == 0 else {
                throw HomebrewGuardErrorCode.restoreFailed
            }
            try verifyEntryIdentity(descriptor, entry: entry, allowedModes: [originalMode])
        }
    }

    private func openJournalEntry(_ entry: JournalEntry) throws -> Int32 {
        guard let descriptor = try openJournalEntry(entry, allowMissing: false) else {
            throw HomebrewGuardErrorCode.restoreFailed
        }
        return descriptor
    }

    private func openJournalEntry(
        _ entry: JournalEntry,
        allowMissing: Bool
    ) throws -> Int32? {
        let path = entry.relativePath.isEmpty
            ? configuration.allowedRoot
            : configuration.allowedRoot + "/" + entry.relativePath
        return try openDirectoryForRestore(path, allowMissing: allowMissing)
    }

    private func openDirectoryForRestore(
        _ path: String,
        allowMissing: Bool
    ) throws -> Int32? {
        guard isContained(path) else { throw HomebrewGuardErrorCode.restoreFailed }
        var descriptor = Darwin.open(
            configuration.allowedRoot,
            O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC
        )
        guard descriptor >= 0 else { throw HomebrewGuardErrorCode.restoreFailed }
        do {
            let relative = relativePath(path)
            if !relative.isEmpty {
                for component in relative.split(separator: "/") {
                    let next = component.withCString {
                        openat(descriptor, $0, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
                    }
                    if next < 0, errno == ENOENT, allowMissing {
                        Darwin.close(descriptor)
                        return nil
                    }
                    guard next >= 0 else { throw HomebrewGuardErrorCode.restoreFailed }
                    Darwin.close(descriptor)
                    descriptor = next
                }
            }
            return descriptor
        } catch {
            Darwin.close(descriptor)
            throw error
        }
    }

    private func verifyEntryIdentity(
        _ descriptor: Int32,
        entry: JournalEntry,
        allowedModes: Set<mode_t>
    ) throws {
        var info = stat()
        guard fstat(descriptor, &info) == 0,
              (info.st_mode & S_IFMT) == S_IFDIR,
              UInt64(info.st_dev) == entry.device,
              UInt64(info.st_ino) == entry.inode,
              allowedModes.contains(info.st_mode & mode_t(0o7777)) else {
            throw HomebrewGuardErrorCode.restoreFailed
        }
    }

    private func validateRuntimeAuthority(peerUID: uid_t) throws {
        guard peerUID != 0 else { throw HomebrewGuardErrorCode.protectionFailed }
        if configuration.requireRoot && geteuid() != 0 {
            throw HomebrewGuardErrorCode.protectionFailed
        }
        guard configuration.allowedRoot == "/opt/homebrew" || !configuration.requireRoot,
              configuration.journalURL.isFileURL,
              configuration.lockURL.isFileURL,
              configuration.journalURL.deletingLastPathComponent() == configuration.lockURL.deletingLastPathComponent() else {
            throw HomebrewGuardErrorCode.protectionFailed
        }
    }

    private func ensureStateDirectory() throws {
        let directory = configuration.journalURL.deletingLastPathComponent().path
        var info = stat()
        if lstat(directory, &info) != 0 {
            guard errno == ENOENT, mkdir(directory, mode_t(0o700)) == 0 else {
                throw HomebrewGuardErrorCode.protectionFailed
            }
            guard lstat(directory, &info) == 0 else {
                throw HomebrewGuardErrorCode.protectionFailed
            }
        }
        let expectedUID: uid_t = configuration.requireRoot ? 0 : geteuid()
        guard (info.st_mode & S_IFMT) == S_IFDIR,
              info.st_uid == expectedUID,
              info.st_mode & mode_t(0o077) == 0 else {
            throw HomebrewGuardErrorCode.protectionFailed
        }
    }

    private func acquireSystemLock() throws {
        if systemLockFD >= 0 { return }
        let descriptor = Darwin.open(
            configuration.lockURL.path,
            O_CREAT | O_RDWR | O_NOFOLLOW | O_CLOEXEC,
            mode_t(0o600)
        )
        guard descriptor >= 0 else { throw HomebrewGuardErrorCode.busy }
        var info = stat()
        let expectedUID: uid_t = configuration.requireRoot ? 0 : geteuid()
        guard fstat(descriptor, &info) == 0,
              (info.st_mode & S_IFMT) == S_IFREG,
              info.st_uid == expectedUID,
              info.st_mode & mode_t(0o077) == 0,
              flock(descriptor, LOCK_EX | LOCK_NB) == 0 else {
            Darwin.close(descriptor)
            throw HomebrewGuardErrorCode.busy
        }
        systemLockFD = descriptor
    }

    private func releaseSystemLock() {
        guard systemLockFD >= 0 else { return }
        _ = flock(systemLockFD, LOCK_UN)
        Darwin.close(systemLockFD)
        systemLockFD = -1
    }

    private func loadJournal() throws -> Journal? {
        let path = configuration.journalURL.path
        var linkInfo = stat()
        if lstat(path, &linkInfo) != 0 {
            if errno == ENOENT { return nil }
            throw HomebrewGuardErrorCode.recoveryRequired
        }
        let expectedUID: uid_t = configuration.requireRoot ? 0 : geteuid()
        guard (linkInfo.st_mode & S_IFMT) == S_IFREG,
              linkInfo.st_uid == expectedUID,
              linkInfo.st_mode & mode_t(0o077) == 0,
              linkInfo.st_size > 0,
              linkInfo.st_size <= off_t(homebrewGuardMaximumMessageBytes) else {
            throw HomebrewGuardErrorCode.recoveryRequired
        }
        let descriptor = Darwin.open(path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        guard descriptor >= 0 else { throw HomebrewGuardErrorCode.recoveryRequired }
        defer { Darwin.close(descriptor) }
        var openedInfo = stat()
        guard fstat(descriptor, &openedInfo) == 0,
              openedInfo.st_dev == linkInfo.st_dev,
              openedInfo.st_ino == linkInfo.st_ino else {
            throw HomebrewGuardErrorCode.recoveryRequired
        }
        var data = Data()
        var buffer = [UInt8](repeating: 0, count: 4_096)
        while true {
            let count = Darwin.read(descriptor, &buffer, buffer.count)
            guard count >= 0 else { throw HomebrewGuardErrorCode.recoveryRequired }
            if count == 0 { break }
            data.append(buffer, count: count)
            guard data.count <= homebrewGuardMaximumMessageBytes else {
                throw HomebrewGuardErrorCode.recoveryRequired
            }
        }
        let journal = try JSONDecoder().decode(Journal.self, from: data)
        guard journal.schemaVersion == 1,
              journal.ownerUID != 0,
              UUID(uuidString: journal.operationID)?.uuidString.lowercased() == journal.operationID,
              journal.entries.count <= 64,
              Set(journal.entries.map(\.relativePath)).count == journal.entries.count,
              journal.entries.allSatisfy({ validRelativePath($0.relativePath) }) else {
            throw HomebrewGuardErrorCode.recoveryRequired
        }
        return journal
    }

    private func writeJournal(_ journal: Journal) throws {
        let data = try JSONEncoder().encode(journal)
        guard !data.isEmpty, data.count <= homebrewGuardMaximumMessageBytes else {
            throw HomebrewGuardErrorCode.protectionFailed
        }
        let temporary = configuration.journalURL.path + ".tmp." + UUID().uuidString.lowercased()
        let descriptor = Darwin.open(
            temporary,
            O_CREAT | O_EXCL | O_WRONLY | O_NOFOLLOW | O_CLOEXEC,
            mode_t(0o600)
        )
        guard descriptor >= 0 else { throw HomebrewGuardErrorCode.protectionFailed }
        var succeeded = false
        defer {
            Darwin.close(descriptor)
            if !succeeded { _ = Darwin.unlink(temporary) }
        }
        try data.withUnsafeBytes { bytes in
            guard let base = bytes.baseAddress else { throw HomebrewGuardErrorCode.protectionFailed }
            var written = 0
            while written < data.count {
                let count = Darwin.write(descriptor, base.advanced(by: written), data.count - written)
                guard count > 0 else { throw HomebrewGuardErrorCode.protectionFailed }
                written += count
            }
        }
        guard fchmod(descriptor, mode_t(0o600)) == 0,
              fsync(descriptor) == 0,
              Darwin.rename(temporary, configuration.journalURL.path) == 0 else {
            throw HomebrewGuardErrorCode.protectionFailed
        }
        succeeded = true
        try syncStateDirectory()
    }

    private func removeJournal() throws {
        if Darwin.unlink(configuration.journalURL.path) != 0 && errno != ENOENT {
            throw HomebrewGuardErrorCode.restoreFailed
        }
        try syncStateDirectory()
    }

    private func syncStateDirectory() throws {
        let directory = configuration.journalURL.deletingLastPathComponent().path
        let descriptor = Darwin.open(directory, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
        guard descriptor >= 0 else { throw HomebrewGuardErrorCode.protectionFailed }
        defer { Darwin.close(descriptor) }
        guard fsync(descriptor) == 0 else { throw HomebrewGuardErrorCode.protectionFailed }
    }

    private func response(
        state: HomebrewGuardState,
        result: HomebrewGuardResultCode,
        error: HomebrewGuardErrorCode? = nil,
        operationID: String? = nil
    ) -> HomebrewGuardResponse {
        HomebrewGuardResponse(
            helperVersion: configuration.helperVersion,
            state: state,
            resultCode: error == nil ? result : .failed,
            errorCode: error,
            operationID: operationID
        )
    }

    private func failureResponse(_ code: HomebrewGuardErrorCode, operationID: String?) -> Data {
        let state: HomebrewGuardState = code == .recoveryRequired || code == .restoreFailed
            ? .recoveryRequired
            : .unavailable
        let response = HomebrewGuardResponse(
            helperVersion: configuration.helperVersion,
            state: state,
            resultCode: .failed,
            errorCode: code,
            operationID: operationID
        )
        return (try? HomebrewGuardCodec.encode(response)) ?? Data()
    }

    private func safeOperationID(from data: Data) -> String? {
        guard data.count <= homebrewGuardMaximumMessageBytes,
              let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let value = object["operation_id"] as? String,
              UUID(uuidString: value)?.uuidString.lowercased() == value else {
            return nil
        }
        return value
    }

    private func isContained(_ path: String) -> Bool {
        path == configuration.allowedRoot || path.hasPrefix(configuration.allowedRoot + "/")
    }

    private func relativePath(_ path: String) -> String {
        path == configuration.allowedRoot
            ? ""
            : String(path.dropFirst(configuration.allowedRoot.count + 1))
    }

    private func validRelativePath(_ value: String) -> Bool {
        if value.isEmpty { return true }
        guard !value.hasPrefix("/"), value.utf8.count <= 4_096 else { return false }
        return value.split(separator: "/").allSatisfy { $0 != "." && $0 != ".." && !$0.isEmpty }
    }
}
