import Darwin
import Foundation
import XCTest
@testable import OpenCodexRelay
import OpenCodexRelayCore

@MainActor
final class GatewaySettingsTests: XCTestCase {
    private actor IntegrationClient: SelfHostedIntegrationManaging {
        private var state: SelfHostedIntegrationState
        private let applyError: RelayctlError?
        private(set) var applications: [(GatewayCandidate, String)] = []
        private(set) var recoveries = 0

        init(
            state: SelfHostedIntegrationState = .integrationRequired,
            applyError: RelayctlError? = nil
        ) {
            self.state = state
            self.applyError = applyError
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
            if let applyError {
                if applyError == .reported(.integrationRecoveryRequired) {
                    state = .recoveryRequired
                }
                throw applyError
            }
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
            recoveries += 1
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

    func testActionModeKeepsSetupAndRecoveryActionsExclusive() {
        XCTAssertEqual(
            GatewaySettingsActionMode.resolve(
                state: .integrationRequired,
                integrationState: .integrationRequired
            ),
            .prepare
        )
        XCTAssertEqual(
            GatewaySettingsActionMode.resolve(
                state: .recoveryRequired,
                integrationState: .integrationRequired
            ),
            .recover
        )
        XCTAssertEqual(
            GatewaySettingsActionMode.resolve(
                state: .bindingUnsafe,
                integrationState: .integrationRequired
            ),
            .testAndApply
        )
        XCTAssertEqual(
            GatewaySettingsActionMode.resolve(
                state: .bindingUnsafe,
                integrationState: .recoveryRequired
            ),
            .testAndApply
        )
        XCTAssertEqual(
            GatewaySettingsActionMode.resolve(
                state: .applying,
                integrationState: .recoveryRequired
            ),
            .recover
        )
        XCTAssertEqual(
            GatewaySettingsActionMode.resolve(
                state: .applying,
                integrationState: nil
            ),
            .testAndApply
        )

        let standardStates: [GatewaySettingsState] = [
            .loading,
            .needsValidation,
            .testing,
            .applying,
            .connected,
            .authenticationMismatch,
            .unreachable,
            .catalogInvalid,
            .appLocationInvalid,
            .integrationArtifactInvalid,
            .bindingUnsafe,
            .bindingInvalid,
            .helperUnavailable,
            .unsupported,
            .failed,
        ]
        for state in standardStates {
            XCTAssertEqual(
                GatewaySettingsActionMode.resolve(
                    state: state,
                    integrationState: nil
                ),
                .testAndApply,
                "unexpected action mode for \(state)"
            )
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

    private final class LegacyACLInspector: LegacyFileKeychainACLInspecting, @unchecked Sendable {
        enum FailurePoint: Equatable {
            case path
            case metadata
            case access
        }

        private let lock = NSLock()
        private let keychainPath: String
        private let itemMetadata: GatewayCredentialMetadata
        private let accessMatches: Bool
        private let failurePoint: FailurePoint?
        private(set) var pathRequests = 0
        private(set) var metadataRequests: [(
            service: String,
            account: String,
            keychainPathWitness: String?
        )] = []
        private(set) var accessRequests: [(
            service: String,
            account: String,
            applicationPath: String
        )] = []

        init(
            keychainPath: String = "/private/tmp/test-default.keychain-db",
            itemMetadata: GatewayCredentialMetadata = GatewayCredentialMetadata(
                configured: true,
                modifiedAt: Date(timeIntervalSince1970: 20)
            ),
            accessMatches: Bool = true,
            failurePoint: FailurePoint? = nil
        ) {
            self.keychainPath = keychainPath
            self.itemMetadata = itemMetadata
            self.accessMatches = accessMatches
            self.failurePoint = failurePoint
        }

        func metadata(
            service: String,
            account: String,
            keychainPathWitness: String?
        ) throws -> GatewayCredentialMetadata {
            lock.lock()
            metadataRequests.append((service, account, keychainPathWitness))
            let shouldFail = failurePoint == .metadata
            lock.unlock()
            if shouldFail {
                throw GatewayCredentialStoreError.keychainFailure
            }
            return itemMetadata
        }

        func inspectDecryptACL(
            service: String,
            account: String,
            applicationPath: String
        ) throws -> LegacyFileKeychainACLInspection {
            lock.lock()
            pathRequests += 1
            accessRequests.append((service, account, applicationPath))
            let shouldFail = failurePoint == .path || failurePoint == .access
            lock.unlock()
            if shouldFail {
                throw GatewayCredentialStoreError.keychainFailure
            }
            return LegacyFileKeychainACLInspection(
                keychainPath: keychainPath,
                matches: accessMatches
            )
        }

        func requestCounts() -> (path: Int, metadata: Int, access: Int) {
            lock.lock()
            defer { lock.unlock() }
            return (pathRequests, metadataRequests.count, accessRequests.count)
        }

        func recordedMetadataRequests() -> [(
            service: String,
            account: String,
            keychainPathWitness: String?
        )] {
            lock.lock()
            defer { lock.unlock() }
            return metadataRequests
        }

        func recordedAccessRequests() -> [(
            service: String,
            account: String,
            applicationPath: String
        )] {
            lock.lock()
            defer { lock.unlock() }
            return accessRequests
        }
    }

    private final class SecurityCommandRunner: GatewaySecurityCommandRunning, @unchecked Sendable {
        struct Invocation {
            let arguments: [String]
            let standardInput: Data?
        }

        private let lock = NSLock()
        private let verificationValue: String
        private let storeStatus: Int32
        private(set) var invocationStorage: [Invocation] = []

        init(
            verificationValue: String,
            storeStatus: Int32 = 0
        ) {
            self.verificationValue = verificationValue
            self.storeStatus = storeStatus
        }

        func run(
            arguments: [String],
            standardInput: Data?
        ) -> GatewaySecurityCommandResult {
            lock.lock()
            invocationStorage.append(Invocation(
                arguments: arguments,
                standardInput: standardInput
            ))
            lock.unlock()
            if arguments == ["-i"] {
                return GatewaySecurityCommandResult(
                    status: storeStatus,
                    output: Data()
                )
            }
            return GatewaySecurityCommandResult(
                status: 0,
                output: Data("\(verificationValue)\n".utf8)
            )
        }

        func invocations() -> [Invocation] {
            lock.lock()
            defer { lock.unlock() }
            return invocationStorage
        }
    }

    private final class FailingReplaceCredentialStore: GatewayCredentialStoring, @unchecked Sendable {
        private let replacementError: GatewayCredentialStoreError
        private let metadata = Dictionary(uniqueKeysWithValues: GatewayCredentialKind.allCases.map {
            ($0, GatewayCredentialMetadata(
                configured: true,
                modifiedAt: Date(timeIntervalSince1970: 10)
            ))
        })

        init(replacementError: GatewayCredentialStoreError) {
            self.replacementError = replacementError
        }

        func inspect(account: String) throws -> [GatewayCredentialKind: GatewayCredentialMetadata] {
            metadata
        }

        func replace(
            _ kind: GatewayCredentialKind,
            account: String,
            value: String
        ) throws -> GatewayCredentialMetadata {
            throw replacementError
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

    func testPrepareRecoveryRequiredSynchronizesInspectionAndExposesOnlyRecovery() async {
        let integration = IntegrationClient(
            applyError: .reported(.integrationRecoveryRequired)
        )
        let controller = GatewaySettingsController(
            client: nil,
            unavailability: .bindingMissing,
            integrationClient: integration,
            credentialStore: CredentialStore(),
            receiptStore: ReceiptStore(),
            activityLog: RelayActivityLogStore(),
            onRoutingRefreshRequested: {}
        )

        await controller.refresh()
        controller.draftURL = "https://recovery.example.test/v1"
        controller.authenticationProfile = .gatewayAPIKey
        controller.authenticationProfileDidChange()
        await waitUntil { controller.credentialMetadataState == .ready }
        XCTAssertTrue(controller.canPrepareIntegration)

        controller.prepareIntegration()
        await waitUntil { !controller.isBusy }

        XCTAssertEqual(controller.state, .recoveryRequired)
        XCTAssertEqual(controller.integrationInspection?.state, .recoveryRequired)
        XCTAssertFalse(controller.canPrepareIntegration)
        XCTAssertTrue(controller.canRecoverIntegration)
        XCTAssertEqual(
            GatewaySettingsActionMode.resolve(
                state: controller.state,
                integrationState: controller.integrationInspection?.state
            ),
            .recover
        )

        controller.prepareIntegration()
        let applicationCount = await integration.applications.count
        XCTAssertEqual(applicationCount, 1)

        controller.recoverIntegration()
        await waitUntil { !controller.isBusy }
        let recoveryCount = await integration.recoveries
        XCTAssertEqual(recoveryCount, 1)
        XCTAssertEqual(controller.state, .integrationRequired)
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

    func testCredentialReplacementMapsLifecycleAdmissionFailuresWithoutLoggingSecret() async {
        let cases: [(GatewayCredentialStoreError, GatewaySettingsState, RelayctlReportedErrorCode)] = [
            (.lifecycleConflict, .recoveryRequired, .integrationRecoveryRequired),
            (.lifecycleUnsafe, .bindingUnsafe, .integrationStateUnsafe),
        ]

        for (error, expectedState, expectedCode) in cases {
            let activityLog = RelayActivityLogStore(
                subsystem: "test.gateway.lifecycle.\(expectedCode.rawValue)"
            )
            let controller = GatewaySettingsController(
                client: nil,
                unavailability: .bindingMissing,
                integrationClient: IntegrationClient(),
                credentialStore: FailingReplaceCredentialStore(replacementError: error),
                receiptStore: ReceiptStore(),
                activityLog: activityLog,
                onRoutingRefreshRequested: {}
            )
            await controller.refresh()

            let secret = "must-never-appear-\(expectedCode.rawValue)"
            let saved = await controller.replaceCredential(.gatewayAPIKey, value: secret)

            XCTAssertFalse(saved)
            XCTAssertEqual(controller.state, expectedState)
            XCTAssertEqual(controller.lastErrorCode, expectedCode.rawValue)
            XCTAssertEqual(controller.credentialMetadataState, .failed)
            let logs = activityLog.jsonLines()
            XCTAssertFalse(logs.contains(secret))
            XCTAssertTrue(logs.contains(expectedCode.rawValue))
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

    func testCredentialLifecycleGateCreatesExactTemporaryHomeBoundary() throws {
        let home = try makeTemporaryHome()
        defer { try? FileManager.default.removeItem(at: home) }
        let gate = GatewayCredentialLifecycleGate(homeDirectory: home.path)
        var entered = false

        try gate.withWriteAdmission {
            entered = true
        }

        XCTAssertTrue(entered)
        let lifecycleDirectory = home
            .appendingPathComponent("Library", isDirectory: true)
            .appendingPathComponent("Application Support", isDirectory: true)
            .appendingPathComponent("OpenCodexRelayLifecycle", isDirectory: true)
        let lifecycleInfo = try lstatInfo(at: lifecycleDirectory)
        XCTAssertEqual(lifecycleInfo.st_mode & S_IFMT, S_IFDIR)
        XCTAssertEqual(lifecycleInfo.st_uid, geteuid())
        XCTAssertEqual(lifecycleInfo.st_mode & mode_t(0o777), mode_t(0o700))

        let lock = lifecycleDirectory.appendingPathComponent("lifecycle.lock", isDirectory: false)
        let lockInfo = try lstatInfo(at: lock)
        XCTAssertEqual(lockInfo.st_mode & S_IFMT, S_IFREG)
        XCTAssertEqual(lockInfo.st_uid, geteuid())
        XCTAssertEqual(lockInfo.st_mode & mode_t(0o777), mode_t(0o600))
    }

    func testCredentialLifecycleGateRejectsStandaloneAndSourceInstallReservations() throws {
        let conflictPaths = [
            "Library/Application Support/OpenCodexRelayLifecycle/standalone-native",
            "Library/Application Support/OpenCodexRelayLifecycle/standalone-native.open-codex-removal.json",
            ".local/lib/opencodex-relay/relay/.source-install-reservation.json",
            ".local/lib/opencodex-relay/relay-dev/.source-install-reservation.json",
        ]

        for relativePath in conflictPaths {
            let home = try makeTemporaryHome()
            defer { try? FileManager.default.removeItem(at: home) }
            let gate = GatewayCredentialLifecycleGate(homeDirectory: home.path)
            try gate.withWriteAdmission {}
            let conflict = URL(fileURLWithPath: home.path + "/" + relativePath)
            try FileManager.default.createDirectory(
                at: conflict.deletingLastPathComponent(),
                withIntermediateDirectories: true,
                attributes: [.posixPermissions: 0o700]
            )
            XCTAssertTrue(FileManager.default.createFile(
                atPath: conflict.path,
                contents: Data("untrusted".utf8),
                attributes: [.posixPermissions: 0o600]
            ))
            var entered = false

            XCTAssertThrowsError(try gate.withWriteAdmission { entered = true }) { error in
                XCTAssertEqual(error as? GatewayCredentialStoreError, .lifecycleConflict)
            }
            XCTAssertFalse(entered)
        }
    }

    func testCredentialLifecycleGateTreatsReservationInspectionErrorAsConflict() throws {
        let home = try makeTemporaryHome()
        defer { try? FileManager.default.removeItem(at: home) }
        let gate = GatewayCredentialLifecycleGate(homeDirectory: home.path)
        try gate.withWriteAdmission {}
        let reservationParent = home
            .appendingPathComponent(".local", isDirectory: true)
            .appendingPathComponent("lib", isDirectory: true)
            .appendingPathComponent("opencodex-relay", isDirectory: true)
            .appendingPathComponent("relay", isDirectory: true)
        try FileManager.default.createDirectory(
            at: reservationParent,
            withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700]
        )
        XCTAssertEqual(Darwin.chmod(reservationParent.path, mode_t(0o000)), 0)
        defer { _ = Darwin.chmod(reservationParent.path, mode_t(0o700)) }
        var entered = false

        XCTAssertThrowsError(try gate.withWriteAdmission { entered = true }) { error in
            XCTAssertEqual(error as? GatewayCredentialStoreError, .lifecycleConflict)
        }
        XCTAssertFalse(entered)
    }

    func testCredentialLifecycleGateRejectsLooseLockWithoutRepair() throws {
        let home = try makeTemporaryHome()
        defer { try? FileManager.default.removeItem(at: home) }
        let lifecycleDirectory = home
            .appendingPathComponent("Library", isDirectory: true)
            .appendingPathComponent("Application Support", isDirectory: true)
            .appendingPathComponent("OpenCodexRelayLifecycle", isDirectory: true)
        try FileManager.default.createDirectory(
            at: lifecycleDirectory,
            withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700]
        )
        let lock = lifecycleDirectory.appendingPathComponent("lifecycle.lock", isDirectory: false)
        XCTAssertTrue(FileManager.default.createFile(
            atPath: lock.path,
            contents: Data(),
            attributes: [.posixPermissions: 0o644]
        ))
        XCTAssertEqual(Darwin.chmod(lock.path, mode_t(0o644)), 0)
        var entered = false

        XCTAssertThrowsError(
            try GatewayCredentialLifecycleGate(homeDirectory: home.path)
                .withWriteAdmission { entered = true }
        ) { error in
            XCTAssertEqual(error as? GatewayCredentialStoreError, .lifecycleUnsafe)
        }
        XCTAssertFalse(entered)
        XCTAssertEqual(
            try lstatInfo(at: lock).st_mode & mode_t(0o777),
            mode_t(0o644)
        )
    }

    func testSystemCredentialStoreRejectsLifecycleConflictBeforeKeychainAccess() throws {
        let home = try makeTemporaryHome()
        defer { try? FileManager.default.removeItem(at: home) }
        let lifecycleDirectory = home
            .appendingPathComponent("Library", isDirectory: true)
            .appendingPathComponent("Application Support", isDirectory: true)
            .appendingPathComponent("OpenCodexRelayLifecycle", isDirectory: true)
        try FileManager.default.createDirectory(
            at: lifecycleDirectory,
            withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700]
        )
        let journal = lifecycleDirectory.appendingPathComponent(
            "standalone-native.open-codex-removal.json",
            isDirectory: false
        )
        XCTAssertTrue(FileManager.default.createFile(
            atPath: journal.path,
            contents: Data("untrusted".utf8),
            attributes: [.posixPermissions: 0o600]
        ))
        let inspector = LegacyACLInspector()
        let runner = SecurityCommandRunner(verificationValue: "temporary-secret")
        let store = SystemGatewayCredentialStore(
            trustedApplicationPath: "/path-that-must-not-be-inspected",
            lifecycleHomeDirectory: home.path,
            legacyACLInspector: inspector,
            securityCommandRunner: runner
        )

        XCTAssertThrowsError(
            try store.replace(
                .gatewayAPIKey,
                account: "temporary-account",
                value: "temporary-secret"
            )
        ) { error in
            XCTAssertEqual(error as? GatewayCredentialStoreError, .lifecycleConflict)
        }
        XCTAssertEqual(inspector.requestCounts().path, 0)
        XCTAssertEqual(inspector.requestCounts().metadata, 0)
        XCTAssertEqual(inspector.requestCounts().access, 0)
        XCTAssertTrue(runner.invocations().isEmpty)
    }

    func testSystemCredentialStoreScopesInspectionThroughLegacyAdapter() throws {
        let inspector = LegacyACLInspector(
            itemMetadata: GatewayCredentialMetadata(
                configured: false,
                modifiedAt: nil
            )
        )
        let store = SystemGatewayCredentialStore(
            serviceNames: [.gatewayAPIKey: "test-gateway-service"],
            trustedApplicationPath: ProcessInfo.processInfo.arguments[0],
            legacyACLInspector: inspector,
            securityCommandRunner: SecurityCommandRunner(
                verificationValue: "unused"
            )
        )

        let metadata = try store.inspect(
            account: "bounded-account",
            kinds: [.gatewayAPIKey]
        )

        XCTAssertEqual(
            metadata[.gatewayAPIKey],
            GatewayCredentialMetadata(configured: false, modifiedAt: nil)
        )
        let requests = inspector.recordedMetadataRequests()
        XCTAssertEqual(requests.count, 1)
        XCTAssertEqual(requests.first?.service, "test-gateway-service")
        XCTAssertEqual(requests.first?.account, "bounded-account")
        XCTAssertNil(requests.first?.keychainPathWitness)
        XCTAssertEqual(inspector.requestCounts().path, 0)
        XCTAssertEqual(inspector.requestCounts().access, 0)
    }

    func testSystemCredentialStoreRepairsOnlyMismatchedLegacyACL() throws {
        let executablePath = ProcessInfo.processInfo.arguments[0]
        let secret = "temporary-secret"

        for accessMatches in [true, false] {
            let home = try makeTemporaryHome()
            defer { try? FileManager.default.removeItem(at: home) }
            let inspector = LegacyACLInspector(accessMatches: accessMatches)
            let runner = SecurityCommandRunner(verificationValue: secret)
            let store = SystemGatewayCredentialStore(
                serviceNames: [.gatewayAPIKey: "test-gateway-service"],
                trustedApplicationPath: executablePath,
                lifecycleHomeDirectory: home.path,
                legacyACLInspector: inspector,
                securityCommandRunner: runner
            )

            let metadata = try store.replace(
                .gatewayAPIKey,
                account: "bounded-account",
                value: secret
            )

            XCTAssertTrue(metadata.configured)
            let invocations = runner.invocations()
            XCTAssertEqual(invocations.count, 2)
            XCTAssertEqual(invocations.first?.arguments, ["-i"])
            let command = try XCTUnwrap(
                invocations.first?.standardInput.flatMap {
                    String(data: $0, encoding: .utf8)
                }
            )
            if accessMatches {
                XCTAssertFalse(command.contains(" -T "))
            } else {
                XCTAssertTrue(command.contains("-T \"\(executablePath)\""))
                XCTAssertTrue(command.contains("-T \"/usr/bin/security\""))
            }
            XCTAssertEqual(
                invocations.last?.arguments,
                [
                    "find-generic-password",
                    "-a", "bounded-account",
                    "-s", "test-gateway-service",
                    "-w", "/private/tmp/test-default.keychain-db",
                ]
            )
            let accessRequests = inspector.recordedAccessRequests()
            XCTAssertEqual(accessRequests.count, 1)
            XCTAssertEqual(accessRequests.first?.service, "test-gateway-service")
            XCTAssertEqual(accessRequests.first?.account, "bounded-account")
            XCTAssertEqual(accessRequests.first?.applicationPath, executablePath)
            let metadataRequests = inspector.recordedMetadataRequests()
            XCTAssertEqual(metadataRequests.count, 1)
            XCTAssertEqual(
                metadataRequests.first?.keychainPathWitness,
                "/private/tmp/test-default.keychain-db"
            )
        }
    }

    func testSystemCredentialStoreFailsClosedOnLegacyAdapterErrors() throws {
        let executablePath = ProcessInfo.processInfo.arguments[0]
        for failurePoint in [
            LegacyACLInspector.FailurePoint.path,
            .access,
        ] {
            let home = try makeTemporaryHome()
            defer { try? FileManager.default.removeItem(at: home) }
            let inspector = LegacyACLInspector(failurePoint: failurePoint)
            let runner = SecurityCommandRunner(verificationValue: "temporary-secret")
            let store = SystemGatewayCredentialStore(
                trustedApplicationPath: executablePath,
                lifecycleHomeDirectory: home.path,
                legacyACLInspector: inspector,
                securityCommandRunner: runner
            )

            XCTAssertThrowsError(
                try store.replace(
                    .gatewayAPIKey,
                    account: "bounded-account",
                    value: "temporary-secret"
                )
            ) { error in
                XCTAssertEqual(error as? GatewayCredentialStoreError, .keychainFailure)
            }
            XCTAssertTrue(runner.invocations().isEmpty)
        }

        let inspector = LegacyACLInspector(failurePoint: .metadata)
        let store = SystemGatewayCredentialStore(
            trustedApplicationPath: executablePath,
            legacyACLInspector: inspector,
            securityCommandRunner: SecurityCommandRunner(
                verificationValue: "unused"
            )
        )
        XCTAssertThrowsError(
            try store.inspect(
                account: "bounded-account",
                kinds: [.gatewayAPIKey]
            )
        ) { error in
            XCTAssertEqual(error as? GatewayCredentialStoreError, .keychainFailure)
        }
    }

    func testSystemCredentialStoreRejectsMissingApplicationBeforeKeychainAccess() throws {
        let home = try makeTemporaryHome()
        defer { try? FileManager.default.removeItem(at: home) }
        let inspector = LegacyACLInspector()
        let runner = SecurityCommandRunner(verificationValue: "temporary-secret")
        let store = SystemGatewayCredentialStore(
            trustedApplicationPath: home
                .appendingPathComponent("missing-executable")
                .path,
            lifecycleHomeDirectory: home.path,
            legacyACLInspector: inspector,
            securityCommandRunner: runner
        )

        XCTAssertThrowsError(
            try store.replace(
                .gatewayAPIKey,
                account: "bounded-account",
                value: "temporary-secret"
            )
        ) { error in
            XCTAssertEqual(
                error as? GatewayCredentialStoreError,
                .accessControlUnavailable
            )
        }
        XCTAssertEqual(inspector.requestCounts().path, 0)
        XCTAssertEqual(inspector.requestCounts().access, 0)
        XCTAssertTrue(runner.invocations().isEmpty)
    }

    func testSystemKeychainAdapterCreatesAndReplacesTemporaryACLItems() throws {
        guard ProcessInfo.processInfo.environment["OPENCODEX_RUN_KEYCHAIN_INTEGRATION"] == "1" else {
            throw XCTSkip("Use the isolated temporary-Keychain integration runner")
        }
        let environment = ProcessInfo.processInfo.environment
        let secondaryKeychain = try XCTUnwrap(
            environment["OPENCODEX_KEYCHAIN_INTEGRATION_SECONDARY_PATH"]
        )
        let nonce = UUID().uuidString.lowercased()
        let account = "opencodex-relay-test-\(nonce)"
        let services = Dictionary(uniqueKeysWithValues: GatewayCredentialKind.allCases.map {
            ($0, "opencodex-relay-test-\($0.rawValue)-\(nonce)")
        })
        let foreignCommand = try makeTemporarySecurityStoreCommand(
            service: try XCTUnwrap(services[.gatewayAPIKey]),
            account: account,
            value: "foreign-search-list-value",
            keychainPath: secondaryKeychain
        )
        XCTAssertEqual(runTemporarySecurityCommand(foreignCommand), 0)
        let store = SystemGatewayCredentialStore(
            serviceNames: services,
            trustedApplicationPath: ProcessInfo.processInfo.arguments[0]
        )

        XCTAssertEqual(
            try store.inspect(
                account: account,
                kinds: [.gatewayAPIKey]
            )[.gatewayAPIKey]?.configured,
            false
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

    private func makeTemporarySecurityStoreCommand(
        service: String,
        account: String,
        value: String,
        keychainPath: String
    ) throws -> Data {
        let values = [account, service, keychainPath]
        let encoded = try values.map { value -> String in
            guard value.unicodeScalars.allSatisfy({
                $0.value != 0 && $0.value != 10 && $0.value != 13
            }) else {
                throw GatewayCredentialStoreError.invalidValue
            }
            let escaped = value
                .replacingOccurrences(of: "\\", with: "\\\\")
                .replacingOccurrences(of: "\"", with: "\\\"")
            return "\"\(escaped)\""
        }
        let secretHex = Data(value.utf8)
            .map { String(format: "%02x", $0) }
            .joined()
        return Data(([
            "add-generic-password -U",
            "-a \(encoded[0])",
            "-s \(encoded[1])",
            "-X \(secretHex)",
            encoded[2],
        ].joined(separator: " ") + "\n").utf8)
    }

    private func runTemporarySecurityCommand(_ command: Data) -> Int32 {
        let process = Process()
        let input = Pipe()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/security")
        process.arguments = ["-i"]
        process.standardInput = input
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
        do {
            try process.run()
            try input.fileHandleForWriting.write(contentsOf: command)
            try input.fileHandleForWriting.close()
            process.waitUntilExit()
            return process.terminationStatus
        } catch {
            process.terminate()
            return -1
        }
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

    private func makeTemporaryHome() throws -> URL {
        let home = FileManager.default.temporaryDirectory
            .appendingPathComponent("opencodex-gateway-home-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(
            at: home,
            withIntermediateDirectories: false,
            attributes: [.posixPermissions: 0o700]
        )
        return home
    }

    private func lstatInfo(at url: URL) throws -> stat {
        var info = stat()
        guard Darwin.lstat(url.path, &info) == 0 else {
            throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno))
        }
        return info
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
