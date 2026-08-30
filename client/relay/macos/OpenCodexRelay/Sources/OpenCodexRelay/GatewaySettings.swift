import Darwin
import Foundation
import Security
import OpenCodexRelayCore

enum GatewayCredentialKind: String, CaseIterable, Codable, Hashable, Sendable {
    case cloudflareClientID = "cloudflare_access_client_id"
    case cloudflareClientSecret = "cloudflare_access_client_secret"
    case gatewayAPIKey = "gateway_api_key"

    var service: String {
        switch self {
        case .cloudflareClientID:
            "opencodex-relay-cf-access-client-id"
        case .cloudflareClientSecret:
            "opencodex-relay-cf-access-client-secret"
        case .gatewayAPIKey:
            "opencodex-relay-gateway-api-key"
        }
    }
}

struct GatewayCredentialMetadata: Equatable, Sendable {
    let configured: Bool
    let modifiedAt: Date?
}

enum GatewayCredentialMetadataState: Equatable {
    case idle
    case loading
    case ready
    case failed
}

enum GatewayCredentialStoreError: Error, Equatable {
    case invalidValue
    case accessControlUnavailable
    case keychainFailure
    case verificationFailed
    case lifecycleConflict
    case lifecycleUnsafe
}

struct GatewayCredentialLifecycleGate: Sendable {
    private static let lifecycleDirectoryName = "OpenCodexRelayLifecycle"
    private static let lockName = "lifecycle.lock"
    private static let standaloneJournalNames = [
        "standalone-native",
        "standalone-native.open-codex-removal.json",
    ]
    private static let sourceInstallReservationName = ".source-install-reservation.json"

    let homeDirectory: String

    init(homeDirectory: String = FileManager.default.homeDirectoryForCurrentUser.path) {
        self.homeDirectory = homeDirectory
    }

    func withWriteAdmission<T>(_ body: () throws -> T) throws -> T {
        let boundary = try boundaryPaths()
        try ensureLifecycleDirectory(boundary.lifecycleDirectory)
        let descriptor = try acquireLock(boundary.lock)
        defer {
            _ = flock(descriptor, LOCK_UN)
            Darwin.close(descriptor)
        }
        for journal in boundary.standaloneJournals {
            try requireAbsent(journal)
        }
        for reservation in boundary.sourceInstallReservations {
            try requireAbsent(reservation)
        }
        return try body()
    }

    private func boundaryPaths() throws -> (
        lifecycleDirectory: String,
        lock: String,
        standaloneJournals: [String],
        sourceInstallReservations: [String]
    ) {
        guard homeDirectory.hasPrefix("/"), !homeDirectory.contains("\0") else {
            throw GatewayCredentialStoreError.lifecycleUnsafe
        }
        let home = URL(fileURLWithPath: homeDirectory, isDirectory: true)
            .resolvingSymlinksInPath()
            .standardizedFileURL.path
        guard home.hasPrefix("/") else {
            throw GatewayCredentialStoreError.lifecycleUnsafe
        }
        var homeInfo = stat()
        guard Darwin.lstat(home, &homeInfo) == 0,
              (homeInfo.st_mode & S_IFMT) == S_IFDIR else {
            throw GatewayCredentialStoreError.lifecycleUnsafe
        }
        let lifecycleDirectory = URL(fileURLWithPath: home, isDirectory: true)
            .appendingPathComponent("Library", isDirectory: true)
            .appendingPathComponent("Application Support", isDirectory: true)
            .appendingPathComponent(Self.lifecycleDirectoryName, isDirectory: true)
            .path
        let installRoot = URL(fileURLWithPath: home, isDirectory: true)
            .appendingPathComponent(".local", isDirectory: true)
            .appendingPathComponent("lib", isDirectory: true)
            .appendingPathComponent("opencodex-relay", isDirectory: true)
        return (
            lifecycleDirectory,
            URL(fileURLWithPath: lifecycleDirectory, isDirectory: true)
                .appendingPathComponent(Self.lockName, isDirectory: false).path,
            Self.standaloneJournalNames.map {
                URL(fileURLWithPath: lifecycleDirectory, isDirectory: true)
                    .appendingPathComponent($0, isDirectory: false).path
            },
            ["relay", "relay-dev"].map {
                installRoot.appendingPathComponent($0, isDirectory: true)
                    .appendingPathComponent(Self.sourceInstallReservationName, isDirectory: false)
                    .path
            }
        )
    }

    private func ensureLifecycleDirectory(_ path: String) throws {
        let parent = URL(fileURLWithPath: path, isDirectory: true).deletingLastPathComponent()
        do {
            try FileManager.default.createDirectory(
                at: parent,
                withIntermediateDirectories: true,
                attributes: [.posixPermissions: 0o700]
            )
        } catch {
            throw GatewayCredentialStoreError.lifecycleUnsafe
        }

        var info = stat()
        if Darwin.lstat(path, &info) != 0 {
            guard errno == ENOENT else {
                throw GatewayCredentialStoreError.lifecycleUnsafe
            }
            guard Darwin.mkdir(path, mode_t(0o700)) == 0 || errno == EEXIST else {
                throw GatewayCredentialStoreError.lifecycleUnsafe
            }
            guard Darwin.lstat(path, &info) == 0 else {
                throw GatewayCredentialStoreError.lifecycleUnsafe
            }
        }
        guard (info.st_mode & S_IFMT) == S_IFDIR,
              info.st_uid == geteuid(),
              info.st_mode & mode_t(0o777) == mode_t(0o700) else {
            throw GatewayCredentialStoreError.lifecycleUnsafe
        }
    }

    private func acquireLock(_ path: String) throws -> Int32 {
        var descriptor: Int32 = -1
        while descriptor < 0 {
            var pathInfo = stat()
            let inspected = Darwin.lstat(path, &pathInfo)
            var flags = O_RDWR | O_NOFOLLOW | O_CLOEXEC
            if inspected != 0 {
                guard errno == ENOENT else {
                    throw GatewayCredentialStoreError.lifecycleUnsafe
                }
                flags |= O_CREAT | O_EXCL
            } else {
                try validateLockFile(pathInfo)
            }
            descriptor = Darwin.open(path, flags, mode_t(0o600))
            if descriptor < 0 {
                if flags & O_EXCL != 0, errno == EEXIST {
                    continue
                }
                throw GatewayCredentialStoreError.lifecycleUnsafe
            }
        }

        var openedInfo = stat()
        guard Darwin.fstat(descriptor, &openedInfo) == 0 else {
            Darwin.close(descriptor)
            throw GatewayCredentialStoreError.lifecycleUnsafe
        }
        do {
            try validateLockFile(openedInfo)
        } catch {
            Darwin.close(descriptor)
            throw error
        }

        while flock(descriptor, LOCK_EX) != 0 {
            guard errno == EINTR else {
                Darwin.close(descriptor)
                throw GatewayCredentialStoreError.lifecycleUnsafe
            }
        }

        var finalPathInfo = stat()
        guard Darwin.lstat(path, &finalPathInfo) == 0 else {
            _ = flock(descriptor, LOCK_UN)
            Darwin.close(descriptor)
            throw GatewayCredentialStoreError.lifecycleUnsafe
        }
        do {
            try validateLockFile(finalPathInfo)
            guard finalPathInfo.st_dev == openedInfo.st_dev,
                  finalPathInfo.st_ino == openedInfo.st_ino else {
                throw GatewayCredentialStoreError.lifecycleUnsafe
            }
        } catch {
            _ = flock(descriptor, LOCK_UN)
            Darwin.close(descriptor)
            throw error
        }
        return descriptor
    }

    private func validateLockFile(_ info: stat) throws {
        guard (info.st_mode & S_IFMT) == S_IFREG,
              info.st_uid == geteuid(),
              info.st_mode & mode_t(0o777) == mode_t(0o600) else {
            throw GatewayCredentialStoreError.lifecycleUnsafe
        }
    }

    private func requireAbsent(_ path: String) throws {
        var info = stat()
        guard Darwin.lstat(path, &info) != 0 else {
            throw GatewayCredentialStoreError.lifecycleConflict
        }
        guard errno == ENOENT else {
            throw GatewayCredentialStoreError.lifecycleConflict
        }
    }
}

protocol GatewayCredentialStoring: Sendable {
    func inspect(account: String) throws -> [GatewayCredentialKind: GatewayCredentialMetadata]
    func inspect(
        account: String,
        kinds: Set<GatewayCredentialKind>
    ) throws -> [GatewayCredentialKind: GatewayCredentialMetadata]
    func replace(
        _ kind: GatewayCredentialKind,
        account: String,
        value: String
    ) throws -> GatewayCredentialMetadata
}

extension GatewayCredentialStoring {
    func inspect(
        account: String,
        kinds: Set<GatewayCredentialKind>
    ) throws -> [GatewayCredentialKind: GatewayCredentialMetadata] {
        try inspect(account: account).filter { kinds.contains($0.key) }
    }
}

struct SystemGatewayCredentialStore: GatewayCredentialStoring, @unchecked Sendable {
    private static let maximumSecretBytes = 16 * 1_024
    private static let maximumSecurityCommandBytes = 4_000
    private let serviceNames: [GatewayCredentialKind: String]
    private let trustedApplicationPath: String?
    private let lifecycleGate: GatewayCredentialLifecycleGate

    init(
        serviceNames: [GatewayCredentialKind: String] = [:],
        trustedApplicationPath: String? = Bundle.main.executableURL?.path,
        lifecycleHomeDirectory: String = FileManager.default.homeDirectoryForCurrentUser.path
    ) {
        self.serviceNames = Dictionary(uniqueKeysWithValues: GatewayCredentialKind.allCases.map {
            ($0, serviceNames[$0] ?? $0.service)
        })
        self.trustedApplicationPath = trustedApplicationPath
        self.lifecycleGate = GatewayCredentialLifecycleGate(homeDirectory: lifecycleHomeDirectory)
    }

    func inspect(account: String) throws -> [GatewayCredentialKind: GatewayCredentialMetadata] {
        guard !account.isEmpty else {
            throw GatewayCredentialStoreError.keychainFailure
        }
        let keychain = try userKeychain()
        return try Dictionary(uniqueKeysWithValues: GatewayCredentialKind.allCases.map {
            ($0, try metadata(for: $0, account: account, keychain: keychain))
        })
    }

    func inspect(
        account: String,
        kinds: Set<GatewayCredentialKind>
    ) throws -> [GatewayCredentialKind: GatewayCredentialMetadata] {
        guard !account.isEmpty else {
            throw GatewayCredentialStoreError.keychainFailure
        }
        let keychain = try userKeychain()
        return try Dictionary(uniqueKeysWithValues: kinds.map {
            ($0, try metadata(for: $0, account: account, keychain: keychain))
        })
    }

    func replace(
        _ kind: GatewayCredentialKind,
        account: String,
        value: String
    ) throws -> GatewayCredentialMetadata {
        let normalized = value.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !account.isEmpty,
              !normalized.isEmpty,
              let data = normalized.data(using: .utf8),
              data.count <= Self.maximumSecretBytes else {
            throw GatewayCredentialStoreError.invalidValue
        }

        return try lifecycleGate.withWriteAdmission {
            let keychain = try userKeychain()
            let service = serviceName(for: kind)
            let query = itemQuery(kind, account: account, keychain: keychain)
            guard let trustedApplicationPath,
                  FileManager.default.isExecutableFile(atPath: trustedApplicationPath) else {
                throw GatewayCredentialStoreError.accessControlUnavailable
            }
            let keychainPath = try path(for: keychain)
            let accessAlreadyValid = try hasExpectedAccess(
                query: query,
                trustedApplicationPaths: [
                    trustedApplicationPath,
                    "/usr/bin/security",
                ]
            )
            let storeCommand = try makeSecurityStoreCommand(
                service: service,
                account: account,
                value: data,
                trustedApplicationPath: trustedApplicationPath,
                keychainPath: keychainPath,
                repairAccess: !accessAlreadyValid
            )
            let storeResult = runSecurityCLI(
                arguments: ["-i"],
                standardInput: storeCommand
            )
            guard storeResult.status == 0 else {
                throw GatewayCredentialStoreError.keychainFailure
            }

            guard verifyWithSecurityCLI(
                service: service,
                account: account,
                keychainPath: keychainPath,
                expectedValue: normalized
            ) else {
                throw GatewayCredentialStoreError.verificationFailed
            }
            return try metadata(for: kind, account: account, keychain: keychain)
        }
    }

    private func userKeychain() throws -> SecKeychain {
        var keychain: SecKeychain?
        guard SecKeychainCopyDomainDefault(.user, &keychain) == errSecSuccess,
              let keychain else {
            throw GatewayCredentialStoreError.keychainFailure
        }
        return keychain
    }

    private func serviceName(for kind: GatewayCredentialKind) -> String {
        serviceNames[kind] ?? kind.service
    }

    private func itemQuery(
        _ kind: GatewayCredentialKind,
        account: String,
        keychain: SecKeychain
    ) -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: serviceName(for: kind),
            kSecAttrAccount as String: account,
            kSecMatchSearchList as String: [keychain],
        ]
    }

    private func metadata(
        for kind: GatewayCredentialKind,
        account: String,
        keychain: SecKeychain
    ) throws -> GatewayCredentialMetadata {
        var query = itemQuery(kind, account: account, keychain: keychain)
        query[kSecReturnAttributes as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne
        var result: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &result)
        if status == errSecItemNotFound {
            return GatewayCredentialMetadata(configured: false, modifiedAt: nil)
        }
        guard status == errSecSuccess,
              let attributes = result as? [String: Any] else {
            throw GatewayCredentialStoreError.keychainFailure
        }
        return GatewayCredentialMetadata(
            configured: true,
            modifiedAt: attributes[kSecAttrModificationDate as String] as? Date
        )
    }

    private func path(for keychain: SecKeychain) throws -> String {
        var length = UInt32(PATH_MAX)
        var buffer = [CChar](repeating: 0, count: Int(PATH_MAX) + 1)
        guard SecKeychainGetPath(keychain, &length, &buffer) == errSecSuccess else {
            throw GatewayCredentialStoreError.keychainFailure
        }
        let bytes = buffer.prefix { $0 != 0 }.map { UInt8(bitPattern: $0) }
        guard let path = String(bytes: bytes, encoding: .utf8), !path.isEmpty else {
            throw GatewayCredentialStoreError.keychainFailure
        }
        return path
    }

    private func makeSecurityStoreCommand(
        service: String,
        account: String,
        value: Data,
        trustedApplicationPath: String,
        keychainPath: String,
        repairAccess: Bool
    ) throws -> Data {
        // Interactive stdin lets Apple's tool own legacy partition/ACL merging
        // without placing the credential in the process argument list.
        let dynamicValues = [
            account,
            service,
            trustedApplicationPath,
            "/usr/bin/security",
            keychainPath,
        ]
        let encoded = try dynamicValues.map { value -> String in
            guard value.unicodeScalars.allSatisfy({
                $0.value != 0 && $0.value != 10 && $0.value != 13
            }) else {
                throw GatewayCredentialStoreError.invalidValue
            }
            let escaped = value
                .replacingOccurrences(of: "\\", with: "\\\\")
                .replacingOccurrences(of: "\"", with: "\\\"")
            return "\"\(escaped)\""
        }
        let secretHex = value.map { String(format: "%02x", $0) }.joined()
        var arguments = [
            "add-generic-password -U",
            "-a \(encoded[0])",
            "-s \(encoded[1])",
            "-X \(secretHex)",
        ]
        if repairAccess {
            arguments.append("-T \(encoded[2])")
            arguments.append("-T \(encoded[3])")
        }
        arguments.append(encoded[4])
        let command = arguments.joined(separator: " ") + "\n"
        guard let commandData = command.data(using: .utf8),
              commandData.count <= Self.maximumSecurityCommandBytes else {
            throw GatewayCredentialStoreError.invalidValue
        }
        return commandData
    }

    private func hasExpectedAccess(
        query: [String: Any],
        trustedApplicationPaths: [String]
    ) throws -> Bool {
        guard let item = try itemReference(query: query) else {
            return false
        }
        var access: SecAccess?
        guard SecKeychainItemCopyAccess(item, &access) == errSecSuccess,
              let access,
              let aclList = SecAccessCopyMatchingACLList(
                access,
                kSecACLAuthorizationDecrypt
              ),
              CFArrayGetCount(aclList) > 0 else {
            return false
        }
        let acl = unsafeBitCast(
            CFArrayGetValueAtIndex(aclList, 0),
            to: SecACL.self
        )
        var applications: CFArray?
        var description: CFString?
        var promptSelector: SecKeychainPromptSelector = []
        guard SecACLCopyContents(
            acl,
            &applications,
            &description,
            &promptSelector
        ) == errSecSuccess, let applications else {
            return false
        }

        let actualIdentities = (0..<CFArrayGetCount(applications)).compactMap {
            let application = unsafeBitCast(
                CFArrayGetValueAtIndex(applications, $0),
                to: SecTrustedApplication.self
            )
            return trustedApplicationIdentity(application)
        }
        let expectedIdentities = try trustedApplicationPaths.map {
            var application: SecTrustedApplication?
            let status = $0.withCString {
                SecTrustedApplicationCreateFromPath($0, &application)
            }
            guard status == errSecSuccess, let application,
                  let identity = trustedApplicationIdentity(application) else {
                throw GatewayCredentialStoreError.accessControlUnavailable
            }
            return identity
        }
        return expectedIdentities.allSatisfy(actualIdentities.contains)
    }

    private func itemReference(query: [String: Any]) throws -> SecKeychainItem? {
        var lookup = query
        lookup[kSecReturnRef as String] = true
        lookup[kSecMatchLimit as String] = kSecMatchLimitOne
        var result: CFTypeRef?
        let status = SecItemCopyMatching(lookup as CFDictionary, &result)
        if status == errSecItemNotFound {
            return nil
        }
        guard status == errSecSuccess,
              let result,
              CFGetTypeID(result) == SecKeychainItemGetTypeID() else {
            throw GatewayCredentialStoreError.keychainFailure
        }
        return (result as! SecKeychainItem)
    }

    private func trustedApplicationIdentity(
        _ application: SecTrustedApplication
    ) -> Data? {
        var identity: CFData?
        guard SecTrustedApplicationCopyData(
            application,
            &identity
        ) == errSecSuccess, let identity else {
            return nil
        }
        return identity as Data
    }

    private func verifyWithSecurityCLI(
        service: String,
        account: String,
        keychainPath: String,
        expectedValue: String
    ) -> Bool {
        let result = runSecurityCLI(arguments: [
            "find-generic-password",
            "-a", account,
            "-s", service,
            "-w",
            keychainPath,
        ])
        guard result.status == 0 else {
            return false
        }
        let value = String(data: result.output, encoding: .utf8)?
            .trimmingCharacters(in: .whitespacesAndNewlines)
        return value == expectedValue
    }

    private func runSecurityCLI(
        arguments: [String],
        standardInput: Data? = nil
    ) -> (status: Int32, output: Data) {
        let process = Process()
        let output = Pipe()
        let input = Pipe()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/security")
        process.arguments = arguments
        process.standardOutput = output
        process.standardError = FileHandle.nullDevice
        if standardInput == nil {
            process.standardInput = FileHandle.nullDevice
        } else {
            process.standardInput = input
        }
        do {
            try process.run()
        } catch {
            return (-1, Data())
        }
        if let standardInput {
            do {
                try input.fileHandleForWriting.write(contentsOf: standardInput)
            } catch {
                process.terminate()
            }
            try? input.fileHandleForWriting.close()
        }
        let deadline = Date().addingTimeInterval(5)
        while process.isRunning, Date() < deadline {
            Thread.sleep(forTimeInterval: 0.02)
        }
        if process.isRunning {
            process.terminate()
            let grace = Date().addingTimeInterval(0.25)
            while process.isRunning, Date() < grace {
                Thread.sleep(forTimeInterval: 0.01)
            }
            if process.isRunning, process.processIdentifier > 0 {
                _ = Darwin.kill(process.processIdentifier, SIGKILL)
            }
        }
        process.waitUntilExit()
        let data = output.fileHandleForReading.readDataToEndOfFile()
        guard data.count <= Self.maximumSecretBytes else {
            return (-1, Data())
        }
        return (process.terminationStatus, data)
    }
}

struct GatewayVerificationReceipt: Codable, Equatable, Sendable {
    let schema: Int
    let configDigest: String
    let credentialModificationTimes: [String: TimeInterval]
    let verifiedAt: Date
    let resultCode: String
}

protocol GatewayVerificationReceiptStoring: Sendable {
    func load() -> GatewayVerificationReceipt?
    func save(_ receipt: GatewayVerificationReceipt)
    func clear()
}

final class UserDefaultsGatewayVerificationReceiptStore:
    GatewayVerificationReceiptStoring,
    @unchecked Sendable
{
    private let defaults: UserDefaults
    private let key: String
    private let lock = NSLock()

    init(
        defaults: UserDefaults = .standard,
        key: String = "externalGatewayVerificationReceipt.v1"
    ) {
        self.defaults = defaults
        self.key = key
    }

    func load() -> GatewayVerificationReceipt? {
        lock.lock()
        defer { lock.unlock() }
        guard let data = defaults.data(forKey: key),
              let receipt = try? JSONDecoder().decode(
                GatewayVerificationReceipt.self,
                from: data
              ),
              receipt.schema == 1,
              GatewayInspection.isDigest(receipt.configDigest),
              receipt.resultCode == "connected" else {
            return nil
        }
        return receipt
    }

    func save(_ receipt: GatewayVerificationReceipt) {
        guard let data = try? JSONEncoder().encode(receipt) else { return }
        lock.lock()
        defaults.set(data, forKey: key)
        lock.unlock()
    }

    func clear() {
        lock.lock()
        defaults.removeObject(forKey: key)
        lock.unlock()
    }
}

enum GatewaySettingsState: Equatable {
    case loading
    case needsValidation
    case testing
    case applying
    case connected
    case authenticationMismatch
    case unreachable
    case catalogInvalid
    case integrationRequired
    case recoveryRequired
    case appLocationInvalid
    case integrationArtifactInvalid
    case bindingUnsafe
    case bindingInvalid
    case helperUnavailable
    case unsupported
    case failed
}

enum GatewaySettingsUnavailability: String, Equatable, Sendable {
    case previewMode = "preview_mode"
    case bindingMissing = "routing_binding_missing"
    case bindingUnsafe = "routing_binding_unsafe"
    case bindingInvalid = "routing_binding_invalid"
    case helperUnavailable = "relayctl_unavailable"

    init?(_ availability: RelayIntegrationAvailability) {
        switch availability {
        case .ready:
            return nil
        case .preview:
            self = .previewMode
        case .missing:
            self = .bindingMissing
        case .unsafe:
            self = .bindingUnsafe
        case .invalid:
            self = .bindingInvalid
        case .helperUnavailable:
            self = .helperUnavailable
        }
    }

    var state: GatewaySettingsState {
        switch self {
        case .previewMode, .bindingMissing:
            .integrationRequired
        case .bindingUnsafe:
            .bindingUnsafe
        case .bindingInvalid:
            .bindingInvalid
        case .helperUnavailable:
            .helperUnavailable
        }
    }
}

struct GatewaySettingsResolution {
    let client: (any GatewayManaging)?
    let unavailability: GatewaySettingsUnavailability?

    init(
        client: (any GatewayManaging)?,
        unavailability: GatewaySettingsUnavailability? = nil
    ) {
        self.client = client
        self.unavailability = unavailability
    }
}

@MainActor
final class GatewaySettingsController: ObservableObject {
    private struct CredentialMetadataContext: Equatable {
        let profile: RemoteAuthenticationProfile
        let account: String?
    }

    @Published private(set) var inspection: GatewayInspection?
    @Published private(set) var integrationInspection: SelfHostedIntegrationInspection?
    @Published private(set) var credentialMetadata: [GatewayCredentialKind: GatewayCredentialMetadata] = [:]
    @Published private(set) var credentialMetadataState: GatewayCredentialMetadataState = .idle
    @Published var draftURL = ""
    @Published var authenticationProfile: RemoteAuthenticationProfile = .cloudflareAccessAndGatewayAPIKey
    @Published var allowInsecurePrivateIP = false
    @Published private(set) var state: GatewaySettingsState = .loading
    @Published private(set) var isBusy = false
    @Published private(set) var lastErrorCode: String?

    private let resolver: () -> GatewaySettingsResolution
    private let integrationClient: (any SelfHostedIntegrationManaging)?
    private let credentialStore: any GatewayCredentialStoring
    private let receiptStore: any GatewayVerificationReceiptStoring
    private let activityLog: RelayActivityLogStore
    private let onRoutingRefreshRequested: () -> Void
    private var credentialMetadataContext: CredentialMetadataContext?
    private var credentialMetadataRequestID = 0

    init(
        client: (any GatewayManaging)?,
        unavailability: GatewaySettingsUnavailability? = nil,
        integrationClient: (any SelfHostedIntegrationManaging)? = nil,
        credentialStore: any GatewayCredentialStoring = SystemGatewayCredentialStore(),
        receiptStore: any GatewayVerificationReceiptStoring = UserDefaultsGatewayVerificationReceiptStore(),
        activityLog: RelayActivityLogStore,
        onRoutingRefreshRequested: @escaping () -> Void
    ) {
        self.resolver = {
            GatewaySettingsResolution(
                client: client,
                unavailability: unavailability
            )
        }
        self.integrationClient = integrationClient
        self.credentialStore = credentialStore
        self.receiptStore = receiptStore
        self.activityLog = activityLog
        self.onRoutingRefreshRequested = onRoutingRefreshRequested
    }

    init(
        resolver: @escaping () -> GatewaySettingsResolution,
        integrationClient: (any SelfHostedIntegrationManaging)? = nil,
        credentialStore: any GatewayCredentialStoring = SystemGatewayCredentialStore(),
        receiptStore: any GatewayVerificationReceiptStoring = UserDefaultsGatewayVerificationReceiptStore(),
        activityLog: RelayActivityLogStore,
        onRoutingRefreshRequested: @escaping () -> Void
    ) {
        self.resolver = resolver
        self.integrationClient = integrationClient
        self.credentialStore = credentialStore
        self.receiptStore = receiptStore
        self.activityLog = activityLog
        self.onRoutingRefreshRequested = onRoutingRefreshRequested
    }

    var canEditCredentials: Bool {
        if inspection?.credentialsEditable == true { return true }
        guard let integrationInspection else { return false }
        return integrationInspection.state == .integrationRequired &&
            !integrationInspection.credentialAccount.isEmpty
    }

    var requiredCredentialKinds: [GatewayCredentialKind] {
        switch authenticationProfile {
        case .none:
            []
        case .gatewayAPIKey:
            [.gatewayAPIKey]
        case .cloudflareAccessAndGatewayAPIKey:
            [.cloudflareClientID, .cloudflareClientSecret, .gatewayAPIKey]
        }
    }

    var requiresInsecureTransportConfirmation: Bool {
        usesHTTPTransport && !hasTransportProfileConflict
    }

    var hasTransportProfileConflict: Bool {
        usesHTTPTransport && authenticationProfile == .cloudflareAccessAndGatewayAPIKey
    }

    private var draftCandidate: GatewayCandidate {
        GatewayCandidate(
            upstreamBaseURL: draftURL,
            authenticationProfile: authenticationProfile,
            allowInsecurePrivateIP: allowInsecurePrivateIP
        )
    }

    var canApply: Bool {
        guard !isBusy, let inspection else { return false }
        return hasCurrentCredentialMetadata &&
            GatewayInspection.isCandidateInput(draftURL) &&
            !hasTransportProfileConflict &&
            (!requiresInsecureTransportConfirmation || allowInsecurePrivateIP) &&
            candidateDiffersFromInspection(draftCandidate, inspection: inspection)
    }

    var canPrepareIntegration: Bool {
        guard !isBusy,
              state == .integrationRequired,
              integrationInspection?.state == .integrationRequired else { return false }
        return hasCurrentCredentialMetadata &&
            GatewayInspection.isCandidateInput(draftURL) &&
            !hasTransportProfileConflict &&
            (!requiresInsecureTransportConfirmation || allowInsecurePrivateIP)
    }

    var canRecoverIntegration: Bool {
        !isBusy && state == .recoveryRequired &&
            integrationInspection?.state == .recoveryRequired
    }

    var canTest: Bool {
        !isBusy && inspection != nil && hasCurrentCredentialMetadata &&
            GatewayInspection.isCandidateInput(draftURL) &&
            !hasTransportProfileConflict &&
            (!requiresInsecureTransportConfirmation || allowInsecurePrivateIP)
    }

    var canSwitchCodexToVerifiedConfiguration: Bool {
        guard !isBusy,
              hasCurrentCredentialMetadata,
              state == .connected,
              let inspection else { return false }
        return !candidateDiffersFromInspection(draftCandidate, inspection: inspection) &&
            receiptMatches(inspection, metadata: credentialMetadata)
    }

    private var usesHTTPTransport: Bool {
        URLComponents(string: draftURL)?.scheme?.lowercased() == "http"
    }

    private var hasCurrentCredentialMetadata: Bool {
        credentialMetadataState == .ready &&
            credentialMetadataContext == CredentialMetadataContext(
                profile: authenticationProfile,
                account: credentialAccountForMetadata
            )
    }

    func load() {
        guard !isBusy else { return }
        Task { await refresh() }
    }

    func refresh() async {
        credentialMetadataRequestID &+= 1
        let requestID = credentialMetadataRequestID
        credentialMetadataContext = nil
        credentialMetadataState = .loading
        let resolution = resolver()
        if resolution.client == nil,
           resolution.unavailability == .bindingMissing,
           integrationClient != nil {
            await refreshIntegration(requestID: requestID)
            return
        }
        guard let client = resolution.client else {
            guard requestID == credentialMetadataRequestID else { return }
            isBusy = false
            transitionToUnavailable(
                resolution.unavailability ?? .helperUnavailable
            )
            return
        }
        isBusy = true
        state = .loading
        defer {
            if requestID == credentialMetadataRequestID {
                isBusy = false
            }
        }
        do {
            let nextInspection = try await client.inspect()
            let metadata: [GatewayCredentialKind: GatewayCredentialMetadata]
            if nextInspection.credentialsEditable,
               let account = nextInspection.credentialAccount {
                let kinds = Set(requiredCredentialKinds(for: nextInspection.authenticationProfile))
                if kinds.isEmpty {
                    metadata = [:]
                } else {
                    let store = credentialStore
                    metadata = try await Task.detached {
                        try store.inspect(account: account, kinds: kinds)
                    }.value
                }
            } else {
                metadata = [:]
            }
            guard requestID == credentialMetadataRequestID else { return }
            inspection = nextInspection
            integrationInspection = nil
            credentialMetadata = metadata
            draftURL = nextInspection.upstreamBaseURL
            authenticationProfile = nextInspection.authenticationProfile
            allowInsecurePrivateIP = nextInspection.allowInsecurePrivateIP
            credentialMetadataContext = CredentialMetadataContext(
                profile: nextInspection.authenticationProfile,
                account: nextInspection.credentialsEditable
                    ? nextInspection.credentialAccount
                    : nil
            )
            credentialMetadataState = .ready
            lastErrorCode = nil
            state = receiptMatches(nextInspection, metadata: metadata)
                ? .connected
                : .needsValidation
        } catch {
            guard requestID == credentialMetadataRequestID else { return }
            credentialMetadata = [:]
            credentialMetadataContext = nil
            credentialMetadataState = .failed
            apply(error)
        }
    }

    func draftDidChange() {
        guard !isBusy else { return }
        lastErrorCode = nil
        if integrationInspection?.state == .integrationRequired {
            receiptStore.clear()
            state = .integrationRequired
            return
        }
        guard let inspection else { return }
        guard credentialMetadataState == .ready else {
            state = .needsValidation
            return
        }
        guard !candidateDiffersFromInspection(draftCandidate, inspection: inspection) else {
            state = .needsValidation
            return
        }
        state = receiptMatches(inspection, metadata: credentialMetadata)
            ? .connected
            : .needsValidation
    }

    func addressDidChange() {
        // The acknowledgement is bound to the currently inspected destination.
        // Editing an HTTP destination requires a fresh, explicit confirmation.
        if draftURL != inspection?.upstreamBaseURL {
            allowInsecurePrivateIP = false
        } else if !requiresInsecureTransportConfirmation {
            allowInsecurePrivateIP = false
        }
        draftDidChange()
    }

    func authenticationProfileDidChange() {
        guard !isBusy else { return }
        if !requiresInsecureTransportConfirmation {
            allowInsecurePrivateIP = false
        }
        let context = CredentialMetadataContext(
            profile: authenticationProfile,
            account: credentialAccountForMetadata
        )
        guard credentialMetadataState != .ready || credentialMetadataContext != context else {
            draftDidChange()
            return
        }

        credentialMetadataRequestID &+= 1
        let requestID = credentialMetadataRequestID
        credentialMetadata = [:]
        credentialMetadataContext = nil
        credentialMetadataState = .loading
        receiptStore.clear()
        draftDidChange()

        let kinds = Set(requiredCredentialKinds)
        guard !kinds.isEmpty else {
            credentialMetadataContext = context
            credentialMetadataState = .ready
            draftDidChange()
            return
        }
        guard let account = context.account, !account.isEmpty else {
            credentialMetadataContext = context
            credentialMetadataState = .ready
            draftDidChange()
            return
        }
        let store = credentialStore
        Task {
            do {
                let metadata = try await Task.detached {
                    try store.inspect(account: account, kinds: kinds)
                }.value
                guard requestID == credentialMetadataRequestID,
                      context == CredentialMetadataContext(
                        profile: authenticationProfile,
                        account: credentialAccountForMetadata
                      ) else { return }
                credentialMetadata = metadata
                credentialMetadataContext = context
                credentialMetadataState = .ready
                draftDidChange()
            } catch {
                guard requestID == credentialMetadataRequestID else { return }
                credentialMetadata = [:]
                credentialMetadataContext = nil
                credentialMetadataState = .failed
                receiptStore.clear()
                lastErrorCode = "keychain_read_failed"
                state = .failed
            }
        }
    }

    func test() {
        guard canTest,
              let client = resolveClient(),
              let startingInspection = inspection else { return }
        isBusy = true
        state = .testing
        lastErrorCode = nil
        let candidateConfiguration = draftCandidate
        activityLog.record(
            category: .operation,
            code: "gateway_test_started",
            fields: ["action": "test"]
        )
        Task {
            defer { isBusy = false }
            do {
                let startingMetadata = try await inspectCredentials(
                    for: startingInspection,
                    profile: candidateConfiguration.authenticationProfile
                )
                let validation = try await client.test(candidate: candidateConfiguration)
                let verifiedMetadata = try await inspectCredentials(
                    for: startingInspection,
                    profile: candidateConfiguration.authenticationProfile
                )
                guard candidateConfiguration == draftCandidate else {
                    markNeedsValidation()
                    activityLog.record(
                        .warning,
                        category: .operation,
                        code: "gateway_test_finished",
                        fields: [
                            "action": "test",
                            "result_code": "verification_required",
                        ]
                    )
                    return
                }
                credentialMetadata = verifiedMetadata
                credentialMetadataContext = CredentialMetadataContext(
                    profile: candidateConfiguration.authenticationProfile,
                    account: startingInspection.credentialsEditable
                        ? startingInspection.credentialAccount
                        : nil
                )
                credentialMetadataState = .ready
                guard self.inspection == startingInspection,
                      validation.configDigest == startingInspection.configDigest else {
                    markNeedsValidation(code: .configChanged)
                    activityLog.record(
                        .warning,
                        category: .operation,
                        code: "gateway_test_finished",
                        fields: [
                            "action": "test",
                            "result_code": RelayctlReportedErrorCode.configChanged.rawValue,
                        ]
                    )
                    return
                }
                guard validation.routingGeneration == startingInspection.routingGeneration else {
                    markNeedsValidation(code: .routingChanged)
                    activityLog.record(
                        .warning,
                        category: .operation,
                        code: "gateway_test_finished",
                        fields: [
                            "action": "test",
                            "result_code": RelayctlReportedErrorCode.routingChanged.rawValue,
                        ]
                    )
                    return
                }
                guard startingMetadata == verifiedMetadata else {
                    markNeedsValidation(code: .credentialUnavailable)
                    activityLog.record(
                        .warning,
                        category: .operation,
                        code: "gateway_test_finished",
                        fields: [
                            "action": "test",
                            "result_code": RelayctlReportedErrorCode.credentialUnavailable.rawValue,
                        ]
                    )
                    return
                }
                state = .connected
                if !candidateDiffersFromInspection(candidateConfiguration, inspection: startingInspection) {
                    saveReceipt(
                        configDigest: validation.configDigest,
                        metadata: verifiedMetadata
                    )
                }
                activityLog.record(
                    category: .operation,
                    code: "gateway_test_finished",
                    fields: ["action": "test", "result_code": "connected"]
                )
            } catch {
                apply(error)
                activityLog.record(
                    .error,
                    category: .operation,
                    code: "gateway_test_finished",
                    fields: [
                        "action": "test",
                        "result_code": lastErrorCode ?? "operation_failed",
                    ]
                )
            }
        }
    }

    func apply() {
        guard canApply,
              let client = resolveClient(),
              let startingInspection = inspection,
              candidateDiffersFromInspection(draftCandidate, inspection: startingInspection) else { return }
        isBusy = true
        state = .applying
        lastErrorCode = nil
        let candidate = draftURL
        let candidateConfiguration = draftCandidate
        activityLog.record(
            category: .operation,
            code: "gateway_apply_started",
            fields: ["action": "apply"]
        )
        Task {
            defer { isBusy = false }
            do {
                let startingMetadata = try await inspectCredentials(
                    for: startingInspection,
                    profile: candidateConfiguration.authenticationProfile
                )
                let receipt = try await client.apply(
                    candidate: candidateConfiguration,
                    expectedConfigDigest: startingInspection.configDigest,
                    expectedRoutingGeneration: startingInspection.routingGeneration
                )
                let refreshed = try await client.inspect()
                let metadata = try await inspectCredentials(for: refreshed)
                self.inspection = refreshed
                credentialMetadata = metadata
                draftURL = refreshed.upstreamBaseURL
                authenticationProfile = refreshed.authenticationProfile
                allowInsecurePrivateIP = refreshed.allowInsecurePrivateIP
                credentialMetadataContext = CredentialMetadataContext(
                    profile: refreshed.authenticationProfile,
                    account: refreshed.credentialsEditable
                        ? refreshed.credentialAccount
                        : nil
                )
                credentialMetadataState = .ready
                onRoutingRefreshRequested()
                let mismatchCode: RelayctlReportedErrorCode?
                if normalizedCandidateURL(candidate) != refreshed.upstreamBaseURL ||
                    refreshed.authenticationProfile != candidateConfiguration.authenticationProfile ||
                    refreshed.allowInsecurePrivateIP != candidateConfiguration.allowInsecurePrivateIP ||
                    refreshed.configDigest != receipt.configDigest {
                    mismatchCode = .configChanged
                } else if refreshed.routingGeneration != receipt.routingGeneration {
                    mismatchCode = .routingChanged
                } else if startingMetadata != metadata {
                    mismatchCode = .credentialUnavailable
                } else {
                    mismatchCode = nil
                }
                if let mismatchCode {
                    markNeedsValidation(code: mismatchCode)
                    activityLog.record(
                        .warning,
                        category: .operation,
                        code: "gateway_apply_finished",
                        fields: [
                            "action": "apply",
                            "result_code": mismatchCode.rawValue,
                        ]
                    )
                    return
                }
                saveReceipt(configDigest: receipt.configDigest, metadata: metadata)
                state = .connected
                activityLog.record(
                    category: .operation,
                    code: "gateway_apply_finished",
                    fields: [
                        "action": "apply",
                        "result_code": receipt.runtimeReloaded
                            ? "runtime_reloaded"
                            : "saved",
                    ]
                )
            } catch {
                apply(error)
                activityLog.record(
                    .error,
                    category: .operation,
                    code: "gateway_apply_finished",
                    fields: [
                        "action": "apply",
                        "result_code": lastErrorCode ?? "operation_failed",
                    ]
                )
            }
        }
    }

    func prepareIntegration() {
        guard !isBusy,
              let integrationClient,
              let startingInspection = integrationInspection,
              startingInspection.state == .integrationRequired,
              canPrepareIntegration else { return }
        isBusy = true
        state = .applying
        lastErrorCode = nil
        let candidate = draftCandidate
        activityLog.record(
            category: .operation,
            code: "self_hosted_integration_started",
            fields: ["action": "prepare"]
        )
        Task {
            defer { isBusy = false }
            do {
                _ = try await integrationClient.apply(
                    candidate: candidate,
                    expectedStateDigest: startingInspection.stateDigest
                )
                guard candidate == draftCandidate else {
                    markNeedsValidation()
                    return
                }
                integrationInspection = nil
                onRoutingRefreshRequested()
                activityLog.record(
                    category: .operation,
                    code: "self_hosted_integration_finished",
                    fields: ["action": "prepare", "result_code": "ready"]
                )
                isBusy = false
                await refresh()
            } catch {
                apply(error)
                if state == .recoveryRequired {
                    await synchronizeRecoveryInspection(using: integrationClient)
                }
                activityLog.record(
                    .error,
                    category: .operation,
                    code: "self_hosted_integration_finished",
                    fields: [
                        "action": "prepare",
                        "result_code": lastErrorCode ?? "operation_failed",
                    ]
                )
            }
        }
    }

    func recoverIntegration() {
        guard !isBusy, let integrationClient, canRecoverIntegration else { return }
        isBusy = true
        state = .applying
        lastErrorCode = nil
        activityLog.record(
            category: .operation,
            code: "self_hosted_integration_started",
            fields: ["action": "recover"]
        )
        Task {
            defer { isBusy = false }
            do {
                _ = try await integrationClient.recover()
                activityLog.record(
                    category: .operation,
                    code: "self_hosted_integration_finished",
                    fields: ["action": "recover", "result_code": "recovered"]
                )
                isBusy = false
                await refresh()
            } catch {
                apply(error)
                if state == .recoveryRequired {
                    await synchronizeRecoveryInspection(using: integrationClient)
                }
                activityLog.record(
                    .error,
                    category: .operation,
                    code: "self_hosted_integration_finished",
                    fields: [
                        "action": "recover",
                        "result_code": lastErrorCode ?? "operation_failed",
                    ]
                )
            }
        }
    }

    func replaceCredential(
        _ kind: GatewayCredentialKind,
        value: String
    ) async -> Bool {
        let resolution = resolver()
        let gatewayClient = resolution.client
        let activeInspection = inspection
        if gatewayClient == nil && integrationInspection?.state != .integrationRequired {
            transitionToUnavailable(
                resolution.unavailability ?? .helperUnavailable
            )
            return false
        }
        let account = activeInspection?.credentialAccount ?? integrationInspection?.credentialAccount
        guard !isBusy,
              hasCurrentCredentialMetadata,
              requiredCredentialKinds.contains(kind),
              let account,
              !account.isEmpty,
              activeInspection?.credentialsEditable == true ||
                integrationInspection?.state == .integrationRequired else {
            return false
        }
        isBusy = true
        lastErrorCode = nil
        receiptStore.clear()
        state = .needsValidation
        activityLog.record(
            category: .operation,
            code: "gateway_credential_replace_started",
            fields: ["action": kind.rawValue]
        )
        defer { isBusy = false }
        do {
            let store = credentialStore
            let kinds = Set(requiredCredentialKinds)
            _ = try await Task.detached {
                try store.replace(
                    kind,
                    account: account,
                    value: value
                )
            }.value
            let startingMetadata = try await Task.detached {
                try store.inspect(account: account, kinds: kinds)
            }.value
            credentialMetadata = startingMetadata
            credentialMetadataContext = CredentialMetadataContext(
                profile: authenticationProfile,
                account: account
            )
            credentialMetadataState = .ready
            guard let client = gatewayClient, let inspection = activeInspection else {
                state = .integrationRequired
                activityLog.record(
                    category: .operation,
                    code: "gateway_credential_replace_finished",
                    fields: ["action": kind.rawValue, "result_code": "saved"]
                )
                return true
            }
            do {
                let validation = try await client.test(candidate: GatewayCandidate(
                    upstreamBaseURL: inspection.upstreamBaseURL,
                    authenticationProfile: inspection.authenticationProfile,
                    allowInsecurePrivateIP: inspection.allowInsecurePrivateIP
                ))
                let verifiedMetadata = try await Task.detached {
                    try store.inspect(account: account, kinds: kinds)
                }.value
                credentialMetadata = verifiedMetadata
                credentialMetadataContext = CredentialMetadataContext(
                    profile: authenticationProfile,
                    account: account
                )
                credentialMetadataState = .ready
                let mismatchCode: RelayctlReportedErrorCode?
                if self.inspection != inspection ||
                    validation.configDigest != inspection.configDigest {
                    mismatchCode = .configChanged
                } else if validation.routingGeneration != inspection.routingGeneration {
                    mismatchCode = .routingChanged
                } else if startingMetadata != verifiedMetadata {
                    mismatchCode = .credentialUnavailable
                } else {
                    mismatchCode = nil
                }
                if let mismatchCode {
                    markNeedsValidation(code: mismatchCode)
                } else {
                    saveReceipt(
                        configDigest: validation.configDigest,
                        metadata: verifiedMetadata
                    )
                    lastErrorCode = nil
                    state = !candidateDiffersFromInspection(draftCandidate, inspection: inspection)
                        ? .connected
                        : .needsValidation
                }
            } catch {
                apply(error)
            }
            activityLog.record(
                category: .operation,
                code: "gateway_credential_replace_finished",
                fields: [
                    "action": kind.rawValue,
                    "result_code": state == .connected
                        ? "connected"
                        : (lastErrorCode ?? "verification_failed"),
                ]
            )
            return true
        } catch {
            credentialMetadata = [:]
            credentialMetadataContext = nil
            credentialMetadataState = .failed
            switch error as? GatewayCredentialStoreError {
            case .lifecycleConflict:
                lastErrorCode = RelayctlReportedErrorCode.integrationRecoveryRequired.rawValue
                state = .recoveryRequired
            case .lifecycleUnsafe:
                lastErrorCode = RelayctlReportedErrorCode.integrationStateUnsafe.rawValue
                state = .bindingUnsafe
            default:
                lastErrorCode = "keychain_write_failed"
                state = .failed
            }
            activityLog.record(
                .error,
                category: .operation,
                code: "gateway_credential_replace_finished",
                fields: [
                    "action": kind.rawValue,
                    "result_code": lastErrorCode ?? "keychain_write_failed",
                ]
            )
            return false
        }
    }

    private func resolveClient() -> (any GatewayManaging)? {
        let resolution = resolver()
        guard let client = resolution.client else {
            transitionToUnavailable(
                resolution.unavailability ?? .helperUnavailable
            )
            return nil
        }
        return client
    }

    private func transitionToUnavailable(_ unavailability: GatewaySettingsUnavailability) {
        credentialMetadataRequestID &+= 1
        inspection = nil
        integrationInspection = nil
        credentialMetadata = [:]
        credentialMetadataContext = nil
        credentialMetadataState = .idle
        draftURL = ""
        state = unavailability.state
        lastErrorCode = unavailability.rawValue
    }

    private func apply(_ error: Error) {
        let relayError = error as? RelayctlError
        lastErrorCode = relayError?.safeCode ?? "operation_failed"
        switch relayError {
        case .reported(.authenticationFailed), .reported(.credentialUnavailable):
            state = .authenticationMismatch
        case .reported(.gatewayUnreachable), .timedOut:
            state = .unreachable
        case .reported(.catalogInvalid):
            state = .catalogInvalid
        case .reported(.gatewayUnsupported):
            state = .unsupported
        case .helperUnavailable:
            state = .helperUnavailable
        case .reported(.integrationRecoveryRequired):
            state = .recoveryRequired
        case .reported(.integrationAppLocationInvalid):
            state = .appLocationInvalid
        case .reported(.integrationArtifactInvalid):
            state = .integrationArtifactInvalid
        case .reported(.integrationStateUnsafe):
            state = .bindingUnsafe
        default:
            state = .failed
        }
    }

    private func synchronizeRecoveryInspection(
        using integrationClient: any SelfHostedIntegrationManaging
    ) async {
        guard state == .recoveryRequired else { return }
        guard let nextInspection = try? await integrationClient.inspect(),
              state == .recoveryRequired,
              nextInspection.state == .recoveryRequired else { return }
        integrationInspection = nextInspection
        inspection = nil
        credentialMetadata = [:]
        credentialMetadataContext = nil
        credentialMetadataState = .idle
    }

    private func refreshIntegration(requestID: Int) async {
        guard let integrationClient else { return }
        let profile = authenticationProfile
        isBusy = true
        state = .loading
        defer {
            if requestID == credentialMetadataRequestID {
                isBusy = false
            }
        }
        do {
            let nextInspection = try await integrationClient.inspect()
            guard requestID == credentialMetadataRequestID,
                  profile == authenticationProfile else { return }
            let metadata: [GatewayCredentialKind: GatewayCredentialMetadata]
            if nextInspection.state == .integrationRequired {
                let kinds = Set(requiredCredentialKinds(for: profile))
                if kinds.isEmpty {
                    metadata = [:]
                } else {
                    let store = credentialStore
                    metadata = try await Task.detached {
                        try store.inspect(
                            account: nextInspection.credentialAccount,
                            kinds: kinds
                        )
                    }.value
                }
            } else {
                metadata = [:]
            }
            guard requestID == credentialMetadataRequestID,
                  profile == authenticationProfile else { return }
            integrationInspection = nextInspection
            inspection = nil
            lastErrorCode = nil
            switch nextInspection.state {
            case .integrationRequired:
                credentialMetadata = metadata
                credentialMetadataContext = CredentialMetadataContext(
                    profile: profile,
                    account: nextInspection.credentialAccount
                )
                credentialMetadataState = .ready
                state = .integrationRequired
            case .ready:
                credentialMetadata = [:]
                credentialMetadataContext = nil
                credentialMetadataState = .idle
                onRoutingRefreshRequested()
                state = .needsValidation
            case .recoveryRequired:
                credentialMetadata = [:]
                credentialMetadataContext = nil
                credentialMetadataState = .idle
                state = .recoveryRequired
            }
        } catch {
            guard requestID == credentialMetadataRequestID,
                  profile == authenticationProfile else { return }
            credentialMetadata = [:]
            credentialMetadataContext = nil
            credentialMetadataState = .failed
            apply(error)
        }
    }

    private func receiptMatches(
        _ inspection: GatewayInspection,
        metadata: [GatewayCredentialKind: GatewayCredentialMetadata]
    ) -> Bool {
        guard let receipt = receiptStore.load(),
              receipt.configDigest == inspection.configDigest else {
            return false
        }
        return receipt.credentialModificationTimes == modificationTimes(metadata)
    }

    private func inspectCredentials(
        for inspection: GatewayInspection,
        profile: RemoteAuthenticationProfile? = nil
    ) async throws -> [GatewayCredentialKind: GatewayCredentialMetadata] {
        guard inspection.credentialsEditable,
              let account = inspection.credentialAccount else {
            return [:]
        }
        let kinds = Set(requiredCredentialKinds(for: profile ?? inspection.authenticationProfile))
        guard !kinds.isEmpty else { return [:] }
        let store = credentialStore
        return try await Task.detached {
            try store.inspect(account: account, kinds: kinds)
        }.value
    }

    private func markNeedsValidation(
        code: RelayctlReportedErrorCode? = nil
    ) {
        receiptStore.clear()
        lastErrorCode = code?.rawValue
        state = .needsValidation
    }

    private func saveReceipt(
        configDigest: String,
        metadata: [GatewayCredentialKind: GatewayCredentialMetadata]
    ) {
        receiptStore.save(GatewayVerificationReceipt(
            schema: 1,
            configDigest: configDigest,
            credentialModificationTimes: modificationTimes(metadata),
            verifiedAt: Date(),
            resultCode: "connected"
        ))
    }

    private func requiredCredentialKinds(
        for profile: RemoteAuthenticationProfile
    ) -> [GatewayCredentialKind] {
        switch profile {
        case .none:
            []
        case .gatewayAPIKey:
            [.gatewayAPIKey]
        case .cloudflareAccessAndGatewayAPIKey:
            [.cloudflareClientID, .cloudflareClientSecret, .gatewayAPIKey]
        }
    }

    private var credentialAccountForMetadata: String? {
        if inspection?.credentialsEditable == true {
            return inspection?.credentialAccount
        }
        if integrationInspection?.state == .integrationRequired {
            return integrationInspection?.credentialAccount
        }
        return nil
    }

    private func candidateDiffersFromInspection(
        _ candidate: GatewayCandidate,
        inspection: GatewayInspection
    ) -> Bool {
        normalizedCandidateURL(candidate.upstreamBaseURL) != inspection.upstreamBaseURL ||
            candidate.authenticationProfile != inspection.authenticationProfile ||
            candidate.allowInsecurePrivateIP != inspection.allowInsecurePrivateIP
    }

    private func normalizedCandidateURL(_ value: String) -> String {
        guard var components = URLComponents(string: value),
              components.query == nil,
              components.fragment == nil else { return value }
        if components.path.isEmpty || components.path == "/" {
            components.path = "/v1"
        }
        return components.string ?? value
    }

    private func modificationTimes(
        _ metadata: [GatewayCredentialKind: GatewayCredentialMetadata]
    ) -> [String: TimeInterval] {
        Dictionary(uniqueKeysWithValues: GatewayCredentialKind.allCases.map {
            ($0.rawValue, metadata[$0]?.modifiedAt?.timeIntervalSince1970 ?? -1)
        })
    }
}

/// Consumer-facing name for the controller that owns integration, credentials,
/// validation and gateway application as one migration flow.
typealias SelfHostedMigrationController = GatewaySettingsController
