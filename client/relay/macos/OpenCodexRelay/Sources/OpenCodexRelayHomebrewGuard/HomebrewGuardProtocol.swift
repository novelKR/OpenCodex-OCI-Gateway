import Foundation

public let homebrewGuardProtocolVersion = 1
public let homebrewGuardMaximumMessageBytes = 64 * 1024

public enum HomebrewGuardDistribution: String, Codable, Sendable, CaseIterable {
    case production
    case localDevelopment = "local_development"
}

public enum HomebrewGuardOperation: String, Codable, Sendable, CaseIterable {
    case status
    case prepare
    case commit
    case release
    case recover
}

public enum HomebrewGuardErrorCode: String, Codable, Error, Sendable, CaseIterable {
    case homebrewGuardNotRegistered = "homebrew_guard_not_registered"
    case approvalRequired = "approval_required"
    case busy
    case candidateChanged = "candidate_changed"
    case protectionFailed = "protection_failed"
    case recoveryRequired = "recovery_required"
    case restoreFailed = "restore_failed"
}

public enum HomebrewGuardState: String, Codable, Sendable, CaseIterable {
    case notRegistered = "not_registered"
    case approvalRequired = "approval_required"
    case ready
    case prepared
    case committed
    case recoveryRequired = "recovery_required"
    case unavailable
}

public enum HomebrewGuardResultCode: String, Codable, Sendable, CaseIterable {
    case statusReady = "status_ready"
    case candidateReady = "candidate_ready"
    case prepared
    case committed
    case released
    case recovered
    case failed
}

public struct HomebrewGuardCandidate: Codable, Equatable, Sendable {
    public let installationID: String
    public let installationFingerprint: String
    public let prefix: String
    public let packageRoot: String
    public let executable: String
    public let cliEntry: String
    public let bunExecutable: String
    public let nodeExecutable: String
    public let npmCLI: String
    public let launchers: [String]

    public init(
        installationID: String,
        installationFingerprint: String,
        prefix: String,
        packageRoot: String,
        executable: String,
        cliEntry: String,
        bunExecutable: String,
        nodeExecutable: String,
        npmCLI: String,
        launchers: [String]
    ) {
        self.installationID = installationID
        self.installationFingerprint = installationFingerprint
        self.prefix = prefix
        self.packageRoot = packageRoot
        self.executable = executable
        self.cliEntry = cliEntry
        self.bunExecutable = bunExecutable
        self.nodeExecutable = nodeExecutable
        self.npmCLI = npmCLI
        self.launchers = launchers
    }

    enum CodingKeys: String, CodingKey {
        case installationID = "installation_id"
        case installationFingerprint = "installation_fingerprint"
        case prefix
        case packageRoot = "package_root"
        case executable
        case cliEntry = "cli_entry"
        case bunExecutable = "bun_executable"
        case nodeExecutable = "node_executable"
        case npmCLI = "npm_cli"
        case launchers
    }

    public func validated(allowedRoot: String) throws -> HomebrewGuardCandidate {
        let root = URL(fileURLWithPath: allowedRoot).standardizedFileURL.path
        guard Self.isLowerHex(installationID, count: 24),
              Self.isLowerHex(installationFingerprint, count: 64),
              Self.isCanonical(prefix), prefix == root,
              packageRoot == root + "/lib/node_modules/@bitkyc08/opencodex",
              launchers.count <= 2 else {
            throw HomebrewGuardErrorCode.candidateChanged
        }
        let criticalPaths = [packageRoot, executable, cliEntry, bunExecutable, nodeExecutable, npmCLI]
        guard criticalPaths.allSatisfy({ Self.isCanonical($0) && Self.isContained($0, by: root) }),
              executable.hasPrefix(packageRoot + "/"),
              cliEntry == packageRoot + "/src/cli/index.ts",
              bunExecutable.hasPrefix(packageRoot + "/node_modules/bun/bin/"),
              launchers.allSatisfy({
                  $0 == root + "/bin/ocx" || $0 == root + "/bin/opencodex"
              }) else {
            throw HomebrewGuardErrorCode.candidateChanged
        }
        return self
    }

    public var criticalFiles: [String] {
        [executable, cliEntry, bunExecutable, nodeExecutable, npmCLI]
    }

    private static func isCanonical(_ value: String) -> Bool {
        guard value.first == "/", value.utf8.count <= 4_096, !value.contains("\0") else { return false }
        return URL(fileURLWithPath: value).standardizedFileURL.path == value
    }

    private static func isContained(_ path: String, by root: String) -> Bool {
        path == root || path.hasPrefix(root + "/")
    }

    private static func isLowerHex(_ value: String, count: Int) -> Bool {
        value.utf8.count == count && value.utf8.allSatisfy {
            ($0 >= 48 && $0 <= 57) || ($0 >= 97 && $0 <= 102)
        }
    }
}

public struct HomebrewGuardRequest: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let distribution: HomebrewGuardDistribution
    public let operationID: String?
    public let candidate: HomebrewGuardCandidate?

    public init(
        distribution: HomebrewGuardDistribution,
        operationID: String? = nil,
        candidate: HomebrewGuardCandidate? = nil
    ) {
        self.schemaVersion = homebrewGuardProtocolVersion
        self.distribution = distribution
        self.operationID = operationID
        self.candidate = candidate
    }

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case distribution
        case operationID = "operation_id"
        case candidate
    }

    public func validated(for operation: HomebrewGuardOperation, allowedRoot: String) throws -> HomebrewGuardRequest {
        guard schemaVersion == homebrewGuardProtocolVersion else {
            throw HomebrewGuardErrorCode.protectionFailed
        }
        if let operationID {
            guard UUID(uuidString: operationID)?.uuidString.lowercased() == operationID else {
                throw HomebrewGuardErrorCode.candidateChanged
            }
        }
        switch operation {
        case .status:
            guard operationID == nil else { throw HomebrewGuardErrorCode.candidateChanged }
            if let candidate { _ = try candidate.validated(allowedRoot: allowedRoot) }
        case .prepare:
            guard operationID != nil, let candidate else { throw HomebrewGuardErrorCode.candidateChanged }
            _ = try candidate.validated(allowedRoot: allowedRoot)
        case .commit, .release, .recover:
            guard operationID != nil, candidate == nil else { throw HomebrewGuardErrorCode.candidateChanged }
        }
        return self
    }
}

public struct HomebrewGuardResponse: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let protocolVersion: Int
    public let helperVersion: String
    public let state: HomebrewGuardState
    public let resultCode: HomebrewGuardResultCode
    public let errorCode: HomebrewGuardErrorCode?
    public let operationID: String?

    public init(
        helperVersion: String,
        state: HomebrewGuardState,
        resultCode: HomebrewGuardResultCode,
        errorCode: HomebrewGuardErrorCode? = nil,
        operationID: String? = nil
    ) {
        self.schemaVersion = homebrewGuardProtocolVersion
        self.protocolVersion = homebrewGuardProtocolVersion
        self.helperVersion = helperVersion
        self.state = state
        self.resultCode = resultCode
        self.errorCode = errorCode
        self.operationID = operationID
    }

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case protocolVersion = "protocol_version"
        case helperVersion = "helper_version"
        case state
        case resultCode = "result_code"
        case errorCode = "error_code"
        case operationID = "operation_id"
    }

    public func validated() throws -> HomebrewGuardResponse {
        guard schemaVersion == homebrewGuardProtocolVersion,
              protocolVersion == homebrewGuardProtocolVersion,
              Self.isVersion(helperVersion),
              (errorCode == nil) == (resultCode != .failed),
              operationID.map({ UUID(uuidString: $0)?.uuidString.lowercased() == $0 }) ?? true else {
            throw HomebrewGuardErrorCode.protectionFailed
        }
        if state == .prepared || state == .committed || state == .recoveryRequired {
            guard operationID != nil else { throw HomebrewGuardErrorCode.protectionFailed }
        }
        return self
    }

    private static func isVersion(_ value: String) -> Bool {
        guard !value.isEmpty, value.utf8.count <= 64, !value.contains("\n"), !value.contains("\r") else {
            return false
        }
        return value == "dev" || value.range(
            of: #"^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$"#,
            options: .regularExpression
        ) != nil
    }
}

public enum HomebrewGuardCodec {
    public static func encode<T: Encodable>(_ value: T) throws -> Data {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        let data = try encoder.encode(value)
        guard !data.isEmpty, data.count <= homebrewGuardMaximumMessageBytes else {
            throw HomebrewGuardErrorCode.protectionFailed
        }
        return data
    }

    public static func decodeRequest(
        _ data: Data,
        operation: HomebrewGuardOperation,
        allowedRoot: String
    ) throws -> HomebrewGuardRequest {
        guard !data.isEmpty, data.count <= homebrewGuardMaximumMessageBytes else {
            throw HomebrewGuardErrorCode.protectionFailed
        }
        try requireKeys(data, allowed: ["schema_version", "distribution", "operation_id", "candidate"])
        let request = try JSONDecoder().decode(HomebrewGuardRequest.self, from: data)
        return try request.validated(for: operation, allowedRoot: allowedRoot)
    }

    public static func decodeResponse(_ data: Data) throws -> HomebrewGuardResponse {
        guard !data.isEmpty, data.count <= homebrewGuardMaximumMessageBytes else {
            throw HomebrewGuardErrorCode.protectionFailed
        }
        try requireKeys(data, allowed: [
            "schema_version", "protocol_version", "helper_version", "state",
            "result_code", "error_code", "operation_id",
        ])
        return try JSONDecoder().decode(HomebrewGuardResponse.self, from: data).validated()
    }

    private static func requireKeys(_ data: Data, allowed: Set<String>) throws {
        guard let object = try JSONSerialization.jsonObject(with: data) as? [String: Any],
              Set(object.keys).isSubset(of: allowed) else {
            throw HomebrewGuardErrorCode.protectionFailed
        }
        if let candidate = object["candidate"] {
            guard let dictionary = candidate as? [String: Any],
                  Set(dictionary.keys) == [
                    "installation_id", "installation_fingerprint", "prefix", "package_root",
                    "executable", "cli_entry", "bun_executable", "node_executable", "npm_cli", "launchers",
                  ] else {
                throw HomebrewGuardErrorCode.protectionFailed
            }
        }
    }
}

@objc public protocol HomebrewGuardXPCProtocol: NSObjectProtocol {
    func status(_ request: Data, withReply reply: @escaping (Data) -> Void)
    func prepare(_ request: Data, withReply reply: @escaping (Data) -> Void)
    func commit(_ request: Data, withReply reply: @escaping (Data) -> Void)
    func release(_ request: Data, withReply reply: @escaping (Data) -> Void)
    func recover(_ request: Data, withReply reply: @escaping (Data) -> Void)
}
