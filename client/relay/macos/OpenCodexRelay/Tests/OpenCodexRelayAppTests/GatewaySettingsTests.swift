import Foundation
import Security
import XCTest
@testable import OpenCodexRelay
import OpenCodexRelayCore

@MainActor
final class GatewaySettingsTests: XCTestCase {
    private actor IntegrationClient: SelfHostedIntegrationManaging {
        private var state: SelfHostedIntegrationState
        private(set) var applications: [(GatewayCandidate, String)] = []

        init(state: SelfHostedIntegrationState = .integrationRequired) {
            self.state = state
        }

        func inspect() async throws -> SelfHostedIntegrationInspection {
            SelfHostedIntegrationInspection(
                schemaVersion: 1,
                state: state,
                stateDigest: String(repeating: "d", count: 64),
                credentialAccount: "test-account"
            )
        }

        func apply(
            candidate: GatewayCandidate,
            expectedStateDigest: String
        ) async throws -> SelfHostedIntegrationReceipt {
            applications.append((candidate, expectedStateDigest))
            state = .ready
            return SelfHostedIntegrationReceipt(
                schemaVersion: 1,
                ok: true,
                state: .ready,
                configDigest: String(repeating: "e", count: 64),
                routingGeneration: 1
            )
        }

        func recover() async throws -> SelfHostedIntegrationReceipt {
            state = .integrationRequired
            return SelfHostedIntegrationReceipt(
                schemaVersion: 1,
                ok: true,
                state: .integrationRequired,
                configDigest: nil,
                routingGeneration: nil
            )
        }
    }

    private actor GatewayClient: GatewayManaging {
        var inspection: GatewayInspection
        var testError: Error?
        var applyReceipt: GatewayApplyReceipt
        var postApplyInspection: GatewayInspection
        var testedURLs: [String] = []
        var appliedValues: [(String, String, UInt64)] = []
        let onTest: (@Sendable () -> Void)?
        let onApply: (@Sendable () -> Void)?

        init(
            inspection: GatewayInspection,
            testError: Error? = nil,
            applyReceipt: GatewayApplyReceipt? = nil,
            postApplyInspection: GatewayInspection? = nil,
            onTest: (@Sendable () -> Void)? = nil,
            onApply: (@Sendable () -> Void)? = nil
        ) {
            self.inspection = inspection
            self.testError = testError
            self.applyReceipt = applyReceipt ?? decodeGatewayApplyReceipt(
                digest: inspection.configDigest,
                generation: inspection.routingGeneration,
                runtimeReloaded: false
            )
            self.postApplyInspection = postApplyInspection ?? inspection
            self.onTest = onTest
            self.onApply = onApply
        }

        func inspect() async throws -> GatewayInspection { inspection }

        func test(upstreamBaseURL: String) async throws -> GatewayValidation {
            testedURLs.append(upstreamBaseURL)
            if let testError { throw testError }
            onTest?()
            return decodeGatewayValidation(
                digest: inspection.configDigest,
                generation: inspection.routingGeneration
            )
        }

        func apply(
            upstreamBaseURL: String,
            expectedConfigDigest: String,
            expectedRoutingGeneration: UInt64
        ) async throws -> GatewayApplyReceipt {
            appliedValues.append((
                upstreamBaseURL,
                expectedConfigDigest,
                expectedRoutingGeneration
            ))
            inspection = postApplyInspection
            onApply?()
            return applyReceipt
        }
    }

    private actor SequencedGatewayClient: GatewayManaging {
        private let firstInspection: GatewayInspection
        private let secondInspection: GatewayInspection
        private var inspectionCount = 0
        private var firstStartedContinuation: CheckedContinuation<Void, Never>?
        private var firstReleaseContinuation: CheckedContinuation<Void, Never>?

        init(first: GatewayInspection, second: GatewayInspection) {
            firstInspection = first
            secondInspection = second
        }

        func inspect() async throws -> GatewayInspection {
            inspectionCount += 1
            guard inspectionCount == 1 else { return secondInspection }
            firstStartedContinuation?.resume()
            firstStartedContinuation = nil
            await withCheckedContinuation { continuation in
                firstReleaseContinuation = continuation
            }
            return firstInspection
        }

        func waitForFirstInspection() async {
            guard inspectionCount == 0 else { return }
            await withCheckedContinuation { continuation in
                firstStartedContinuation = continuation
            }
        }

        func releaseFirstInspection() {
            firstReleaseContinuation?.resume()
            firstReleaseContinuation = nil
        }

        func test(upstreamBaseURL: String) async throws -> GatewayValidation {
            throw RelayctlError.helperUnavailable
        }

        func apply(
            upstreamBaseURL: String,
            expectedConfigDigest: String,
            expectedRoutingGeneration: UInt64
        ) async throws -> GatewayApplyReceipt {
            throw RelayctlError.helperUnavailable
        }
    }

    private final class CredentialStore: GatewayCredentialStoring, @unchecked Sendable {
        private let lock = NSLock()
        private var metadata: [GatewayCredentialKind: GatewayCredentialMetadata]
        private var inspectedKindSetsStorage: [Set<GatewayCredentialKind>] = []
        private(set) var replacements: [GatewayCredentialKind: String] = [:]

        init(configured: Set<GatewayCredentialKind> = []) {
            metadata = Dictionary(uniqueKeysWithValues: GatewayCredentialKind.allCases.map {
                ($0, GatewayCredentialMetadata(
                    configured: configured.contains($0),
                    modifiedAt: configured.contains($0) ? Date(timeIntervalSince1970: 10) : nil
                ))
            })
        }

        func inspect(account: String) throws -> [GatewayCredentialKind: GatewayCredentialMetadata] {
            lock.lock()
            defer { lock.unlock() }
            return metadata
        }

        func inspect(
            account: String,
            kinds: Set<GatewayCredentialKind>
        ) throws -> [GatewayCredentialKind: GatewayCredentialMetadata] {
            lock.lock()
            defer { lock.unlock() }
            inspectedKindSetsStorage.append(kinds)
            return metadata.filter { kinds.contains($0.key) }
        }

        func replace(
            _ kind: GatewayCredentialKind,
            account: String,
            value: String
        ) throws -> GatewayCredentialMetadata {
            lock.lock()
            defer { lock.unlock() }
            replacements[kind] = value
            let next = GatewayCredentialMetadata(
                configured: true,
                modifiedAt: Date(timeIntervalSince1970: 20)
            )
            metadata[kind] = next
            return next
        }

        func replacement(for kind: GatewayCredentialKind) -> String? {
            lock.lock()
            defer { lock.unlock() }
            return replacements[kind]
        }

        func setModifiedAt(
            _ kind: GatewayCredentialKind,
            timeIntervalSince1970: TimeInterval
        ) {
            lock.lock()
            metadata[kind] = GatewayCredentialMetadata(
                configured: true,
                modifiedAt: Date(timeIntervalSince1970: timeIntervalSince1970)
            )
            lock.unlock()
        }

        func inspectedKindSets() -> [Set<GatewayCredentialKind>] {
            lock.lock()
            defer { lock.unlock() }
            return inspectedKindSetsStorage
        }
    }

    private final class ControlledCredentialStore: GatewayCredentialStoring, @unchecked Sendable {
        private let lock = NSLock()
        private let blockedInspectionStarted = DispatchSemaphore(value: 0)
        private let blockedInspectionRelease = DispatchSemaphore(value: 0)
        private var shouldBlockCloudflareInspection = false
        private var failingKinds: Set<GatewayCredentialKind> = []
        private let metadata = Dictionary(uniqueKeysWithValues: GatewayCredentialKind.allCases.map {
            ($0, GatewayCredentialMetadata(
                configured: true,
                modifiedAt: Date(timeIntervalSince1970: 10)
            ))
        })

        func inspect(account: String) throws -> [GatewayCredentialKind: GatewayCredentialMetadata] {
            try inspect(account: account, kinds: Set(GatewayCredentialKind.allCases))
        }

        func inspect(
            account: String,
            kinds: Set<GatewayCredentialKind>
        ) throws -> [GatewayCredentialKind: GatewayCredentialMetadata] {
            lock.lock()
            let shouldBlock = shouldBlockCloudflareInspection &&
                kinds.contains(.cloudflareClientID)
            if shouldBlock {
                shouldBlockCloudflareInspection = false
            }
            let shouldFail = !failingKinds.isDisjoint(with: kinds)
            lock.unlock()

            if shouldBlock {
                blockedInspectionStarted.signal()
                blockedInspectionRelease.wait()
            }
            if shouldFail {
                throw GatewayCredentialStoreError.keychainFailure
            }
            return metadata.filter { kinds.contains($0.key) }
        }

        func replace(
            _ kind: GatewayCredentialKind,
            account: String,
            value: String
        ) throws -> GatewayCredentialMetadata {
            metadata[kind]!
        }

        func blockNextCloudflareInspection() {
            lock.lock()
            shouldBlockCloudflareInspection = true
            lock.unlock()
        }

        func waitForBlockedInspection() -> Bool {
            blockedInspectionStarted.wait(timeout: .now() + 1) == .success
        }

        func releaseBlockedInspection() {
            blockedInspectionRelease.signal()
        }

        func failInspections(containing kinds: Set<GatewayCredentialKind>) {
            lock.lock()
            failingKinds = kinds
            lock.unlock()
        }
    }

    private final class ReceiptStore: GatewayVerificationReceiptStoring, @unchecked Sendable {
        private let lock = NSLock()
        private var receipt: GatewayVerificationReceipt?
        private(set) var clearCount = 0

        init(_ receipt: GatewayVerificationReceipt? = nil) {
            self.receipt = receipt
        }

        func load() -> GatewayVerificationReceipt? {
            lock.lock()
            defer { lock.unlock() }
            return receipt
        }

        func save(_ receipt: GatewayVerificationReceipt) {
            lock.lock()
            self.receipt = receipt
            lock.unlock()
        }

        func clear() {
            lock.lock()
            receipt = nil
            clearCount += 1
            lock.unlock()
        }

        func snapshot() -> GatewayVerificationReceipt? {
            load()
        }
    }

    private final class ResolutionBox {
        var value: GatewaySettingsResolution

        init(_ value: GatewaySettingsResolution) {
            self.value = value
        }
    }

    func testLoadProjectsCredentialPresenceAndRequiresValidationWithoutReceipt() async {
        let inspection = decodeGatewayInspection()
        let client = GatewayClient(inspection: inspection)
        let credentials = CredentialStore(configured: [.cloudflareClientID])
        let controller = GatewaySettingsController(
            client: client,
            credentialStore: credentials,
            receiptStore: ReceiptStore(),
            activityLog: RelayActivityLogStore(),
            onRoutingRefreshRequested: {}
        )

        await controller.refresh()

        XCTAssertEqual(controller.state, .needsValidation)
        XCTAssertEqual(controller.draftURL, inspection.upstreamBaseURL)
        XCTAssertEqual(controller.credentialMetadata[.cloudflareClientID]?.configured, true)
        XCTAssertEqual(controller.credentialMetadata[.gatewayAPIKey]?.configured, false)
        XCTAssertTrue(controller.canTest)
        XCTAssertFalse(controller.canApply)
    }

    func testResolverRefreshRecoversAndClearsStateWhenBindingBecomesUnsafe() async {
        let inspection = decodeGatewayInspection()
        let client = GatewayClient(inspection: inspection)
        let credentials = CredentialStore(configured: [.cloudflareClientID])
        let resolution = ResolutionBox(GatewaySettingsResolution(
            client: nil,
            unavailability: .bindingMissing
        ))
        let controller = GatewaySettingsController(
            resolver: { resolution.value },
            credentialStore: credentials,
            receiptStore: ReceiptStore(),
            activityLog: RelayActivityLogStore(),
            onRoutingRefreshRequested: {}
        )

        await controller.refresh()
        XCTAssertEqual(controller.state, .integrationRequired)
        XCTAssertNil(controller.inspection)

        resolution.value = GatewaySettingsResolution(client: client)
        await controller.refresh()
        XCTAssertEqual(controller.state, .needsValidation)
        XCTAssertEqual(controller.inspection, inspection)
        XCTAssertEqual(controller.draftURL, inspection.upstreamBaseURL)
        XCTAssertEqual(
            controller.credentialMetadata[.cloudflareClientID]?.configured,
            true
        )

        controller.draftURL = "https://candidate.example.test/v1"
        controller.draftDidChange()
        resolution.value = GatewaySettingsResolution(
            client: nil,
            unavailability: .bindingUnsafe
        )
        await controller.refresh()

        XCTAssertEqual(controller.state, .bindingUnsafe)
        XCTAssertEqual(controller.lastErrorCode, "routing_binding_unsafe")
        XCTAssertNil(controller.inspection)
        XCTAssertTrue(controller.credentialMetadata.isEmpty)
        XCTAssertEqual(controller.draftURL, "")
        XCTAssertFalse(controller.canEditCredentials)
        XCTAssertFalse(controller.canTest)
        XCTAssertFalse(controller.canApply)
    }

    func testMissingBindingPreparesIntegrationFromConsumerInputsWithoutLoggingValues() async {
        let integration = IntegrationClient()
        let activityLog = RelayActivityLogStore(subsystem: "test.gateway.integration")
        var refreshCount = 0
        let controller = GatewaySettingsController(
            client: nil,
            unavailability: .bindingMissing,
            integrationClient: integration,
            credentialStore: CredentialStore(),
            receiptStore: ReceiptStore(),
            activityLog: activityLog,
            onRoutingRefreshRequested: { refreshCount += 1 }
        )

        await controller.refresh()
        XCTAssertEqual(controller.state, .integrationRequired)
        XCTAssertTrue(controller.canEditCredentials)

        let privateURL = "https://private-self-hosted.example.test/v1"
        controller.draftURL = privateURL
        controller.authenticationProfile = .gatewayAPIKey
        controller.authenticationProfileDidChange()
        await waitUntil { controller.credentialMetadataState == .ready }
        XCTAssertTrue(controller.canPrepareIntegration)

        controller.prepareIntegration()
        await waitUntil { refreshCount >= 1 && !controller.isBusy }

        let applications = await integration.applications
        XCTAssertEqual(applications.count, 1)
        XCTAssertEqual(
            applications.first?.0,
            GatewayCandidate(
                upstreamBaseURL: privateURL,
                authenticationProfile: .gatewayAPIKey
            )
        )
        XCTAssertEqual(applications.first?.1, String(repeating: "d", count: 64))
        XCTAssertFalse(activityLog.jsonLines().contains(privateURL))
        XCTAssertTrue(activityLog.jsonLines().contains("self_hosted_integration_finished"))
    }

    func testResolverBlocksActionsWhenBindingChangesImmediatelyBeforeStart() async {
        let inspection = decodeGatewayInspection()
        let client = GatewayClient(inspection: inspection)
        let credentials = CredentialStore(configured: Set(GatewayCredentialKind.allCases))
        let resolution = ResolutionBox(GatewaySettingsResolution(client: client))
        let controller = GatewaySettingsController(
            resolver: { resolution.value },
            credentialStore: credentials,
            receiptStore: ReceiptStore(),
            activityLog: RelayActivityLogStore(),
            onRoutingRefreshRequested: {}
        )

        await controller.refresh()
        controller.draftURL = "https://candidate.example.test/v1"
        controller.draftDidChange()
        resolution.value = GatewaySettingsResolution(
            client: nil,
            unavailability: .bindingUnsafe
        )
        controller.test()
        XCTAssertEqual(controller.state, .bindingUnsafe)
        let testedURLs = await client.testedURLs
        XCTAssertTrue(testedURLs.isEmpty)

        resolution.value = GatewaySettingsResolution(client: client)
        await controller.refresh()
        controller.draftURL = "https://candidate.example.test/v1"
        controller.draftDidChange()
        resolution.value = GatewaySettingsResolution(
            client: nil,
            unavailability: .bindingInvalid
        )
        controller.apply()
        XCTAssertEqual(controller.state, .bindingInvalid)
        let appliedValues = await client.appliedValues
        XCTAssertTrue(appliedValues.isEmpty)

        resolution.value = GatewaySettingsResolution(client: client)
        await controller.refresh()
        resolution.value = GatewaySettingsResolution(
            client: nil,
            unavailability: .bindingMissing
        )
        let replaced = await controller.replaceCredential(
            .gatewayAPIKey,
            value: "must-not-be-written"
        )
        XCTAssertFalse(replaced)
        XCTAssertEqual(controller.state, .integrationRequired)
        XCTAssertNil(credentials.replacement(for: .gatewayAPIKey))
        XCTAssertNil(controller.inspection)
        XCTAssertTrue(controller.credentialMetadata.isEmpty)
        XCTAssertEqual(controller.draftURL, "")
    }

    func testCredentialReplacementPersistsOnAuthenticationFailureAndInvalidatesReceipt() async {
        let inspection = decodeGatewayInspection()
        let client = GatewayClient(
            inspection: inspection,
            testError: RelayctlError.reported(.authenticationFailed)
        )
        let credentials = CredentialStore(configured: Set(GatewayCredentialKind.allCases))
        let receipts = ReceiptStore(GatewayVerificationReceipt(
            schema: 1,
            configDigest: inspection.configDigest,
            credentialModificationTimes: [:],
            verifiedAt: Date(),
            resultCode: "connected"
        ))
        let activityLog = RelayActivityLogStore()
        let controller = GatewaySettingsController(
            client: client,
            credentialStore: credentials,
            receiptStore: receipts,
            activityLog: activityLog,
            onRoutingRefreshRequested: {}
        )
        await controller.refresh()

        let saved = await controller.replaceCredential(
            .gatewayAPIKey,
            value: "new-private-value"
        )

        XCTAssertTrue(saved)
        XCTAssertEqual(credentials.replacement(for: .gatewayAPIKey), "new-private-value")
        XCTAssertEqual(controller.state, .authenticationMismatch)
        XCTAssertNil(receipts.snapshot())
        XCTAssertGreaterThan(receipts.clearCount, 0)
        let logs = activityLog.jsonLines()
        XCTAssertFalse(logs.contains("new-private-value"))
        XCTAssertFalse(logs.contains("origin.example.test"))
    }

    func testCredentialValidationMapsSafeFailureStates() async {
        let cases: [(RelayctlError, GatewaySettingsState)] = [
            (.reported(.authenticationFailed), .authenticationMismatch),
            (.reported(.gatewayUnreachable), .unreachable),
            (.reported(.catalogInvalid), .catalogInvalid),
        ]
        for (error, expected) in cases {
            let inspection = decodeGatewayInspection()
            let client = GatewayClient(inspection: inspection, testError: error)
            let controller = GatewaySettingsController(
                client: client,
                credentialStore: CredentialStore(),
                receiptStore: ReceiptStore(),
                activityLog: RelayActivityLogStore(),
                onRoutingRefreshRequested: {}
            )
            await controller.refresh()
            let saved = await controller.replaceCredential(
                .cloudflareClientID,
                value: "new-value"
            )
            XCTAssertTrue(saved)
            XCTAssertEqual(controller.state, expected)
        }
    }

    func testCredentialReplacementValidatesCurrentAddressButKeepsEditedDraftPending() async {
        let inspection = decodeGatewayInspection()
        let client = GatewayClient(inspection: inspection)
        let credentials = CredentialStore(configured: Set(GatewayCredentialKind.allCases))
        let receipts = ReceiptStore()
        let controller = GatewaySettingsController(
            client: client,
            credentialStore: credentials,
            receiptStore: receipts,
            activityLog: RelayActivityLogStore(),
            onRoutingRefreshRequested: {}
        )
        await controller.refresh()
        controller.draftURL = "https://draft.example.test/v1"
        controller.draftDidChange()

        let saved = await controller.replaceCredential(
            .gatewayAPIKey,
            value: "new-private-value"
        )

        XCTAssertTrue(saved)
        XCTAssertEqual(controller.state, .needsValidation)
        XCTAssertNil(controller.lastErrorCode)
        XCTAssertEqual(controller.draftURL, "https://draft.example.test/v1")
        XCTAssertEqual(receipts.snapshot()?.configDigest, inspection.configDigest)
        let testedURLs = await client.testedURLs
        XCTAssertEqual(testedURLs, [inspection.upstreamBaseURL])
    }

    func testCredentialReplacementRequiresStableKeychainWitness() async {
        let inspection = decodeGatewayInspection()
        let credentials = CredentialStore(configured: Set(GatewayCredentialKind.allCases))
        let client = GatewayClient(
            inspection: inspection,
            onTest: {
                credentials.setModifiedAt(
                    .cloudflareClientSecret,
                    timeIntervalSince1970: 30
                )
            }
        )
        let receipts = ReceiptStore()
        let controller = GatewaySettingsController(
            client: client,
            credentialStore: credentials,
            receiptStore: receipts,
            activityLog: RelayActivityLogStore(),
            onRoutingRefreshRequested: {}
        )
        await controller.refresh()

        let saved = await controller.replaceCredential(
            .gatewayAPIKey,
            value: "new-private-value"
        )

        XCTAssertTrue(saved)
        XCTAssertEqual(controller.state, .needsValidation)
        XCTAssertEqual(
            controller.lastErrorCode,
            RelayctlReportedErrorCode.credentialUnavailable.rawValue
        )
        XCTAssertNil(receipts.snapshot())
    }

    func testConnectionTestRequiresStableKeychainWitness() async {
        let inspection = decodeGatewayInspection()
        let credentials = CredentialStore(configured: Set(GatewayCredentialKind.allCases))
        let client = GatewayClient(
            inspection: inspection,
            onTest: {
                credentials.setModifiedAt(
                    .gatewayAPIKey,
                    timeIntervalSince1970: 30
                )
            }
        )
        let receipts = ReceiptStore()
        let controller = GatewaySettingsController(
            client: client,
            credentialStore: credentials,
            receiptStore: receipts,
            activityLog: RelayActivityLogStore(),
            onRoutingRefreshRequested: {}
        )
        await controller.refresh()

        controller.test()
        await waitUntil { !controller.isBusy }

        XCTAssertEqual(controller.state, .needsValidation)
        XCTAssertEqual(
            controller.lastErrorCode,
            RelayctlReportedErrorCode.credentialUnavailable.rawValue
        )
        XCTAssertNil(receipts.snapshot())
    }

    func testSwitchRequiresTestedCandidateToBeAppliedConfiguration() async {
        let inspection = decodeGatewayInspection()
        let controller = GatewaySettingsController(
            client: GatewayClient(inspection: inspection),
            credentialStore: CredentialStore(configured: Set(GatewayCredentialKind.allCases)),
            receiptStore: ReceiptStore(),
            activityLog: RelayActivityLogStore(),
            onRoutingRefreshRequested: {}
        )
        await controller.refresh()

        controller.test()
        await waitUntil { !controller.isBusy }
        XCTAssertTrue(controller.canSwitchCodexToVerifiedConfiguration)

        controller.draftURL = "https://candidate.example.test/v1"
        controller.draftDidChange()
        controller.test()
        await waitUntil { !controller.isBusy }

        XCTAssertEqual(controller.state, .connected)
        XCTAssertFalse(controller.canSwitchCodexToVerifiedConfiguration)
        XCTAssertTrue(controller.canApply)
    }

    func testApplyUsesInspectionWitnessAndStoresBoundedSuccessReceipt() async {
        let before = decodeGatewayInspection()
        let after = decodeGatewayInspection(
            url: "https://candidate.example.test/v1",
            digest: String(repeating: "b", count: 64),
            generation: 9
        )
        let client = GatewayClient(
            inspection: before,
            applyReceipt: decodeGatewayApplyReceipt(
                digest: after.configDigest,
                generation: after.routingGeneration,
                runtimeReloaded: true
            ),
            postApplyInspection: after
        )
        let receipts = ReceiptStore()
        var refreshes = 0
        let controller = GatewaySettingsController(
            client: client,
            credentialStore: CredentialStore(configured: Set(GatewayCredentialKind.allCases)),
            receiptStore: receipts,
            activityLog: RelayActivityLogStore()
        ) {
            refreshes += 1
        }
        await controller.refresh()
        controller.draftURL = after.upstreamBaseURL
        controller.draftDidChange()

        controller.apply()
        await waitUntil { !controller.isBusy }

        XCTAssertEqual(controller.state, .connected)
        XCTAssertEqual(controller.inspection, after)
        XCTAssertEqual(receipts.snapshot()?.configDigest, after.configDigest)
        XCTAssertEqual(receipts.snapshot()?.resultCode, "connected")
        XCTAssertEqual(refreshes, 1)
        let applied = await client.appliedValues
        XCTAssertEqual(applied.count, 1)
        XCTAssertEqual(applied.first?.0, after.upstreamBaseURL)
        XCTAssertEqual(applied.first?.1, before.configDigest)
        XCTAssertEqual(applied.first?.2, before.routingGeneration)
    }

    func testApplyRequiresPostInspectionToMatchReceipt() async {
        let before = decodeGatewayInspection()
        let candidateURL = "https://candidate.example.test/v1"
        let receiptDigest = String(repeating: "b", count: 64)
        let receiptGeneration: UInt64 = 9
        let cases: [(GatewayInspection, RelayctlReportedErrorCode)] = [
            (
                decodeGatewayInspection(
                    url: "https://competitor.example.test/v1",
                    digest: receiptDigest,
                    generation: receiptGeneration
                ),
                .configChanged
            ),
            (
                decodeGatewayInspection(
                    url: candidateURL,
                    digest: String(repeating: "c", count: 64),
                    generation: receiptGeneration
                ),
                .configChanged
            ),
            (
                decodeGatewayInspection(
                    url: candidateURL,
                    digest: receiptDigest,
                    generation: receiptGeneration + 1
                ),
                .routingChanged
            ),
        ]

        for (refreshed, expectedCode) in cases {
            let client = GatewayClient(
                inspection: before,
                applyReceipt: decodeGatewayApplyReceipt(
                    digest: receiptDigest,
                    generation: receiptGeneration,
                    runtimeReloaded: true
                ),
                postApplyInspection: refreshed
            )
            let receipts = ReceiptStore()
            var refreshes = 0
            let controller = GatewaySettingsController(
                client: client,
                credentialStore: CredentialStore(configured: Set(GatewayCredentialKind.allCases)),
                receiptStore: receipts,
                activityLog: RelayActivityLogStore()
            ) {
                refreshes += 1
            }
            await controller.refresh()
            controller.draftURL = candidateURL
            controller.draftDidChange()

            controller.apply()
            await waitUntil { !controller.isBusy }

            XCTAssertEqual(controller.state, .needsValidation)
            XCTAssertEqual(controller.lastErrorCode, expectedCode.rawValue)
            XCTAssertEqual(controller.inspection, refreshed)
            XCTAssertEqual(controller.draftURL, refreshed.upstreamBaseURL)
            XCTAssertNil(receipts.snapshot())
            XCTAssertGreaterThan(receipts.clearCount, 0)
            XCTAssertEqual(refreshes, 1)
        }
    }

    func testApplyRequiresStableKeychainWitness() async {
        let before = decodeGatewayInspection()
        let after = decodeGatewayInspection(
            url: "https://candidate.example.test/v1",
            digest: String(repeating: "b", count: 64),
            generation: 9
        )
        let credentials = CredentialStore(configured: Set(GatewayCredentialKind.allCases))
        let client = GatewayClient(
            inspection: before,
            applyReceipt: decodeGatewayApplyReceipt(
                digest: after.configDigest,
                generation: after.routingGeneration,
                runtimeReloaded: true
            ),
            postApplyInspection: after,
            onApply: {
                credentials.setModifiedAt(
                    .cloudflareClientID,
                    timeIntervalSince1970: 30
                )
            }
        )
        let receipts = ReceiptStore()
        var refreshes = 0
        let controller = GatewaySettingsController(
            client: client,
            credentialStore: credentials,
            receiptStore: receipts,
            activityLog: RelayActivityLogStore()
        ) {
            refreshes += 1
        }
        await controller.refresh()
        controller.draftURL = after.upstreamBaseURL
        controller.draftDidChange()

        controller.apply()
        await waitUntil { !controller.isBusy }

        XCTAssertEqual(controller.state, .needsValidation)
        XCTAssertEqual(
            controller.lastErrorCode,
            RelayctlReportedErrorCode.credentialUnavailable.rawValue
        )
        XCTAssertNil(receipts.snapshot())
        XCTAssertEqual(refreshes, 1)
    }

    func testSystemKeychainAdapterCreatesAndReplacesTemporaryACLItems() throws {
        guard ProcessInfo.processInfo.environment["OPENCODEX_RUN_KEYCHAIN_INTEGRATION"] == "1" else {
            throw XCTSkip("Set OPENCODEX_RUN_KEYCHAIN_INTEGRATION=1 for temporary login-Keychain ACL verification")
        }
        let nonce = UUID().uuidString.lowercased()
        let account = "opencodex-relay-test-\(nonce)"
        let services = Dictionary(uniqueKeysWithValues: GatewayCredentialKind.allCases.map {
            ($0, "opencodex-relay-test-\($0.rawValue)-\(nonce)")
        })
        defer {
            for service in services.values {
                SecItemDelete([
                    kSecClass as String: kSecClassGenericPassword,
                    kSecAttrService as String: service,
                    kSecAttrAccount as String: account,
                ] as CFDictionary)
            }
        }
        let store = SystemGatewayCredentialStore(
            serviceNames: services,
            trustedApplicationPath: ProcessInfo.processInfo.arguments[0]
        )

        let first = try store.replace(
            .gatewayAPIKey,
            account: account,
            value: "temporary-first-value"
        )
        XCTAssertTrue(first.configured)
        XCTAssertNotNil(first.modifiedAt)
        let second = try store.replace(
            .gatewayAPIKey,
            account: account,
            value: "temporary-second-value"
        )
        XCTAssertTrue(second.configured)
        XCTAssertEqual(try store.inspect(account: account)[.gatewayAPIKey]?.configured, true)
    }

    func testUnavailableGatewayMapsIntegrationFailuresWithoutUnsupportedFallback() async {
        let cases: [(GatewaySettingsUnavailability, GatewaySettingsState)] = [
            (.previewMode, .integrationRequired),
            (.bindingMissing, .integrationRequired),
            (.bindingUnsafe, .bindingUnsafe),
            (.bindingInvalid, .bindingInvalid),
            (.helperUnavailable, .helperUnavailable),
        ]

        for (unavailability, expectedState) in cases {
            let controller = GatewaySettingsController(
                client: nil,
                unavailability: unavailability,
                credentialStore: CredentialStore(),
                receiptStore: ReceiptStore(),
                activityLog: RelayActivityLogStore(
                    subsystem: "test.gateway.unavailable.\(unavailability.rawValue)"
                ),
                onRoutingRefreshRequested: {}
            )

            await controller.refresh()

            XCTAssertEqual(controller.state, expectedState)
            XCTAssertEqual(controller.lastErrorCode, unavailability.rawValue)
            XCTAssertNotEqual(controller.state, .unsupported)
            XCTAssertFalse(controller.canTest)
            XCTAssertFalse(controller.canApply)
        }
    }

    func testChangingPrivateHTTPDestinationRequiresFreshAcknowledgement() async {
        let inspection = try! JSONDecoder().decode(
            GatewayInspection.self,
            from: Data("""
            {"schema_version":2,"upstream_base_url":"http://10.0.0.8/v1","config_digest":"\(String(repeating: "a", count: 64))","routing_generation":7,"credential_source":"keychain","credential_account":"test-account","credentials_editable":true,"authentication_profile":"gateway_api_key","required_credentials":["gateway_api_key"],"allow_insecure_private_ip":true}
            """.utf8)
        ).validated()
        let controller = GatewaySettingsController(
            client: GatewayClient(inspection: inspection),
            credentialStore: CredentialStore(configured: [.gatewayAPIKey]),
            receiptStore: ReceiptStore(),
            activityLog: RelayActivityLogStore(),
            onRoutingRefreshRequested: {}
        )

        await controller.refresh()
        XCTAssertTrue(controller.allowInsecurePrivateIP)
        controller.draftURL = "http://10.0.0.9/v1"
        controller.addressDidChange()

        XCTAssertFalse(controller.allowInsecurePrivateIP)
        XCTAssertTrue(controller.requiresInsecureTransportConfirmation)
        XCTAssertFalse(controller.canApply)
    }

    func testPrivateHTTPNoneRequiresAcknowledgementAndCloudflareRequiresHTTPS() async {
        let controller = GatewaySettingsController(
            client: GatewayClient(inspection: decodeGatewayInspection()),
            credentialStore: CredentialStore(configured: Set(GatewayCredentialKind.allCases)),
            receiptStore: ReceiptStore(),
            activityLog: RelayActivityLogStore(),
            onRoutingRefreshRequested: {}
        )
        await controller.refresh()
        controller.draftURL = "http://192.168.1.40/v1"
        controller.authenticationProfile = .none
        controller.authenticationProfileDidChange()
        controller.addressDidChange()

        XCTAssertEqual(controller.credentialMetadataState, .ready)
        XCTAssertTrue(controller.requiresInsecureTransportConfirmation)
        XCTAssertFalse(controller.canTest)
        XCTAssertFalse(controller.canApply)

        controller.allowInsecurePrivateIP = true
        controller.draftDidChange()
        XCTAssertTrue(controller.canTest)
        XCTAssertTrue(controller.canApply)

        controller.authenticationProfile = .cloudflareAccessAndGatewayAPIKey
        controller.authenticationProfileDidChange()
        XCTAssertTrue(controller.hasTransportProfileConflict)
        XCTAssertFalse(controller.requiresInsecureTransportConfirmation)
        XCTAssertFalse(controller.allowInsecurePrivateIP)
        XCTAssertFalse(controller.canTest)
        XCTAssertFalse(controller.canApply)
    }

    func testProfileChangeReloadsOnlyRequiredCredentialsAndPreservesDraft() async {
        let credentials = CredentialStore(configured: Set(GatewayCredentialKind.allCases))
        let activityLog = RelayActivityLogStore(subsystem: "test.gateway.profile-change")
        let controller = GatewaySettingsController(
            client: GatewayClient(inspection: decodeGatewayInspection()),
            credentialStore: credentials,
            receiptStore: ReceiptStore(),
            activityLog: activityLog,
            onRoutingRefreshRequested: {}
        )
        await controller.refresh()
        let draft = "https://private-profile-change.example.test/v1"
        controller.draftURL = draft
        controller.draftDidChange()

        controller.authenticationProfile = .gatewayAPIKey
        XCTAssertFalse(controller.canTest, "stale metadata must not enable an action")
        controller.authenticationProfileDidChange()
        await waitUntil { controller.credentialMetadataState == .ready }

        XCTAssertEqual(controller.draftURL, draft)
        XCTAssertEqual(Set(controller.credentialMetadata.keys), [.gatewayAPIKey])
        XCTAssertEqual(credentials.inspectedKindSets().last, [.gatewayAPIKey])
        XCTAssertTrue(controller.canTest)
        XCTAssertFalse(activityLog.jsonLines().contains(draft))
        XCTAssertFalse(activityLog.jsonLines().contains("test-account"))
    }

    func testLateCredentialInspectionIsDiscardedAfterRapidProfileChange() async {
        let credentials = ControlledCredentialStore()
        let controller = GatewaySettingsController(
            client: GatewayClient(inspection: decodeGatewayInspection()),
            credentialStore: credentials,
            receiptStore: ReceiptStore(),
            activityLog: RelayActivityLogStore(),
            onRoutingRefreshRequested: {}
        )
        await controller.refresh()
        controller.authenticationProfile = .gatewayAPIKey
        controller.authenticationProfileDidChange()
        await waitUntil { controller.credentialMetadataState == .ready }

        credentials.blockNextCloudflareInspection()
        controller.authenticationProfile = .cloudflareAccessAndGatewayAPIKey
        controller.authenticationProfileDidChange()
        let blocked = await Task.detached {
            credentials.waitForBlockedInspection()
        }.value
        XCTAssertTrue(blocked)

        controller.authenticationProfile = .gatewayAPIKey
        controller.authenticationProfileDidChange()
        await waitUntil {
            controller.credentialMetadataState == .ready &&
                Set(controller.credentialMetadata.keys) == [.gatewayAPIKey]
        }
        credentials.releaseBlockedInspection()
        try? await Task.sleep(for: .milliseconds(50))

        XCTAssertEqual(controller.authenticationProfile, .gatewayAPIKey)
        XCTAssertEqual(Set(controller.credentialMetadata.keys), [.gatewayAPIKey])
        XCTAssertTrue(controller.canTest)
    }

    func testCredentialInspectionFailureIsNotReportedAsMissing() async {
        let credentials = ControlledCredentialStore()
        let controller = GatewaySettingsController(
            client: GatewayClient(inspection: decodeGatewayInspection()),
            credentialStore: credentials,
            receiptStore: ReceiptStore(),
            activityLog: RelayActivityLogStore(),
            onRoutingRefreshRequested: {}
        )
        await controller.refresh()
        credentials.failInspections(containing: [.gatewayAPIKey])
        controller.authenticationProfile = .gatewayAPIKey
        controller.authenticationProfileDidChange()
        await waitUntil { controller.credentialMetadataState == .failed }

        XCTAssertTrue(controller.credentialMetadata.isEmpty)
        XCTAssertEqual(controller.lastErrorCode, "keychain_read_failed")
        XCTAssertEqual(controller.state, .failed)
        XCTAssertFalse(controller.canTest)
        XCTAssertFalse(controller.canApply)
    }

    func testNoneProfileDoesNotOpenKeychainDuringRefreshOrIntegration() async {
        let inspection = try! JSONDecoder().decode(
            GatewayInspection.self,
            from: Data("""
            {"schema_version":2,"upstream_base_url":"https://none.example.test/v1","config_digest":"\(String(repeating: "a", count: 64))","routing_generation":7,"credential_source":"keychain","credential_account":"test-account","credentials_editable":true,"authentication_profile":"none","required_credentials":[],"allow_insecure_private_ip":false}
            """.utf8)
        ).validated()
        let credentials = ControlledCredentialStore()
        credentials.failInspections(containing: Set(GatewayCredentialKind.allCases))
        let controller = GatewaySettingsController(
            client: GatewayClient(inspection: inspection),
            credentialStore: credentials,
            receiptStore: ReceiptStore(),
            activityLog: RelayActivityLogStore(),
            onRoutingRefreshRequested: {}
        )

        await controller.refresh()

        XCTAssertEqual(controller.credentialMetadataState, .ready)
        XCTAssertTrue(controller.credentialMetadata.isEmpty)
        XCTAssertEqual(controller.authenticationProfile, .none)
        XCTAssertNil(controller.lastErrorCode)

        let integrationController = GatewaySettingsController(
            client: nil,
            unavailability: .bindingMissing,
            integrationClient: IntegrationClient(),
            credentialStore: credentials,
            receiptStore: ReceiptStore(),
            activityLog: RelayActivityLogStore(),
            onRoutingRefreshRequested: {}
        )
        integrationController.authenticationProfile = .none

        await integrationController.refresh()

        XCTAssertEqual(integrationController.credentialMetadataState, .ready)
        XCTAssertTrue(integrationController.credentialMetadata.isEmpty)
        XCTAssertEqual(integrationController.state, .integrationRequired)
        XCTAssertNil(integrationController.lastErrorCode)
    }

    func testOlderRefreshCannotOverwriteNewerInspectionOrBusyState() async {
        let first = decodeGatewayInspection(
            url: "https://stale.example.test/v1",
            digest: String(repeating: "a", count: 64),
            generation: 7
        )
        let second = decodeGatewayInspection(
            url: "https://current.example.test/v1",
            digest: String(repeating: "b", count: 64),
            generation: 8
        )
        let client = SequencedGatewayClient(first: first, second: second)
        let controller = GatewaySettingsController(
            client: client,
            credentialStore: CredentialStore(configured: Set(GatewayCredentialKind.allCases)),
            receiptStore: ReceiptStore(),
            activityLog: RelayActivityLogStore(),
            onRoutingRefreshRequested: {}
        )

        let staleRefresh = Task { await controller.refresh() }
        await client.waitForFirstInspection()
        let currentRefresh = Task { await controller.refresh() }
        await currentRefresh.value
        XCTAssertEqual(controller.inspection, second)
        XCTAssertFalse(controller.isBusy)

        await client.releaseFirstInspection()
        await staleRefresh.value

        XCTAssertEqual(controller.inspection, second)
        XCTAssertEqual(controller.draftURL, second.upstreamBaseURL)
        XCTAssertFalse(controller.isBusy)
    }

    func testNewerUnavailableRefreshClearsBusyAndRejectsOlderCompletion() async {
        let stale = decodeGatewayInspection(
            url: "https://stale.example.test/v1",
            digest: String(repeating: "a", count: 64),
            generation: 7
        )
        let client = SequencedGatewayClient(first: stale, second: stale)
        let resolution = ResolutionBox(GatewaySettingsResolution(client: client))
        let controller = GatewaySettingsController(
            resolver: { resolution.value },
            credentialStore: CredentialStore(configured: Set(GatewayCredentialKind.allCases)),
            receiptStore: ReceiptStore(),
            activityLog: RelayActivityLogStore(),
            onRoutingRefreshRequested: {}
        )

        let staleRefresh = Task { await controller.refresh() }
        await client.waitForFirstInspection()
        XCTAssertTrue(controller.isBusy)

        resolution.value = GatewaySettingsResolution(
            client: nil,
            unavailability: .helperUnavailable
        )
        await controller.refresh()

        XCTAssertEqual(controller.state, .helperUnavailable)
        XCTAssertNil(controller.inspection)
        XCTAssertFalse(controller.isBusy)

        await client.releaseFirstInspection()
        await staleRefresh.value

        XCTAssertEqual(controller.state, .helperUnavailable)
        XCTAssertNil(controller.inspection)
        XCTAssertFalse(controller.isBusy)
    }

    private func waitUntil(
        timeout: TimeInterval = 2,
        condition: @escaping @MainActor () -> Bool
    ) async {
        let deadline = Date().addingTimeInterval(timeout)
        while !condition(), Date() < deadline {
            try? await Task.sleep(for: .milliseconds(10))
        }
        XCTAssertTrue(condition())
    }
}

private func decodeGatewayInspection(
    url: String = "https://origin.example.test/v1",
    digest: String = String(repeating: "a", count: 64),
    generation: UInt64 = 7
) -> GatewayInspection {
    try! JSONDecoder().decode(GatewayInspection.self, from: Data("""
    {"schema_version":1,"upstream_base_url":"\(url)","config_digest":"\(digest)","routing_generation":\(generation),"credential_source":"keychain","credential_account":"test-account","credentials_editable":true}
    """.utf8)).validated()
}

private func decodeGatewayValidation(digest: String, generation: UInt64) -> GatewayValidation {
    try! JSONDecoder().decode(GatewayValidation.self, from: Data("""
    {"schema_version":1,"ok":true,"config_digest":"\(digest)","routing_generation":\(generation),"model_count":2}
    """.utf8)).validated()
}

private func decodeGatewayApplyReceipt(
    digest: String,
    generation: UInt64,
    runtimeReloaded: Bool
) -> GatewayApplyReceipt {
    try! JSONDecoder().decode(GatewayApplyReceipt.self, from: Data("""
    {"schema_version":1,"ok":true,"config_digest":"\(digest)","routing_generation":\(generation),"runtime_reloaded":\(runtimeReloaded)}
    """.utf8)).validated()
}
