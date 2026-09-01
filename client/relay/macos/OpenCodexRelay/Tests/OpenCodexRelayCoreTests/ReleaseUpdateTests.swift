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

    func testSharedStageReceiptFixtureDecodesStrictly() throws {
        let receipt = try ReleaseUpdateStageReceipt.decodeStrict(Data(contentsOf: stageFixtureURL))
        XCTAssertEqual(receipt.schemaVersion, 1)
        XCTAssertEqual(receipt.releaseID, 42)
        XCTAssertEqual(receipt.tag, "0.3.8-rc.7")
        XCTAssertEqual(receipt.channel, .preview)
        XCTAssertTrue(receipt.stagingPath.hasSuffix("/OpenCodexRelay.app"))
    }

    func testStageReceiptRejectsUnknownDuplicateAndSelectionDrift() throws {
        let data = try Data(contentsOf: stageFixtureURL)
        var object = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        object["unknown"] = true
        XCTAssertThrowsError(
            try ReleaseUpdateStageReceipt.decodeStrict(JSONSerialization.data(withJSONObject: object))
        )
        let duplicate = Data(#"{"schema_version":1,"schema_version":1}"#.utf8)
        XCTAssertThrowsError(try ReleaseUpdateStageReceipt.decodeStrict(duplicate)) {
            XCTAssertEqual($0 as? ReleaseUpdateContractError, .duplicateField)
        }

        let receipt = try ReleaseUpdateStageReceipt.decodeStrict(data)
        XCTAssertThrowsError(try receipt.validated(for: stageSelection(tag: "0.3.8-rc.8"))) {
            XCTAssertEqual($0 as? ReleaseUpdateContractError, .invalidStage)
        }
    }

    func testProcessStagerUsesSnapshotBoundPublicCommandContract() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("release-stage-process.\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let script = directory.appendingPathComponent("relayctl", isDirectory: false)
        let trustKey = directory.appendingPathComponent("trust.pub", isDirectory: false)
        try Data().write(to: trustKey)
        let escapedFixture = stageFixtureURL.path.replacingOccurrences(of: "'", with: "'\\''")
        let escapedKey = trustKey.path.replacingOccurrences(of: "'", with: "'\\''")
        let hash = String(repeating: "1", count: 64)
        let body = """
        #!/bin/sh
        test "$1" = release || exit 2
        test "$2" = stage || exit 2
        test "$3" = --channel || exit 2
        test "$4" = preview || exit 2
        test "$5" = --current-version || exit 2
        test "$6" = 0.3.8-rc.6 || exit 2
        test "$7" = --release-id || exit 2
        test "$8" = 42 || exit 2
        test "$9" = --tag || exit 2
        test "${10}" = 0.3.8-rc.7 || exit 2
        test "${11}" = --expected-manifest-sha256 || exit 2
        test "${12}" = \(hash) || exit 2
        test "${13}" = --public-key || exit 2
        test "${14}" = '\(escapedKey)' || exit 2
        test "${15}" = --json || exit 2
        exec /bin/cat '\(escapedFixture)'
        """
        try Data(body.utf8).write(to: script)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)
        let receipt = try await ProcessReleaseUpdateStager(executableURL: script).stage(
            selection: stageSelection(tag: "0.3.8-rc.7"),
            currentVersion: "0.3.8-rc.6",
            publicKeyURL: trustKey
        )
        XCTAssertEqual(receipt.releaseID, 42)
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

    private var stageFixtureURL: URL {
        var root = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
        for _ in 0..<4 { root.deleteLastPathComponent() }
        return root.appendingPathComponent(
            "testdata/release-update/stage-ready-v1.json",
            isDirectory: false
        )
    }

    private func stageSelection(tag: String) -> ReleaseUpdateCheckResult {
        ReleaseUpdateCheckResult(
            schemaVersion: 1,
            status: .updateAvailable,
            channel: .preview,
            currentVersion: "0.3.8-rc.6",
            checkedAt: "2026-09-01T00:00:00Z",
            etagCacheState: .refreshed,
            releaseID: 42,
            tag: tag,
            version: tag,
            releaseURL: "https://github.com/novelKR/OpenCodex-OCI-Gateway/releases/tag/\(tag)",
            manifestSHA256: String(repeating: "1", count: 64),
            appAssetID: 102,
            appSHA256: String(repeating: "a", count: 64),
            minimumUpdaterVersion: "0.3.8-rc.6",
            minimumMacOSVersion: "26.0",
            integrationProtocol: 1,
            helperProtocol: 1,
            trustKeyID: String(repeating: "c", count: 64)
        )
    }
}
