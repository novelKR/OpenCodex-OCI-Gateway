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

    private actor Stager: ReleaseUpdateStaging {
        var receipts: [ReleaseUpdateStageReceipt]
        private(set) var calls: [ReleaseUpdateCheckResult] = []

        init(receipts: [ReleaseUpdateStageReceipt]) {
            self.receipts = receipts
        }

        func stage(
            selection: ReleaseUpdateCheckResult,
            currentVersion: String,
            publicKeyURL: URL
        ) async throws -> ReleaseUpdateStageReceipt {
            calls.append(selection)
            guard selection.currentVersion == currentVersion,
                  publicKeyURL.isFileURL,
                  !receipts.isEmpty else {
                throw ReleaseUpdateContractError.invalidStage
            }
            return receipts.removeFirst()
        }

        func callCount() -> Int { calls.count }
    }

    private actor SuspendedStager: ReleaseUpdateStaging {
        private var continuation: CheckedContinuation<ReleaseUpdateStageReceipt, Error>?

        func stage(
            selection: ReleaseUpdateCheckResult,
            currentVersion: String,
            publicKeyURL: URL
        ) async throws -> ReleaseUpdateStageReceipt {
            guard selection.currentVersion == currentVersion, publicKeyURL.isFileURL else {
                throw ReleaseUpdateContractError.invalidStage
            }
            return try await withCheckedThrowingContinuation { continuation in
                self.continuation = continuation
            }
        }

        func isWaiting() -> Bool { continuation != nil }

        func succeed(with receipt: ReleaseUpdateStageReceipt) {
            continuation?.resume(returning: receipt)
            continuation = nil
        }
    }

    private final class Quarantine: ReleaseUpdateQuarantining, @unchecked Sendable {
        var calls: [(URL, ReleaseUpdateCheckResult)] = []

        func applyAndVerify(
            stagedApplicationURL: URL,
            selection: ReleaseUpdateCheckResult
        ) throws {
            calls.append((stagedApplicationURL, selection))
        }
    }

    @MainActor
    private final class FinderHandoff: ReleaseUpdateFinderHandingOff {
        var revealed: [URL] = []
        var terminations = 0

        func reveal(_ applicationURL: URL) { revealed.append(applicationURL) }
        func terminateCurrentApplication() { terminations += 1 }
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

    func testDownloadStagesOnlyOnDemandAndFinderHandoffRevalidatesBeforeQuit() async {
        let selection = stageResult()
        let checker = Checker(results: [selection])
        let receipt = stageReceipt()
        let stager = Stager(receipts: [receipt, receipt])
        let quarantine = Quarantine()
        let finder = FinderHandoff()
        let controller = makeStageController(
            checker: checker,
            stager: stager,
            quarantine: quarantine,
            finder: finder,
            flavor: .production
        )

        await controller.performCheck(manual: true)
        XCTAssertTrue(controller.canDownloadUpdate)
        let callsBeforeDownload = await stager.callCount()
        XCTAssertEqual(callsBeforeDownload, 0)

        await controller.performStage()
        XCTAssertEqual(controller.stageState, .ready)
        XCTAssertFalse(controller.canDownloadUpdate)
        XCTAssertEqual(controller.stagedApplicationURL?.path, receipt.stagingPath)
        let callsAfterDownload = await stager.callCount()
        XCTAssertEqual(callsAfterDownload, 1)
        XCTAssertTrue(quarantine.calls.isEmpty)
        XCTAssertTrue(finder.revealed.isEmpty)

        await controller.performFinderHandoff()
        XCTAssertEqual(controller.stageState, .awaitingQuit)
        let callsAfterHandoff = await stager.callCount()
        XCTAssertEqual(callsAfterHandoff, 2)
        XCTAssertEqual(quarantine.calls.count, 1)
        XCTAssertEqual(finder.revealed, [receipt.stagedApplicationURL])
        XCTAssertEqual(finder.terminations, 0)

        controller.confirmQuitForFinderInstall()
        XCTAssertEqual(finder.terminations, 1)
    }

    func testFinderHandoffFailureNeverRevealsOrQuits() async {
        let selection = stageResult()
        let checker = Checker(results: [selection])
        let receipt = stageReceipt()
        let stager = Stager(receipts: [receipt])
        let quarantine = Quarantine()
        let finder = FinderHandoff()
        let controller = makeStageController(
            checker: checker,
            stager: stager,
            quarantine: quarantine,
            finder: finder,
            flavor: .production
        )
        await controller.performCheck(manual: true)
        await controller.performStage()

        await controller.performFinderHandoff()

        XCTAssertEqual(controller.stageState, .failed)
        XCTAssertTrue(controller.canDownloadUpdate)
        XCTAssertTrue(quarantine.calls.isEmpty)
        XCTAssertTrue(finder.revealed.isEmpty)
        controller.confirmQuitForFinderInstall()
        XCTAssertEqual(finder.terminations, 0)
    }

    func testLocalDevelopmentCannotStageProductionUpdate() async {
        let checker = Checker(results: [stageResult()])
        let receipt = stageReceipt()
        let stager = Stager(receipts: [receipt])
        let controller = makeStageController(
            checker: checker,
            stager: stager,
            quarantine: Quarantine(),
            finder: FinderHandoff(),
            flavor: .localDevelopment
        )
        await controller.performCheck(manual: true)
        XCTAssertFalse(controller.canDownloadUpdate)
        await controller.performStage()
        XCTAssertEqual(controller.stageState, .idle)
        let stageCalls = await stager.callCount()
        XCTAssertEqual(stageCalls, 0)
    }

    func testStaleStageCompletionCannotRestoreClearedCandidate() async {
        let selection = stageResult()
        let stager = SuspendedStager()
        let controller = makeStageController(
            checker: Checker(results: [selection]),
            stager: stager,
            quarantine: Quarantine(),
            finder: FinderHandoff(),
            flavor: .production
        )
        await controller.performCheck(manual: true)

        let staging = Task { await controller.performStage() }
        while !(await stager.isWaiting()) { await Task.yield() }
        controller.setChannel(.stable)
        await stager.succeed(with: stageReceipt())
        await staging.value

        XCTAssertEqual(controller.stageState, .idle)
        XCTAssertNil(controller.stagedApplicationURL)
        XCTAssertFalse(controller.canDownloadUpdate)
    }

    func testFoundationQuarantineIsAppliedAndReadBack() throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("quarantine-stage.\(UUID().uuidString)", isDirectory: true)
            .appendingPathComponent("OpenCodexRelay.app", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory.deletingLastPathComponent()) }
        XCTAssertNoThrow(
            try SystemReleaseUpdateQuarantine().applyAndVerify(
                stagedApplicationURL: directory,
                selection: stageResult()
            )
        )
        XCTAssertNotNil(
            try directory.resourceValues(forKeys: [.quarantinePropertiesKey]).quarantineProperties
        )
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

    private func makeStageController(
        checker: Checker,
        stager: any ReleaseUpdateStaging,
        quarantine: Quarantine,
        finder: FinderHandoff,
        flavor: DistributionFlavor
    ) -> ReleaseUpdateController {
        let suite = "ReleaseUpdateControllerStageTests.\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        defaults.removePersistentDomain(forName: suite)
        let controller = ReleaseUpdateController(
            checker: checker,
            stager: stager,
            quarantine: quarantine,
            finderHandoff: finder,
            distributionFlavor: flavor,
            currentVersion: "0.3.8-rc.6",
            publicKeyURL: URL(fileURLWithPath: "/tmp/trust.pub"),
            defaults: defaults,
            now: { Date(timeIntervalSince1970: 2_000_000) },
            jitter: { 600 }
        )
        controller.setChannel(.preview)
        return controller
    }

    private func stageResult() -> ReleaseUpdateCheckResult {
        ReleaseUpdateCheckResult(
            schemaVersion: 1,
            status: .updateAvailable,
            channel: .preview,
            currentVersion: "0.3.8-rc.6",
            checkedAt: "2026-09-01T00:00:00Z",
            etagCacheState: .refreshed,
            releaseID: 42,
            tag: "0.3.8-rc.7",
            version: "0.3.8-rc.7",
            releaseURL: "https://github.com/novelKR/OpenCodex-OCI-Gateway/releases/tag/0.3.8-rc.7",
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

    private func stageReceipt() -> ReleaseUpdateStageReceipt {
        ReleaseUpdateStageReceipt(
            schemaVersion: 1,
            releaseID: 42,
            tag: "0.3.8-rc.7",
            channel: .preview,
            manifestSHA256: String(repeating: "1", count: 64),
            appSHA256: String(repeating: "a", count: 64),
            bundleFingerprint: String(repeating: "b", count: 64),
            trustKeyID: String(repeating: "c", count: 64),
            stagingPath: "/private/tmp/OpenCodexRelay/Updates/42-1111111111111111111111111111111111111111111111111111111111111111/OpenCodexRelay.app",
            verifiedAt: "2026-09-01T00:00:00Z"
        )
    }
}
