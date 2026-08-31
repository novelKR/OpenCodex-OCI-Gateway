import Darwin
import Foundation
import XCTest
@testable import OpenCodexRelay
@testable import OpenCodexRelayCore
import OpenCodexRelayLocalization

@MainActor
final class ApplicationRelocationTests: XCTestCase {
    func testLaunchRequestRequiresOneBoundedNonce() {
        let nonce = "01234567-89ab-cdef-0123-456789abcdef"
        XCTAssertEqual(
            ApplicationRelocationLaunchRequest.parse([
                "OpenCodexRelay",
                ApplicationRelocationLaunchRequest.flag,
                nonce,
            ]),
            ApplicationRelocationLaunchRequest(nonce: nonce)
        )
        XCTAssertNil(ApplicationRelocationLaunchRequest.parse([
            "OpenCodexRelay",
            ApplicationRelocationLaunchRequest.flag,
            "short",
        ]))
        XCTAssertNil(ApplicationRelocationLaunchRequest.parse([
            "OpenCodexRelay",
            ApplicationRelocationLaunchRequest.flag,
            nonce,
            ApplicationRelocationLaunchRequest.flag,
            nonce,
        ]))
    }

    func testPreviewControllerNeverBeginsFileMutation() async throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()
        let controller = ApplicationRelocationController(
            runtimeMode: .preview,
            relocator: relocator
        )

        controller.begin()

        let state = controller.state
        XCTAssertEqual(state, .preview)
        XCTAssertFalse(FileManager.default.fileExists(atPath: fixture.destination.path))
    }

    func testGatewayConfigurationUnlocksOnlyAtAStandardApplicationLocation() async throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let outside = ApplicationRelocationController(
            runtimeMode: .managed,
            relocator: fixture.makeRelocator()
        )
        try fixture.createBundle(at: fixture.destination, fingerprint: fixture.sourceFingerprint)
        let destinationRelocator = SystemApplicationRelocator(
            sourceURL: fixture.destination,
            homeURL: fixture.home,
            systemApplicationsURL: fixture.systemApplications,
            userApplicationsURL: fixture.userApplications,
            fileManager: .default,
            validator: FixtureApplicationBundleValidator()
        )
        let installed = ApplicationRelocationController(
            runtimeMode: .managed,
            relocator: destinationRelocator
        )

        let outsidePermitsConfiguration = outside.permitsGatewayConfiguration
        let installedPermitsConfiguration = installed.permitsGatewayConfiguration
        XCTAssertFalse(outsidePermitsConfiguration)
        XCTAssertTrue(installedPermitsConfiguration)
    }

    func testGatewayRemainsLockedForDestinationLaunchUntilHandoffIsVerified() async throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        try fixture.createBundle(at: fixture.destination, fingerprint: fixture.sourceFingerprint)
        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            resumesDestinationLaunch: true,
            relocator: fixture.makeDestinationRelocator()
        )

        XCTAssertEqual(controller.state, .waitingForDestination)
        XCTAssertFalse(controller.handoffVerified)
        XCTAssertFalse(controller.permitsGatewayConfiguration)
    }

    func testSourceExitWitnessBlocksThenRetryCompletesHandoff() async throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let sourceRelocator = fixture.makeRelocator()
        guard case let .launch(journal) = try sourceRelocator.prepare(
            scope: .user,
            allowReplacement: false,
            sourceProcessWitness: fixture.sourceProcessWitness
        ) else {
            return XCTFail("expected launch")
        }
        let observer = FixtureApplicationProcessObserver(
            witness: fixture.sourceProcessWitness,
            isRunning: true
        )
        let activityLog = RelayActivityLogStore(subsystem: "test.relocation")
        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            resumesDestinationLaunch: true,
            relocator: fixture.makeDestinationRelocator(),
            processObserver: observer,
            activityLog: activityLog,
            sourceExitCheckCount: 1,
            sourceExitCheckInterval: .zero
        )

        await controller.completeLaunchRequest(.init(nonce: journal.nonce))

        XCTAssertEqual(controller.state, .sourceExitRequired)
        XCTAssertEqual(try sourceRelocator.currentJournal()?.stage, .destinationStarted)
        XCTAssertFalse(controller.permitsGatewayConfiguration)
        controller.keepOriginal()
        XCTAssertEqual(try sourceRelocator.currentJournal()?.stage, .destinationStarted)
        observer.setRunning(false)
        controller.retryHandoff()
        try await waitForState(.sourceCleanupRequired, controller: controller)

        XCTAssertTrue(controller.handoffVerified)
        XCTAssertTrue(controller.permitsGatewayConfiguration)
        XCTAssertEqual(try sourceRelocator.currentJournal()?.stage, .cleanupPending)
        let exported = activityLog.jsonLines()
        XCTAssertFalse(exported.contains(fixture.source.path))
        XCTAssertFalse(exported.contains(journal.nonce))
        XCTAssertFalse(exported.contains(String(fixture.sourceProcessWitness.processIdentifier)))
        XCTAssertFalse(exported.contains(fixture.sourceFingerprint))
    }

    func testBackupCleanupFailureIsRetryableAndKeepsGatewayLocked() async throws {
        let fixture = try RelocationFixture(existingFingerprint: String(repeating: "b", count: 64))
        defer { fixture.cleanup() }
        let sourceRelocator = fixture.makeRelocator()
        guard case let .launch(journal) = try sourceRelocator.prepare(
            scope: .user,
            allowReplacement: true,
            sourceProcessWitness: fixture.sourceProcessWitness
        ) else {
            return XCTFail("expected launch")
        }
        let observer = FixtureApplicationProcessObserver(
            witness: fixture.sourceProcessWitness,
            isRunning: false
        )
        let trash = FixtureApplicationTrashMover(results: [false, true])
        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            resumesDestinationLaunch: true,
            relocator: fixture.makeDestinationRelocator(),
            trashMover: trash,
            processObserver: observer,
            sourceExitCheckCount: 1,
            sourceExitCheckInterval: .zero
        )

        await controller.completeLaunchRequest(.init(nonce: journal.nonce))

        XCTAssertEqual(controller.state, .backupCleanupFailed)
        XCTAssertEqual(try sourceRelocator.currentJournal()?.stage, .backupCleanupPending)
        XCTAssertFalse(controller.permitsGatewayConfiguration)
        controller.retryHandoff()
        try await waitForState(.sourceCleanupRequired, controller: controller)

        XCTAssertTrue(controller.handoffVerified)
        XCTAssertTrue(controller.permitsGatewayConfiguration)
        XCTAssertEqual(try sourceRelocator.currentJournal()?.stage, .cleanupPending)
    }

    func testDestinationRestartResumesDestinationStartedAfterSourceExit() async throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()
        guard case let .launch(journal) = try relocator.prepare(
            scope: .user,
            allowReplacement: false,
            sourceProcessWitness: fixture.sourceProcessWitness
        ) else {
            return XCTFail("expected launch")
        }
        _ = try relocator.completeDestinationStart(
            nonce: journal.nonce,
            currentBundleURL: fixture.destination
        )

        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            relocator: fixture.makeDestinationRelocator(),
            processObserver: FixtureApplicationProcessObserver(
                witness: fixture.sourceProcessWitness,
                isRunning: false
            ),
            sourceExitCheckCount: 1,
            sourceExitCheckInterval: .zero
        )
        try await waitForState(.sourceCleanupRequired, controller: controller)

        XCTAssertTrue(controller.handoffVerified)
        XCTAssertEqual(try relocator.currentJournal()?.stage, .cleanupPending)
    }

    func testDestinationRestartResumesSourceExitedStage() async throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()
        guard case let .launch(journal) = try relocator.prepare(
            scope: .user,
            allowReplacement: false,
            sourceProcessWitness: fixture.sourceProcessWitness
        ) else {
            return XCTFail("expected launch")
        }
        _ = try relocator.completeDestinationStart(
            nonce: journal.nonce,
            currentBundleURL: fixture.destination
        )
        _ = try relocator.markSourceExited(
            nonce: journal.nonce,
            currentBundleURL: fixture.destination
        )

        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            relocator: fixture.makeDestinationRelocator(),
            processObserver: FixtureApplicationProcessObserver(
                witness: fixture.sourceProcessWitness,
                isRunning: false
            )
        )
        try await waitForState(.sourceCleanupRequired, controller: controller)

        XCTAssertTrue(controller.handoffVerified)
        XCTAssertEqual(try relocator.currentJournal()?.stage, .cleanupPending)
    }

    func testUnchangedBackupCleanupIntentIsRetriedOnRestart() async throws {
        let fixture = try RelocationFixture(existingFingerprint: String(repeating: "b", count: 64))
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()
        let pending = try fixture.prepareBackupCleanupPending(relocator: relocator)
        let backupPath = try XCTUnwrap(pending.backupPath)
        let trash = FixtureApplicationTrashMover(results: [true])

        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            relocator: fixture.makeDestinationRelocator(),
            trashMover: trash,
            processObserver: FixtureApplicationProcessObserver(
                witness: fixture.sourceProcessWitness,
                isRunning: false
            )
        )
        try await waitForState(.sourceCleanupRequired, controller: controller)

        XCTAssertFalse(FileManager.default.fileExists(atPath: backupPath))
        XCTAssertTrue(controller.handoffVerified)
        XCTAssertEqual(try relocator.currentJournal()?.stage, .cleanupPending)
    }

    func testMissingBackupAfterPersistedCleanupIntentCompletesOnRestart() async throws {
        let fixture = try RelocationFixture(existingFingerprint: String(repeating: "b", count: 64))
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()
        let pending = try fixture.prepareBackupCleanupPending(relocator: relocator)
        let backupPath = try XCTUnwrap(pending.backupPath)
        try FileManager.default.removeItem(atPath: backupPath)

        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            relocator: fixture.makeDestinationRelocator(),
            processObserver: FixtureApplicationProcessObserver(
                witness: fixture.sourceProcessWitness,
                isRunning: false
            ),
            sourceExitCheckCount: 1,
            sourceExitCheckInterval: .zero
        )
        try await waitForState(.sourceCleanupRequired, controller: controller)

        XCTAssertTrue(controller.handoffVerified)
        XCTAssertEqual(try relocator.currentJournal()?.stage, .cleanupPending)
    }

    func testChangedBackupFailsClosedOnRestart() async throws {
        let fixture = try RelocationFixture(existingFingerprint: String(repeating: "b", count: 64))
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()
        let pending = try fixture.prepareBackupCleanupPending(relocator: relocator)
        let backup = URL(fileURLWithPath: try XCTUnwrap(pending.backupPath), isDirectory: true)
        try FileManager.default.removeItem(at: backup)
        try fixture.createBundle(at: backup, fingerprint: String(repeating: "c", count: 64))

        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            relocator: fixture.makeDestinationRelocator(),
            processObserver: FixtureApplicationProcessObserver(
                witness: fixture.sourceProcessWitness,
                isRunning: false
            ),
            sourceExitCheckCount: 1,
            sourceExitCheckInterval: .zero
        )
        try await waitForState(.recoveryRequired, controller: controller)

        XCTAssertFalse(controller.handoffVerified)
        XCTAssertFalse(controller.permitsGatewayConfiguration)
        XCTAssertTrue(FileManager.default.fileExists(atPath: backup.path))
    }

    func testPersistedSourceTrashIntentDetectsReplacementAndCanBeChangedToKeep() async throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()
        let cleanup = try fixture.prepareCleanupPending(relocator: relocator)
        guard case .trash = try relocator.prepareSourceCleanup(
            disposition: .trash,
            currentBundleURL: fixture.destination
        ) else {
            return XCTFail("expected persisted source Trash intent")
        }
        try FileManager.default.removeItem(at: fixture.source)
        try fixture.createBundle(at: fixture.source, fingerprint: fixture.sourceFingerprint)

        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            relocator: fixture.makeDestinationRelocator(),
            trashMover: FixtureApplicationTrashMover(results: []),
            processObserver: FixtureApplicationProcessObserver(
                witness: fixture.sourceProcessWitness,
                isRunning: false
            )
        )
        try await waitForState(.failed(.sourceChanged), controller: controller)

        XCTAssertTrue(controller.handoffVerified)
        XCTAssertTrue(controller.permitsGatewayConfiguration)
        XCTAssertEqual(cleanup.nonce, try relocator.currentJournal()?.nonce)
        controller.keepOriginal()
        XCTAssertEqual(controller.state, .completed)
        XCTAssertNil(try relocator.currentJournal())
        XCTAssertTrue(FileManager.default.fileExists(atPath: fixture.source.path))
    }

    func testPersistedSourceTrashIntentRetriesUnchangedSourceOnRestart() async throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()
        _ = try fixture.prepareCleanupPending(relocator: relocator)
        guard case .trash = try relocator.prepareSourceCleanup(
            disposition: .trash,
            currentBundleURL: fixture.destination
        ) else {
            return XCTFail("expected persisted source Trash intent")
        }

        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            relocator: fixture.makeDestinationRelocator(),
            trashMover: FixtureApplicationTrashMover(results: [true]),
            processObserver: FixtureApplicationProcessObserver(
                witness: fixture.sourceProcessWitness,
                isRunning: false
            )
        )
        try await waitForState(.completed, controller: controller)

        XCTAssertTrue(controller.handoffVerified)
        XCTAssertFalse(FileManager.default.fileExists(atPath: fixture.source.path))
        XCTAssertNil(try relocator.currentJournal())
    }

    func testPersistedSourceTrashIntentTreatsMissingSourceAsComplete() async throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()
        _ = try fixture.prepareCleanupPending(relocator: relocator)
        guard case .trash = try relocator.prepareSourceCleanup(
            disposition: .trash,
            currentBundleURL: fixture.destination
        ) else {
            return XCTFail("expected persisted source Trash intent")
        }
        try FileManager.default.removeItem(at: fixture.source)

        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            relocator: fixture.makeDestinationRelocator(),
            trashMover: FixtureApplicationTrashMover(results: []),
            processObserver: FixtureApplicationProcessObserver(
                witness: fixture.sourceProcessWitness,
                isRunning: false
            )
        )
        try await waitForState(.completed, controller: controller)

        XCTAssertTrue(controller.handoffVerified)
        XCTAssertNil(try relocator.currentJournal())
    }

    func testSchemaOnePrestartJournalUsesLegacyRollback() async throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()
        guard case let .launch(journal) = try relocator.prepare(
            scope: .user,
            allowReplacement: false,
            sourceProcessWitness: fixture.sourceProcessWitness
        ) else {
            return XCTFail("expected launch")
        }
        try fixture.writeSchemaOneJournal(journal, stage: .swapped)

        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            relocator: fixture.makeRelocator()
        )

        XCTAssertEqual(controller.state, .available)
        XCTAssertFalse(FileManager.default.fileExists(atPath: fixture.destination.path))
        XCTAssertNil(try relocator.currentJournal())
    }

    func testSchemaOnePoststartJournalFailsClosedWithoutDeletingBundles() async throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()
        guard case let .launch(journal) = try relocator.prepare(
            scope: .user,
            allowReplacement: false,
            sourceProcessWitness: fixture.sourceProcessWitness
        ) else {
            return XCTFail("expected launch")
        }
        let started = try relocator.completeDestinationStart(
            nonce: journal.nonce,
            currentBundleURL: fixture.destination
        )
        try fixture.writeSchemaOneJournal(started, stage: .destinationStarted)

        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            relocator: fixture.makeDestinationRelocator()
        )

        XCTAssertEqual(controller.state, .recoveryRequired)
        XCTAssertFalse(controller.permitsGatewayConfiguration)
        XCTAssertTrue(FileManager.default.fileExists(atPath: fixture.source.path))
        XCTAssertTrue(FileManager.default.fileExists(atPath: fixture.destination.path))
        XCTAssertEqual(try relocator.currentJournal()?.stage, .recoveryRequired)
    }

    func testConcurrentDestinationStartUsesExpectedStageCAS() async throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()
        guard case let .launch(journal) = try relocator.prepare(
            scope: .user,
            allowReplacement: false,
            sourceProcessWitness: fixture.sourceProcessWitness
        ) else {
            return XCTFail("expected launch")
        }
        let destination = fixture.destination
        let nonce = journal.nonce

        async let first = Task.detached {
            (try? relocator.completeDestinationStart(
                nonce: nonce,
                currentBundleURL: destination
            )) != nil
        }.value
        async let second = Task.detached {
            (try? relocator.completeDestinationStart(
                nonce: nonce,
                currentBundleURL: destination
            )) != nil
        }.value
        let successes = await [first, second].filter { $0 }.count

        XCTAssertEqual(successes, 1)
        XCTAssertEqual(try relocator.currentJournal()?.stage, .destinationStarted)
    }

    func testProcessWitnessRejectsPIDLaunchDateAndBundlePathMismatch() {
        let witness = ApplicationProcessWitness(
            processIdentifier: 101,
            launchDate: 123,
            bundlePath: "/Applications/OpenCodexRelay.app"
        )

        XCTAssertTrue(WorkspaceApplicationProcessObserver.matches(
            witness,
            processIdentifier: 101,
            launchDate: 123,
            bundleURL: URL(fileURLWithPath: witness.bundlePath, isDirectory: true)
        ))
        XCTAssertFalse(WorkspaceApplicationProcessObserver.matches(
            witness,
            processIdentifier: 102,
            launchDate: 123,
            bundleURL: URL(fileURLWithPath: witness.bundlePath, isDirectory: true)
        ))
        XCTAssertFalse(WorkspaceApplicationProcessObserver.matches(
            witness,
            processIdentifier: 101,
            launchDate: 124,
            bundleURL: URL(fileURLWithPath: witness.bundlePath, isDirectory: true)
        ))
        XCTAssertFalse(WorkspaceApplicationProcessObserver.matches(
            witness,
            processIdentifier: 101,
            launchDate: 123,
            bundleURL: URL(fileURLWithPath: "/Applications/Other.app", isDirectory: true)
        ))
    }

    func testEquivalentWitnessPathUsesCanonicalDirectoryIdentity() throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()
        let equivalentPath = fixture.source.deletingLastPathComponent().path +
            "/./" + fixture.source.lastPathComponent
        let witness = ApplicationProcessWitness(
            processIdentifier: getpid(),
            launchDate: 1,
            bundlePath: equivalentPath
        )

        guard case .launch = try relocator.prepare(
            scope: .user,
            allowReplacement: false,
            sourceProcessWitness: witness
        ) else {
            return XCTFail("expected canonical witness path to launch")
        }

        try relocator.recoverBeforeDestinationStart()
    }

    func testSourceProcessWitnessRequiresCurrentPIDAndDirectoryIdentity() throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()
        let other = fixture.root.appendingPathComponent("Other.app", isDirectory: true)
        try fixture.createBundle(at: other, fingerprint: fixture.sourceFingerprint)
        let mismatchedWitnesses = [
            ApplicationProcessWitness(
                processIdentifier: getpid() + 1,
                launchDate: 1,
                bundlePath: fixture.source.path
            ),
            ApplicationProcessWitness(
                processIdentifier: getpid(),
                launchDate: 1,
                bundlePath: other.path
            ),
        ]

        for witness in mismatchedWitnesses {
            XCTAssertThrowsError(
                try relocator.prepare(
                    scope: .user,
                    allowReplacement: false,
                    sourceProcessWitness: witness
                )
            ) {
                XCTAssertEqual(
                    $0 as? ApplicationRelocationFailure,
                    .sourceProcessInvalid
                )
            }
            XCTAssertNil(try relocator.currentJournal())
            XCTAssertFalse(FileManager.default.fileExists(atPath: fixture.destination.path))
        }
    }

    func testStandardSourceLocationHasDistinctFailure() throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        try fixture.createBundle(at: fixture.destination, fingerprint: fixture.sourceFingerprint)
        let relocator = fixture.makeDestinationRelocator()
        let witness = ApplicationProcessWitness(
            processIdentifier: getpid(),
            launchDate: 1,
            bundlePath: fixture.destination.path
        )

        XCTAssertThrowsError(
            try relocator.prepare(
                scope: .user,
                allowReplacement: false,
                sourceProcessWitness: witness
            )
        ) {
            XCTAssertEqual($0 as? ApplicationRelocationFailure, .sourceLocationInvalid)
        }
        XCTAssertNil(try relocator.currentJournal())
    }

    func testControllerBeginReachesVerifiedLaunchAndRollsBackFailedOpen() async throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let opener = FixtureApplicationOpener(result: false)
        let activityLog = RelayActivityLogStore(subsystem: "test.relocation.begin")
        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            relocator: fixture.makeRelocator(),
            opener: opener,
            processObserver: FixtureApplicationProcessObserver(
                witness: fixture.sourceProcessWitness,
                isRunning: true
            ),
            activityLog: activityLog
        )

        controller.begin()
        try await waitForState(.failed(.launchFailed), controller: controller)

        let expectedDestination = fixture.systemApplications.appendingPathComponent(
            SystemApplicationRelocator.applicationName,
            isDirectory: true
        )
        XCTAssertEqual(opener.requests.count, 1)
        let request = try XCTUnwrap(opener.requests.first)
        XCTAssertEqual(request.url.lastPathComponent, expectedDestination.lastPathComponent)
        XCTAssertEqual(
            ApplicationRelocationFileIdentity.regularDirectory(
                at: request.url.deletingLastPathComponent()
            ),
            ApplicationRelocationFileIdentity.regularDirectory(
                at: fixture.systemApplications
            )
        )
        XCTAssertFalse(FileManager.default.fileExists(atPath: expectedDestination.path))
        XCTAssertNil(try fixture.makeRelocator().currentJournal())
        XCTAssertTrue(controller.canStart)
        let exported = activityLog.jsonLines()
        XCTAssertTrue(exported.contains("\"result_code\":\"launch_failed\""))
        XCTAssertFalse(exported.contains(fixture.root.path))
    }

    func testUnavailableProcessWitnessIsVisibleAndRetryable() throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let invalidWitness = ApplicationProcessWitness(
            processIdentifier: getpid(),
            launchDate: 1,
            bundlePath: "/Applications/Other.app"
        )
        let activityLog = RelayActivityLogStore(subsystem: "test.relocation.witness")
        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            relocator: fixture.makeRelocator(),
            processObserver: FixtureApplicationProcessObserver(
                witness: invalidWitness,
                isRunning: true
            ),
            activityLog: activityLog
        )

        controller.begin()

        XCTAssertEqual(controller.state, .failed(.sourceProcessInvalid))
        XCTAssertTrue(controller.canStart)
        XCTAssertTrue(
            activityLog.jsonLines().contains(
                "\"result_code\":\"source_process_invalid\""
            )
        )
    }

    func testSourceFailuresMapToDistinctGuidanceAndRetryAction() {
        XCTAssertEqual(
            ApplicationRelocationView.detailKey(for: .failed(.sourceBundleInvalid)),
            .relocationSourceBundleInvalid
        )
        XCTAssertEqual(
            ApplicationRelocationView.detailKey(for: .failed(.sourceProcessInvalid)),
            .relocationSourceProcessInvalid
        )
        XCTAssertEqual(
            ApplicationRelocationView.detailKey(for: .failed(.sourceLocationInvalid)),
            .relocationSourceLocationInvalid
        )
        XCTAssertEqual(
            ApplicationRelocationView.primaryActionKey(
                for: .failed(.sourceProcessInvalid)
            ),
            .relocationRetryValidation
        )
    }

    func testUserApplicationsMoveStagesValidatesAndKeepsSource() throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()

        let prepared = try relocator.prepare(
            scope: .user,
            allowReplacement: false,
            sourceProcessWitness: fixture.sourceProcessWitness
        )
        guard case let .launch(journal) = prepared else {
            return XCTFail("expected launch")
        }
        XCTAssertEqual(journal.stage, .swapped)
        XCTAssertEqual(journal.destinationScope, .user)
        XCTAssertTrue(FileManager.default.fileExists(atPath: fixture.source.path))
        XCTAssertTrue(FileManager.default.fileExists(atPath: fixture.destination.path))
        let journalURL = fixture.home.appendingPathComponent(
            "Library/Application Support/OpenCodexRelay/application-relocation.json"
        )
        var journalInfo = stat()
        XCTAssertEqual(Darwin.lstat(journalURL.path, &journalInfo), 0)
        XCTAssertEqual(journalInfo.st_mode & mode_t(0o777), mode_t(0o600))

        let started = try relocator.completeDestinationStart(
            nonce: journal.nonce,
            currentBundleURL: fixture.destination
        )
        XCTAssertEqual(started.stage, .destinationStarted)
        let exited = try relocator.markSourceExited(
            nonce: journal.nonce,
            currentBundleURL: fixture.destination
        )
        XCTAssertEqual(exited.stage, .sourceExited)
        guard case let .complete(completed) = try relocator.prepareBackupCleanup(
            nonce: journal.nonce,
            currentBundleURL: fixture.destination
        ) else {
            return XCTFail("expected backup cleanup completion")
        }
        XCTAssertEqual(completed.stage, .cleanupPending)
        XCTAssertTrue(relocator.sourceIsStillOriginal(completed))
        guard case let .complete(sourceCleanup) = try relocator.prepareSourceCleanup(
            disposition: .keep,
            currentBundleURL: fixture.destination
        ) else {
            return XCTFail("expected source keep completion")
        }
        try relocator.finishSourceCleanup(
            nonce: sourceCleanup.nonce,
            currentBundleURL: fixture.destination
        )
        XCTAssertNil(try relocator.currentJournal())
    }

    func testIdenticalValidatedDestinationIsReusedWithoutBackup() throws {
        let fixture = try RelocationFixture(existingFingerprint: String(repeating: "a", count: 64))
        defer { fixture.cleanup() }
        let originalIdentity = try FixtureApplicationBundleValidator().inspect(
            bundleAt: fixture.destination
        ).fileIdentity

        let relocator = fixture.makeRelocator()
        guard case let .launch(journal) = try relocator.prepare(
            scope: .user,
            allowReplacement: false,
            sourceProcessWitness: fixture.sourceProcessWitness
        ) else {
            return XCTFail("expected launch")
        }

        XCTAssertNil(journal.backupPath)
        XCTAssertEqual(
            try FixtureApplicationBundleValidator().inspect(bundleAt: fixture.destination).fileIdentity,
            originalIdentity
        )
        try relocator.recoverBeforeDestinationStart()
        XCTAssertTrue(FileManager.default.fileExists(atPath: fixture.destination.path))
    }

    func testSystemPermissionFailureRequiresUserApplicationsConfirmation() throws {
        let fixture = try RelocationFixture()
        defer {
            try? FileManager.default.setAttributes(
                [.posixPermissions: 0o700],
                ofItemAtPath: fixture.systemApplications.path
            )
            fixture.cleanup()
        }
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o500],
            ofItemAtPath: fixture.systemApplications.path
        )

        let result = try fixture.makeRelocator().prepare(
            scope: .system,
            allowReplacement: false,
            sourceProcessWitness: fixture.sourceProcessWitness
        )
        guard case .fallbackRequired = result else {
            return XCTFail("expected user Applications fallback confirmation")
        }
    }

    func testSymlinkedUserApplicationsDirectoryIsRejected() throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let actual = fixture.root.appendingPathComponent("ActualApplications", isDirectory: true)
        try FileManager.default.createDirectory(at: actual, withIntermediateDirectories: true)
        try FileManager.default.removeItem(at: fixture.userApplications)
        try FileManager.default.createSymbolicLink(
            at: fixture.userApplications,
            withDestinationURL: actual
        )

        XCTAssertThrowsError(
            try fixture.makeRelocator().prepare(
                scope: .user,
                allowReplacement: false,
                sourceProcessWitness: fixture.sourceProcessWitness
            )
        ) {
            XCTAssertEqual($0 as? ApplicationRelocationFailure, .destinationUnavailable)
        }
    }

    func testDifferentDestinationRequiresConfirmationAndLaunchFailureRestoresBackup() throws {
        let fixture = try RelocationFixture(existingFingerprint: String(repeating: "b", count: 64))
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()

        let first = try relocator.prepare(
            scope: .user,
            allowReplacement: false,
            sourceProcessWitness: fixture.sourceProcessWitness
        )
        guard case .replacementRequired(.user) = first else {
            return XCTFail("expected replacement confirmation")
        }
        let replacement = try relocator.prepare(
            scope: .user,
            allowReplacement: true,
            sourceProcessWitness: fixture.sourceProcessWitness
        )
        guard case let .launch(journal) = replacement else {
            return XCTFail("expected replacement launch")
        }
        XCTAssertEqual(
            journal.stagingPath.map {
                URL(fileURLWithPath: $0, isDirectory: true).pathExtension
            },
            "app"
        )
        XCTAssertNotNil(journal.backupPath)
        XCTAssertEqual(
            journal.backupPath.map {
                URL(fileURLWithPath: $0, isDirectory: true).pathExtension
            },
            "app"
        )
        XCTAssertEqual(try fixture.fingerprint(at: fixture.destination), fixture.sourceFingerprint)

        try relocator.recoverBeforeDestinationStart()

        XCTAssertEqual(
            try fixture.fingerprint(at: fixture.destination),
            String(repeating: "b", count: 64)
        )
        XCTAssertNil(try relocator.currentJournal())
    }

    func testSourceRestartRollsBackAnUnconfirmedDestination() async throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        guard case .launch = try fixture.makeRelocator().prepare(
            scope: .user,
            allowReplacement: false,
            sourceProcessWitness: fixture.sourceProcessWitness
        ) else {
            return XCTFail("expected launch")
        }

        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            relocator: fixture.makeRelocator()
        )

        let state = controller.state
        XCTAssertEqual(state, .available)
        XCTAssertFalse(FileManager.default.fileExists(atPath: fixture.destination.path))
    }

    func testDestinationRestartWithoutNonceFailsClosedWithoutDeletingItself() async throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        guard case .launch = try fixture.makeRelocator().prepare(
            scope: .user,
            allowReplacement: false,
            sourceProcessWitness: fixture.sourceProcessWitness
        ) else {
            return XCTFail("expected launch")
        }
        let destinationRelocator = SystemApplicationRelocator(
            sourceURL: fixture.destination,
            homeURL: fixture.home,
            systemApplicationsURL: fixture.systemApplications,
            userApplicationsURL: fixture.userApplications,
            fileManager: .default,
            validator: FixtureApplicationBundleValidator()
        )

        let controller = ApplicationRelocationController(
            runtimeMode: .managed,
            relocator: destinationRelocator
        )

        let state = controller.state
        XCTAssertEqual(state, .recoveryRequired)
        XCTAssertTrue(FileManager.default.fileExists(atPath: fixture.destination.path))
        XCTAssertEqual(try destinationRelocator.currentJournal()?.stage, .recoveryRequired)
    }

    func testSourceIdentityChangeBlocksCleanup() throws {
        let fixture = try RelocationFixture()
        defer { fixture.cleanup() }
        let relocator = fixture.makeRelocator()
        guard case let .launch(journal) = try relocator.prepare(
            scope: .user,
            allowReplacement: false,
            sourceProcessWitness: fixture.sourceProcessWitness
        ) else {
            return XCTFail("expected launch")
        }
        let completed = try relocator.completeDestinationStart(
            nonce: journal.nonce,
            currentBundleURL: fixture.destination
        )
        try FileManager.default.removeItem(at: fixture.source)
        try fixture.createBundle(at: fixture.source, fingerprint: fixture.sourceFingerprint)
        XCTAssertFalse(relocator.sourceIsStillOriginal(completed))
    }

    func testSymlinkBundleIsRejectedBeforeCodeSignatureInspection() throws {
        let root = try temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let real = root.appendingPathComponent("Real.app", isDirectory: true)
        try FileManager.default.createDirectory(at: real, withIntermediateDirectories: true)
        let link = root.appendingPathComponent("Linked.app", isDirectory: true)
        try FileManager.default.createSymbolicLink(at: link, withDestinationURL: real)
        XCTAssertThrowsError(try SecurityApplicationBundleValidator().inspect(bundleAt: link)) {
            XCTAssertEqual($0 as? ApplicationRelocationFailure, .sourceBundleInvalid)
        }
    }

    func testHelperSheetStaysWithinCommonVisibleFrames() {
        XCTAssertEqual(
            DevelopmentSetupSheetLayout.size(for: CGSize(width: 1024, height: 640)),
            CGSize(width: 640, height: 560)
        )
        XCTAssertEqual(
            DevelopmentSetupSheetLayout.size(for: CGSize(width: 480, height: 360)),
            CGSize(width: 432, height: 320)
        )
    }

    func testQuickMenuContainsOnlyStandardNonDestructiveActions() {
        XCTAssertEqual(
            RelayQuickMenuAction.allCases,
            [.openControlCenter, .refresh, .checkForUpdates, .openUpdateRelease, .openLoginItemsSettings, .quit]
        )
        XCTAssertEqual(RelayStatusItemActivation.resolve(eventType: .rightMouseUp), .quickMenu)
        XCTAssertEqual(RelayStatusItemActivation.resolve(eventType: .leftMouseUp), .popover)
        XCTAssertEqual(RelayStatusItemActivation.resolve(eventType: nil), .popover)
    }

    private func temporaryDirectory() throws -> URL {
        let url = URL(fileURLWithPath: "/private/tmp", isDirectory: true)
            .appendingPathComponent("opencodex-relocation-tests.\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }

    private func waitForState(
        _ expected: ApplicationRelocationState,
        controller: ApplicationRelocationController
    ) async throws {
        for _ in 0..<100 {
            if controller.state == expected { return }
            try await Task.sleep(for: .milliseconds(10))
        }
        XCTFail("timed out waiting for relocation state")
    }
}

@MainActor
private final class FixtureApplicationProcessObserver: ApplicationProcessObserving {
    private let witness: ApplicationProcessWitness
    private var running: Bool

    init(witness: ApplicationProcessWitness, isRunning: Bool) {
        self.witness = witness
        self.running = isRunning
    }

    func currentProcessWitness(bundleAt url: URL) -> ApplicationProcessWitness? {
        url.resolvingSymlinksInPath().standardizedFileURL.path == witness.bundlePath
            ? witness
            : nil
    }

    func isProcessRunning(_ witness: ApplicationProcessWitness) -> Bool {
        running && witness == self.witness
    }

    func setRunning(_ value: Bool) {
        running = value
    }
}

@MainActor
private final class FixtureApplicationOpener: ApplicationOpening {
    struct Request: Equatable {
        let url: URL
        let nonce: String
    }

    private let result: Bool
    private(set) var requests: [Request] = []

    init(result: Bool) {
        self.result = result
    }

    func openApplication(at url: URL, nonce: String) async -> Bool {
        requests.append(Request(url: url, nonce: nonce))
        return result
    }
}

private final class FixtureApplicationTrashMover: ApplicationTrashMoving {
    private var results: [Bool]

    init(results: [Bool]) {
        self.results = results
    }

    func moveToTrash(_ url: URL) async -> Bool {
        let result = results.isEmpty ? false : results.removeFirst()
        guard result else { return false }
        do {
            try FileManager.default.removeItem(at: url)
            return true
        } catch {
            return false
        }
    }
}

private struct FixtureApplicationBundleValidator: ApplicationBundleValidating {
    func inspect(bundleAt url: URL) throws -> ApplicationBundleInspection {
        var info = stat()
        guard Darwin.lstat(url.path, &info) == 0,
              (info.st_mode & S_IFMT) == S_IFDIR else {
            throw ApplicationRelocationFailure.sourceBundleInvalid
        }
        let fingerprintURL = url.appendingPathComponent("fingerprint", isDirectory: false)
        let fingerprint = try String(contentsOf: fingerprintURL, encoding: .utf8)
        guard fingerprint.count == 64 else {
            throw ApplicationRelocationFailure.artifactInvalid
        }
        return ApplicationBundleInspection(
            fingerprint: fingerprint,
            fileIdentity: ApplicationRelocationFileIdentity(
                device: UInt64(info.st_dev),
                inode: UInt64(info.st_ino)
            ),
            runtimeMode: .managed
        )
    }
}

private final class RelocationFixture {
    let root: URL
    let home: URL
    let source: URL
    let systemApplications: URL
    let userApplications: URL
    let destination: URL
    let sourceFingerprint = String(repeating: "a", count: 64)

    var journalURL: URL {
        home.appendingPathComponent(
            "Library/Application Support/OpenCodexRelay/application-relocation.json",
            isDirectory: false
        )
    }

    var sourceProcessWitness: ApplicationProcessWitness {
        ApplicationProcessWitness(
            processIdentifier: getpid(),
            launchDate: 1,
            bundlePath: source.resolvingSymlinksInPath().standardizedFileURL.path
        )
    }

    init(existingFingerprint: String? = nil) throws {
        root = URL(fileURLWithPath: "/private/tmp", isDirectory: true)
            .appendingPathComponent("opencodex-relocation-fixture.\(UUID().uuidString)", isDirectory: true)
        home = root.appendingPathComponent("home", isDirectory: true)
        source = root.appendingPathComponent("Downloads/Source.app", isDirectory: true)
        systemApplications = root.appendingPathComponent("SystemApplications", isDirectory: true)
        userApplications = home.appendingPathComponent("Applications", isDirectory: true)
        destination = userApplications.appendingPathComponent(
            SystemApplicationRelocator.applicationName,
            isDirectory: true
        )
        try FileManager.default.createDirectory(at: systemApplications, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(at: userApplications, withIntermediateDirectories: true)
        try createBundle(at: source, fingerprint: sourceFingerprint)
        if let existingFingerprint {
            try createBundle(at: destination, fingerprint: existingFingerprint)
        }
    }

    func makeRelocator() -> SystemApplicationRelocator {
        SystemApplicationRelocator(
            sourceURL: source,
            homeURL: home,
            systemApplicationsURL: systemApplications,
            userApplicationsURL: userApplications,
            fileManager: .default,
            validator: FixtureApplicationBundleValidator()
        )
    }

    func makeDestinationRelocator() -> SystemApplicationRelocator {
        SystemApplicationRelocator(
            sourceURL: destination,
            homeURL: home,
            systemApplicationsURL: systemApplications,
            userApplicationsURL: userApplications,
            fileManager: .default,
            validator: FixtureApplicationBundleValidator()
        )
    }

    func prepareBackupCleanupPending(
        relocator: SystemApplicationRelocator
    ) throws -> ApplicationRelocationJournal {
        guard case let .launch(journal) = try relocator.prepare(
            scope: .user,
            allowReplacement: true,
            sourceProcessWitness: sourceProcessWitness
        ) else {
            throw ApplicationRelocationFailure.journalInvalid
        }
        _ = try relocator.completeDestinationStart(
            nonce: journal.nonce,
            currentBundleURL: destination
        )
        _ = try relocator.markSourceExited(
            nonce: journal.nonce,
            currentBundleURL: destination
        )
        guard case let .trash(_, pending) = try relocator.prepareBackupCleanup(
            nonce: journal.nonce,
            currentBundleURL: destination
        ) else {
            throw ApplicationRelocationFailure.journalInvalid
        }
        return pending
    }

    func prepareCleanupPending(
        relocator: SystemApplicationRelocator
    ) throws -> ApplicationRelocationJournal {
        guard case let .launch(journal) = try relocator.prepare(
            scope: .user,
            allowReplacement: false,
            sourceProcessWitness: sourceProcessWitness
        ) else {
            throw ApplicationRelocationFailure.journalInvalid
        }
        _ = try relocator.completeDestinationStart(
            nonce: journal.nonce,
            currentBundleURL: destination
        )
        _ = try relocator.markSourceExited(
            nonce: journal.nonce,
            currentBundleURL: destination
        )
        guard case let .complete(completed) = try relocator.prepareBackupCleanup(
            nonce: journal.nonce,
            currentBundleURL: destination
        ) else {
            throw ApplicationRelocationFailure.journalInvalid
        }
        return completed
    }

    func fingerprint(at url: URL) throws -> String {
        try String(contentsOf: url.appendingPathComponent("fingerprint"), encoding: .utf8)
    }

    func createBundle(at url: URL, fingerprint: String) throws {
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        try Data(fingerprint.utf8).write(to: url.appendingPathComponent("fingerprint"))
    }

    func writeSchemaOneJournal(
        _ journal: ApplicationRelocationJournal,
        stage: ApplicationRelocationStage
    ) throws {
        let legacy = ApplicationRelocationJournal(
            schema: 1,
            stage: stage,
            nonce: journal.nonce,
            sourcePath: journal.sourcePath,
            destinationPath: journal.destinationPath,
            destinationScope: journal.destinationScope,
            stagingPath: journal.stagingPath,
            backupPath: journal.backupPath,
            backupFingerprint: journal.backupFingerprint,
            backupIdentity: nil,
            destinationWasReused: journal.destinationWasReused,
            sourceIdentity: journal.sourceIdentity,
            sourceFingerprint: journal.sourceFingerprint,
            sourceProcessWitness: nil,
            destinationFingerprint: journal.destinationFingerprint,
            sourceDisposition: nil
        )
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        try encoder.encode(legacy).write(to: journalURL, options: .atomic)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o600],
            ofItemAtPath: journalURL.path
        )
    }

    func cleanup() {
        try? FileManager.default.removeItem(at: root)
    }
}
