import Foundation
import XCTest
@testable import OpenCodexRelay
@testable import OpenCodexRelayCore

@MainActor
final class ReleaseUpdateControllerTests: XCTestCase {
    private actor Checker: ReleaseUpdateChecking {
        var results: [ReleaseUpdateCheckResult]
        private(set) var calls: [(ReleaseUpdateChannel, String, URL)] = []

        init(results: [ReleaseUpdateCheckResult]) {
            self.results = results
        }

        func check(
            channel: ReleaseUpdateChannel,
            currentVersion: String,
            publicKeyURL: URL
        ) async throws -> ReleaseUpdateCheckResult {
            calls.append((channel, currentVersion, publicKeyURL))
            guard !results.isEmpty else { throw ReleaseUpdateContractError.invalidJSON }
            return results.removeFirst()
        }

        func callCount() -> Int { calls.count }
    }

    func testAutomaticSchedulingPolicyUsesJitterThenTwentyFourHours() {
        let now = Date(timeIntervalSince1970: 2_000_000)
        XCTAssertEqual(
            ReleaseUpdateController.nextAutomaticDelay(
                distributionFlavor: .production,
                enabled: true,
                lastCheckedAt: nil,
                now: now,
                firstCheckDelay: 120
            ),
            300
        )
        XCTAssertEqual(
            ReleaseUpdateController.nextAutomaticDelay(
                distributionFlavor: .production,
                enabled: true,
                lastCheckedAt: nil,
                now: now,
                firstCheckDelay: 1_200
            ),
            900
        )
        XCTAssertEqual(
            ReleaseUpdateController.nextAutomaticDelay(
                distributionFlavor: .production,
                enabled: true,
                lastCheckedAt: now.addingTimeInterval(-3_600),
                now: now,
                firstCheckDelay: 600
            ),
            23 * 60 * 60
        )
        XCTAssertEqual(
            ReleaseUpdateController.nextAutomaticDelay(
                distributionFlavor: .production,
                enabled: true,
                lastCheckedAt: now.addingTimeInterval(-25 * 60 * 60),
                now: now,
                firstCheckDelay: 600
            ),
            600
        )
        XCTAssertNil(
            ReleaseUpdateController.nextAutomaticDelay(
                distributionFlavor: .localDevelopment,
                enabled: true,
                lastCheckedAt: nil,
                now: now,
                firstCheckDelay: 600
            )
        )
        XCTAssertNil(
            ReleaseUpdateController.nextAutomaticDelay(
                distributionFlavor: .production,
                enabled: false,
                lastCheckedAt: nil,
                now: now,
                firstCheckDelay: 600
            )
        )
    }

    func testLocalDevelopmentNeverSchedulesAutomaticNetworkCheckButManualWorks() async {
        let checker = Checker(results: [result(version: "0.3.8-rc.6")])
        let controller = makeController(checker: checker, flavor: .localDevelopment)
        controller.start()
        XCTAssertFalse(controller.permitsAutomaticNetworkChecks)
        let callsAfterStart = await checker.callCount()
        XCTAssertEqual(callsAfterStart, 0)

        await controller.performCheck(manual: false)
        let callsAfterAutomaticAttempt = await checker.callCount()
        XCTAssertEqual(callsAfterAutomaticAttempt, 0)
        await controller.performCheck(manual: true)
        let callsAfterManualCheck = await checker.callCount()
        XCTAssertEqual(callsAfterManualCheck, 1)
        XCTAssertEqual(controller.status, .updateAvailable)
    }

    func testBadgeDismissalIsCandidateSpecificAndManualCheckReshowsIt() async {
        let checker = Checker(results: [
            result(version: "0.3.8-rc.6"),
            result(version: "0.3.8-rc.6"),
            result(version: "0.3.8-rc.7"),
        ])
        let controller = makeController(checker: checker, flavor: .production)

        await controller.performCheck(manual: true)
        XCTAssertTrue(controller.isUpdateBadgeVisible)
        controller.dismissBadge()
        XCTAssertFalse(controller.isUpdateBadgeVisible)

        await controller.performCheck(manual: true)
        XCTAssertTrue(controller.isUpdateBadgeVisible)
        controller.dismissBadge()
        await controller.performCheck(manual: false)
        XCTAssertEqual(controller.candidateVersion, "0.3.8-rc.7")
        XCTAssertTrue(controller.isUpdateBadgeVisible)
    }

    func testChannelAndAutomaticPreferencePersist() {
        let suite = "ReleaseUpdateControllerTests.\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        defer { defaults.removePersistentDomain(forName: suite) }
        let checker = Checker(results: [])
        let first = ReleaseUpdateController(
            checker: checker,
            distributionFlavor: .localDevelopment,
            currentVersion: "0.3.8-rc.5",
            publicKeyURL: URL(fileURLWithPath: "/tmp/trust.pub"),
            defaults: defaults
        )
        first.setChannel(.preview)
        first.setAutomaticChecksEnabled(false)
        XCTAssertNil(first.lastCheckedAt)
        let restored = ReleaseUpdateController(
            checker: checker,
            distributionFlavor: .localDevelopment,
            currentVersion: "0.3.8-rc.5",
            publicKeyURL: URL(fileURLWithPath: "/tmp/trust.pub"),
            defaults: defaults
        )
        XCTAssertEqual(restored.channel, .preview)
        XCTAssertFalse(restored.automaticChecksEnabled)
    }

    private func makeController(
        checker: Checker,
        flavor: DistributionFlavor
    ) -> ReleaseUpdateController {
        let suite = "ReleaseUpdateControllerTests.\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        defaults.removePersistentDomain(forName: suite)
        let controller = ReleaseUpdateController(
            checker: checker,
            distributionFlavor: flavor,
            currentVersion: "0.3.8-rc.5",
            publicKeyURL: URL(fileURLWithPath: "/tmp/trust.pub"),
            defaults: defaults,
            now: { Date(timeIntervalSince1970: 2_000_000) },
            jitter: { 600 }
        )
        controller.setChannel(.preview)
        return controller
    }

    private func result(version: String) -> ReleaseUpdateCheckResult {
        ReleaseUpdateCheckResult(
            schemaVersion: 1,
            status: .updateAvailable,
            channel: .preview,
            currentVersion: "0.3.8-rc.5",
            checkedAt: "2026-09-01T00:00:00Z",
            etagCacheState: .refreshed,
            releaseID: 42,
            tag: version,
            version: version,
            releaseURL: "https://github.com/novelKR/OpenCodex-OCI-Gateway/releases/tag/\(version)",
            manifestSHA256: String(repeating: "1", count: 64),
            appAssetID: 102,
            appSHA256: String(repeating: "a", count: 64),
            minimumUpdaterVersion: nil,
            minimumMacOSVersion: nil,
            integrationProtocol: nil,
            helperProtocol: nil,
            trustKeyID: nil
        )
    }
}
