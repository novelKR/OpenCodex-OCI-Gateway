import Foundation
import XCTest
@testable import OpenCodexRelay

@MainActor
final class CodexDesktopTrustTests: XCTestCase {
    private struct SignatureInspector: CodeSignatureInspecting {
        let evidence: CodeSignatureEvidence?

        func validatedEvidence(at _: URL) -> CodeSignatureEvidence? {
            evidence
        }
    }

    func testReviewedIdentityRequiresExactBundleAndUppercaseTenCharacterTeamID() {
        XCTAssertNil(CodexDesktopTrustPolicy(bundleIdentifier: nil, teamIdentifier: nil).reviewedIdentity)
        XCTAssertNil(CodexDesktopTrustPolicy(
            bundleIdentifier: "com.example.codex",
            teamIdentifier: "abcde12345"
        ).reviewedIdentity)
        XCTAssertNil(CodexDesktopTrustPolicy(
            bundleIdentifier: "com..example",
            teamIdentifier: "ABCDE12345"
        ).reviewedIdentity)

        XCTAssertEqual(
            CodexDesktopTrustPolicy(
                bundleIdentifier: "com.example.codex",
                teamIdentifier: "ABCDE12345"
            ).reviewedIdentity,
            ReviewedCodexDesktopIdentity(
                bundleIdentifier: "com.example.codex",
                teamIdentifier: "ABCDE12345"
            )
        )
    }

    func testValidatorRequiresBundleSignatureIdentifierAndTeamIDToAllMatch() throws {
        let appURL = try makeBundle(identifier: "com.example.codex")
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let policy = CodexDesktopTrustPolicy(
            bundleIdentifier: "com.example.codex",
            teamIdentifier: "ABCDE12345"
        )

        let trusted = SecurityFrameworkCodexDesktopTrustValidator(signatureInspector: SignatureInspector(
            evidence: CodeSignatureEvidence(
                signedIdentifier: "com.example.codex",
                teamIdentifier: "ABCDE12345"
            )
        ))
        XCTAssertEqual(
            trusted.verify(appURL, policy: policy),
            .trusted(VerifiedCodexDesktop(
                url: appURL.resolvingSymlinksInPath().standardizedFileURL,
                bundleIdentifier: "com.example.codex",
                teamIdentifier: "ABCDE12345"
            ))
        )

        let wrongSignedIdentifier = SecurityFrameworkCodexDesktopTrustValidator(signatureInspector: SignatureInspector(
            evidence: CodeSignatureEvidence(
                signedIdentifier: "com.example.lookalike",
                teamIdentifier: "ABCDE12345"
            )
        ))
        XCTAssertEqual(
            wrongSignedIdentifier.verify(appURL, policy: policy),
            .rejected(.invalidSignature)
        )

        let wrongTeam = SecurityFrameworkCodexDesktopTrustValidator(signatureInspector: SignatureInspector(
            evidence: CodeSignatureEvidence(
                signedIdentifier: "com.example.codex",
                teamIdentifier: "ZZZZZ99999"
            )
        ))
        XCTAssertEqual(wrongTeam.verify(appURL, policy: policy), .rejected(.teamIdentifierMismatch))

        let invalidSignature = SecurityFrameworkCodexDesktopTrustValidator(
            signatureInspector: SignatureInspector(evidence: nil)
        )
        XCTAssertEqual(invalidSignature.verify(appURL, policy: policy), .rejected(.invalidSignature))
    }

    func testValidatorRejectsInfoPlistBundleMismatchBeforeSignatureEvidence() throws {
        let appURL = try makeBundle(identifier: "com.example.lookalike")
        defer { try? FileManager.default.removeItem(at: appURL.deletingLastPathComponent()) }
        let validator = SecurityFrameworkCodexDesktopTrustValidator(signatureInspector: SignatureInspector(
            evidence: CodeSignatureEvidence(
                signedIdentifier: "com.example.codex",
                teamIdentifier: "ABCDE12345"
            )
        ))

        XCTAssertEqual(
            validator.verify(
                appURL,
                policy: CodexDesktopTrustPolicy(
                    bundleIdentifier: "com.example.codex",
                    teamIdentifier: "ABCDE12345"
                )
            ),
            .rejected(.bundleIdentifierMismatch)
        )
    }

    private func makeBundle(identifier: String) throws -> URL {
        let root = URL(fileURLWithPath: NSTemporaryDirectory(), isDirectory: true)
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        let appURL = root.appendingPathComponent("Codex.app", isDirectory: true)
        let contents = appURL.appendingPathComponent("Contents", isDirectory: true)
        try FileManager.default.createDirectory(at: contents, withIntermediateDirectories: true)
        let plist: [String: Any] = [
            "CFBundleIdentifier": identifier,
            "CFBundlePackageType": "APPL",
            "CFBundleName": "Codex",
        ]
        let payload = try PropertyListSerialization.data(
            fromPropertyList: plist,
            format: .xml,
            options: 0
        )
        try payload.write(to: contents.appendingPathComponent("Info.plist"), options: .atomic)
        return appURL
    }
}
