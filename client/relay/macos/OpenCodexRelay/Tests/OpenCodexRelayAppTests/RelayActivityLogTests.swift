import Foundation
import XCTest
@testable import OpenCodexRelay

@MainActor
final class RelayActivityLogTests: XCTestCase {
    func testStoreKeepsOnlyNewestEventsWithinItsBound() {
        let store = RelayActivityLogStore(subsystem: "test.relay.activity", capacity: 3)

        for generation in 1 ... 4 {
            store.record(
                category: .status,
                code: "routing_snapshot_updated",
                fields: ["generation": String(generation)]
            )
        }

        XCTAssertEqual(store.events.map(\.sequence), [2, 3, 4])
        XCTAssertEqual(store.events.map { $0.fields["generation"] }, ["2", "3", "4"])
    }

    func testStoreRejectsUnknownFieldsUnsafeValuesAndInvalidCodes() {
        let store = RelayActivityLogStore(subsystem: "test.relay.activity")

        store.record(
            category: .operation,
            code: "safe_event",
            fields: [
                "phase": "status_refresh",
                "path": "/home/example/secret",
                "token": "sk-secret-token",
                "failure_code": "bad value with spaces",
            ]
        )
        store.record(category: .operation, code: "Unsafe Event")

        XCTAssertEqual(store.events.count, 1)
        XCTAssertEqual(store.events[0].fields, ["phase": "status_refresh"])
        let jsonLines = store.jsonLines()
        XCTAssertFalse(jsonLines.contains("/home/example"))
        XCTAssertFalse(jsonLines.contains("sk-secret-token"))
        XCTAssertFalse(jsonLines.contains("bad value with spaces"))
        XCTAssertFalse(jsonLines.contains("Unsafe Event"))
    }

    func testOwnerRepairFieldsAreBoundedAndPathsRemainRejected() {
        let store = RelayActivityLogStore(subsystem: "test.relay.activity")
        store.record(
            category: .repair,
            code: "native_routing_repair_finished",
            fields: [
                "adapter_id": "opencodex_npm_2_22_0_preserve_v1",
                "configuration": "valid",
                "data_preserved": "true",
                "integration": "disabled",
                "owner_restore_attempts": "4",
                "owner_restore_result": "already_native",
                "retry_exhausted": "false",
                "teardown_capability": "true",
                "path": "/private/secret",
                "fingerprint": "secret",
                "stdout": "secret",
            ]
        )
        XCTAssertEqual(store.events.first?.fields, [
            "adapter_id": "opencodex_npm_2_22_0_preserve_v1",
            "configuration": "valid",
            "data_preserved": "true",
            "integration": "disabled",
            "owner_restore_attempts": "4",
            "owner_restore_result": "already_native",
            "retry_exhausted": "false",
            "teardown_capability": "true",
        ])
    }

    func testLifecycleStartKeepsDistributionAndRuntimeModeOnly() {
        let store = RelayActivityLogStore(subsystem: "test.relay.activity")

        for runtimeMode in ["preview", "managed"] {
            store.record(
                category: .lifecycle,
                code: "app_model_started",
                fields: [
                    "distribution": "local_development",
                    "runtime_mode": runtimeMode,
                    "path": "/private/source",
                    "command": "sudo install",
                    "address": "https://secret.example.test/v1",
                ]
            )
        }

        XCTAssertEqual(
            store.events.map(\.fields),
            [
                ["distribution": "local_development", "runtime_mode": "preview"],
                ["distribution": "local_development", "runtime_mode": "managed"],
            ]
        )
        let jsonLines = store.jsonLines()
        XCTAssertFalse(jsonLines.contains("/private/source"))
        XCTAssertFalse(jsonLines.contains("sudo install"))
        XCTAssertFalse(jsonLines.contains("secret.example.test"))
    }

    func testJSONLinesUseStableStructuredSchema() throws {
        let store = RelayActivityLogStore(subsystem: "test.relay.activity")
        let timestamp = Date(timeIntervalSince1970: 1_700_000_000)
        store.record(
            .warning,
            category: .refresh,
            code: "refresh_failed",
            fields: ["failure_code": "operation_failed"],
            timestamp: timestamp
        )

        let line = try XCTUnwrap(store.jsonLines().split(separator: "\n").first)
        let object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: Data(line.utf8)) as? [String: Any]
        )

        XCTAssertEqual(object["sequence"] as? Int, 1)
        XCTAssertEqual(object["level"] as? String, "warning")
        XCTAssertEqual(object["category"] as? String, "refresh")
        XCTAssertEqual(object["code"] as? String, "refresh_failed")
        XCTAssertEqual(
            (object["fields"] as? [String: String])?["failure_code"],
            "operation_failed"
        )
        XCTAssertNotNil(object["timestamp"] as? String)
    }

    func testClearRetainsOnlyAnAuditableClearEvent() {
        let store = RelayActivityLogStore(subsystem: "test.relay.activity")
        store.record(category: .operation, code: "safe_event")

        store.clear()

        XCTAssertEqual(store.events.map(\.code), ["activity_log_cleared"])
    }

    func testUnifiedLogCommandIncludesInfoEventsAndExactActivityPredicate() {
        let store = RelayActivityLogStore(subsystem: "test.relay.activity")

        XCTAssertTrue(store.unifiedLogCommand.contains("--info"))
        XCTAssertTrue(store.unifiedLogCommand.contains("--debug"))
        XCTAssertTrue(store.unifiedLogCommand.contains(
            "subsystem == \"test.relay.activity\" && category == \"Activity\""
        ))
    }
}
