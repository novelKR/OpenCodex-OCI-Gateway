import Foundation
import XCTest
import OpenCodexRelayCore
@testable import OpenCodexRelay

@MainActor
final class RuntimeUpgradeControllerTests: XCTestCase {
    private actor Client: SelfHostedRuntimeUpgrading {
        var inspection: SelfHostedRuntimeUpgradeInspection
        var applyCalls = 0
        var recoverCalls = 0
        var lastConfirmed = false

        init(state: SelfHostedRuntimeUpgradeState) {
            inspection = Self.makeInspection(state: state)
        }

        func inspect() async throws -> SelfHostedRuntimeUpgradeInspection {
            inspection
        }

        func apply(
            expectedStateDigest: String,
            confirmRelayRestart: Bool
        ) async throws -> SelfHostedRuntimeUpgradeReceipt {
            guard expectedStateDigest == inspection.stateDigest else {
                throw RelayctlError.invalidStatus
            }
            applyCalls += 1
            lastConfirmed = confirmRelayRestart
            inspection = Self.makeInspection(state: .current)
            return Self.makeReceipt(inspection)
        }

        func recover() async throws -> SelfHostedRuntimeUpgradeReceipt {
            recoverCalls += 1
            inspection = Self.makeInspection(state: .upgradeAvailable)
            return Self.makeReceipt(inspection)
        }

        func counts() -> (Int, Int, Bool) {
            (applyCalls, recoverCalls, lastConfirmed)
        }

        private static func makeInspection(
            state: SelfHostedRuntimeUpgradeState
        ) -> SelfHostedRuntimeUpgradeInspection {
            let installedVersion = state == .notIntegrated ? "" : (
                state == .current ? "0.3.8-rc.8" : "0.3.8-rc.7"
            )
            let installedDigest = state == .notIntegrated ? "" : (
                state == .current
                    ? String(repeating: "c", count: 64)
                    : String(repeating: "b", count: 64)
            )
            return SelfHostedRuntimeUpgradeInspection(
                schemaVersion: 1,
                state: state,
                stateDigest: String(repeating: "a", count: 64),
                installedRuntimeVersion: installedVersion,
                installedRuntimeDigest: installedDigest,
                bundledRuntimeVersion: "0.3.8-rc.8",
                bundledRuntimeDigest: String(repeating: "c", count: 64),
                integrationProtocol: 1,
                restartRequired: state == .upgradeAvailable || state == .recoveryRequired
            )
        }

        private static func makeReceipt(
            _ inspection: SelfHostedRuntimeUpgradeInspection
        ) -> SelfHostedRuntimeUpgradeReceipt {
            SelfHostedRuntimeUpgradeReceipt(
                schemaVersion: inspection.schemaVersion,
                ok: true,
                state: inspection.state,
                stateDigest: inspection.stateDigest,
                installedRuntimeVersion: inspection.installedRuntimeVersion,
                installedRuntimeDigest: inspection.installedRuntimeDigest,
                bundledRuntimeVersion: inspection.bundledRuntimeVersion,
                bundledRuntimeDigest: inspection.bundledRuntimeDigest,
                integrationProtocol: inspection.integrationProtocol,
                restartRequired: inspection.restartRequired
            )
        }
    }

    func testStartInspectsAndApplyRequiresExplicitControllerAction() async throws {
        let client = Client(state: .upgradeAvailable)
        let controller = RuntimeUpgradeController(client: client)
        controller.start()
        try await wait { controller.inspection?.state == .upgradeAvailable }
        XCTAssertTrue(controller.canApply)
        let before = await client.counts()
        XCTAssertEqual(before.0, 0)

        controller.applyConfirmed()
        try await wait { controller.inspection?.state == .current }
        let counts = await client.counts()
        XCTAssertEqual(counts.0, 1)
        XCTAssertTrue(counts.2)
        XCTAssertFalse(controller.canApply)
    }

    func testRecoveryRunsOnlyFromRecoveryState() async throws {
        let client = Client(state: .recoveryRequired)
        let controller = RuntimeUpgradeController(client: client)
        controller.start()
        try await wait { controller.canRecover }
        controller.recoverConfirmed()
        try await wait { controller.inspection?.state == .upgradeAvailable }
        let counts = await client.counts()
        XCTAssertEqual(counts.1, 1)
    }

    private func wait(
        timeout: TimeInterval = 2,
        condition: @escaping @MainActor () -> Bool
    ) async throws {
        let deadline = Date().addingTimeInterval(timeout)
        while !condition() {
            if Date() >= deadline {
                XCTFail("timed out waiting for runtime upgrade state")
                return
            }
            try await Task.sleep(for: .milliseconds(10))
        }
    }
}
