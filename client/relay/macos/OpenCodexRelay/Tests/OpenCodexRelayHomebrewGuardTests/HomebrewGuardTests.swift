import Darwin
import Foundation
import XCTest
@testable import OpenCodexRelayHomebrewGuard

final class HomebrewGuardTests: XCTestCase {
    private struct Fixture {
        let temporaryRoot: URL
        let homebrewRoot: URL
        let stateDirectory: URL
        let candidate: HomebrewGuardCandidate
        let leaseID: UUID

        init() throws {
            temporaryRoot = URL(fileURLWithPath: NSTemporaryDirectory(), isDirectory: true)
                .appendingPathComponent("pw-homebrew-guard-\(UUID().uuidString)", isDirectory: true)
                .standardizedFileURL
            homebrewRoot = temporaryRoot.appendingPathComponent("homebrew", isDirectory: true)
            stateDirectory = temporaryRoot.appendingPathComponent("state", isDirectory: true)
            leaseID = UUID()

            let packageRoot = homebrewRoot
                .appendingPathComponent("lib/node_modules/@bitkyc08/opencodex", isDirectory: true)
            let executable = packageRoot.appendingPathComponent("bin/opencodex")
            let cliEntry = packageRoot.appendingPathComponent("src/cli/index.ts")
            let bunExecutable = packageRoot.appendingPathComponent("node_modules/bun/bin/bun")
            let nodeExecutable = homebrewRoot.appendingPathComponent("bin/node")
            let npmCLI = homebrewRoot.appendingPathComponent("lib/node_modules/npm/bin/npm-cli.js")

            try Self.createDirectory(homebrewRoot)
            for file in [executable, cliEntry, bunExecutable, nodeExecutable, npmCLI] {
                try Self.createDirectory(file.deletingLastPathComponent())
                XCTAssertTrue(FileManager.default.createFile(
                    atPath: file.path,
                    contents: Data("#!/bin/sh\n".utf8)
                ))
                XCTAssertEqual(chmod(file.path, mode_t(0o755)), 0)
            }
            for path in [
                homebrewRoot.path,
                homebrewRoot.appendingPathComponent("lib", isDirectory: true).path,
                homebrewRoot.appendingPathComponent("lib/node_modules", isDirectory: true).path,
                packageRoot.path,
            ] {
                XCTAssertEqual(chmod(path, mode_t(0o775)), 0)
            }

            candidate = HomebrewGuardCandidate(
                installationID: String(repeating: "a", count: 24),
                installationFingerprint: String(repeating: "b", count: 64),
                prefix: homebrewRoot.path,
                packageRoot: packageRoot.path,
                executable: executable.path,
                cliEntry: cliEntry.path,
                bunExecutable: bunExecutable.path,
                nodeExecutable: nodeExecutable.path,
                npmCLI: npmCLI.path,
                launchers: []
            )
        }

        func configuration(
            distribution: HomebrewGuardDistribution = .localDevelopment
        ) -> HomebrewGuardEngineConfiguration {
            HomebrewGuardEngineConfiguration(
                allowedRoot: homebrewRoot.path,
                journalURL: stateDirectory.appendingPathComponent("guard.json"),
                lockURL: stateDirectory.appendingPathComponent("guard.lock"),
                distribution: distribution,
                helperVersion: "1.2.3",
                requireRoot: false
            )
        }

        func remove() {
            try? FileManager.default.removeItem(at: temporaryRoot)
        }

        private static func createDirectory(_ url: URL) throws {
            try FileManager.default.createDirectory(
                at: url,
                withIntermediateDirectories: true
            )
            var current = url
            while current.path.hasPrefix(url.deletingLastPathComponent().path) {
                _ = chmod(current.path, mode_t(0o755))
                let parent = current.deletingLastPathComponent()
                if parent == current { break }
                current = parent
                if current.path == NSTemporaryDirectory() { break }
            }
        }
    }

    func testProtocolVersionIsOne() {
        XCTAssertEqual(homebrewGuardProtocolVersion, 1)
    }

    func testPrepareCommitReleaseProtectsAndRestoresModes() throws {
        let fixture = try Fixture()
        defer { fixture.remove() }
        let engine = HomebrewGuardEngine(configuration: fixture.configuration())
        let operationID = UUID().uuidString.lowercased()

        XCTAssertEqual(try call(.status, engine, fixture, candidate: fixture.candidate).state, .ready)
        let prepared = try call(
            .prepare,
            engine,
            fixture,
            operationID: operationID,
            candidate: fixture.candidate
        )
        XCTAssertEqual(prepared.resultCode, .prepared)
        XCTAssertEqual(mode(fixture.homebrewRoot.path), mode_t(0o755))
        XCTAssertEqual(mode(fixture.candidate.packageRoot), mode_t(0o755))
        XCTAssertEqual(mode(fixture.configuration().journalURL.path), mode_t(0o600))

        XCTAssertEqual(
            try call(.commit, engine, fixture, operationID: operationID).state,
            .committed
        )
        XCTAssertEqual(
            try call(.release, engine, fixture, operationID: operationID).resultCode,
            .released
        )
        XCTAssertEqual(mode(fixture.homebrewRoot.path), mode_t(0o775))
        XCTAssertEqual(mode(fixture.candidate.packageRoot), mode_t(0o775))
        XCTAssertFalse(FileManager.default.fileExists(atPath: fixture.configuration().journalURL.path))
    }

    func testPreparedCrashIsAutomaticallyRestoredOnStatus() throws {
        let fixture = try Fixture()
        defer { fixture.remove() }
        var first: HomebrewGuardEngine? = HomebrewGuardEngine(configuration: fixture.configuration())
        let operationID = UUID().uuidString.lowercased()
        _ = try call(
            .prepare,
            try XCTUnwrap(first),
            fixture,
            operationID: operationID,
            candidate: fixture.candidate
        )
        XCTAssertEqual(mode(fixture.homebrewRoot.path), mode_t(0o755))
        first = nil

        let replacement = HomebrewGuardEngine(configuration: fixture.configuration())
        let response = try call(.status, replacement, fixture, candidate: fixture.candidate)
        XCTAssertEqual(response.state, .ready)
        XCTAssertEqual(mode(fixture.homebrewRoot.path), mode_t(0o775))
        XCTAssertFalse(FileManager.default.fileExists(atPath: fixture.configuration().journalURL.path))
    }

    func testCommittedCrashRequiresExplicitRecovery() throws {
        let fixture = try Fixture()
        defer { fixture.remove() }
        var first: HomebrewGuardEngine? = HomebrewGuardEngine(configuration: fixture.configuration())
        let operationID = UUID().uuidString.lowercased()
        _ = try call(
            .prepare,
            try XCTUnwrap(first),
            fixture,
            operationID: operationID,
            candidate: fixture.candidate
        )
        _ = try call(.commit, try XCTUnwrap(first), fixture, operationID: operationID)
        first = nil

        let replacement = HomebrewGuardEngine(configuration: fixture.configuration())
        let status = try call(.status, replacement, fixture)
        XCTAssertEqual(status.state, .recoveryRequired)
        XCTAssertEqual(status.errorCode, .recoveryRequired)
        XCTAssertEqual(status.operationID, operationID)
        XCTAssertEqual(mode(fixture.homebrewRoot.path), mode_t(0o755))

        let recovered = try call(.recover, replacement, fixture, operationID: operationID)
        XCTAssertEqual(recovered.resultCode, .recovered)
        XCTAssertEqual(mode(fixture.homebrewRoot.path), mode_t(0o775))
    }

    func testPreparedConnectionInvalidationRestoresWhileDaemonEngineLives() throws {
        let fixture = try Fixture()
        defer { fixture.remove() }
        let engine = HomebrewGuardEngine(configuration: fixture.configuration())
        let operationID = UUID().uuidString.lowercased()
        _ = try call(
            .prepare,
            engine,
            fixture,
            operationID: operationID,
            candidate: fixture.candidate
        )

        engine.connectionInvalidated(leaseID: fixture.leaseID, peerUID: geteuid())

        let status = try call(.status, engine, fixture, candidate: fixture.candidate)
        XCTAssertEqual(status.state, .ready)
        XCTAssertEqual(mode(fixture.homebrewRoot.path), mode_t(0o775))
        XCTAssertFalse(FileManager.default.fileExists(atPath: fixture.configuration().journalURL.path))
    }

    func testCommittedConnectionInvalidationKeepsExplicitRecovery() throws {
        let fixture = try Fixture()
        defer { fixture.remove() }
        let engine = HomebrewGuardEngine(configuration: fixture.configuration())
        let operationID = UUID().uuidString.lowercased()
        _ = try call(
            .prepare,
            engine,
            fixture,
            operationID: operationID,
            candidate: fixture.candidate
        )
        _ = try call(.commit, engine, fixture, operationID: operationID)

        engine.connectionInvalidated(leaseID: fixture.leaseID, peerUID: geteuid())

        let status = try call(.status, engine, fixture)
        XCTAssertEqual(status.state, .recoveryRequired)
        XCTAssertEqual(status.errorCode, .recoveryRequired)
        XCTAssertEqual(mode(fixture.homebrewRoot.path), mode_t(0o755))
        let recovered = try call(.recover, engine, fixture, operationID: operationID)
        XCTAssertEqual(recovered.resultCode, .recovered)
        XCTAssertEqual(mode(fixture.homebrewRoot.path), mode_t(0o775))
    }

    func testCommitRequiresPreparingConnectionLease() throws {
        let fixture = try Fixture()
        defer { fixture.remove() }
        let engine = HomebrewGuardEngine(configuration: fixture.configuration())
        let operationID = UUID().uuidString.lowercased()
        _ = try call(
            .prepare,
            engine,
            fixture,
            operationID: operationID,
            candidate: fixture.candidate
        )

        let blocked = try call(
            .commit,
            engine,
            fixture,
            operationID: operationID,
            leaseID: UUID()
        )
        XCTAssertEqual(blocked.errorCode, .busy)

        XCTAssertEqual(
            try call(.commit, engine, fixture, operationID: operationID).resultCode,
            .committed
        )
        XCTAssertEqual(
            try call(.release, engine, fixture, operationID: operationID).resultCode,
            .released
        )
    }

    func testPrepareInspectionFailureReleasesSystemLock() throws {
        let fixture = try Fixture()
        defer { fixture.remove() }
        let engine = HomebrewGuardEngine(configuration: fixture.configuration())
        let candidate = HomebrewGuardCandidate(
            installationID: fixture.candidate.installationID,
            installationFingerprint: fixture.candidate.installationFingerprint,
            prefix: fixture.candidate.prefix,
            packageRoot: fixture.candidate.packageRoot,
            executable: fixture.candidate.packageRoot + "/bin/missing",
            cliEntry: fixture.candidate.cliEntry,
            bunExecutable: fixture.candidate.bunExecutable,
            nodeExecutable: fixture.candidate.nodeExecutable,
            npmCLI: fixture.candidate.npmCLI,
            launchers: fixture.candidate.launchers
        )
        let failed = try call(
            .prepare,
            engine,
            fixture,
            operationID: UUID().uuidString.lowercased(),
            candidate: candidate
        )
        XCTAssertEqual(failed.errorCode, .candidateChanged)

        let contender = HomebrewGuardEngine(configuration: fixture.configuration())
        let status = try call(.status, contender, fixture, candidate: fixture.candidate)
        XCTAssertEqual(status.state, .ready)
    }

    func testPrepareJournalWriteFailureReleasesSystemLockWithoutJournal() throws {
        let fixture = try Fixture()
        defer { fixture.remove() }
        try FileManager.default.createDirectory(
            at: fixture.stateDirectory,
            withIntermediateDirectories: true
        )
        let configuration = fixture.configuration()
        XCTAssertTrue(FileManager.default.createFile(
            atPath: configuration.lockURL.path,
            contents: Data()
        ))
        XCTAssertEqual(chmod(configuration.lockURL.path, mode_t(0o600)), 0)
        XCTAssertEqual(chmod(fixture.stateDirectory.path, mode_t(0o500)), 0)
        defer { _ = chmod(fixture.stateDirectory.path, mode_t(0o700)) }

        let engine = HomebrewGuardEngine(configuration: configuration)
        let failed = try call(
            .prepare,
            engine,
            fixture,
            operationID: UUID().uuidString.lowercased(),
            candidate: fixture.candidate
        )
        XCTAssertEqual(failed.errorCode, .protectionFailed)
        XCTAssertFalse(FileManager.default.fileExists(atPath: configuration.journalURL.path))

        let contender = HomebrewGuardEngine(configuration: configuration)
        let status = try call(.status, contender, fixture)
        XCTAssertEqual(status.state, .ready)
    }

    func testCommittedPackageRemovalRestoresSurvivingHomebrewDirectories() throws {
        let fixture = try Fixture()
        defer { fixture.remove() }
        let engine = HomebrewGuardEngine(configuration: fixture.configuration())
        let operationID = UUID().uuidString.lowercased()
        _ = try call(
            .prepare,
            engine,
            fixture,
            operationID: operationID,
            candidate: fixture.candidate
        )
        _ = try call(.commit, engine, fixture, operationID: operationID)

        try FileManager.default.removeItem(atPath: fixture.candidate.packageRoot)

        let released = try call(.release, engine, fixture, operationID: operationID)
        XCTAssertEqual(released.resultCode, .released)
        XCTAssertEqual(mode(fixture.homebrewRoot.path), mode_t(0o775))
        XCTAssertFalse(FileManager.default.fileExists(atPath: fixture.configuration().journalURL.path))
    }

    func testWorldWritableAndExtendedACLRemainFailClosed() throws {
        let fixture = try Fixture()
        defer { fixture.remove() }
        let engine = HomebrewGuardEngine(configuration: fixture.configuration())

        XCTAssertEqual(chmod(fixture.homebrewRoot.path, mode_t(0o777)), 0)
        var response = try call(.status, engine, fixture, candidate: fixture.candidate)
        XCTAssertEqual(response.errorCode, .protectionFailed)

        XCTAssertEqual(chmod(fixture.homebrewRoot.path, mode_t(0o775)), 0)
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/bin/chmod")
        process.arguments = ["+a", "everyone allow read", fixture.homebrewRoot.path]
        try process.run()
        process.waitUntilExit()
        guard process.terminationStatus == 0 else {
            throw XCTSkip("This host does not permit an ACL fixture")
        }
        defer {
            let cleanup = Process()
            cleanup.executableURL = URL(fileURLWithPath: "/bin/chmod")
            cleanup.arguments = ["-N", fixture.homebrewRoot.path]
            try? cleanup.run()
            cleanup.waitUntilExit()
        }
        response = try call(.status, engine, fixture, candidate: fixture.candidate)
        XCTAssertEqual(response.errorCode, .protectionFailed)
    }

    func testForeignDistributionDoesNotRetainOrphanedPreparedLock() throws {
        let fixture = try Fixture()
        defer { fixture.remove() }
        var original: HomebrewGuardEngine? = HomebrewGuardEngine(configuration: fixture.configuration())
        let operationID = UUID().uuidString.lowercased()
        _ = try call(
            .prepare,
            try XCTUnwrap(original),
            fixture,
            operationID: operationID,
            candidate: fixture.candidate
        )
        original = nil

        let foreign = HomebrewGuardEngine(
            configuration: fixture.configuration(distribution: .production)
        )
        let blocked = try call(
            .status,
            foreign,
            fixture,
            distribution: .production
        )
        XCTAssertEqual(blocked.errorCode, .busy)

        let owner = HomebrewGuardEngine(configuration: fixture.configuration())
        let restored = try call(.status, owner, fixture)
        XCTAssertEqual(restored.state, .ready)
        XCTAssertEqual(restored.resultCode, .statusReady)
        XCTAssertEqual(mode(fixture.homebrewRoot.path), mode_t(0o775))
        XCTAssertFalse(FileManager.default.fileExists(atPath: fixture.configuration().journalURL.path))
    }

    func testForeignDistributionDoesNotRetainOrphanedCommittedLock() throws {
        let fixture = try Fixture()
        defer { fixture.remove() }
        var original: HomebrewGuardEngine? = HomebrewGuardEngine(configuration: fixture.configuration())
        let operationID = UUID().uuidString.lowercased()
        _ = try call(
            .prepare,
            try XCTUnwrap(original),
            fixture,
            operationID: operationID,
            candidate: fixture.candidate
        )
        _ = try call(.commit, try XCTUnwrap(original), fixture, operationID: operationID)
        original = nil

        let foreign = HomebrewGuardEngine(
            configuration: fixture.configuration(distribution: .production)
        )
        let blocked = try call(
            .recover,
            foreign,
            fixture,
            distribution: .production,
            operationID: operationID
        )
        XCTAssertEqual(blocked.errorCode, .recoveryRequired)

        let owner = HomebrewGuardEngine(configuration: fixture.configuration())
        let recovered = try call(
            .recover,
            owner,
            fixture,
            operationID: operationID
        )
        XCTAssertEqual(recovered.resultCode, .recovered)
        XCTAssertEqual(mode(fixture.homebrewRoot.path), mode_t(0o775))
        XCTAssertFalse(FileManager.default.fileExists(atPath: fixture.configuration().journalURL.path))
    }

    func testForeignUserCannotDisruptPreparedProtection() throws {
        let fixture = try Fixture()
        defer { fixture.remove() }
        let owner = HomebrewGuardEngine(configuration: fixture.configuration())
        let operationID = UUID().uuidString.lowercased()
        _ = try call(
            .prepare,
            owner,
            fixture,
            operationID: operationID,
            candidate: fixture.candidate
        )
        let foreignUID: uid_t = geteuid() == 501 ? 502 : 501

        let blocked = try call(
            .status,
            owner,
            fixture,
            peerUID: foreignUID
        )
        XCTAssertEqual(blocked.errorCode, .busy)
        XCTAssertNil(blocked.operationID)

        let stillPrepared = try call(.status, owner, fixture)
        XCTAssertEqual(stillPrepared.state, .prepared)
        XCTAssertEqual(stillPrepared.operationID, operationID)
        XCTAssertEqual(mode(fixture.homebrewRoot.path), mode_t(0o755))

        let released = try call(.release, owner, fixture, operationID: operationID)
        XCTAssertEqual(released.resultCode, .released)
        XCTAssertEqual(mode(fixture.homebrewRoot.path), mode_t(0o775))
    }

    func testForeignUserCannotRecoverCommittedProtection() throws {
        let fixture = try Fixture()
        defer { fixture.remove() }
        let owner = HomebrewGuardEngine(configuration: fixture.configuration())
        let operationID = UUID().uuidString.lowercased()
        _ = try call(
            .prepare,
            owner,
            fixture,
            operationID: operationID,
            candidate: fixture.candidate
        )
        _ = try call(.commit, owner, fixture, operationID: operationID)
        let foreignUID: uid_t = geteuid() == 501 ? 502 : 501

        let blocked = try call(
            .recover,
            owner,
            fixture,
            operationID: operationID,
            peerUID: foreignUID
        )
        XCTAssertEqual(blocked.errorCode, .recoveryRequired)

        let recovered = try call(
            .recover,
            owner,
            fixture,
            operationID: operationID
        )
        XCTAssertEqual(recovered.resultCode, .recovered)
        XCTAssertEqual(mode(fixture.homebrewRoot.path), mode_t(0o775))
    }

    func testDistributionLockAndModeDriftFailClosed() throws {
        let fixture = try Fixture()
        defer { fixture.remove() }
        let first = HomebrewGuardEngine(configuration: fixture.configuration())
        let second = HomebrewGuardEngine(
            configuration: fixture.configuration(distribution: .production)
        )
        let operationID = UUID().uuidString.lowercased()
        _ = try call(
            .prepare,
            first,
            fixture,
            operationID: operationID,
            candidate: fixture.candidate
        )

        let competing = try call(
            .status,
            second,
            fixture,
            distribution: .production,
            candidate: fixture.candidate
        )
        XCTAssertEqual(competing.errorCode, .busy)

        XCTAssertEqual(chmod(fixture.homebrewRoot.path, mode_t(0o700)), 0)
        let commit = try call(.commit, first, fixture, operationID: operationID)
        XCTAssertEqual(commit.errorCode, .restoreFailed)

        XCTAssertEqual(chmod(fixture.homebrewRoot.path, mode_t(0o755)), 0)
        let released = try call(.release, first, fixture, operationID: operationID)
        XCTAssertEqual(released.resultCode, .released)
        XCTAssertEqual(mode(fixture.homebrewRoot.path), mode_t(0o775))
    }

    private func call(
        _ operation: HomebrewGuardOperation,
        _ engine: HomebrewGuardEngine,
        _ fixture: Fixture,
        distribution: HomebrewGuardDistribution = .localDevelopment,
        operationID: String? = nil,
        candidate: HomebrewGuardCandidate? = nil,
        peerUID: uid_t = geteuid(),
        leaseID: UUID? = nil
    ) throws -> HomebrewGuardResponse {
        let request = HomebrewGuardRequest(
            distribution: distribution,
            operationID: operationID,
            candidate: candidate
        )
        let responseData = engine.perform(
            operation: operation,
            requestData: try HomebrewGuardCodec.encode(request),
            peerUID: peerUID,
            leaseID: leaseID ?? fixture.leaseID
        )
        return try HomebrewGuardCodec.decodeResponse(responseData)
    }

    private func mode(_ path: String) -> mode_t {
        var info = stat()
        XCTAssertEqual(lstat(path, &info), 0)
        return info.st_mode & mode_t(0o7777)
    }
}
