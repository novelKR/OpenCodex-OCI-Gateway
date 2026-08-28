import Darwin
import CryptoKit
import Foundation
import XCTest
@testable import OpenCodexRelayHelperInstallerCore

final class HelperInstallerTests: XCTestCase {
    private final class Runtime: @unchecked Sendable, HelperInstallerRuntime {
        var loaded = false
        var probeReady = true
        var bootstrapFails = false
        var validationFailure: HelperInstallerFailureReason?
        var probeFailure: HelperInstallerFailureReason?
        var bootoutFailureAtCall: Int?
        var onServiceStatus: (() -> Void)?
        private var bootoutCalls = 0
        private(set) var events: [String] = []

        func bootout(serviceName _: String) throws {
            bootoutCalls += 1
            events.append("bootout")
            if bootoutFailureAtCall == bootoutCalls {
                throw HelperInstallerFailureReason.daemonStopFailed
            }
            loaded = false
        }

        func bootstrap(plistURL _: URL) throws {
            events.append("bootstrap")
            if bootstrapFails { throw HelperInstallerErrorCode.daemonLaunchFailed }
            loaded = true
        }

        func serviceIsLoaded(serviceName _: String) -> Bool {
            let callback = onServiceStatus
            onServiceStatus = nil
            callback?()
            return loaded
        }

        func validateInstalledHelper(at _: URL) throws {
            if let validationFailure { throw validationFailure }
        }

        func probeReadiness() throws {
            events.append("probe")
            if let probeFailure { throw probeFailure }
            if !probeReady { throw HelperInstallerFailureReason.invalidResponse }
        }
    }

    private struct Fixture {
        let root: URL
        let sourceHelper: URL
        let runtime: Runtime
        let controller: HelperInstallerController
    }

    func testProductionAndLocalDevelopmentProfilesAreFullyIsolated() {
        let production = HelperInstallerProfile.production
        let local = HelperInstallerProfile.localDevelopment

        XCTAssertEqual(production.distribution, .production)
        XCTAssertEqual(local.distribution, .localDevelopment)
        XCTAssertNotEqual(production.appIdentifier, local.appIdentifier)
        XCTAssertNotEqual(production.helperIdentifier, local.helperIdentifier)
        XCTAssertNotEqual(production.installerIdentifier, local.installerIdentifier)
        XCTAssertNotEqual(production.serviceName, local.serviceName)
        XCTAssertNotEqual(production.helperRelativePath, local.helperRelativePath)
        XCTAssertNotEqual(production.plistRelativePath, local.plistRelativePath)
        XCTAssertNotEqual(production.transactionDirectoryName, local.transactionDirectoryName)
    }

    func testInstallStatusAndUninstallUseFixedLocations() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }

        XCTAssertEqual(try fixture.controller.perform(.status).state, .installRequired)
        let installed = try fixture.controller.perform(.install)
        XCTAssertEqual(installed.state, .ready)
        XCTAssertEqual(installed.resultCode, "installed")

        let helper = fixture.root.appending(path: HelperInstallerConstants.helperRelativePath)
        let plist = fixture.root.appending(path: HelperInstallerConstants.plistRelativePath)
        XCTAssertEqual(try Data(contentsOf: helper), try Data(contentsOf: fixture.sourceHelper))
        XCTAssertEqual(fileMode(helper), 0o755)
        XCTAssertEqual(fileMode(plist), 0o644)
        XCTAssertEqual(try fixture.controller.perform(.status).state, .ready)
        XCTAssertEqual(fixture.runtime.events, ["bootout", "bootstrap", "probe"])

        let removed = try fixture.controller.perform(.uninstall)
        XCTAssertEqual(removed.state, .installRequired)
        XCTAssertFalse(FileManager.default.fileExists(atPath: helper.path))
        XCTAssertFalse(FileManager.default.fileExists(atPath: plist.path))
    }

    func testReadOnlyStatusDoesNotRequireRoot() throws {
        let fixture = try makeFixture(requireRoot: true)
        defer { try? FileManager.default.removeItem(at: fixture.root) }

        XCTAssertEqual(try fixture.controller.perform(.status).state, .installRequired)
        if geteuid() != 0 {
            XCTAssertThrowsError(try fixture.controller.perform(.install)) { error in
                XCTAssertEqual(error as? HelperInstallerErrorCode, .rootRequired)
            }
        }
    }

    func testUpdateReplacesChangedHelperAndRestartsDaemon() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        _ = try fixture.controller.perform(.install)
        let helper = fixture.root.appending(path: HelperInstallerConstants.helperRelativePath)
        try Data("changed".utf8).write(to: helper)
        XCTAssertEqual(try fixture.controller.perform(.status).state, .updateRequired)

        let updated = try fixture.controller.perform(.update)

        XCTAssertEqual(updated.resultCode, "updated")
        XCTAssertEqual(try Data(contentsOf: helper), try Data(contentsOf: fixture.sourceHelper))
        XCTAssertEqual(fixture.runtime.events.suffix(3), ["bootout", "bootstrap", "probe"])
    }

    func testProtectionJournalBlocksMutationBeforeLaunchctl() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        let state = fixture.root.appending(path: HelperInstallerConstants.stateDirectoryRelativePath)
        try FileManager.default.createDirectory(at: state, withIntermediateDirectories: true)
        XCTAssertEqual(chmod(state.path, 0o700), 0)
        try Data("{}".utf8).write(
            to: state.appending(path: HelperInstallerConstants.protectionJournalName)
        )

        XCTAssertThrowsError(try fixture.controller.perform(.install)) { error in
            XCTAssertEqual(error as? HelperInstallerErrorCode, .protectionActive)
        }
        XCTAssertTrue(fixture.runtime.events.isEmpty)
    }

    func testDaemonProbeFailureRollsBackNewInstallation() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        fixture.runtime.probeReady = false

        XCTAssertThrowsError(try fixture.controller.perform(.install)) { error in
            let failure = error as? HelperInstallerFailure
            XCTAssertEqual(failure?.errorCode, .daemonLaunchFailed)
            XCTAssertEqual(failure?.phase, .readiness)
            XCTAssertEqual(failure?.reason, .invalidResponse)
            XCTAssertEqual(failure?.rollbackResult, .completed)
        }

        let helper = fixture.root.appending(path: HelperInstallerConstants.helperRelativePath)
        let plist = fixture.root.appending(path: HelperInstallerConstants.plistRelativePath)
        XCTAssertFalse(FileManager.default.fileExists(atPath: helper.path))
        XCTAssertFalse(FileManager.default.fileExists(atPath: plist.path))
        XCTAssertEqual(try fixture.controller.perform(.status).state, .installRequired)
    }

    func testBootstrapFailureRollsBackNewInstallation() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        fixture.runtime.bootstrapFails = true

        XCTAssertThrowsError(try fixture.controller.perform(.install)) { error in
            let failure = error as? HelperInstallerFailure
            XCTAssertEqual(failure?.errorCode, .daemonLaunchFailed)
            XCTAssertEqual(failure?.phase, .daemonStart)
            XCTAssertEqual(failure?.reason, .daemonStartRejected)
            XCTAssertEqual(failure?.rollbackResult, .completed)
        }
        XCTAssertEqual(try fixture.controller.perform(.status).state, .installRequired)
    }

    func testSignatureFailurePreservesPhaseAndRollbackResult() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        fixture.runtime.validationFailure = .signatureInvalid

        XCTAssertThrowsError(try fixture.controller.perform(.install)) { error in
            let failure = error as? HelperInstallerFailure
            XCTAssertEqual(failure?.errorCode, .artifactInvalid)
            XCTAssertEqual(failure?.phase, .signatureCheck)
            XCTAssertEqual(failure?.reason, .signatureInvalid)
            XCTAssertEqual(failure?.rollbackResult, .completed)
        }
        XCTAssertEqual(try fixture.controller.perform(.status).state, .installRequired)
    }

    func testUnreadableHelperSourceReportsPublishFailureWithoutLeakingPath() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        fixture.runtime.onServiceStatus = {
            XCTAssertEqual(chmod(fixture.sourceHelper.path, 0o111), 0)
        }

        XCTAssertThrowsError(try fixture.controller.perform(.install)) { error in
            let failure = error as? HelperInstallerFailure
            XCTAssertEqual(failure?.errorCode, .installationFailed)
            XCTAssertEqual(failure?.phase, .helperPublish)
            XCTAssertEqual(failure?.reason, .sourceUnreadable)
            XCTAssertEqual(failure?.rollbackResult, .completed)
        }
    }

    func testUnsafeStateParentIsDiagnosedBeforeTransaction() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        let database = fixture.root.appending(path: "var/db")
        XCTAssertEqual(chmod(database.path, 0o777), 0)

        XCTAssertThrowsError(try fixture.controller.perform(.install)) { error in
            let failure = error as? HelperInstallerFailure
            XCTAssertEqual(failure?.phase, .statePrepare)
            XCTAssertEqual(failure?.reason, .unsafeParent)
            XCTAssertEqual(failure?.rollbackResult, .notNeeded)
        }
    }

    func testRollbackFailurePreservesOriginalFailureDiagnostics() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        fixture.runtime.probeFailure = .probeTimeout
        fixture.runtime.bootoutFailureAtCall = 2

        XCTAssertThrowsError(try fixture.controller.perform(.install)) { error in
            let failure = error as? HelperInstallerFailure
            XCTAssertEqual(failure?.errorCode, .rollbackFailed)
            XCTAssertEqual(failure?.phase, .readiness)
            XCTAssertEqual(failure?.reason, .probeTimeout)
            XCTAssertEqual(failure?.rollbackResult, .failed)
        }
        XCTAssertEqual(try fixture.controller.perform(.status).state, .recoveryRequired)
    }

    func testReadinessFailuresRemainDistinct() throws {
        for reason in [
            HelperInstallerFailureReason.probeSpawnFailed,
            .probeTimeout,
            .xpcRejected,
            .invalidResponse,
        ] {
            let fixture = try makeFixture()
            defer { try? FileManager.default.removeItem(at: fixture.root) }
            fixture.runtime.probeFailure = reason

            XCTAssertThrowsError(try fixture.controller.perform(.install)) { error in
                let failure = error as? HelperInstallerFailure
                XCTAssertEqual(failure?.errorCode, .daemonLaunchFailed)
                XCTAssertEqual(failure?.phase, .readiness)
                XCTAssertEqual(failure?.reason, reason)
                XCTAssertEqual(failure?.rollbackResult, .completed)
            }
        }
    }

    func testDaemonStopFailureIsNotReportedAsBusy() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        fixture.runtime.bootoutFailureAtCall = 1

        XCTAssertThrowsError(try fixture.controller.perform(.install)) { error in
            let failure = error as? HelperInstallerFailure
            XCTAssertEqual(failure?.errorCode, .installationFailed)
            XCTAssertEqual(failure?.phase, .daemonStop)
            XCTAssertEqual(failure?.reason, .daemonStopFailed)
            XCTAssertEqual(failure?.rollbackResult, .completed)
        }
    }

    func testFailureReceiptAddsOnlyBoundedDiagnostics() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        let failure = HelperInstallerFailure(
            errorCode: .daemonLaunchFailed,
            phase: .daemonStart,
            reason: .daemonStartRejected,
            rollbackResult: .completed
        )

        let receipt = fixture.controller.failureReceipt(command: .install, error: failure)
        let object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: JSONEncoder().encode(receipt)) as? [String: Any]
        )
        XCTAssertEqual(object["schema_version"] as? Int, 1)
        XCTAssertEqual(object["failure_phase"] as? String, "daemon_start")
        XCTAssertEqual(object["failure_reason"] as? String, "daemon_start_rejected")
        XCTAssertEqual(object["rollback_result"] as? String, "completed")
        XCTAssertNil(object["path"])
        XCTAssertNil(object["stderr"])
        XCTAssertNil(object["cdhash"])
    }

    func testPreflightDiagnosticsPreserveBoundedPhaseReasonAndVersion() throws {
        func receipt(
            for error: any Error,
            fallbackReason: HelperInstallerFailureReason,
            fallbackCode: HelperInstallerErrorCode,
            helperVersion: String?
        ) -> HelperInstallerReceipt {
            let failure = HelperInstallerDiagnostics.failure(
                error,
                phase: .preflight,
                fallbackReason: fallbackReason,
                fallbackCode: fallbackCode
            )
            return HelperInstallerDiagnostics.receipt(
                command: .install,
                fallbackState: .installRequired,
                helperVersion: helperVersion,
                error: failure
            )
        }

        let source = receipt(
            for: HelperInstallerErrorCode.artifactInvalid,
            fallbackReason: .sourceUnreadable,
            fallbackCode: .artifactInvalid,
            helperVersion: nil
        )
        XCTAssertEqual(source.errorCode, .artifactInvalid)
        XCTAssertEqual(source.failurePhase, .preflight)
        XCTAssertEqual(source.failureReason, .sourceUnreadable)
        XCTAssertEqual(source.rollbackResult, .notNeeded)
        XCTAssertNil(source.helperVersion)

        let signature = receipt(
            for: HelperInstallerErrorCode.artifactInvalid,
            fallbackReason: .signatureInvalid,
            fallbackCode: .artifactInvalid,
            helperVersion: "0.0.0-relay-preserve.6"
        )
        XCTAssertEqual(signature.errorCode, .artifactInvalid)
        XCTAssertEqual(signature.failureReason, .signatureInvalid)
        XCTAssertEqual(signature.helperVersion, "0.0.0-relay-preserve.6")

        let runtime = HelperInstallerDiagnostics.receipt(
            command: .install,
            fallbackState: .installRequired,
            helperVersion: "0.0.0-relay-preserve.6",
            error: HelperInstallerFailure(
                errorCode: .installationFailed,
                phase: .preflight,
                reason: .probeSpawnFailed
            )
        )
        XCTAssertEqual(runtime.errorCode, .installationFailed)
        XCTAssertEqual(runtime.failurePhase, .preflight)
        XCTAssertEqual(runtime.failureReason, .probeSpawnFailed)
        XCTAssertEqual(runtime.rollbackResult, .notNeeded)

        let unknown = receipt(
            for: CocoaError(.fileReadUnknown),
            fallbackReason: .unknown,
            fallbackCode: .installationFailed,
            helperVersion: nil
        )
        XCTAssertEqual(unknown.errorCode, .installationFailed)
        XCTAssertEqual(unknown.failureReason, .unknown)
        XCTAssertEqual(unknown.rollbackResult, .notNeeded)

        let object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: JSONEncoder().encode(runtime)) as? [String: Any]
        )
        XCTAssertEqual(object["failure_phase"] as? String, "preflight")
        XCTAssertEqual(object["failure_reason"] as? String, "probe_spawn_failed")
        XCTAssertEqual(object["rollback_result"] as? String, "not_needed")
        XCTAssertNil(object["path"])
        XCTAssertNil(object["mode"])
        XCTAssertNil(object["uid"])
        XCTAssertNil(object["cdhash"])
        XCTAssertNil(object["stderr"])
    }

    func testProbeTimeoutCleanupTerminatesThenForceKillsAStuckChild() {
        var events: [String] = []
        var waits = [false, true]
        let stopped = HelperInstallerProbeTimeoutCleanup.stop(
            isRunning: {
                events.append("is-running")
                return true
            },
            terminate: { events.append("terminate") },
            waitForExit: { seconds in
                events.append("wait-\(Int(seconds))")
                return waits.removeFirst()
            },
            forceKill: { events.append("force-kill") }
        )

        XCTAssertTrue(stopped)
        XCTAssertEqual(
            events,
            ["terminate", "wait-1", "is-running", "force-kill", "wait-1"]
        )
    }

    func testProbeTimeoutCleanupDoesNotForceKillAfterGracefulExit() {
        var events: [String] = []
        let stopped = HelperInstallerProbeTimeoutCleanup.stop(
            isRunning: {
                XCTFail("running state should not be checked after graceful exit")
                return false
            },
            terminate: { events.append("terminate") },
            waitForExit: { _ in
                events.append("wait")
                return true
            },
            forceKill: { XCTFail("graceful exit must not be force killed") }
        )

        XCTAssertTrue(stopped)
        XCTAssertEqual(events, ["terminate", "wait"])
    }

    func testActiveProtectionLockBlocksMutation() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        let state = fixture.root.appending(path: HelperInstallerConstants.stateDirectoryRelativePath)
        try FileManager.default.createDirectory(at: state, withIntermediateDirectories: true)
        XCTAssertEqual(chmod(state.path, 0o700), 0)
        let lockURL = state.appending(path: HelperInstallerConstants.protectionLockName)
        let descriptor = Darwin.open(lockURL.path, O_CREAT | O_RDWR | O_CLOEXEC, mode_t(0o600))
        XCTAssertGreaterThanOrEqual(descriptor, 0)
        defer { Darwin.close(descriptor) }
        XCTAssertEqual(fchmod(descriptor, 0o600), 0)
        XCTAssertEqual(flock(descriptor, LOCK_EX | LOCK_NB), 0)

        XCTAssertThrowsError(try fixture.controller.perform(.install)) { error in
            let failure = error as? HelperInstallerFailure
            XCTAssertEqual(failure?.errorCode, .busy)
            XCTAssertEqual(failure?.phase, .statePrepare)
            XCTAssertEqual(failure?.rollbackResult, .notNeeded)
        }
        XCTAssertTrue(fixture.runtime.events.isEmpty)
    }

    func testExistingTransactionFailsClosed() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        let transaction = fixture.root
            .appending(path: HelperInstallerConstants.stateDirectoryRelativePath)
            .appending(path: HelperInstallerConstants.transactionDirectoryName)
        let state = transaction.deletingLastPathComponent()
        try FileManager.default.createDirectory(at: state, withIntermediateDirectories: true)
        XCTAssertEqual(chmod(state.path, 0o700), 0)
        try FileManager.default.createDirectory(at: transaction, withIntermediateDirectories: false)

        XCTAssertEqual(try fixture.controller.perform(.status).state, .recoveryRequired)
        XCTAssertThrowsError(try fixture.controller.perform(.install)) { error in
            XCTAssertEqual(error as? HelperInstallerErrorCode, .recoveryRequired)
        }
        XCTAssertThrowsError(try fixture.controller.perform(.recover)) { error in
            XCTAssertEqual(error as? HelperInstallerErrorCode, .recoveryUnavailable)
        }
    }

    func testRecoverCompletesTargetAcrossMutationCrashCheckpoints() throws {
        for phase in [
            "mutation_started",
            "helper_changed",
            "plist_changed",
            "service_stopped",
            "service_started",
            "activation_verified",
        ] {
            let fixture = try makeFixture()
            defer { try? FileManager.default.removeItem(at: fixture.root) }
            _ = try fixture.controller.perform(.install)
            try createTransaction(
                fixture,
                command: "install",
                phase: phase,
                hadHelper: false,
                hadPlist: false,
                wasServiceLoaded: false
            )

            let receipt = try fixture.controller.perform(.recover)

            XCTAssertEqual(receipt.state, .ready)
            XCTAssertEqual(receipt.resultCode, "recovered")
            XCTAssertFalse(transactionExists(fixture))
            XCTAssertTrue(fixture.runtime.loaded)
        }
    }

    func testRecoverClearsPristinePreparingAcrossBackupCheckpointsWithoutMutation() throws {
        for completedBackups in 0...2 {
            let fixture = try makeFixture()
            defer { try? FileManager.default.removeItem(at: fixture.root) }
            _ = try fixture.controller.perform(.install)
            let helper = installedHelper(fixture)
            let plist = installedPlist(fixture)
            let helperBefore = try Data(contentsOf: helper)
            let plistBefore = try Data(contentsOf: plist)
            let eventsBefore = fixture.runtime.events
            try createTransaction(
                fixture,
                command: "update",
                phase: "preparing",
                hadHelper: true,
                hadPlist: true,
                wasServiceLoaded: true,
                helperBackup: helperBefore,
                plistBackup: plistBefore,
                writesBackups: false
            )
            let transaction = try transactionDirectory(fixture)
            if completedBackups >= 1 {
                try writeData(
                    helperBefore,
                    to: transaction.appending(path: "helper.backup"),
                    mode: 0o600
                )
            }
            if completedBackups == 2 {
                try writeData(
                    plistBefore,
                    to: transaction.appending(path: "daemon.backup.plist"),
                    mode: 0o600
                )
            }

            let receipt = try fixture.controller.perform(.recover)

            XCTAssertEqual(receipt.state, .ready)
            XCTAssertEqual(receipt.resultCode, "recovered")
            XCTAssertEqual(try Data(contentsOf: helper), helperBefore)
            XCTAssertEqual(try Data(contentsOf: plist), plistBefore)
            XCTAssertEqual(fixture.runtime.events, eventsBefore)
            XCTAssertFalse(transactionExists(fixture))
        }
    }

    func testRecoverClearsPristinePreparingForNewInstallWithoutMutation() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        try createTransaction(
            fixture,
            command: "install",
            phase: "preparing",
            hadHelper: false,
            hadPlist: false,
            wasServiceLoaded: false,
            writesBackups: false
        )

        let receipt = try fixture.controller.perform(.recover)

        XCTAssertEqual(receipt.state, .installRequired)
        XCTAssertEqual(receipt.resultCode, "recovered")
        XCTAssertFalse(FileManager.default.fileExists(atPath: installedHelper(fixture).path))
        XCTAssertFalse(FileManager.default.fileExists(atPath: installedPlist(fixture).path))
        XCTAssertTrue(fixture.runtime.events.isEmpty)
        XCTAssertFalse(transactionExists(fixture))
    }

    func testRecoverClearsPristinePreparingForStoppedUninstallWithoutMutation() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        _ = try fixture.controller.perform(.install)
        fixture.runtime.loaded = false
        let helperBefore = try Data(contentsOf: installedHelper(fixture))
        let plistBefore = try Data(contentsOf: installedPlist(fixture))
        let eventsBefore = fixture.runtime.events
        try createTransaction(
            fixture,
            command: "uninstall",
            phase: "preparing",
            hadHelper: true,
            hadPlist: true,
            wasServiceLoaded: false,
            helperBackup: helperBefore,
            plistBackup: plistBefore,
            writesBackups: false
        )

        let receipt = try fixture.controller.perform(.recover)

        XCTAssertEqual(receipt.state, .ready)
        XCTAssertEqual(try Data(contentsOf: installedHelper(fixture)), helperBefore)
        XCTAssertEqual(try Data(contentsOf: installedPlist(fixture)), plistBefore)
        XCTAssertFalse(fixture.runtime.loaded)
        XCTAssertEqual(fixture.runtime.events, eventsBefore)
        XCTAssertFalse(transactionExists(fixture))
    }

    func testRecoverPreservesPreparingWhenOriginalOrServiceDrifts() throws {
        for scenario in 0..<4 {
            let fixture = try makeFixture()
            defer { try? FileManager.default.removeItem(at: fixture.root) }
            _ = try fixture.controller.perform(.install)
            let helperBefore = try Data(contentsOf: installedHelper(fixture))
            let plistBefore = try Data(contentsOf: installedPlist(fixture))
            try createTransaction(
                fixture,
                command: "update",
                phase: "preparing",
                hadHelper: true,
                hadPlist: true,
                wasServiceLoaded: true,
                helperBackup: helperBefore,
                plistBackup: plistBefore,
                writesBackups: false
            )
            switch scenario {
            case 0:
                try Data("changed helper".utf8).write(to: installedHelper(fixture))
            case 1:
                XCTAssertEqual(chmod(installedHelper(fixture).path, 0o775), 0)
            case 2:
                try FileManager.default.removeItem(at: installedPlist(fixture))
            default:
                fixture.runtime.loaded = false
            }
            let helperAfterDrift = try? Data(contentsOf: installedHelper(fixture))
            let plistAfterDrift = try? Data(contentsOf: installedPlist(fixture))
            let helperModeAfterDrift = fileMode(installedHelper(fixture))
            let loadedAfterDrift = fixture.runtime.loaded
            let eventsBefore = fixture.runtime.events

            XCTAssertThrowsError(try fixture.controller.perform(.recover)) { error in
                let expected: HelperInstallerErrorCode =
                    scenario == 0 || scenario == 3
                        ? .recoveryVerificationFailed
                        : .recoveryUnavailable
                XCTAssertEqual(error as? HelperInstallerErrorCode, expected)
            }
            XCTAssertEqual(try? Data(contentsOf: installedHelper(fixture)), helperAfterDrift)
            XCTAssertEqual(try? Data(contentsOf: installedPlist(fixture)), plistAfterDrift)
            XCTAssertEqual(fileMode(installedHelper(fixture)), helperModeAfterDrift)
            XCTAssertEqual(fixture.runtime.loaded, loadedAfterDrift)
            XCTAssertEqual(fixture.runtime.events, eventsBefore)
            XCTAssertTrue(transactionExists(fixture))
        }
    }

    func testRecoverClearsBackupsReadyOnlyWhenBackupsAndOriginalMatch() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        _ = try fixture.controller.perform(.install)
        let helperBefore = try Data(contentsOf: installedHelper(fixture))
        let plistBefore = try Data(contentsOf: installedPlist(fixture))
        let eventsBefore = fixture.runtime.events
        try createTransaction(
            fixture,
            command: "update",
            phase: "backups_ready",
            hadHelper: true,
            hadPlist: true,
            wasServiceLoaded: true,
            helperBackup: helperBefore,
            plistBackup: plistBefore
        )

        let receipt = try fixture.controller.perform(.recover)

        XCTAssertEqual(receipt.state, .ready)
        XCTAssertEqual(fixture.runtime.events, eventsBefore)
        XCTAssertFalse(transactionExists(fixture))
    }

    func testRecoverRollsBackPreviousArtifactsAndRequiresUpdate() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        _ = try fixture.controller.perform(.install)
        let oldHelper = try Data(contentsOf: installedHelper(fixture))
        let oldPlist = try Data(contentsOf: installedPlist(fixture))
        try Data("new helper".utf8).write(to: fixture.sourceHelper)
        try Data("partial helper".utf8).write(to: installedHelper(fixture))
        XCTAssertEqual(chmod(installedHelper(fixture).path, 0o755), 0)
        try createTransaction(
            fixture,
            command: "update",
            phase: "mutation_started",
            hadHelper: true,
            hadPlist: true,
            wasServiceLoaded: true,
            helperBackup: oldHelper,
            plistBackup: oldPlist
        )

        let receipt = try fixture.controller.perform(.recover)

        XCTAssertEqual(receipt.state, .updateRequired)
        XCTAssertEqual(receipt.resultCode, "rollback_completed_update_required")
        XCTAssertEqual(try Data(contentsOf: installedHelper(fixture)), oldHelper)
        XCTAssertTrue(fixture.runtime.loaded)
        XCTAssertFalse(transactionExists(fixture))
    }

    func testRecoverRollsBackNewInstallToAbsentWithoutStartingDaemon() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        try FileManager.default.createDirectory(
            at: installedHelper(fixture).deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        try FileManager.default.createDirectory(
            at: installedPlist(fixture).deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        try Data("partial helper".utf8).write(to: installedHelper(fixture))
        try Data("partial plist".utf8).write(to: installedPlist(fixture))
        try createTransaction(
            fixture,
            command: "install",
            phase: "mutation_started",
            hadHelper: false,
            hadPlist: false,
            wasServiceLoaded: false
        )

        let receipt = try fixture.controller.perform(.recover)

        XCTAssertEqual(receipt.state, .installRequired)
        XCTAssertFalse(FileManager.default.fileExists(atPath: installedHelper(fixture).path))
        XCTAssertFalse(FileManager.default.fileExists(atPath: installedPlist(fixture).path))
        XCTAssertFalse(fixture.runtime.loaded)
    }

    func testRecoverPreservesPreviouslyStoppedDaemonState() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        _ = try fixture.controller.perform(.install)
        let oldHelper = try Data(contentsOf: installedHelper(fixture))
        let oldPlist = try Data(contentsOf: installedPlist(fixture))
        fixture.runtime.loaded = false
        try Data("new helper".utf8).write(to: fixture.sourceHelper)
        try Data("partial helper".utf8).write(to: installedHelper(fixture))
        XCTAssertEqual(chmod(installedHelper(fixture).path, 0o755), 0)
        try createTransaction(
            fixture,
            command: "update",
            phase: "mutation_started",
            hadHelper: true,
            hadPlist: true,
            wasServiceLoaded: false,
            helperBackup: oldHelper,
            plistBackup: oldPlist
        )

        let receipt = try fixture.controller.perform(.recover)

        XCTAssertEqual(receipt.state, .updateRequired)
        XCTAssertFalse(fixture.runtime.loaded)
        XCTAssertNotEqual(fixture.runtime.events.last, "bootstrap")
    }

    func testRecoverKeepsTransactionWhenRollbackDaemonVerificationFails() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        _ = try fixture.controller.perform(.install)
        let oldHelper = try Data(contentsOf: installedHelper(fixture))
        let oldPlist = try Data(contentsOf: installedPlist(fixture))
        try Data("new helper".utf8).write(to: fixture.sourceHelper)
        try Data("partial helper".utf8).write(to: installedHelper(fixture))
        XCTAssertEqual(chmod(installedHelper(fixture).path, 0o755), 0)
        try createTransaction(
            fixture,
            command: "update",
            phase: "mutation_started",
            hadHelper: true,
            hadPlist: true,
            wasServiceLoaded: true,
            helperBackup: oldHelper,
            plistBackup: oldPlist
        )
        fixture.runtime.bootstrapFails = true

        XCTAssertThrowsError(try fixture.controller.perform(.recover)) { error in
            XCTAssertEqual(
                error as? HelperInstallerErrorCode,
                .recoveryVerificationFailed
            )
        }
        XCTAssertTrue(transactionExists(fixture))
    }

    func testRecoverRejectsSchemaOneWithoutMutation() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        _ = try fixture.controller.perform(.install)
        let helperBefore = try Data(contentsOf: installedHelper(fixture))
        let transaction = try transactionDirectory(fixture)
        let schemaOne: [String: Any] = [
            "schema_version": 1,
            "command": "update",
            "had_helper": true,
            "had_plist": true,
        ]
        try writeJSON(schemaOne, to: transaction.appending(path: "journal.json"), mode: 0o600)

        XCTAssertThrowsError(try fixture.controller.perform(.recover)) { error in
            XCTAssertEqual(error as? HelperInstallerErrorCode, .recoveryUnavailable)
        }
        XCTAssertEqual(try Data(contentsOf: installedHelper(fixture)), helperBefore)
        XCTAssertTrue(transactionExists(fixture))
    }

    func testRecoverRejectsBackupsReadyWithMissingBackupWithoutMutation() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        _ = try fixture.controller.perform(.install)
        let helperBefore = try Data(contentsOf: installedHelper(fixture))
        let plistBefore = try Data(contentsOf: installedPlist(fixture))
        try createTransaction(
            fixture,
            command: "update",
            phase: "backups_ready",
            hadHelper: true,
            hadPlist: true,
            wasServiceLoaded: true,
            helperBackup: helperBefore,
            plistBackup: plistBefore,
            writesBackups: false
        )

        XCTAssertThrowsError(try fixture.controller.perform(.recover)) { error in
            XCTAssertEqual(error as? HelperInstallerErrorCode, .recoveryUnavailable)
        }
        XCTAssertEqual(try Data(contentsOf: installedHelper(fixture)), helperBefore)
        XCTAssertEqual(try Data(contentsOf: installedPlist(fixture)), plistBefore)
        XCTAssertTrue(transactionExists(fixture))
    }

    func testRecoverRejectsInsecureTransactionDirectoryWithoutMutation() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        _ = try fixture.controller.perform(.install)
        let helperBefore = try Data(contentsOf: installedHelper(fixture))
        let plistBefore = try Data(contentsOf: installedPlist(fixture))
        try createTransaction(
            fixture,
            command: "update",
            phase: "backups_ready",
            hadHelper: true,
            hadPlist: true,
            wasServiceLoaded: true,
            helperBackup: helperBefore,
            plistBackup: plistBefore
        )
        XCTAssertEqual(chmod(try transactionDirectory(fixture).path, 0o755), 0)

        XCTAssertThrowsError(try fixture.controller.perform(.recover)) { error in
            XCTAssertEqual(error as? HelperInstallerErrorCode, .recoveryUnavailable)
        }
        XCTAssertEqual(try Data(contentsOf: installedHelper(fixture)), helperBefore)
        XCTAssertEqual(try Data(contentsOf: installedPlist(fixture)), plistBefore)
        XCTAssertTrue(transactionExists(fixture))
    }

    func testSymlinkTargetIsNeverAccepted() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        let helper = fixture.root.appending(path: HelperInstallerConstants.helperRelativePath)
        try FileManager.default.createDirectory(
            at: helper.deletingLastPathComponent(),
            withIntermediateDirectories: true
        )
        try FileManager.default.createSymbolicLink(at: helper, withDestinationURL: fixture.sourceHelper)

        XCTAssertEqual(try fixture.controller.perform(.status).state, .recoveryRequired)
        XCTAssertThrowsError(try fixture.controller.perform(.install)) { error in
            XCTAssertEqual(error as? HelperInstallerErrorCode, .recoveryRequired)
        }
    }

    func testInstalledModeDriftRequiresRecoveryInsteadOfUpdate() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        _ = try fixture.controller.perform(.install)
        let helper = fixture.root.appending(path: HelperInstallerConstants.helperRelativePath)
        XCTAssertEqual(chmod(helper.path, 0o775), 0)

        XCTAssertEqual(try fixture.controller.perform(.status).state, .recoveryRequired)
        XCTAssertThrowsError(try fixture.controller.perform(.update)) { error in
            XCTAssertEqual(error as? HelperInstallerErrorCode, .recoveryRequired)
        }
    }

    private func makeFixture(requireRoot: Bool = false) throws -> Fixture {
        let root = FileManager.default.temporaryDirectory
            .appending(path: "pw-relay-dev-installer-tests-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        for relative in ["var", "var/db", "Library"] {
            let directory = root.appending(path: relative)
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false)
            XCTAssertEqual(chmod(directory.path, 0o755), 0)
        }
        let source = root.appending(path: "source-helper")
        try Data("verified helper fixture".utf8).write(to: source)
        XCTAssertEqual(chmod(source.path, 0o755), 0)
        let runtime = Runtime()
        let artifacts = HelperInstallerArtifacts(
            sourceHelperURL: source,
            helperVersion: "1.2.3-dev",
            daemonClientRequirement: "cdhash H\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\""
        )
        let controller = HelperInstallerController(
            configuration: HelperInstallerConfiguration(
                systemRootURL: root,
                requireRoot: requireRoot,
                expectedOwnerUID: geteuid(),
                expectedOwnerGID: getegid()
            ),
            artifacts: artifacts,
            runtime: runtime
        )
        return Fixture(root: root, sourceHelper: source, runtime: runtime, controller: controller)
    }

    private func fileMode(_ url: URL) -> mode_t? {
        var info = stat()
        guard lstat(url.path, &info) == 0 else { return nil }
        return info.st_mode & mode_t(0o7777)
    }

    private func installedHelper(_ fixture: Fixture) -> URL {
        fixture.root.appending(path: HelperInstallerConstants.helperRelativePath)
    }

    private func installedPlist(_ fixture: Fixture) -> URL {
        fixture.root.appending(path: HelperInstallerConstants.plistRelativePath)
    }

    private func transactionExists(_ fixture: Fixture) -> Bool {
        FileManager.default.fileExists(
            atPath: fixture.root
                .appending(path: HelperInstallerConstants.stateDirectoryRelativePath)
                .appending(path: HelperInstallerConstants.transactionDirectoryName).path
        )
    }

    @discardableResult
    private func transactionDirectory(_ fixture: Fixture) throws -> URL {
        let state = fixture.root
            .appending(path: HelperInstallerConstants.stateDirectoryRelativePath)
        if !FileManager.default.fileExists(atPath: state.path) {
            try FileManager.default.createDirectory(at: state, withIntermediateDirectories: true)
            XCTAssertEqual(chmod(state.path, 0o700), 0)
        }
        let transaction = state.appending(path: HelperInstallerConstants.transactionDirectoryName)
        if !FileManager.default.fileExists(atPath: transaction.path) {
            try FileManager.default.createDirectory(at: transaction, withIntermediateDirectories: false)
            XCTAssertEqual(chmod(transaction.path, 0o700), 0)
        }
        return transaction
    }

    private func createTransaction(
        _ fixture: Fixture,
        command: String,
        phase: String,
        hadHelper: Bool,
        hadPlist: Bool,
        wasServiceLoaded: Bool,
        helperBackup: Data? = nil,
        plistBackup: Data? = nil,
        writesBackups: Bool = true
    ) throws {
        let transaction = try transactionDirectory(fixture)
        let helperSource: Data?
        if let helperBackup {
            helperSource = helperBackup
        } else if hadHelper {
            helperSource = try Data(contentsOf: installedHelper(fixture))
        } else {
            helperSource = nil
        }

        let plistSource: Data?
        if let plistBackup {
            plistSource = plistBackup
        } else if hadPlist {
            plistSource = try Data(contentsOf: installedPlist(fixture))
        } else {
            plistSource = nil
        }
        var journal: [String: Any] = [
            "schema_version": 2,
            "transaction_id": UUID().uuidString.lowercased(),
            "command": command,
            "phase": phase,
            "had_helper": hadHelper,
            "had_plist": hadPlist,
            "was_service_loaded": wasServiceLoaded,
        ]
        if let helperSource {
            journal["helper_witness"] = witness(helperSource)
            if writesBackups {
                try writeData(
                    helperSource,
                    to: transaction.appending(path: "helper.backup"),
                    mode: 0o600
                )
            }
        }
        if let plistSource {
            journal["plist_witness"] = witness(plistSource)
            if writesBackups {
                try writeData(
                    plistSource,
                    to: transaction.appending(path: "daemon.backup.plist"),
                    mode: 0o600
                )
            }
        }
        try writeJSON(journal, to: transaction.appending(path: "journal.json"), mode: 0o600)
    }

    private func witness(_ data: Data) -> [String: Any] {
        [
            "size": data.count,
            "sha256": SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined(),
        ]
    }

    private func writeJSON(_ value: [String: Any], to url: URL, mode: mode_t) throws {
        try writeData(
            try JSONSerialization.data(withJSONObject: value, options: [.sortedKeys]),
            to: url,
            mode: mode
        )
    }

    private func writeData(_ data: Data, to url: URL, mode: mode_t) throws {
        try data.write(to: url)
        XCTAssertEqual(chmod(url.path, mode), 0)
    }
}
