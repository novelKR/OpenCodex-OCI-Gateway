import Foundation
import XCTest
@testable import OpenCodexRelay
@testable import OpenCodexRelayCore

@MainActor
final class ContainerRuntimeControllerTests: XCTestCase {
    private enum WaitError: Error {
        case timedOut
    }

    private actor Client: ContainerRuntimeManaging {
        private(set) var inspectCalls = 0
        private(set) var checkCalls = 0
        private(set) var stageCalls: [(String, String, UInt64)] = []
        private(set) var activateCalls: [(String, UInt64, Bool)] = []
        private(set) var stopCalls: [(String, UInt64, Bool)] = []
        private(set) var recoverCalls: [(String, Bool)] = []
        private(set) var submitted: [(String?, String?)] = []
        var inspection: ContainerRuntimeInspection
        var stableManifestAvailable = true

        init() {
            inspection = Client.makeInspection(state: .stopped, staged: false, active: false)
        }

        func inspect() async throws -> ContainerRuntimeInspection {
            inspectCalls += 1
            return inspection
        }

        func check() async throws -> ContainerRuntimeCheckReceipt {
            checkCalls += 1
            return stableManifestAvailable
                ? Self.makeCheck(inspection: inspection)
                : Self.makeUnavailableCheck(inspection: inspection)
        }

        func stage(
            expectedManifestSHA256: String,
            expectedStateDigest: String,
            expectedRoutingGeneration: UInt64
        ) async throws -> ContainerRuntimeMutationReceipt {
            stageCalls.append((expectedManifestSHA256, expectedStateDigest, expectedRoutingGeneration))
            inspection = Self.makeInspection(state: .stopped, staged: true, active: false)
            return Self.makeMutation(inspection)
        }

        func activate(
            expectedStateDigest: String,
            expectedRoutingGeneration: UInt64,
            confirmDesktopExited: Bool
        ) async throws -> ContainerRuntimeMutationReceipt {
            activateCalls.append((expectedStateDigest, expectedRoutingGeneration, confirmDesktopExited))
            inspection = Self.makeInspection(state: .healthy, staged: false, active: true)
            return Self.makeMutation(inspection)
        }

        func stop(
            expectedStateDigest: String,
            expectedRoutingGeneration: UInt64,
            confirmDesktopExited: Bool
        ) async throws -> ContainerRuntimeMutationReceipt {
            stopCalls.append((
                expectedStateDigest,
                expectedRoutingGeneration,
                confirmDesktopExited
            ))
            inspection = Self.makeInspection(state: .stopped, staged: false, active: true)
            return Self.makeMutation(inspection)
        }

        func recover(
            expectedStateDigest: String,
            confirmDesktopExited: Bool
        ) async throws -> ContainerRuntimeMutationReceipt {
            recoverCalls.append((expectedStateDigest, confirmDesktopExited))
            inspection = Self.makeInspection(state: .stopped, staged: true, active: false)
            return Self.makeMutation(inspection)
        }

        func oauthProviders() async throws -> ContainerRuntimeOAuthProvidersReceipt {
            try ContainerRuntimeOAuthProvidersReceipt.decodeStrict(Data(#"{"schema_version":1,"ok":true,"providers":[{"id":"chatgpt","name":"OpenAI Codex","kind":"codex","supports_device_flow":false}]}"#.utf8))
        }

        func oauthStart(
            provider: String,
            kind: ContainerRuntimeOAuthKind
        ) async throws -> ContainerRuntimeOAuthReceipt {
            Self.makeOAuth(status: .pending)
        }

        func oauthStatus(operationID: String) async throws -> ContainerRuntimeOAuthReceipt {
            Self.makeOAuth(status: .pending)
        }

        func oauthSubmit(
            operationID: String,
            redirectURL: String?,
            code: String?
        ) async throws -> ContainerRuntimeOAuthReceipt {
            submitted.append((redirectURL, code))
            return Self.makeOAuth(status: .complete)
        }

        func oauthCancel(operationID: String) async throws -> ContainerRuntimeOAuthReceipt {
            Self.makeOAuth(status: .cancelled)
        }

        func counts() -> (Int, Int, Int, Int) {
            (inspectCalls, checkCalls, stageCalls.count, activateCalls.count)
        }

        func stageWitness() -> (String, String, UInt64)? { stageCalls.last }
        func activationWitness() -> (String, UInt64, Bool)? { activateCalls.last }
        func stopWitness() -> (String, UInt64, Bool)? { stopCalls.last }
        func recoveryWitness() -> (String, Bool)? { recoverCalls.last }
        func submission() -> (String?, String?)? { submitted.last }

        func setInspection(state: ContainerRuntimeState, staged: Bool, active: Bool) {
            inspection = Self.makeInspection(state: state, staged: staged, active: active)
        }

        func setStableManifestAvailable(_ value: Bool) {
            stableManifestAvailable = value
        }

        private static func makeInspection(
            state: ContainerRuntimeState,
            staged: Bool,
            active: Bool
        ) -> ContainerRuntimeInspection {
            let value = ContainerRuntimeInspection(
                schemaVersion: 1,
                ok: true,
                state: state,
                capability: ContainerRuntimeCapability(
                    available: true,
                    reason: "",
                    macOSVersion: "26.5.1",
                    appleContainerVersion: "1.3.1",
                    systemServiceState: "running"
                ),
                staged: staged ? artifact : nil,
                active: active ? artifact : nil,
                stateDigest: String(repeating: "a", count: 64),
                routingGeneration: 11,
                recoveryRequired: false
            )
            return value
        }

        private static var artifact: ContainerRuntimeArtifactSummary {
            ContainerRuntimeArtifactSummary(
                artifactVersion: "2.40.0-r1",
                releaseSequence: 7,
                manifestSHA256: String(repeating: "b", count: 64),
                indexDigest: "sha256:" + String(repeating: "c", count: 64),
                arm64Digest: "sha256:" + String(repeating: "d", count: 64)
            )
        }

        private static func makeCheck(
            inspection: ContainerRuntimeInspection
        ) -> ContainerRuntimeCheckReceipt {
            ContainerRuntimeCheckReceipt(
                schemaVersion: 1,
                ok: true,
                state: inspection.state,
                capability: inspection.capability,
                staged: inspection.staged,
                active: inspection.active,
                stateDigest: inspection.stateDigest,
                routingGeneration: inspection.routingGeneration,
                recoveryRequired: inspection.recoveryRequired,
                status: .updateAvailable,
                candidate: artifact,
                compatible: true,
                reason: ""
            )
        }

        private static func makeUnavailableCheck(
            inspection: ContainerRuntimeInspection
        ) -> ContainerRuntimeCheckReceipt {
            ContainerRuntimeCheckReceipt(
                schemaVersion: 1,
                ok: true,
                state: .unavailable,
                capability: inspection.capability,
                staged: nil,
                active: nil,
                stateDigest: inspection.stateDigest,
                routingGeneration: inspection.routingGeneration,
                recoveryRequired: false,
                status: .unavailable,
                candidate: nil,
                compatible: false,
                reason: "stable_runtime_manifest_unavailable"
            )
        }

        private static func makeMutation(
            _ inspection: ContainerRuntimeInspection
        ) -> ContainerRuntimeMutationReceipt {
            ContainerRuntimeMutationReceipt(
                schemaVersion: inspection.schemaVersion,
                ok: inspection.ok,
                state: inspection.state,
                capability: inspection.capability,
                staged: inspection.staged,
                active: inspection.active,
                stateDigest: inspection.stateDigest,
                routingGeneration: inspection.routingGeneration,
                recoveryRequired: inspection.recoveryRequired
            )
        }

        private static func makeOAuth(
            status: ContainerRuntimeOAuthStatus
        ) -> ContainerRuntimeOAuthReceipt {
            ContainerRuntimeOAuthReceipt(
                schemaVersion: 1,
                ok: true,
                operationID: String(repeating: "e", count: 64),
                provider: "chatgpt",
                kind: .codex,
                status: status,
                authorizationURL: status == .pending ? "https://example.test/login" : nil,
                instructions: status == .pending ? "Open the page" : "done",
                userCode: nil
            )
        }
    }

    func testOptInChecksRuntimeButNeverStagesAutomatically() async throws {
        let suite = "ContainerRuntimeControllerTests.\(UUID().uuidString)"
        let defaults = try XCTUnwrap(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        let client = Client()
        let controller = ContainerRuntimeController(
            client: client,
            defaults: defaults
        )
        controller.start()
        try await wait { controller.inspection != nil }
        var counts = await client.counts()
        XCTAssertEqual(counts.0, 1)
        XCTAssertEqual(counts.1, 0)
        XCTAssertEqual(counts.2, 0)

        controller.setOptedIn(true)
        try await wait { controller.checkReceipt?.status == .updateAvailable }
        counts = await client.counts()
        XCTAssertEqual(counts.1, 1)
        XCTAssertEqual(counts.2, 0)
        XCTAssertTrue(controller.canStage)
    }

    func testNoSignedStableManifestKeepsUIUnavailableAndCannotMutate() async throws {
        let suite = "ContainerRuntimeControllerTests.\(UUID().uuidString)"
        let defaults = try XCTUnwrap(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        let client = Client()
        await client.setStableManifestAvailable(false)
        let controller = ContainerRuntimeController(client: client, defaults: defaults)

        controller.setOptedIn(true)
        try await wait { controller.checkReceipt?.status == .unavailable }

        XCTAssertEqual(controller.inspection?.state, .unavailable)
        XCTAssertFalse(controller.canStage)
        XCTAssertFalse(controller.canActivate)
        controller.stageConfirmed()
        let counts = await client.counts()
        XCTAssertEqual(counts.2, 0)
        XCTAssertEqual(counts.3, 0)
    }

    func testPersistedOptInRunsDueMetadataCheckOnStartup() async throws {
        let suite = "ContainerRuntimePersistedOptIn.\(UUID().uuidString)"
        let defaults = try XCTUnwrap(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        defaults.set(true, forKey: "appleContainerRuntime.optIn.v1")
        defaults.set(
            Date(timeIntervalSince1970: 1_000),
            forKey: "appleContainerRuntime.lastCheck.v1"
        )
        let client = Client()
        let controller = ContainerRuntimeController(
            client: client,
            defaults: defaults,
            now: { Date(timeIntervalSince1970: 1_000 + 24 * 60 * 60) }
        )

        controller.start()

        try await wait { controller.checkReceipt?.status == .updateAvailable }
        let counts = await client.counts()
        XCTAssertEqual(counts.0, 0)
        XCTAssertEqual(counts.1, 1)
        XCTAssertEqual(counts.2, 0)
    }

    func testAutomaticCheckDelayUsesRemainingPersistedInterval() throws {
        let suite = "ContainerRuntimeRemainingInterval.\(UUID().uuidString)"
        let defaults = try XCTUnwrap(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        defaults.set(true, forKey: "appleContainerRuntime.optIn.v1")
        defaults.set(
            Date(timeIntervalSince1970: 1_000),
            forKey: "appleContainerRuntime.lastCheck.v1"
        )
        let controller = ContainerRuntimeController(
            client: Client(),
            defaults: defaults,
            now: { Date(timeIntervalSince1970: 1_000 + 23 * 60 * 60) }
        )

        XCTAssertEqual(controller.automaticCheckDelaySeconds, 60 * 60)
    }

    func testLocalDevelopmentRuntimeUIFailsClosedBeforeRelayctlInvocation() async throws {
        let suite = "ContainerRuntimeLocalDevelopment.\(UUID().uuidString)"
        let defaults = try XCTUnwrap(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        let client = ProcessContainerRuntimeClient(
            executableURL: URL(fileURLWithPath: "/missing/opencodex-relayctl"),
            bindingURL: URL(fileURLWithPath: "/missing/routing-binding.json"),
            runtimeMode: .managed,
            distributionFlavor: .localDevelopment
        )
        let controller = ContainerRuntimeController(client: client, defaults: defaults)

        controller.start()

        try await wait { controller.lastErrorCode != nil }
        XCTAssertEqual(
            controller.lastErrorCode,
            ContainerRuntimeClientError.unsupportedDistribution.safeCode
        )
        XCTAssertNil(controller.inspection)
        XCTAssertFalse(controller.canStage)
        XCTAssertFalse(controller.canActivate)
        XCTAssertFalse(controller.canStop)
        XCTAssertFalse(controller.canRecover)
    }

    func testStageAndActivateCarryCurrentCASWitnesses() async throws {
        let client = Client()
        let defaults = UserDefaults(suiteName: "ContainerRuntimeCAS.\(UUID().uuidString)")!
        let controller = ContainerRuntimeController(
            client: client,
            defaults: defaults
        )
        controller.start()
        try await wait { controller.inspection != nil }
        controller.setOptedIn(true)
        try await wait { controller.canStage }
        controller.stageConfirmed()
        try await wait { controller.canActivate }
        let stagedValue = await client.stageWitness()
        let staged = try XCTUnwrap(stagedValue)
        XCTAssertEqual(staged.0, String(repeating: "b", count: 64))
        XCTAssertEqual(staged.1, String(repeating: "a", count: 64))
        XCTAssertEqual(staged.2, 11)

        let activationWitness = try XCTUnwrap(controller.activationWitness)
        let changedWitness = ContainerRuntimeActivationWitness(
            stateDigest: activationWitness.stateDigest,
            routingGeneration: activationWitness.routingGeneration + 1
        )
        let changedAccepted = await controller.activateAfterVerifiedDesktopExit(
            expected: changedWitness
        )
        XCTAssertFalse(changedAccepted)
        let countsBeforeActivation = await client.counts()
        XCTAssertEqual(countsBeforeActivation.3, 0)
        let accepted = await controller.activateAfterVerifiedDesktopExit(
            expected: activationWitness
        )
        XCTAssertTrue(accepted)
        XCTAssertEqual(controller.inspection?.state, .healthy)
        let activationValue = await client.activationWitness()
        let activated = try XCTUnwrap(activationValue)
        XCTAssertTrue(activated.2)
    }

    func testStoppedActiveRuntimeCanBeExplicitlyRecreated() async throws {
        let client = Client()
        let suite = "ContainerRuntimeRestart.\(UUID().uuidString)"
        let defaults = try XCTUnwrap(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        let controller = ContainerRuntimeController(client: client, defaults: defaults)

        _ = try await client.activate(
            expectedStateDigest: String(repeating: "a", count: 64),
            expectedRoutingGeneration: 11,
            confirmDesktopExited: true
        )
        controller.start()
        try await wait { controller.canStop }
        let stopWitness = try XCTUnwrap(controller.stopWitness)
        let stopped = await controller.stopAfterVerifiedDesktopExit(expected: stopWitness)
        XCTAssertTrue(stopped)
        try await wait { controller.inspection?.state == .stopped }

        XCTAssertNotNil(controller.inspection?.active)
        XCTAssertNil(controller.inspection?.staged)
        XCTAssertTrue(controller.canActivate)

        let before = await client.counts()
        let witness = try XCTUnwrap(controller.activationWitness)
        let accepted = await controller.activateAfterVerifiedDesktopExit(expected: witness)
        XCTAssertTrue(accepted)
        XCTAssertEqual(controller.inspection?.state, .healthy)
        let after = await client.counts()
        XCTAssertEqual(after.3, before.3 + 1)
    }

    func testStopAndRecoveryRequireCurrentDesktopExitWitnesses() async throws {
        let client = Client()
        let suite = "ContainerRuntimeDesktopWitnesses.\(UUID().uuidString)"
        let defaults = try XCTUnwrap(UserDefaults(suiteName: suite))
        defer { defaults.removePersistentDomain(forName: suite) }
        let controller = ContainerRuntimeController(client: client, defaults: defaults)

        _ = try await client.activate(
            expectedStateDigest: String(repeating: "a", count: 64),
            expectedRoutingGeneration: 11,
            confirmDesktopExited: true
        )
        controller.start()
        try await wait { controller.stopWitness != nil }
        let stopWitness = try XCTUnwrap(controller.stopWitness)
        let staleStop = ContainerRuntimeStopWitness(
            stateDigest: stopWitness.stateDigest,
            routingGeneration: stopWitness.routingGeneration + 1
        )
        let staleStopAccepted = await controller.stopAfterVerifiedDesktopExit(expected: staleStop)
        XCTAssertFalse(staleStopAccepted)
        let missingStop = await client.stopWitness()
        XCTAssertNil(missingStop)
        let stopAccepted = await controller.stopAfterVerifiedDesktopExit(expected: stopWitness)
        XCTAssertTrue(stopAccepted)
        let recordedStop = await client.stopWitness()
        XCTAssertEqual(recordedStop?.2, true)

        await client.setInspection(state: .recoveryRequired, staged: true, active: false)
        controller.refresh()
        try await wait { controller.recoveryWitness != nil }
        let recoveryWitness = try XCTUnwrap(controller.recoveryWitness)
        let staleRecovery = ContainerRuntimeRecoveryWitness(
            stateDigest: recoveryWitness.stateDigest,
            routingGeneration: recoveryWitness.routingGeneration + 1
        )
        let staleRecoveryAccepted = await controller.recoverAfterVerifiedDesktopExit(
            expected: staleRecovery
        )
        XCTAssertFalse(staleRecoveryAccepted)
        let missingRecovery = await client.recoveryWitness()
        XCTAssertNil(missingRecovery)
        let recoveryAccepted = await controller.recoverAfterVerifiedDesktopExit(
            expected: recoveryWitness
        )
        XCTAssertTrue(recoveryAccepted)
        let recordedRecovery = await client.recoveryWitness()
        XCTAssertEqual(recordedRecovery?.1, true)
    }

    func testOAuthSubmissionRequiresExplicitAction() async throws {
        let client = Client()
        let controller = ContainerRuntimeController(
            client: client,
            defaults: UserDefaults(suiteName: "ContainerRuntimeActions.\(UUID().uuidString)")!
        )
        controller.start()
        try await wait { controller.inspection != nil }

        _ = try await client.activate(
            expectedStateDigest: String(repeating: "a", count: 64),
            expectedRoutingGeneration: 11,
            confirmDesktopExited: true
        )
        controller.refresh()
        try await wait { controller.canManageOAuth }
        controller.loadOAuthProviders()
        try await wait { controller.providers.count == 1 }
        controller.startOAuth(provider: controller.providers[0])
        try await wait { controller.oauthReceipt?.status == .pending }
        let redirect = "https://localhost/callback?code=secret"
        controller.submitOAuth(redirect)
        try await wait { controller.oauthReceipt?.status == .complete }
        let submissionValue = await client.submission()
        let submission = try XCTUnwrap(submissionValue)
        XCTAssertEqual(submission.0, redirect)
        XCTAssertNil(submission.1)
    }

    private func wait(
        timeout: TimeInterval = 2,
        condition: @escaping @MainActor () -> Bool
    ) async throws {
        let deadline = Date().addingTimeInterval(timeout)
        while !condition() {
            if Date() >= deadline {
                XCTFail("timed out waiting for container runtime controller")
                throw WaitError.timedOut
            }
            try await Task.sleep(for: .milliseconds(10))
        }
    }

}
