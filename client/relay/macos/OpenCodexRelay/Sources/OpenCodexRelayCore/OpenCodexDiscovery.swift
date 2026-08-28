import Foundation

public enum OpenCodexDiscoveryTier: String, Codable, CaseIterable, Sendable {
    case a
    case b
    case c
}

public enum OpenCodexPackageManager: String, Codable, Sendable {
    case npm
    case homebrew
    case nvm
    case fnm
    case volta
    case asdf
}

public enum OpenCodexRemovalCapability: String, Codable, Sendable {
    case exactNPM = "exact_npm"
    case homebrewGuardedNPM = "homebrew_guarded_npm"
    case volta
    case manual
}

public enum OpenCodexRemovalAuthority: String, Codable, Sendable {
    case automatic
    case manual
}

public enum OpenCodexTeardownCapability: String, Codable, Sendable {
    case none
    case relayPreserveV1 = "relay_preserve_v1"
}

public enum OpenCodexDataCapability: String, Codable, Sendable {
    case preserveOnly = "preserve_only"
    case selectiveTrashV1 = "selective_trash_v1"
}

public enum OpenCodexNativeRestoreCapability: String, Codable, Sendable {
    case verifiedSnapshot = "verified_snapshot"
}

public struct OpenCodexDiscoveryCoverage: Codable, Equatable, Sendable {
    public let source: String
    public let root: String
    public let state: String
}

public struct OpenCodexInstallationCandidate: Codable, Equatable, Identifiable, Sendable {
    public let id: String
    public let tier: OpenCodexDiscoveryTier
    public let source: String
    public let manager: OpenCodexPackageManager
    public let prefix: String
    public let packageRoot: String
    public let version: String
    public let executable: String
    public let executableSHA256: String
    public let cliEntry: String?
    public let cliEntrySHA256: String?
    public let bunExecutable: String?
    public let bunSHA256: String?
    public let packageTreeSHA256: String?
    public let npmTreeSHA256: String?
    public let launchers: [String]
    public let nodeExecutable: String?
    public let nodeSHA256: String?
    public let npmCLI: String?
    public let npmCLISHA256: String?
    public let confidence: String
    public let removalCapability: OpenCodexRemovalCapability
    public let removalAuthority: OpenCodexRemovalAuthority
    public var homebrewGuardRequired: Bool? = nil
    public var teardownCapability: OpenCodexTeardownCapability? = nil
    public var dataCapability: OpenCodexDataCapability? = nil
    public var teardownCompatibilityReason: String? = nil
    public var teardownAdapterID: String? = nil
    public let nativeRestoreCapability: OpenCodexNativeRestoreCapability?
    public let nativeRestoreFingerprint: String?
    public let userWritable: Bool
    public let requiresElevation: Bool
    public let fingerprint: String
    public let warnings: [String]

    enum CodingKeys: String, CodingKey {
        case id
        case tier
        case source
        case manager
        case prefix
        case packageRoot = "package_root"
        case version
        case executable
        case executableSHA256 = "executable_sha256"
        case cliEntry = "cli_entry"
        case cliEntrySHA256 = "cli_entry_sha256"
        case bunExecutable = "bun_executable"
        case bunSHA256 = "bun_sha256"
        case packageTreeSHA256 = "package_tree_sha256"
        case npmTreeSHA256 = "npm_tree_sha256"
        case launchers
        case nodeExecutable = "node_executable"
        case nodeSHA256 = "node_sha256"
        case npmCLI = "npm_cli"
        case npmCLISHA256 = "npm_cli_sha256"
        case confidence
        case removalCapability = "removal_capability"
        case removalAuthority = "removal_authority"
        case homebrewGuardRequired = "homebrew_guard_required"
        case teardownCapability = "teardown_capability"
        case dataCapability = "data_capability"
        case teardownCompatibilityReason = "teardown_compatibility_reason"
        case teardownAdapterID = "teardown_adapter_id"
        case nativeRestoreCapability = "native_restore_capability"
        case nativeRestoreFingerprint = "native_restore_fingerprint"
        case userWritable = "user_writable"
        case requiresElevation = "requires_elevation"
        case fingerprint
        case warnings
    }

    public var handoffExecutable: OpenCodexExecutable? {
        try? OpenCodexExecutable(path: executable, sha256: executableSHA256)
    }

    public var nativeRepairSelection: OpenCodexNativeRepairSelection? {
        try? OpenCodexNativeRepairSelection(candidate: self)
    }

    public var requiresHomebrewGuard: Bool {
        homebrewGuardRequired == true
    }

    public var isAutomaticRemovalEligible: Bool {
        let automaticCapability = removalCapability == .exactNPM ||
            removalCapability == .homebrewGuardedNPM
        guard tier == .a || tier == .b,
              automaticCapability,
              removalAuthority == .automatic,
              userWritable,
              !requiresElevation,
              manager != .volta,
              nodeExecutable != nil,
              nodeSHA256 != nil,
              npmCLI != nil,
              npmCLISHA256 != nil,
              cliEntry != nil,
              cliEntrySHA256 != nil,
              bunExecutable != nil,
              bunSHA256 != nil,
              packageTreeSHA256 != nil,
              npmTreeSHA256 != nil,
              teardownCapability == .relayPreserveV1,
              dataCapability == .preserveOnly || dataCapability == .selectiveTrashV1,
              teardownCompatibilityReason == "compatible",
              teardownAdapterID.map(Self.isSafeToken) == true else {
            return false
        }
        if removalCapability == .homebrewGuardedNPM {
            guard requiresHomebrewGuard,
                  manager == .homebrew,
                  prefix == "/opt/homebrew",
                  packageRoot == "/opt/homebrew/lib/node_modules/@bitkyc08/opencodex",
                  warnings.contains("homebrew_guard_required") else {
                return false
            }
        } else if requiresHomebrewGuard {
            return false
        }
        return !warnings.contains(where: Self.isBlockingRemovalWarning)
    }

    fileprivate func validated(schemaVersion: Int) throws -> OpenCodexInstallationCandidate {
        guard Self.isLowercaseHex(id, count: 24),
              Self.isLowercaseHex(executableSHA256, count: 64),
              Self.isLowercaseHex(fingerprint, count: 64),
              Self.isSafeToken(source),
              Self.isSafeToken(confidence),
              !version.isEmpty,
              version.utf8.count <= 128,
              launchers.count <= 4,
              warnings.count <= 16,
              Self.isCanonicalAbsolutePath(prefix),
              Self.isCanonicalAbsolutePath(packageRoot),
              Self.isCanonicalAbsolutePath(executable),
              packageRoot.hasSuffix("/lib/node_modules/@bitkyc08/opencodex"),
              executable.hasPrefix(packageRoot + "/"),
              handoffExecutable != nil,
              launchers.allSatisfy(Self.isCanonicalAbsolutePath),
              warnings.allSatisfy(Self.isSafeToken),
              Self.validPathHashPair(path: nodeExecutable, hash: nodeSHA256),
              Self.validPathHashPair(path: npmCLI, hash: npmCLISHA256),
              Self.validPathHashPair(path: cliEntry, hash: cliEntrySHA256),
              Self.validPathHashPair(path: bunExecutable, hash: bunSHA256),
              packageTreeSHA256.map({ Self.isLowercaseHex($0, count: 64) }) ?? true,
              npmTreeSHA256.map({ Self.isLowercaseHex($0, count: 64) }) ?? true,
              (schemaVersion == 2 || schemaVersion == 3 || schemaVersion == 4 || schemaVersion == 5),
              (schemaVersion >= 3 ? homebrewGuardRequired != nil : homebrewGuardRequired != true),
              Self.validNativeRestoreProof(
                  capability: nativeRestoreCapability,
                  fingerprint: nativeRestoreFingerprint
              ) else {
            throw RelayctlError.invalidStatus
        }
        if schemaVersion == 4 || schemaVersion == 5 {
            guard let teardownCapability,
                  (schemaVersion == 4
                    ? dataCapability == .preserveOnly
                    : dataCapability == .preserveOnly || dataCapability == .selectiveTrashV1),
                  let teardownCompatibilityReason,
                  Self.isSafeToken(teardownCompatibilityReason) else {
                throw RelayctlError.invalidStatus
            }
            switch teardownCapability {
            case .relayPreserveV1:
                guard teardownCompatibilityReason == "compatible",
                      teardownAdapterID.map(Self.isSafeToken) == true else {
                    throw RelayctlError.invalidStatus
                }
            case .none:
                guard teardownCompatibilityReason != "compatible",
                      teardownAdapterID == nil else {
                    throw RelayctlError.invalidStatus
                }
            }
        } else {
            guard teardownCapability == nil,
                  dataCapability == nil,
                  teardownCompatibilityReason == nil,
                  teardownAdapterID == nil else {
                throw RelayctlError.invalidStatus
            }
        }
        if removalCapability != .manual {
            guard nodeExecutable != nil, npmCLI != nil, !requiresElevation else {
                throw RelayctlError.invalidStatus
            }
        }
        if removalCapability == .exactNPM || removalCapability == .homebrewGuardedNPM {
            guard cliEntry != nil,
                  bunExecutable != nil,
                  packageTreeSHA256 != nil,
                  npmTreeSHA256 != nil else {
                throw RelayctlError.invalidStatus
            }
            if removalCapability == .homebrewGuardedNPM {
                    guard schemaVersion == 3 || schemaVersion == 4 || schemaVersion == 5,
                      homebrewGuardRequired == true,
                      manager == .homebrew,
                      prefix == "/opt/homebrew",
                      packageRoot == "/opt/homebrew/lib/node_modules/@bitkyc08/opencodex",
                      warnings.contains("homebrew_guard_required") else {
                    throw RelayctlError.invalidStatus
                }
            } else if homebrewGuardRequired == true {
                throw RelayctlError.invalidStatus
            }
        }
        return self
    }

    private static func validNativeRestoreProof(
        capability: OpenCodexNativeRestoreCapability?,
        fingerprint: String?
    ) -> Bool {
        switch (capability, fingerprint) {
        case (nil, nil):
            return true
        case (.verifiedSnapshot?, let fingerprint?):
            return isLowercaseHex(fingerprint, count: 64)
        default:
            return false
        }
    }

    private static func validPathHashPair(path: String?, hash: String?) -> Bool {
        switch (path, hash) {
        case (nil, nil):
            return true
        case let (.some(path), .some(hash)):
            return isCanonicalAbsolutePath(path) && isLowercaseHex(hash, count: 64)
        default:
            return false
        }
    }

    private static func isBlockingRemovalWarning(_ warning: String) -> Bool {
        warning == "writable_parent_chain" ||
            warning == "exact_npm_pair_unavailable" ||
            warning == "execution_closure_unavailable" ||
            warning == "extended_acl" ||
            warning == "external_launcher_requires_manual_removal" ||
            warning.hasPrefix("launcher_mismatch_") ||
            warning == "launcher_evidence_truncated" ||
            warning == "package_identity_conflict"
    }

    fileprivate static func isCanonicalAbsolutePath(_ value: String) -> Bool {
        guard value.utf8.count <= 4_096, value.first == "/", !value.contains("\0") else { return false }
        return URL(fileURLWithPath: value).standardizedFileURL.path == value
    }

    private static func isLowercaseHex(_ value: String, count: Int) -> Bool {
        value.utf8.count == count && value.utf8.allSatisfy {
            ($0 >= 48 && $0 <= 57) || ($0 >= 97 && $0 <= 102)
        }
    }

    fileprivate static func isSafeToken(_ value: String) -> Bool {
        let bytes = Array(value.utf8)
        return !bytes.isEmpty && bytes.count <= 64 && bytes.allSatisfy {
            ($0 >= 48 && $0 <= 57) || ($0 >= 97 && $0 <= 122) || $0 == 95 || $0 == 45
        }
    }
}

public struct OpenCodexDiscoveryResult: Codable, Equatable, Sendable {
    public let schemaVersion: Int
    public let requestedTier: OpenCodexDiscoveryTier
    public let broadScanApproved: Bool
    public let candidates: [OpenCodexInstallationCandidate]
    public let coverage: [OpenCodexDiscoveryCoverage]
    public let rejected: Int
    public let truncated: Bool

    enum CodingKeys: String, CodingKey {
        case schemaVersion = "schema_version"
        case requestedTier = "requested_tier"
        case broadScanApproved = "broad_scan_approved"
        case candidates
        case coverage
        case rejected
        case truncated
    }

    public func validated() throws -> OpenCodexDiscoveryResult {
        guard schemaVersion == 2 || schemaVersion == 3 || schemaVersion == 4 || schemaVersion == 5,
              candidates.count <= 128,
              coverage.count <= 64,
              rejected >= 0,
              (requestedTier == .c) == broadScanApproved else {
            throw RelayctlError.invalidStatus
        }
        let incomplete = truncated ||
            rejected != 0 ||
            coverage.contains { $0.state == "refused" || $0.state == "truncated" }
        var ids = Set<String>()
        var roots = Set<String>()
        var coverageKeys = Set<String>()
        for candidate in candidates {
            _ = try candidate.validated(schemaVersion: schemaVersion)
            guard Self.rank(candidate.tier) <= Self.rank(requestedTier),
                  ids.insert(candidate.id).inserted,
                  roots.insert(candidate.packageRoot).inserted else {
                throw RelayctlError.invalidStatus
            }
            // Authority is an explicit projection from a complete sanitized
            // pass.  A v2 payload that claims automatic authority while its
            // evidence is incomplete, or while the candidate itself fails
            // the local automatic-removal contract, is invalid rather than a
            // usable manual fallback.
            if candidate.removalAuthority == .automatic &&
                (incomplete || (schemaVersion == 4 && !candidate.isAutomaticRemovalEligible)) {
                throw RelayctlError.invalidStatus
            }
        }
        for item in coverage {
            guard OpenCodexInstallationCandidate.isSafeToken(item.source),
                  OpenCodexInstallationCandidate.isCanonicalAbsolutePath(item.root),
                  coverageKeys.insert(item.source + "\u{0}" + item.root).inserted,
                  item.state == "scanned" || item.state == "absent" || item.state == "refused" || item.state == "truncated" else {
                throw RelayctlError.invalidStatus
            }
        }
        return self
    }

    private static func rank(_ tier: OpenCodexDiscoveryTier) -> Int {
        switch tier {
        case .a: 0
        case .b: 1
        case .c: 2
        }
    }
}

public protocol OpenCodexDiscovering: Sendable {
    func discover(tier: OpenCodexDiscoveryTier, broadScanApproved: Bool) async throws -> OpenCodexDiscoveryResult
}
