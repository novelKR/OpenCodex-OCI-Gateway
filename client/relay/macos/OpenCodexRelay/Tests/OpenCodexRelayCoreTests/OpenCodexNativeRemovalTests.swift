import Foundation
import XCTest
@testable import OpenCodexRelayCore

final class OpenCodexNativeRemovalTests: XCTestCase {
    private let installationID = "0123456789abcdef01234567"
    private let installationFingerprint = String(repeating: "a", count: 64)
    private let nativeRestoreFingerprint = String(repeating: "c", count: 64)
    private let boundaryRevision = String(repeating: "b", count: 64)
    private let terminalReceiptDigest = String(repeating: "f", count: 64)

    func testDiscoveryUsesASeparateStrictPathFreeContract() throws {
        let result = try decodeDiscovery(candidateSuffix: "")
        XCTAssertEqual(result.context, .standaloneNative)
        XCTAssertEqual(result.nativeState, .openCodex)
        XCTAssertEqual(result.candidates.count, 1)
        XCTAssertNil(result.candidates[0].homebrewGuard)

        XCTAssertThrowsError(try decodeDiscovery(candidateSuffix: ",\"package_root\":\"/private/value\""))
        XCTAssertThrowsError(try decodeDiscovery(candidateSuffix: ",\"homebrew_guard\":{}"))
    }

    func testConditionalHomebrewGuardRequiresTheExactBoundedSnapshot() throws {
        let guardJSON = """
        ,"homebrew_guard":{"prefix":"/opt/homebrew","package_root":"/opt/homebrew/lib/node_modules/@bitkyc08/opencodex","executable":"/opt/homebrew/lib/node_modules/@bitkyc08/opencodex/bin/ocx.mjs","executable_sha256":"\(String(repeating: "1", count: 64))","cli_entry":"/opt/homebrew/lib/node_modules/@bitkyc08/opencodex/dist/cli.js","cli_entry_sha256":"\(String(repeating: "2", count: 64))","bun_executable":"/opt/homebrew/lib/node_modules/@bitkyc08/opencodex/node_modules/bun/bin/bun","bun_sha256":"\(String(repeating: "3", count: 64))","node_executable":"/opt/homebrew/bin/node","node_sha256":"\(String(repeating: "4", count: 64))","npm_cli":"/opt/homebrew/lib/node_modules/npm/bin/npm-cli.js","npm_cli_sha256":"\(String(repeating: "5", count: 64))","launchers":["/opt/homebrew/bin/ocx"]}
        """
        let candidate = """
        {"installation_id":"\(installationID)","installation_fingerprint":"\(installationFingerprint)","native_restore_fingerprint":"\(nativeRestoreFingerprint)","version":"2.22.0","manager":"homebrew","removal_capability":"homebrew_guarded_npm","removal_authority":"automatic","data_capability":"preserve_only","automatic_removal_eligible":true,"homebrew_guard_required":true\(guardJSON)}
        """
        let decoded = try JSONDecoder().decode(
            OpenCodexNativeRemovalCandidate.self,
            from: Data(candidate.utf8)
        )
        XCTAssertTrue(decoded.homebrewGuardRequired)
        XCTAssertEqual(decoded.homebrewGuard?.prefix, "/opt/homebrew")

        let missingGuard = candidate.replacingOccurrences(of: guardJSON, with: "")
        XCTAssertThrowsError(try JSONDecoder().decode(
            OpenCodexNativeRemovalCandidate.self,
            from: Data(missingGuard.utf8)
        ))
    }

    func testNativeSelectorsAndMutationConfirmationAreExactAndContainNoConfigPath() throws {
        let selection = try makeSelection()
        XCTAssertEqual(selection.selectorArguments, [
            "--installation-id", installationID,
            "--installation-fingerprint", installationFingerprint,
            "--native-restore-fingerprint", nativeRestoreFingerprint,
            "--expected-boundary-revision", boundaryRevision,
        ])
        let request = try OpenCodexNativeRemovalRequest(
            selection: selection,
            mode: .preserveData,
            dataItemIDs: [],
            confirmsRemoval: true,
            confirmsTrash: false,
            confirmsDesktopExited: true
        )
        XCTAssertEqual(request.arguments, [
            "mode", "remove-open-codex-native",
            "--installation-id", installationID,
            "--installation-fingerprint", installationFingerprint,
            "--native-restore-fingerprint", nativeRestoreFingerprint,
            "--expected-boundary-revision", boundaryRevision,
            "--removal-mode", "preserve_data",
            "--confirm-opencodex-native-removal", "--confirm-desktop-exited", "--json",
        ])
        XCTAssertFalse(request.arguments.contains("--config"))
        XCTAssertFalse(request.arguments.contains("--codex-config"))

        XCTAssertEqual(
            try ProcessOpenCodexNativeRemovalClient.terminalAcknowledgementArguments(
                receiptDigest: terminalReceiptDigest
            ),
            [
                "mode", "discover-open-codex-native",
                "--acknowledge-terminal-receipt-digest", terminalReceiptDigest,
                "--json",
            ]
        )
        XCTAssertThrowsError(try ProcessOpenCodexNativeRemovalClient.terminalAcknowledgementArguments(
            receiptDigest: terminalReceiptDigest.uppercased()
        ))
    }

    func testRecoveryReadEnvelopesAreUnambiguousAndFailClosed() throws {
        let recoveryDiscovery = Data("""
        {"schema_version":1,"operation":"discover-open-codex-native","context":"standalone_native","status":"recovery_required","boundary_revision":"\(boundaryRevision)","native_state":"unavailable","native_recovery_required":true,"candidates":[],"rejected":0,"truncated":false}
        """.utf8)
        XCTAssertNoThrow(try JSONDecoder().decode(
            OpenCodexNativeDiscoveryResult.self,
            from: recoveryDiscovery
        ).validated())

        let ambiguousDiscovery = String(decoding: recoveryDiscovery, as: UTF8.self)
            .replacingOccurrences(of: #""candidates":[]"#, with: #""candidates":[{}]"#)
        XCTAssertThrowsError(try JSONDecoder().decode(
            OpenCodexNativeDiscoveryResult.self,
            from: Data(ambiguousDiscovery.utf8)
        ).validated())

        let recoveryInspection = Data("""
        {"schema_version":1,"operation":"inspect-open-codex-native-removal","context":"standalone_native","status":"recovery_required","boundary_revision":"\(boundaryRevision)","native_state":"unavailable","native_recovery_required":true}
        """.utf8)
        XCTAssertNoThrow(try JSONDecoder().decode(
            OpenCodexNativeRemovalInspection.self,
            from: recoveryInspection
        ).validated(for: makeSelection()))

        let spoofedReady = String(decoding: recoveryInspection, as: UTF8.self)
            .replacingOccurrences(
                of: #""status":"recovery_required""#,
                with: #""status":"ready""#
            )
        XCTAssertThrowsError(try JSONDecoder().decode(
            OpenCodexNativeRemovalInspection.self,
            from: Data(spoofedReady.utf8)
        ).validated(for: makeSelection()))
    }

    func testNativeInventoryBindsAllThreeCandidateWitnessesAndBoundary() throws {
        let inventoryRevision = String(repeating: "d", count: 64)
        let source = Data("""
        {"schema_version":1,"operation":"open-codex-native-data-inventory","context":"standalone_native","status":"verified","boundary_revision":"\(boundaryRevision)","native_state":"opencodex","native_recovery_required":false,"installation_id":"\(installationID)","installation_fingerprint":"\(installationFingerprint)","native_restore_fingerprint":"\(nativeRestoreFingerprint)","inventory_revision":"\(inventoryRevision)","items":[]}
        """.utf8)
        XCTAssertNoThrow(try JSONDecoder().decode(
            OpenCodexNativeDataInventoryReceipt.self,
            from: source
        ).validated(for: makeSelection()))

        let changedRestore = String(decoding: source, as: UTF8.self)
            .replacingOccurrences(
                of: nativeRestoreFingerprint,
                with: String(repeating: "e", count: 64)
            )
        XCTAssertThrowsError(try JSONDecoder().decode(
            OpenCodexNativeDataInventoryReceipt.self,
            from: Data(changedRestore.utf8)
        ).validated(for: makeSelection()))
    }

    func testRecoveryPayloadSchemaThreeBindsContextAndTerminalAcknowledgement() throws {
        let selection = try OpenCodexRemovalSelection(
            installationID: installationID,
            installationFingerprint: installationFingerprint
        )
        let session = try OpenCodexRemovalRecoverySession(
            context: .standaloneNative,
            selection: selection,
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .nativeRecoveryRequired,
            lastCode: "native_restore_unverified",
            expectedBoundaryRevision: boundaryRevision,
            nativeRestoreFingerprint: nativeRestoreFingerprint
        )
        let data = try JSONEncoder().encode(session)
        XCTAssertEqual(try JSONDecoder().decode(OpenCodexRemovalRecoverySession.self, from: data), session)
        XCTAssertTrue(String(decoding: data, as: UTF8.self).contains("\"context\":\"standalone_native\""))

        let schemaThree = try OpenCodexRemovalRecoverySession(
            schema: 3,
            context: .standaloneNative,
            selection: selection,
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .nativeRecoveryRequired,
            lastCode: "native_restore_unverified",
            expectedBoundaryRevision: boundaryRevision,
            nativeRestoreFingerprint: nativeRestoreFingerprint
        )
        XCTAssertEqual(
            try JSONDecoder().decode(
                OpenCodexRemovalRecoverySession.self,
                from: JSONEncoder().encode(schemaThree)
            ),
            schemaThree
        )

        let terminal = try OpenCodexRemovalRecoverySession(
            context: .standaloneNative,
            selection: selection,
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .terminalAckPending,
            lastCode: "terminal_ack_pending",
            expectedBoundaryRevision: boundaryRevision,
            nativeRestoreFingerprint: nativeRestoreFingerprint,
            terminalReceiptDigest: terminalReceiptDigest
        )
        XCTAssertEqual(
            try JSONDecoder().decode(
                OpenCodexRemovalRecoverySession.self,
                from: JSONEncoder().encode(terminal)
            ),
            terminal
        )
        XCTAssertThrowsError(try OpenCodexRemovalRecoverySession(
            schema: 4,
            context: .standaloneNative,
            selection: selection,
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .terminalAckPending,
            lastCode: "terminal_ack_pending",
            expectedBoundaryRevision: boundaryRevision,
            nativeRestoreFingerprint: nativeRestoreFingerprint,
            terminalReceiptDigest: terminalReceiptDigest
        ))

        XCTAssertThrowsError(try OpenCodexRemovalRecoverySession(
            schema: 2,
            context: .standaloneNative,
            selection: selection,
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .nativeRecoveryRequired,
            lastCode: "native_restore_unverified",
            expectedBoundaryRevision: boundaryRevision,
            nativeRestoreFingerprint: nativeRestoreFingerprint
        ))
        XCTAssertThrowsError(try OpenCodexRemovalRecoverySession(
            schema: 2,
            selection: selection,
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .nativeRecoveryRequired,
            lastCode: "native_recovery_required"
        ))

        let legacyDataItem = "ocx-data-v1:" + String(repeating: "1", count: 32)
        let legacyIntegratedTrash = try OpenCodexRemovalRecoverySession(
            schema: 2,
            selection: selection,
            mode: .trashSelected,
            orderedDataItemIDs: [legacyDataItem],
            retiredDataItemIDs: [],
            recoveryKind: .dataSelectionRefreshRequired,
            lastCode: "data_selection_refresh_required",
            inventoryRevision: String(repeating: "d", count: 64)
        )
        let legacyData = try JSONEncoder().encode(legacyIntegratedTrash)
        XCTAssertFalse(String(decoding: legacyData, as: UTF8.self).contains("\"context\""))
        XCTAssertEqual(
            try JSONDecoder().decode(OpenCodexRemovalRecoverySession.self, from: legacyData),
            legacyIntegratedTrash
        )
    }

    func testNativeReceiptBindsBoundaryAndAcceptsOnlyFiniteTerminalProofs() throws {
        let request = try OpenCodexNativeRemovalRequest(
            selection: makeSelection(),
            mode: .preserveData,
            dataItemIDs: [],
            confirmsRemoval: true,
            confirmsTrash: false,
            confirmsDesktopExited: true
        )
        let source = nativeReceipt(
            status: "completed",
            boundary: boundaryRevision,
            nativeState: "native",
            recoveryRequired: false,
            packageRemoved: true,
            terminalReceiptDigest: terminalReceiptDigest,
            stages: """
            [{"stage":"native_boundary_final_verification","status":"completed","code":"native_ownership_reverified"},{"stage":"package_verification","status":"completed","code":"package_absent","subject_id":"\(installationID)"},{"stage":"cleanup_journal_retained","status":"completed","code":"terminal_receipt_replayable"}]
            """
        )
        let receipt = try JSONDecoder().decode(OpenCodexNativeRemovalReceipt.self, from: source)
        XCTAssertTrue(try receipt.validated(for: request).isSuccessful)

        let changedBoundary = nativeReceipt(
            status: "completed",
            boundary: String(repeating: "d", count: 64),
            nativeState: "native",
            recoveryRequired: false,
            packageRemoved: true,
            terminalReceiptDigest: terminalReceiptDigest,
            stages: """
            [{"stage":"native_boundary_final_verification","status":"completed","code":"native_ownership_reverified"},{"stage":"package_verification","status":"completed","code":"package_absent","subject_id":"\(installationID)"},{"stage":"cleanup_journal_retained","status":"completed","code":"terminal_receipt_replayable"}]
            """
        )
        XCTAssertThrowsError(try JSONDecoder().decode(
            OpenCodexNativeRemovalReceipt.self,
            from: changedBoundary
        ).validated(for: request))

        let missingDigest = String(decoding: source, as: UTF8.self)
            .replacingOccurrences(
                of: ",\"terminal_receipt_digest\":\"\(terminalReceiptDigest)\"",
                with: ""
            )
        XCTAssertThrowsError(try JSONDecoder().decode(
            OpenCodexNativeRemovalReceipt.self,
            from: Data(missingDigest.utf8)
        ).validated(for: request))

        let nullDigest = String(decoding: source, as: UTF8.self)
            .replacingOccurrences(
                of: "\"terminal_receipt_digest\":\"\(terminalReceiptDigest)\"",
                with: "\"terminal_receipt_digest\":null"
            )
        XCTAssertThrowsError(try JSONDecoder().decode(
            OpenCodexNativeRemovalReceipt.self,
            from: Data(nullDigest.utf8)
        ))

        let inventedStageCode = String(decoding: source, as: UTF8.self)
            .replacingOccurrences(of: "terminal_receipt_replayable\"}", with: "cleanup_finished\"}")
        XCTAssertThrowsError(try JSONDecoder().decode(
            OpenCodexNativeRemovalReceipt.self,
            from: Data(inventedStageCode.utf8)
        ).validated(for: request))
    }

    func testNativeRecoveryReceiptRequiresOneExplicitRecoveryStage() throws {
        let request = try OpenCodexNativeRemovalRequest(
            selection: makeSelection(),
            mode: .preserveData,
            dataItemIDs: [],
            confirmsRemoval: true,
            confirmsTrash: false,
            confirmsDesktopExited: true
        )
        let valid = nativeReceipt(
            status: "partial",
            boundary: boundaryRevision,
            nativeState: "unavailable",
            recoveryRequired: true,
            packageRemoved: false,
            stages: """
            [{"stage":"native_recovery","status":"failed","code":"native_recovery_required"}]
            """
        )
        XCTAssertNoThrow(try JSONDecoder().decode(
            OpenCodexNativeRemovalReceipt.self,
            from: valid
        ).validated(for: request))

        let missingRecoveryStage = nativeReceipt(
            status: "partial",
            boundary: boundaryRevision,
            nativeState: "unavailable",
            recoveryRequired: true,
            packageRemoved: false,
            stages: """
            [{"stage":"candidate_revalidation","status":"completed","code":"candidate_verified","subject_id":"\(installationID)"}]
            """
        )
        XCTAssertThrowsError(try JSONDecoder().decode(
            OpenCodexNativeRemovalReceipt.self,
            from: missingRecoveryStage
        ).validated(for: request))
    }

    func testNativeReceiptAcceptsProducerTeardownAlreadyCompletedRecoveryStage() throws {
        let request = try OpenCodexNativeRemovalRequest(
            selection: makeSelection(),
            mode: .preserveData,
            dataItemIDs: [],
            confirmsRemoval: true,
            confirmsTrash: false,
            confirmsRebootedProcessRecovery: true,
            confirmsDesktopExited: true
        )
        let stages = """
        [{"stage":"candidate_revalidation","status":"completed","code":"candidate_verified","subject_id":"\(installationID)"},{"stage":"teardown_preflight","status":"completed","code":"teardown_preflight_verified","subject_id":"\(installationID)"},{"stage":"cleanup_journal","status":"completed","code":"cleanup_resume","subject_id":"\(installationID)"},{"stage":"teardown","status":"skipped","code":"teardown_already_completed","subject_id":"\(installationID)"},{"stage":"native_restore","status":"skipped","code":"native_already_active"},{"stage":"native_boundary_verification","status":"completed","code":"native_ownership_reverified"},{"stage":"package_verification","status":"completed","code":"package_absent","subject_id":"\(installationID)"},{"stage":"native_boundary_final_verification","status":"completed","code":"native_ownership_reverified"},{"stage":"cleanup_journal_retained","status":"completed","code":"terminal_receipt_replayable"}]
        """
        let receipt = try JSONDecoder().decode(
            OpenCodexNativeRemovalReceipt.self,
            from: nativeReceipt(
                status: "completed",
                boundary: boundaryRevision,
                nativeState: "native",
                recoveryRequired: false,
                packageRemoved: true,
                terminalReceiptDigest: terminalReceiptDigest,
                stages: stages
            )
        )
        XCTAssertTrue(try receipt.validated(for: request).isSuccessful)

        let inventedCode = stages.replacingOccurrences(
            of: "teardown_already_completed",
            with: "teardown_skipped"
        )
        XCTAssertThrowsError(try JSONDecoder().decode(
            OpenCodexNativeRemovalReceipt.self,
            from: nativeReceipt(
                status: "completed",
                boundary: boundaryRevision,
                nativeState: "native",
                recoveryRequired: false,
                packageRemoved: true,
                terminalReceiptDigest: terminalReceiptDigest,
                stages: inventedCode
            )
        ).validated(for: request))
    }

    private func makeSelection() throws -> OpenCodexNativeRemovalSelection {
        try OpenCodexNativeRemovalSelection(
            installationID: installationID,
            installationFingerprint: installationFingerprint,
            nativeRestoreFingerprint: nativeRestoreFingerprint,
            boundaryRevision: boundaryRevision
        )
    }

    private func decodeDiscovery(candidateSuffix: String) throws -> OpenCodexNativeDiscoveryResult {
        let source = Data("""
        {"schema_version":1,"operation":"discover-open-codex-native","context":"standalone_native","status":"ready","boundary_revision":"\(boundaryRevision)","native_state":"opencodex","native_recovery_required":false,"candidates":[{"installation_id":"\(installationID)","installation_fingerprint":"\(installationFingerprint)","native_restore_fingerprint":"\(nativeRestoreFingerprint)","version":"2.22.0","manager":"npm","removal_capability":"exact_npm","removal_authority":"automatic","data_capability":"preserve_only","automatic_removal_eligible":true,"homebrew_guard_required":false\(candidateSuffix)}],"rejected":0,"truncated":false}
        """.utf8)
        return try JSONDecoder().decode(OpenCodexNativeDiscoveryResult.self, from: source).validated()
    }

    private func nativeReceipt(
        status: String,
        boundary: String,
        nativeState: String,
        recoveryRequired: Bool,
        packageRemoved: Bool,
        terminalReceiptDigest: String? = nil,
        stages: String
    ) -> Data {
        let terminalField = terminalReceiptDigest.map {
            ",\"terminal_receipt_digest\":\"\($0)\""
        } ?? ""
        return Data("""
        {"schema_version":1,"operation":"remove-open-codex-native","context":"standalone_native","status":"\(status)","boundary_revision":"\(boundary)","native_state":"\(nativeState)","native_recovery_required":\(recoveryRequired),"mode":"preserve_data","installation_id":"\(installationID)","data_scope":"preserved","selected_data_items":0,"moved_data_items":0,"package_removed":\(packageRemoved),"data_movement_unknown":false,"permanent_delete_fallback":false\(terminalField),"stages":\(stages)}
        """.utf8)
    }
}
