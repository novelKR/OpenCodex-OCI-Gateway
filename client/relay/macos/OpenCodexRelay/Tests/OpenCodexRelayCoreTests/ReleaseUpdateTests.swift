import Foundation
import XCTest
@testable import OpenCodexRelayCore

final class ReleaseUpdateTests: XCTestCase {
    func testSharedUpdateAvailableFixtureDecodesStrictly() throws {
        let result = try ReleaseUpdateCheckResult.decodeStrict(Data(contentsOf: fixtureURL))
        XCTAssertEqual(result.schemaVersion, 1)
        XCTAssertEqual(result.status, .updateAvailable)
        XCTAssertEqual(result.channel, .preview)
        XCTAssertEqual(result.currentVersion, "0.3.8-rc.5")
        XCTAssertEqual(result.candidateVersion, "0.3.8-rc.6")
        XCTAssertEqual(result.releaseID, 42)
        XCTAssertEqual(
            result.canonicalReleaseURL?.absoluteString,
            "https://github.com/novelKR/OpenCodex-OCI-Gateway/releases/tag/0.3.8-rc.6"
        )
    }

    func testStrictDecoderRejectsUnknownFieldsAndMalformedCandidate() throws {
        var object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: Data(contentsOf: fixtureURL)) as? [String: Any]
        )
        object["unknown"] = true
        XCTAssertThrowsError(try ReleaseUpdateCheckResult.decodeStrict(JSONSerialization.data(withJSONObject: object))) {
            XCTAssertEqual($0 as? ReleaseUpdateContractError, .unknownField)
        }

        object.removeValue(forKey: "unknown")
        object["release_url"] = "https://example.invalid/0.3.8-rc.6"
        XCTAssertThrowsError(try ReleaseUpdateCheckResult.decodeStrict(JSONSerialization.data(withJSONObject: object))) {
            XCTAssertEqual($0 as? ReleaseUpdateContractError, .invalidCandidate)
        }
    }

    func testStrictDecoderRejectsUnknownStatusAndUnsupportedSchema() throws {
        var object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: Data(contentsOf: fixtureURL)) as? [String: Any]
        )
        object["status"] = "downloading"
        XCTAssertThrowsError(try ReleaseUpdateCheckResult.decodeStrict(JSONSerialization.data(withJSONObject: object))) {
            XCTAssertEqual($0 as? ReleaseUpdateContractError, .invalidJSON)
        }
        object["status"] = "update_available"
        object["schema_version"] = 2
        XCTAssertThrowsError(try ReleaseUpdateCheckResult.decodeStrict(JSONSerialization.data(withJSONObject: object))) {
            XCTAssertEqual($0 as? ReleaseUpdateContractError, .invalidSchema)
        }
    }

    func testStrictDecoderRejectsDuplicateFields() {
        let duplicate = Data(#"{"schema_version":1,"schema_version":1}"#.utf8)
        XCTAssertThrowsError(try ReleaseUpdateCheckResult.decodeStrict(duplicate)) {
            XCTAssertEqual($0 as? ReleaseUpdateContractError, .duplicateField)
        }
    }

    func testStrictDecoderRequiresCandidateForAvailableStatusAndStableTagMatch() throws {
        var object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: Data(contentsOf: fixtureURL)) as? [String: Any]
        )
        for key in [
            "release_id", "tag", "version", "release_url", "manifest_sha256",
            "app_asset_id", "app_sha256",
        ] {
            object.removeValue(forKey: key)
        }
        XCTAssertThrowsError(try ReleaseUpdateCheckResult.decodeStrict(JSONSerialization.data(withJSONObject: object))) {
            XCTAssertEqual($0 as? ReleaseUpdateContractError, .invalidCandidate)
        }

        object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: Data(contentsOf: fixtureURL)) as? [String: Any]
        )
        object["channel"] = "stable"
        XCTAssertThrowsError(try ReleaseUpdateCheckResult.decodeStrict(JSONSerialization.data(withJSONObject: object))) {
            XCTAssertEqual($0 as? ReleaseUpdateContractError, .invalidCandidate)
        }

        object["channel"] = "preview"
        object["app_sha256"] = String(repeating: "١", count: 64)
        XCTAssertThrowsError(try ReleaseUpdateCheckResult.decodeStrict(JSONSerialization.data(withJSONObject: object))) {
            XCTAssertEqual($0 as? ReleaseUpdateContractError, .invalidCandidate)
        }
    }

    func testTrustKeyLocationIsFixedInsideBundleResources() {
        let resolved = ReleaseUpdateTrustKeyLocation.resolve(bundle: .main)
        XCTAssertTrue(
            resolved.path.hasSuffix(
                "/Contents/Resources/ReleaseTrust/opencodex-relay-release-ed25519.pub"
            )
        )
    }

    func testCanonicalReleaseURLRequiresStrictChannelCompatibleVersion() {
        XCTAssertEqual(
            ReleaseUpdateCheckResult.canonicalReleaseURL(
                version: "0.3.8-rc.6",
                channel: .preview
            )?.absoluteString,
            "https://github.com/novelKR/OpenCodex-OCI-Gateway/releases/tag/0.3.8-rc.6"
        )
        XCTAssertNil(
            ReleaseUpdateCheckResult.canonicalReleaseURL(
                version: "0.3.8-rc.6",
                channel: .stable
            )
        )
        XCTAssertNil(
            ReleaseUpdateCheckResult.canonicalReleaseURL(
                version: "v0.3.8",
                channel: .preview
            )
        )
    }

    func testProcessCheckerUsesExactPublicCommandContract() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("release-update-process.\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let script = directory.appendingPathComponent("relayctl", isDirectory: false)
        let trustKey = directory.appendingPathComponent("trust.pub", isDirectory: false)
        try Data().write(to: trustKey)
        let escapedFixture = fixtureURL.path.replacingOccurrences(of: "'", with: "'\\''")
        let escapedKey = trustKey.path.replacingOccurrences(of: "'", with: "'\\''")
        let body = """
        #!/bin/sh
        test "$1" = release || exit 2
        test "$2" = check || exit 2
        test "$3" = --channel || exit 2
        test "$4" = preview || exit 2
        test "$5" = --current-version || exit 2
        test "$6" = 0.3.8-rc.5 || exit 2
        test "$7" = --public-key || exit 2
        test "$8" = '\(escapedKey)' || exit 2
        test "$9" = --json || exit 2
        exec /bin/cat '\(escapedFixture)'
        """
        try Data(body.utf8).write(to: script)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)

        let checker = ProcessReleaseUpdateChecker(executableURL: script)
        let result = try await checker.check(
            channel: .preview,
            currentVersion: "0.3.8-rc.5",
            publicKeyURL: trustKey
        )
        XCTAssertEqual(result.status, .updateAvailable)
        XCTAssertEqual(result.candidateVersion, "0.3.8-rc.6")
    }

    private var fixtureURL: URL {
        var root = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
        for _ in 0..<4 {
            root.deleteLastPathComponent()
        }
        return root.appendingPathComponent(
            "testdata/release-update/check-update-available-v1.json",
            isDirectory: false
        )
    }
}
