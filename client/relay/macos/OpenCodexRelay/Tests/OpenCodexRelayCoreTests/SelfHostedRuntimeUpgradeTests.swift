import Foundation
import XCTest
@testable import OpenCodexRelayCore

final class SelfHostedRuntimeUpgradeTests: XCTestCase {
    func testSharedInspectionFixtureDecodesStrictly() throws {
        let result = try SelfHostedRuntimeUpgradeInspection.decodeStrict(
            Data(contentsOf: fixtureURL)
        )
        XCTAssertEqual(result.state, .upgradeAvailable)
        XCTAssertEqual(result.installedRuntimeVersion, "0.3.8-rc.7")
        XCTAssertEqual(result.bundledRuntimeVersion, "0.3.8-rc.8")
        XCTAssertTrue(result.restartRequired)
    }

    func testStrictInspectionRejectsUnknownDuplicateMissingAndInvalidState() throws {
        let data = try Data(contentsOf: fixtureURL)
        var object = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        object["unknown"] = true
        XCTAssertThrowsError(
            try SelfHostedRuntimeUpgradeInspection.decodeStrict(
                JSONSerialization.data(withJSONObject: object)
            )
        ) {
            XCTAssertEqual($0 as? SelfHostedRuntimeUpgradeContractError, .unknownField)
        }
        object.removeValue(forKey: "unknown")
        object.removeValue(forKey: "state_digest")
        XCTAssertThrowsError(
            try SelfHostedRuntimeUpgradeInspection.decodeStrict(
                JSONSerialization.data(withJSONObject: object)
            )
        )
        let duplicate = Data(#"{"schema_version":1,"schema_version":1}"#.utf8)
        XCTAssertThrowsError(try SelfHostedRuntimeUpgradeInspection.decodeStrict(duplicate)) {
            XCTAssertEqual($0 as? SelfHostedRuntimeUpgradeContractError, .duplicateField)
        }

        object = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        object["restart_required"] = false
        XCTAssertThrowsError(
            try SelfHostedRuntimeUpgradeInspection.decodeStrict(
                JSONSerialization.data(withJSONObject: object)
            )
        ) {
            XCTAssertEqual($0 as? SelfHostedRuntimeUpgradeContractError, .invalidState)
        }
    }

    func testProcessClientUsesExactInspectApplyRecoverContracts() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("runtime-upgrade-process.\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let script = directory.appendingPathComponent("relayctl")
        let escapedFixture = fixtureURL.path.replacingOccurrences(of: "'", with: "'\\''")
        let receipt = directory.appendingPathComponent("receipt.json")
        let inspection = try SelfHostedRuntimeUpgradeInspection.decodeStrict(Data(contentsOf: fixtureURL))
        let receiptObject: [String: Any] = [
            "schema_version": 1,
            "ok": true,
            "state": "current",
            "state_digest": String(repeating: "d", count: 64),
            "installed_runtime_version": inspection.bundledRuntimeVersion,
            "installed_runtime_digest": inspection.bundledRuntimeDigest,
            "bundled_runtime_version": inspection.bundledRuntimeVersion,
            "bundled_runtime_digest": inspection.bundledRuntimeDigest,
            "integration_protocol": 1,
            "restart_required": false,
        ]
        try JSONSerialization.data(withJSONObject: receiptObject).write(to: receipt)
        let escapedReceipt = receipt.path.replacingOccurrences(of: "'", with: "'\\''")
        let body = """
        #!/bin/sh
        test "$1" = integration || exit 2
        test "$2" = upgrade || exit 2
        case "$3" in
          inspect)
            test "$4" = --json || exit 2
            exec /bin/cat '\(escapedFixture)'
            ;;
          apply)
            test "$4" = --expected-state-digest || exit 2
            test "$5" = \(String(repeating: "a", count: 64)) || exit 2
            test "$6" = --confirm-relay-restart || exit 2
            test "$7" = --json || exit 2
            exec /bin/cat '\(escapedReceipt)'
            ;;
          recover)
            test "$4" = --json || exit 2
            exec /bin/cat '\(escapedReceipt)'
            ;;
          *) exit 2 ;;
        esac
        """
        try Data(body.utf8).write(to: script)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)

        let client = ProcessSelfHostedRuntimeUpgradeClient(executableURL: script)
        let inspected = try await client.inspect()
        let applied = try await client.apply(
            expectedStateDigest: String(repeating: "a", count: 64),
            confirmRelayRestart: true
        )
        let recovered = try await client.recover()
        XCTAssertEqual(inspected.state, .upgradeAvailable)
        XCTAssertEqual(applied.state, .current)
        XCTAssertEqual(recovered.state, .current)
    }

    private var fixtureURL: URL {
        var root = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
        for _ in 0..<4 { root.deleteLastPathComponent() }
        return root.appendingPathComponent(
            "testdata/runtime-upgrade/inspect-upgrade-available-v1.json"
        )
    }
}
