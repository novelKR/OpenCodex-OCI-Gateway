import AppKit
import Foundation
import OpenCodexRelayCore
import Security

struct ReviewedCodexDesktopIdentity: Equatable, Sendable {
    let bundleIdentifier: String
    let teamIdentifier: String
}

struct CodexDesktopTrustPolicy: Equatable, Sendable {
    static let bundleIdentifierInfoKey = "OpenCodexTrustedCodexBundleIdentifier"
    static let teamIdentifierInfoKey = "OpenCodexTrustedCodexTeamIdentifier"

    let bundleIdentifier: String?
    let teamIdentifier: String?

    init(bundleIdentifier: String?, teamIdentifier: String?) {
        self.bundleIdentifier = bundleIdentifier
        self.teamIdentifier = teamIdentifier
    }

    init(infoDictionary: [String: Any]?) {
        self.init(
            bundleIdentifier: infoDictionary?[Self.bundleIdentifierInfoKey] as? String,
            teamIdentifier: infoDictionary?[Self.teamIdentifierInfoKey] as? String
        )
    }

    static var current: CodexDesktopTrustPolicy {
        CodexDesktopTrustPolicy(infoDictionary: Bundle.main.infoDictionary)
    }

    var reviewedIdentity: ReviewedCodexDesktopIdentity? {
        guard let bundleIdentifier = normalizedBundleIdentifier(bundleIdentifier),
              let teamIdentifier = normalizedTeamIdentifier(teamIdentifier) else {
            return nil
        }
        return ReviewedCodexDesktopIdentity(
            bundleIdentifier: bundleIdentifier,
            teamIdentifier: teamIdentifier
        )
    }

    private func normalizedBundleIdentifier(_ value: String?) -> String? {
        guard let value,
              value == value.trimmingCharacters(in: .whitespacesAndNewlines),
              value.count <= 255 else {
            return nil
        }
        let components = value.split(separator: ".", omittingEmptySubsequences: false)
        guard components.count >= 2,
              components.allSatisfy({ component in
                  !component.isEmpty && component.allSatisfy { character in
                      character.isASCII && (character.isLetter || character.isNumber || character == "-")
                  }
              }) else {
            return nil
        }
        return value
    }

    private func normalizedTeamIdentifier(_ value: String?) -> String? {
        guard let value,
              value == value.trimmingCharacters(in: .whitespacesAndNewlines),
              value.count == 10,
              value.allSatisfy({ character in
                  character.isASCII && (character.isNumber || (character.isLetter && character.isUppercase))
              }) else {
            return nil
        }
        return value
    }
}

struct VerifiedCodexDesktop: Equatable, Sendable {
    let url: URL
    let bundleIdentifier: String
    let teamIdentifier: String
}

enum CodexDesktopTrustFailure: String, Equatable, Sendable {
    case configurationMissing = "configuration_missing"
    case unavailable
    case bundleIdentifierMismatch = "bundle_identifier_mismatch"
    case invalidSignature = "invalid_signature"
    case teamIdentifierMismatch = "team_identifier_mismatch"
}

enum CodexDesktopTrustResult: Equatable, Sendable {
    case trusted(VerifiedCodexDesktop)
    case rejected(CodexDesktopTrustFailure)
}

@MainActor
protocol CodexDesktopTrustValidating: AnyObject {
    func verify(_ url: URL, policy: CodexDesktopTrustPolicy) -> CodexDesktopTrustResult
}

struct CodeSignatureEvidence: Equatable, Sendable {
    let signedIdentifier: String?
    let teamIdentifier: String?
}

protocol CodeSignatureInspecting {
    func validatedEvidence(at url: URL) -> CodeSignatureEvidence?
}

struct SecurityCodeSignatureInspector: CodeSignatureInspecting {
    func validatedEvidence(at url: URL) -> CodeSignatureEvidence? {
        var staticCode: SecStaticCode?
        guard SecStaticCodeCreateWithPath(url as CFURL, SecCSFlags(), &staticCode) == errSecSuccess,
              let staticCode else {
            return nil
        }
        let validationFlags = SecCSFlags(rawValue: kSecCSStrictValidate | kSecCSCheckAllArchitectures)
        guard SecStaticCodeCheckValidity(staticCode, validationFlags, nil) == errSecSuccess else {
            return nil
        }
        var signingInformation: CFDictionary?
        guard SecCodeCopySigningInformation(
            staticCode,
            SecCSFlags(rawValue: kSecCSSigningInformation),
            &signingInformation
        ) == errSecSuccess,
            let values = signingInformation as? [String: Any] else {
            return nil
        }
        return CodeSignatureEvidence(
            signedIdentifier: values[kSecCodeInfoIdentifier as String] as? String,
            teamIdentifier: values[kSecCodeInfoTeamIdentifier as String] as? String
        )
    }
}

@MainActor
final class SecurityFrameworkCodexDesktopTrustValidator: CodexDesktopTrustValidating {
    private let signatureInspector: any CodeSignatureInspecting

    init(signatureInspector: any CodeSignatureInspecting = SecurityCodeSignatureInspector()) {
        self.signatureInspector = signatureInspector
    }

    func verify(_ url: URL, policy: CodexDesktopTrustPolicy) -> CodexDesktopTrustResult {
        guard let identity = policy.reviewedIdentity else {
            return .rejected(.configurationMissing)
        }
        guard let resolved = try? DesktopTargetResolver.validate(url) else {
            return .rejected(.unavailable)
        }
        guard Bundle(url: resolved)?.bundleIdentifier == identity.bundleIdentifier else {
            return .rejected(.bundleIdentifierMismatch)
        }
        guard let evidence = signatureInspector.validatedEvidence(at: resolved),
              evidence.signedIdentifier == identity.bundleIdentifier else {
            return .rejected(.invalidSignature)
        }
        guard evidence.teamIdentifier == identity.teamIdentifier else {
            return .rejected(.teamIdentifierMismatch)
        }
        return .trusted(VerifiedCodexDesktop(
            url: resolved.standardizedFileURL,
            bundleIdentifier: identity.bundleIdentifier,
            teamIdentifier: identity.teamIdentifier
        ))
    }
}

@MainActor
protocol CodexDesktopDiscovering: AnyObject {
    func candidates(for bundleIdentifier: String) -> [URL]
}

@MainActor
final class WorkspaceCodexDesktopDiscoverer: CodexDesktopDiscovering {
    func candidates(for bundleIdentifier: String) -> [URL] {
        var candidates: [URL] = []
        if let installed = NSWorkspace.shared.urlForApplication(withBundleIdentifier: bundleIdentifier) {
            candidates.append(installed)
        }
        candidates.append(contentsOf: NSWorkspace.shared.runningApplications.compactMap { application in
            guard application.bundleIdentifier == bundleIdentifier else { return nil }
            return application.bundleURL
        })

        var seen = Set<String>()
        return candidates.compactMap { candidate in
            let resolved = candidate.resolvingSymlinksInPath().standardizedFileURL
            guard seen.insert(resolved.path).inserted else { return nil }
            return resolved
        }.sorted { $0.path < $1.path }
    }
}
