import Foundation

public enum ReleaseUpdateChannel: String, Codable, CaseIterable, Sendable {
    case stable
    case preview
}

public enum ReleaseUpdateStatus: String, Codable, Sendable {
    case current
    case newerThanSelectedChannel = "newer_than_selected_channel"
    case updateAvailable = "update_available"
    case offline
    case rateLimited = "rate_limited"
    case invalidRelease = "invalid_release"
    case updaterTooOld = "updater_too_old"
    case unsupportedSystem = "unsupported_system"
}

public enum ReleaseUpdateETagCacheState: String, Codable, Sendable {
    case miss
    case refreshed
    case notModified = "not_modified"
    case unavailable
}

public struct ReleaseUpdateCheckResult: Codable, Sendable, Equatable {
    public let schemaVersion: Int
    public let status: ReleaseUpdateStatus
    public let channel: ReleaseUpdateChannel
    public let currentVersion: String
    public let checkedAt: String
    public let etagCacheState: ReleaseUpdateETagCacheState
    public let releaseID: Int64?
    public let tag: String?
    public let version: String?
    public let releaseURL: String?
    public let manifestSHA256: String?
    public let appAssetID: Int64?
    public let appSHA256: String?
    public let minimumUpdaterVersion: String?
    public let minimumMacOSVersion: String?
    public let integrationProtocol: Int?
    public let helperProtocol: Int?
    public let trustKeyID: String?

    enum CodingKeys: String, CodingKey, CaseIterable {
        case schemaVersion = "schema_version"
        case status
        case channel
        case currentVersion = "current_version"
        case checkedAt = "checked_at"
        case etagCacheState = "etag_cache_state"
        case releaseID = "release_id"
        case tag
        case version
        case releaseURL = "release_url"
        case manifestSHA256 = "manifest_sha256"
        case appAssetID = "app_asset_id"
        case appSHA256 = "app_sha256"
        case minimumUpdaterVersion = "minimum_updater_version"
        case minimumMacOSVersion = "minimum_macos_version"
        case integrationProtocol = "integration_protocol"
        case helperProtocol = "helper_protocol"
        case trustKeyID = "trust_key_id"
    }

    public static func decodeStrict(_ data: Data) throws -> Self {
        guard data.count <= 64 * 1024 else {
            throw ReleaseUpdateContractError.invalidJSON
        }
        do {
            var scanner = FlatJSONKeyScanner(data: data)
            try scanner.validateUniqueKeys()
        } catch let error as ReleaseUpdateContractError {
            throw error
        } catch {
            throw ReleaseUpdateContractError.invalidJSON
        }
        guard let object = try JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            throw ReleaseUpdateContractError.invalidJSON
        }
        let allowed = Set(CodingKeys.allCases.map(\.rawValue))
        guard Set(object.keys).isSubset(of: allowed) else {
            throw ReleaseUpdateContractError.unknownField
        }
        let result: Self
        do {
            result = try JSONDecoder().decode(Self.self, from: data)
        } catch {
            throw ReleaseUpdateContractError.invalidJSON
        }
        return try result.validated()
    }

    public var candidateVersion: String? {
        releaseID == nil ? nil : version
    }

    public var canonicalReleaseURL: URL? {
        guard let releaseURL else { return nil }
        return URL(string: releaseURL)
    }

    public static func canonicalReleaseURL(
        version: String,
        channel: ReleaseUpdateChannel
    ) -> URL? {
        guard let parsed = StrictReleaseVersion(version),
              channel != .stable || !parsed.isPrerelease else {
            return nil
        }
        return URL(
            string: "https://github.com/novelKR/OpenCodex-OCI-Gateway/releases/tag/\(version)"
        )
    }

    private func validated() throws -> Self {
        guard schemaVersion == 1,
              StrictReleaseVersion(currentVersion) != nil,
              Self.validRFC3339(checkedAt) else {
            throw ReleaseUpdateContractError.invalidSchema
        }
        let candidateValues: [Any?] = [
            releaseID, tag, version, releaseURL, manifestSHA256, appAssetID, appSHA256,
        ]
        let hasCandidate = candidateValues.contains { $0 != nil }
        if hasCandidate {
            guard candidateValues.allSatisfy({ $0 != nil }),
                  let releaseID, releaseID > 0,
                  let tag, let version, tag == version,
                  let parsedCandidate = StrictReleaseVersion(tag),
                  channel != .stable || !parsedCandidate.isPrerelease,
                  let appAssetID, appAssetID > 0,
                  let releaseURL,
                  releaseURL == "https://github.com/novelKR/OpenCodex-OCI-Gateway/releases/tag/\(tag)",
                  Self.validSHA256(manifestSHA256), Self.validSHA256(appSHA256) else {
                throw ReleaseUpdateContractError.invalidCandidate
            }
        }
        switch status {
        case .updateAvailable, .newerThanSelectedChannel, .updaterTooOld, .unsupportedSystem:
            guard hasCandidate else { throw ReleaseUpdateContractError.invalidCandidate }
        case .offline, .rateLimited, .invalidRelease:
            guard !hasCandidate else { throw ReleaseUpdateContractError.invalidCandidate }
        case .current:
            break
        }
        let compatibilityValues: [Any?] = [
            minimumUpdaterVersion, minimumMacOSVersion, integrationProtocol, helperProtocol, trustKeyID,
        ]
        if compatibilityValues.contains(where: { $0 != nil }) {
            guard hasCandidate,
                  compatibilityValues.allSatisfy({ $0 != nil }),
                  let minimumUpdaterVersion, StrictReleaseVersion(minimumUpdaterVersion) != nil,
                  let minimumMacOSVersion, Self.validNumericVersion(minimumMacOSVersion),
                  let integrationProtocol, integrationProtocol > 0,
                  let helperProtocol, helperProtocol > 0,
                  Self.validSHA256(trustKeyID) else {
                throw ReleaseUpdateContractError.invalidCompatibility
            }
        }
        return self
    }

    private static func validSHA256(_ value: String?) -> Bool {
        guard let value, value.count == 64 else { return false }
        return value.allSatisfy { character in
            guard let ascii = character.asciiValue else { return false }
            return (0x30...0x39).contains(ascii) || (0x61...0x66).contains(ascii)
        }
    }

    private static func validRFC3339(_ value: String) -> Bool {
        guard value.count <= 64 else { return false }
        return ISO8601DateFormatter().date(from: value) != nil
    }

    private static func validNumericVersion(_ value: String) -> Bool {
        let parts = value.split(separator: ".", omittingEmptySubsequences: false)
        guard (1...3).contains(parts.count) else { return false }
        return parts.allSatisfy { part in
            !part.isEmpty && part.count <= 6 && part.allSatisfy(Self.isASCIIDigit) &&
                (part == "0" || part.first != "0")
        }
    }

    private static func isASCIIDigit(_ character: Character) -> Bool {
        guard let ascii = character.asciiValue else { return false }
        return (0x30...0x39).contains(ascii)
    }
}

public enum ReleaseUpdateContractError: Error, Equatable, Sendable {
    case invalidJSON
    case duplicateField
    case unknownField
    case invalidSchema
    case invalidCandidate
    case invalidCompatibility
    case invalidRequest
    case invalidStage
}

private struct FlatJSONKeyScanner {
    private let bytes: [UInt8]
    private var index = 0

    init(data: Data) {
        self.bytes = Array(data)
    }

    mutating func validateUniqueKeys() throws {
        skipWhitespace()
        try consume(ascii: "{")
        skipWhitespace()
        if consumeIf(ascii: "}") {
            skipWhitespace()
            guard index == bytes.count else { throw ReleaseUpdateContractError.invalidJSON }
            return
        }
        var keys = Set<String>()
        while true {
            let key = try scanString()
            guard keys.insert(key).inserted else {
                throw ReleaseUpdateContractError.duplicateField
            }
            skipWhitespace()
            try consume(ascii: ":")
            skipWhitespace()
            try scanScalarValue()
            skipWhitespace()
            if consumeIf(ascii: "}") { break }
            try consume(ascii: ",")
            skipWhitespace()
        }
        skipWhitespace()
        guard index == bytes.count else { throw ReleaseUpdateContractError.invalidJSON }
    }

    private mutating func scanString() throws -> String {
        guard index < bytes.count, bytes[index] == 0x22 else {
            throw ReleaseUpdateContractError.invalidJSON
        }
        let start = index
        index += 1
        var escaped = false
        while index < bytes.count {
            let byte = bytes[index]
            index += 1
            if escaped {
                escaped = false
            } else if byte == 0x5C {
                escaped = true
            } else if byte == 0x22 {
                let slice = Data(bytes[start..<index])
                guard let value = try? JSONDecoder().decode(String.self, from: slice) else {
                    throw ReleaseUpdateContractError.invalidJSON
                }
                return value
            } else if byte < 0x20 {
                throw ReleaseUpdateContractError.invalidJSON
            }
        }
        throw ReleaseUpdateContractError.invalidJSON
    }

    private mutating func scanScalarValue() throws {
        guard index < bytes.count else { throw ReleaseUpdateContractError.invalidJSON }
        if bytes[index] == 0x22 {
            _ = try scanString()
            return
        }
        if bytes[index] == 0x7B || bytes[index] == 0x5B {
            throw ReleaseUpdateContractError.invalidJSON
        }
        let start = index
        while index < bytes.count,
              bytes[index] != 0x2C,
              bytes[index] != 0x7D {
            index += 1
        }
        let token = bytes[start..<index].reversed().drop(while: Self.isWhitespace).reversed()
        guard !token.isEmpty else { throw ReleaseUpdateContractError.invalidJSON }
    }

    private mutating func skipWhitespace() {
        while index < bytes.count, Self.isWhitespace(bytes[index]) {
            index += 1
        }
    }

    private mutating func consume(ascii character: Character) throws {
        guard consumeIf(ascii: character) else { throw ReleaseUpdateContractError.invalidJSON }
    }

    private mutating func consumeIf(ascii character: Character) -> Bool {
        guard let ascii = character.asciiValue,
              index < bytes.count, bytes[index] == ascii else { return false }
        index += 1
        return true
    }

    private static func isWhitespace(_ byte: UInt8) -> Bool {
        byte == 0x20 || byte == 0x09 || byte == 0x0A || byte == 0x0D
    }
}

public protocol ReleaseUpdateChecking: Sendable {
    func check(
        channel: ReleaseUpdateChannel,
        currentVersion: String,
        publicKeyURL: URL
    ) async throws -> ReleaseUpdateCheckResult
}

public struct ProcessReleaseUpdateChecker: ReleaseUpdateChecking, Sendable {
    public let executableURL: URL
    public let executionPolicy: RelayctlExecutionPolicy

    public init(
        executableURL: URL = RelayctlHelperLocation.resolve(),
        executionPolicy: RelayctlExecutionPolicy = RelayctlExecutionPolicy(
            timeout: 45,
            terminationGracePeriod: 0.5,
            maximumOutputBytes: 64 * 1024
        )
    ) {
        self.executableURL = executableURL
        self.executionPolicy = executionPolicy
    }

    public func check(
        channel: ReleaseUpdateChannel,
        currentVersion: String,
        publicKeyURL: URL
    ) async throws -> ReleaseUpdateCheckResult {
        guard StrictReleaseVersion(currentVersion) != nil,
              publicKeyURL.isFileURL,
              publicKeyURL.path.hasPrefix("/") else {
            throw ReleaseUpdateContractError.invalidRequest
        }
        guard executableURL.isFileURL,
              FileManager.default.isExecutableFile(atPath: executableURL.path) else {
            throw RelayctlError.helperUnavailable
        }
        let operation = RelayctlProcessOperation(
            executableURL: executableURL,
            arguments: [
                "release", "check",
                "--channel", channel.rawValue,
                "--current-version", currentVersion,
                "--public-key", publicKeyURL.path,
                "--json",
            ],
            policy: executionPolicy
        )
        let result = try await withTaskCancellationHandler {
            try await Task.detached(priority: .utility) {
                try operation.run()
            }.value
        } onCancel: {
            operation.cancel()
        }
        if Task.isCancelled { throw RelayctlError.cancelled }
        guard result.exitCode == 0 else {
            if let envelope = try? JSONDecoder().decode(RelayctlOperationErrorEnvelope.self, from: result.stdout),
               let code = envelope.reportedCode() {
                throw RelayctlError.reported(code)
            }
            throw RelayctlError.invocationFailed(exitCode: result.exitCode)
        }
        return try ReleaseUpdateCheckResult.decodeStrict(result.stdout)
    }
}

public struct ReleaseUpdateStageReceipt: Codable, Sendable, Equatable {
    public let schemaVersion: Int
    public let releaseID: Int64
    public let tag: String
    public let channel: ReleaseUpdateChannel
    public let manifestSHA256: String
    public let appSHA256: String
    public let bundleFingerprint: String
    public let trustKeyID: String
    public let stagingPath: String
    public let verifiedAt: String

    enum CodingKeys: String, CodingKey, CaseIterable {
        case schemaVersion = "schema_version"
        case releaseID = "release_id"
        case tag
        case channel
        case manifestSHA256 = "manifest_sha256"
        case appSHA256 = "app_sha256"
        case bundleFingerprint = "bundle_fingerprint"
        case trustKeyID = "trust_key_id"
        case stagingPath = "staging_path"
        case verifiedAt = "verified_at"
    }

    public var stagedApplicationURL: URL {
        URL(fileURLWithPath: stagingPath, isDirectory: true)
    }

    public static func decodeStrict(_ data: Data) throws -> Self {
        guard data.count <= 64 * 1024 else { throw ReleaseUpdateContractError.invalidStage }
        do {
            var scanner = FlatJSONKeyScanner(data: data)
            try scanner.validateUniqueKeys()
        } catch let error as ReleaseUpdateContractError {
            throw error
        } catch {
            throw ReleaseUpdateContractError.invalidStage
        }
        guard let object = try JSONSerialization.jsonObject(with: data) as? [String: Any],
              Set(object.keys).isSubset(of: Set(CodingKeys.allCases.map(\.rawValue))) else {
            throw ReleaseUpdateContractError.invalidStage
        }
        let receipt: Self
        do {
            receipt = try JSONDecoder().decode(Self.self, from: data)
        } catch {
            throw ReleaseUpdateContractError.invalidStage
        }
        return try receipt.validated()
    }

    public func validated(for selection: ReleaseUpdateCheckResult) throws -> Self {
        let validated = try validated()
        guard selection.status == .updateAvailable,
              validated.releaseID == selection.releaseID,
              validated.tag == selection.tag,
              validated.channel == selection.channel,
              validated.manifestSHA256 == selection.manifestSHA256,
              validated.appSHA256 == selection.appSHA256,
              validated.trustKeyID == selection.trustKeyID else {
            throw ReleaseUpdateContractError.invalidStage
        }
        return validated
    }

    private func validated() throws -> Self {
        let standardized = URL(fileURLWithPath: stagingPath, isDirectory: true)
            .standardizedFileURL.path
        guard schemaVersion == 1,
              releaseID > 0,
              StrictReleaseVersion(tag) != nil,
              channel != .stable || !tag.contains("-"),
              Self.validSHA256(manifestSHA256),
              Self.validSHA256(appSHA256),
              Self.validSHA256(bundleFingerprint),
              Self.validSHA256(trustKeyID),
              stagingPath.hasPrefix("/"),
              standardized == stagingPath,
              stagingPath.hasSuffix("/OpenCodexRelay.app"),
              ISO8601DateFormatter().date(from: verifiedAt) != nil else {
            throw ReleaseUpdateContractError.invalidStage
        }
        return self
    }

    private static func validSHA256(_ value: String) -> Bool {
        value.count == 64 && value.allSatisfy { character in
            guard let ascii = character.asciiValue else { return false }
            return (0x30...0x39).contains(ascii) || (0x61...0x66).contains(ascii)
        }
    }
}

public protocol ReleaseUpdateStaging: Sendable {
    func stage(
        selection: ReleaseUpdateCheckResult,
        currentVersion: String,
        publicKeyURL: URL
    ) async throws -> ReleaseUpdateStageReceipt
}

public struct ProcessReleaseUpdateStager: ReleaseUpdateStaging, Sendable {
    public let executableURL: URL
    public let executionPolicy: RelayctlExecutionPolicy

    public init(
        executableURL: URL = RelayctlHelperLocation.resolve(),
        executionPolicy: RelayctlExecutionPolicy = RelayctlExecutionPolicy(
            timeout: 300,
            terminationGracePeriod: 0.5,
            maximumOutputBytes: 64 * 1024
        )
    ) {
        self.executableURL = executableURL
        self.executionPolicy = executionPolicy
    }

    public func stage(
        selection: ReleaseUpdateCheckResult,
        currentVersion: String,
        publicKeyURL: URL
    ) async throws -> ReleaseUpdateStageReceipt {
        guard selection.status == .updateAvailable,
              selection.currentVersion == currentVersion,
              let releaseID = selection.releaseID,
              let tag = selection.tag,
              let manifestSHA256 = selection.manifestSHA256,
              selection.minimumUpdaterVersion != nil,
              selection.minimumMacOSVersion != nil,
              selection.trustKeyID != nil,
              StrictReleaseVersion(currentVersion) != nil,
              publicKeyURL.isFileURL,
              publicKeyURL.path.hasPrefix("/") else {
            throw ReleaseUpdateContractError.invalidRequest
        }
        guard executableURL.isFileURL,
              FileManager.default.isExecutableFile(atPath: executableURL.path) else {
            throw RelayctlError.helperUnavailable
        }
        let operation = RelayctlProcessOperation(
            executableURL: executableURL,
            arguments: [
                "release", "stage",
                "--channel", selection.channel.rawValue,
                "--current-version", currentVersion,
                "--release-id", String(releaseID),
                "--tag", tag,
                "--expected-manifest-sha256", manifestSHA256,
                "--public-key", publicKeyURL.path,
                "--json",
            ],
            policy: executionPolicy
        )
        let result = try await withTaskCancellationHandler {
            try await Task.detached(priority: .utility) { try operation.run() }.value
        } onCancel: {
            operation.cancel()
        }
        if Task.isCancelled { throw RelayctlError.cancelled }
        guard result.exitCode == 0 else {
            if let envelope = try? JSONDecoder().decode(
                RelayctlOperationErrorEnvelope.self,
                from: result.stdout
            ), let code = envelope.reportedCode() {
                throw RelayctlError.reported(code)
            }
            throw RelayctlError.invocationFailed(exitCode: result.exitCode)
        }
        return try ReleaseUpdateStageReceipt.decodeStrict(result.stdout).validated(for: selection)
    }
}

public enum ReleaseUpdateTrustKeyLocation {
    public static func resolve(bundle: Bundle = .main) -> URL {
        bundle.bundleURL
            .resolvingSymlinksInPath()
            .appendingPathComponent(
                "Contents/Resources/ReleaseTrust/opencodex-relay-release-ed25519.pub",
                isDirectory: false
            )
    }
}

private struct StrictReleaseVersion {
    let isPrerelease: Bool

    init?(_ value: String) {
        guard !value.isEmpty, !value.contains("+"), !value.hasPrefix("v") else { return nil }
        let pieces = value.split(separator: "-", maxSplits: 1, omittingEmptySubsequences: false)
        let core = pieces[0].split(separator: ".", omittingEmptySubsequences: false)
        guard core.count == 3, core.allSatisfy(Self.validNumeric) else { return nil }
        self.isPrerelease = pieces.count == 2
        if pieces.count == 2 {
            let identifiers = pieces[1].split(separator: ".", omittingEmptySubsequences: false)
            guard !identifiers.isEmpty, identifiers.allSatisfy(Self.validPrerelease) else { return nil }
        }
    }

    private static func validNumeric(_ value: Substring) -> Bool {
        !value.isEmpty && value.allSatisfy { character in
            guard let ascii = character.asciiValue else { return false }
            return (0x30...0x39).contains(ascii)
        } && (value == "0" || value.first != "0")
    }

    private static func validPrerelease(_ value: Substring) -> Bool {
        guard !value.isEmpty,
              value.allSatisfy({ $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "-") }) else {
            return false
        }
        let numeric = value.allSatisfy { character in
            guard let ascii = character.asciiValue else { return false }
            return (0x30...0x39).contains(ascii)
        }
        return !numeric || value == "0" || value.first != "0"
    }
}
