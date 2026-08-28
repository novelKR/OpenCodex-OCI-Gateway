import Darwin
import Foundation
import XCTest
@testable import OpenCodexRelayCore

final class OpenCodexRelayCoreTests: XCTestCase {
    private final class MockLoginRegistration: LoginRegistrationManaging {
        enum Failure: Error {
            case registrationFailed
        }

        var registrationState: LoginRegistrationState
        var stateAfterRegister: LoginRegistrationState
        var stateAfterUnregister: LoginRegistrationState
        var shouldFail = false
        var shouldFailUnregister = false
        private(set) var registerCalls = 0
        private(set) var unregisterCalls = 0

        init(
            registrationState: LoginRegistrationState,
            stateAfterRegister: LoginRegistrationState,
            stateAfterUnregister: LoginRegistrationState = .disabled
        ) {
            self.registrationState = registrationState
            self.stateAfterRegister = stateAfterRegister
            self.stateAfterUnregister = stateAfterUnregister
        }

        func register() throws {
            registerCalls += 1
            if shouldFail {
                throw Failure.registrationFailed
            }
            registrationState = stateAfterRegister
        }

        func unregister() throws {
            unregisterCalls += 1
            if shouldFailUnregister {
                throw Failure.registrationFailed
            }
            registrationState = stateAfterUnregister
        }
    }

    private func connection(
        localRelay: LocalRelayConnection = .healthy,
		localOpenCodex: LocalOpenCodexAvailability = .unknown,
        routingSync: RoutingSync = .acknowledged,
        remoteGateway: RemoteGatewayConnection = .reachable,
        catalog: CatalogConnection = .running
    ) -> RelayConnectionStatus {
        RelayConnectionStatus(
            localRelay: localRelay,
			localOpenCodex: localOpenCodex,
            routingSync: routingSync,
            remoteGateway: remoteGateway,
            catalog: catalog
        )
    }

    private func writeRoutingBinding(in root: URL, codexConfig: URL) throws -> URL {
        let bindingURL = root.appendingPathComponent("routing-binding.json", isDirectory: false)
        let relayConfig = root.appendingPathComponent("relay.json", isDirectory: false)
        let binding = RoutingBinding(relayConfig: relayConfig.path, codexConfig: codexConfig.path)
        try JSONEncoder().encode(binding).write(to: bindingURL, options: .atomic)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o600],
            ofItemAtPath: bindingURL.path
        )
        return bindingURL
    }

    private func status(
        schemaVersion: Int = 2,
		desiredMode: RoutingMode? = nil,
		appliedMode: RoutingMode? = nil,
		desiredBackend: RoutingBackend = .external,
		appliedBackend: RoutingBackend = .external,
        phase: RoutingPhase = .relayActive,
        relayAdmission: RelayAdmission = .allow,
        catalogRefresh: CatalogRefresh = .run,
        relayRunning: Bool = true,
        activeRequests: Int? = 0,
        desktopRestartRequired: Bool = false,
        connection: RelayConnectionStatus? = nil,
        recoveryCapabilities: RecoveryCapabilities? = nil
    ) -> RoutingStatus {
        RoutingStatus(
            schemaVersion: schemaVersion,
			desiredMode: desiredMode,
			appliedMode: appliedMode,
			desiredBackend: desiredBackend,
			appliedBackend: appliedBackend,
            phase: phase,
            relayAdmission: relayAdmission,
            catalogRefresh: catalogRefresh,
            relayRunning: relayRunning,
            activeRequests: activeRequests,
            desktopRestartRequired: desktopRestartRequired,
            desktopEffectiveMode: .unverifiable,
            generation: 2,
            connection: connection ?? self.connection(),
            recoveryCapabilities: recoveryCapabilities
        )
    }

    func testRelayctlCommandsRemainExplicitAndJSONOnly() throws {
        let removalSelection = try OpenCodexRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64)
        )
        XCTAssertEqual(RelayctlCommand.status.arguments, ["mode", "status", "--json"])
        XCTAssertEqual(RelayctlCommand.request(.native).arguments, ["mode", "request", "native", "--json"])
        XCTAssertEqual(RelayctlCommand.request(.external).arguments, ["mode", "request", "external", "--json"])
        XCTAssertEqual(RelayctlCommand.request(.localOpenCodex).arguments, ["mode", "request", "local_opencodex", "--json"])
        XCTAssertEqual(
            RelayctlCommand.requestExternalMigratingKnownLegacy(
                expectedConfigDigest: String(repeating: "b", count: 64),
                expectedRoutingGeneration: 9
            ).arguments,
            [
                "mode", "request", "external",
                "--known-legacy-backup-and-migrate",
                "--expected-config-digest", String(repeating: "b", count: 64),
                "--expected-routing-generation", "9",
                "--json",
            ]
        )
        XCTAssertEqual(RelayctlCommand.apply.arguments, ["mode", "apply", "--confirm-desktop-exited", "--json"])
        XCTAssertEqual(RelayctlCommand.cancel.arguments, ["mode", "cancel", "--json"])
        XCTAssertEqual(RelayctlCommand.recoverComplete.arguments, ["mode", "recover", "--complete", "--confirm-desktop-exited", "--json"])
        XCTAssertEqual(
            RelayctlCommand.recoverOpenCodexRemoval(
                selection: removalSelection,
                expectedRoutingGeneration: 7
            ).arguments,
            [
                "mode", "recover",
                "--installation-id", removalSelection.installationID,
                "--installation-fingerprint", removalSelection.installationFingerprint,
                "--expected-routing-generation", "7",
                "--complete",
                "--confirm-desktop-exited",
                "--json",
            ]
        )
        XCTAssertEqual(RelayctlCommand.recoverRollback.arguments, ["mode", "recover", "--rollback", "--confirm-desktop-exited", "--json"])
        XCTAssertEqual(RelayctlCommand.status.minimumHelperTimeout, 20)
        XCTAssertEqual(RelayctlCommand.apply.minimumHelperTimeout, 110)
    }

    func testOpenCodexDiscoveryContractValidatesExactPackageEvidence() throws {
        let source = """
        {"schema_version":2,"requested_tier":"b","broad_scan_approved":false,"candidates":[{"id":"0123456789abcdef01234567","tier":"b","source":"nvm","manager":"nvm","prefix":"/home/example/.nvm/versions/node/v22","package_root":"/home/example/.nvm/versions/node/v22/lib/node_modules/@bitkyc08/opencodex","version":"2.33.0","executable":"/home/example/.nvm/versions/node/v22/lib/node_modules/@bitkyc08/opencodex/bin/ocx.mjs","executable_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","cli_entry":"/home/example/.nvm/versions/node/v22/lib/node_modules/@bitkyc08/opencodex/dist/cli.js","cli_entry_sha256":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","bun_executable":"/home/example/.bun/bin/bun","bun_sha256":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","package_tree_sha256":"1111111111111111111111111111111111111111111111111111111111111111","npm_tree_sha256":"2222222222222222222222222222222222222222222222222222222222222222","launchers":["/home/example/.nvm/versions/node/v22/bin/ocx"],"node_executable":"/home/example/.nvm/versions/node/v22/bin/node","node_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","npm_cli":"/home/example/.nvm/versions/node/v22/lib/node_modules/npm/bin/npm-cli.js","npm_cli_sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","confidence":"trusted","removal_capability":"exact_npm","removal_authority":"automatic","user_writable":true,"requires_elevation":false,"fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","warnings":[]}],"coverage":[{"source":"nvm","root":"/home/example/.nvm","state":"scanned"}],"rejected":0,"truncated":false}
        """.data(using: .utf8)!
        let result = try JSONDecoder().decode(OpenCodexDiscoveryResult.self, from: source).validated()
        XCTAssertEqual(result.requestedTier, .b)
        XCTAssertEqual(result.candidates.first?.manager, .nvm)
        XCTAssertEqual(result.candidates.first?.removalCapability, .exactNPM)
        XCTAssertEqual(result.candidates.first?.handoffExecutable?.sha256, String(repeating: "a", count: 64))
        XCTAssertEqual(result.candidates.first?.cliEntrySHA256, String(repeating: "e", count: 64))
        XCTAssertFalse(result.candidates.first?.isAutomaticRemovalEligible == true)

        let preserveV4 = String(data: source, encoding: .utf8)!
            .replacingOccurrences(of: #""schema_version":2"#, with: #""schema_version":4"#)
            .replacingOccurrences(
                of: #""removal_authority":"automatic""#,
                with: #""removal_authority":"automatic","homebrew_guard_required":false,"teardown_capability":"relay_preserve_v1","data_capability":"preserve_only","teardown_compatibility_reason":"compatible","teardown_adapter_id":"opencodex_npm_2_33_0_preserve_v1""#
            )
            .data(using: .utf8)!
        let preserveResult = try JSONDecoder()
            .decode(OpenCodexDiscoveryResult.self, from: preserveV4)
            .validated()
        XCTAssertTrue(preserveResult.candidates[0].isAutomaticRemovalEligible)
        XCTAssertEqual(preserveResult.candidates[0].version, "2.33.0")
        XCTAssertEqual(
            preserveResult.candidates[0].teardownAdapterID,
            "opencodex_npm_2_33_0_preserve_v1"
        )

        for unsupportedVersion in ["2.30.0", "2.33.0-preview.1"] {
            let unsupported = String(data: preserveV4, encoding: .utf8)!
                .replacingOccurrences(of: #""version":"2.33.0""#, with: #""version":"\#(unsupportedVersion)""#)
                .replacingOccurrences(
                    of: #""removal_authority":"automatic","homebrew_guard_required":false,"teardown_capability":"relay_preserve_v1","data_capability":"preserve_only","teardown_compatibility_reason":"compatible","teardown_adapter_id":"opencodex_npm_2_33_0_preserve_v1""#,
                    with: #""removal_authority":"manual","homebrew_guard_required":false,"teardown_capability":"none","data_capability":"preserve_only","teardown_compatibility_reason":"unsupported_version""#
                )
                .data(using: .utf8)!
            let unsupportedResult = try JSONDecoder()
                .decode(OpenCodexDiscoveryResult.self, from: unsupported)
                .validated()
            XCTAssertFalse(unsupportedResult.candidates[0].isAutomaticRemovalEligible)
            XCTAssertNil(unsupportedResult.candidates[0].teardownAdapterID)
        }

        let tamperedAdapterProof = String(data: preserveV4, encoding: .utf8)!
            .replacingOccurrences(
                of: "opencodex_npm_2_33_0_preserve_v1",
                with: "opencodex.npm.2.33.0.preserve.v1"
            )
            .data(using: .utf8)!
        XCTAssertThrowsError(try JSONDecoder()
            .decode(OpenCodexDiscoveryResult.self, from: tamperedAdapterProof)
            .validated())

        let invalid = String(data: source, encoding: .utf8)!
            .replacingOccurrences(of: "@bitkyc08/opencodex", with: "lookalike/opencodex")
            .data(using: .utf8)!
        XCTAssertThrowsError(try JSONDecoder().decode(OpenCodexDiscoveryResult.self, from: invalid).validated())

        let manual = String(data: source, encoding: .utf8)!
            .replacingOccurrences(of: #""removal_authority":"automatic""#, with: #""removal_authority":"manual""#)
            .data(using: .utf8)!
        let manualResult = try JSONDecoder().decode(OpenCodexDiscoveryResult.self, from: manual).validated()
        XCTAssertFalse(manualResult.candidates[0].isAutomaticRemovalEligible)

        let restoreVerified = String(data: source, encoding: .utf8)!
            .replacingOccurrences(
                of: #""removal_authority":"automatic""#,
                with: #""removal_authority":"automatic","native_restore_capability":"verified_snapshot","native_restore_fingerprint":"9999999999999999999999999999999999999999999999999999999999999999""#
            )
            .data(using: .utf8)!
        let restoreResult = try JSONDecoder()
            .decode(OpenCodexDiscoveryResult.self, from: restoreVerified)
            .validated()
        XCTAssertEqual(restoreResult.candidates[0].nativeRestoreCapability, .verifiedSnapshot)
        XCTAssertEqual(
            restoreResult.candidates[0].nativeRepairSelection?.nativeRestoreFingerprint,
            String(repeating: "9", count: 64)
        )

        let incompleteRestoreProof = String(data: source, encoding: .utf8)!
            .replacingOccurrences(
                of: #""removal_authority":"automatic""#,
                with: #""removal_authority":"automatic","native_restore_capability":"verified_snapshot""#
            )
            .data(using: .utf8)!
        XCTAssertThrowsError(try JSONDecoder()
            .decode(OpenCodexDiscoveryResult.self, from: incompleteRestoreProof)
            .validated())

        let legacy = String(data: source, encoding: .utf8)!
            .replacingOccurrences(of: #""schema_version":2"#, with: #""schema_version":1"#)
            .data(using: .utf8)!
        XCTAssertThrowsError(try JSONDecoder().decode(OpenCodexDiscoveryResult.self, from: legacy).validated())

        let truncatedAutomatic = String(data: source, encoding: .utf8)!
            .replacingOccurrences(of: #""truncated":false"#, with: #""truncated":true"#)
            .data(using: .utf8)!
        XCTAssertThrowsError(try JSONDecoder().decode(OpenCodexDiscoveryResult.self, from: truncatedAutomatic).validated())

        let rejectedAutomatic = String(data: source, encoding: .utf8)!
            .replacingOccurrences(of: #""rejected":0"#, with: #""rejected":1"#)
            .data(using: .utf8)!
        XCTAssertThrowsError(try JSONDecoder().decode(OpenCodexDiscoveryResult.self, from: rejectedAutomatic).validated())

        let refusedCoverageAutomatic = String(data: source, encoding: .utf8)!
            .replacingOccurrences(of: #""state":"scanned""#, with: #""state":"refused""#)
            .data(using: .utf8)!
        XCTAssertThrowsError(try JSONDecoder().decode(OpenCodexDiscoveryResult.self, from: refusedCoverageAutomatic).validated())

        let warningAutomatic = String(data: preserveV4, encoding: .utf8)!
            .replacingOccurrences(of: #""warnings":[]"#, with: #""warnings":["launcher_evidence_truncated"]"#)
            .data(using: .utf8)!
        XCTAssertThrowsError(try JSONDecoder().decode(OpenCodexDiscoveryResult.self, from: warningAutomatic).validated())

        let identityConflictAutomatic = String(data: preserveV4, encoding: .utf8)!
            .replacingOccurrences(of: #""warnings":[]"#, with: #""warnings":["package_identity_conflict"]"#)
            .data(using: .utf8)!
        XCTAssertThrowsError(try JSONDecoder().decode(OpenCodexDiscoveryResult.self, from: identityConflictAutomatic).validated())
    }

    func testOpenCodexDiscoverySchemaV3AcceptsOnlyExactHomebrewGuardContract() throws {
        let source = Data(#"{"schema_version":3,"requested_tier":"b","broad_scan_approved":false,"candidates":[{"id":"0123456789abcdef01234567","tier":"b","source":"trusted_prefix","manager":"homebrew","prefix":"/opt/homebrew","package_root":"/opt/homebrew/lib/node_modules/@bitkyc08/opencodex","version":"2.22.0","executable":"/opt/homebrew/lib/node_modules/@bitkyc08/opencodex/bin/ocx.mjs","executable_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","cli_entry":"/opt/homebrew/lib/node_modules/@bitkyc08/opencodex/src/cli/index.ts","cli_entry_sha256":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","bun_executable":"/opt/homebrew/lib/node_modules/@bitkyc08/opencodex/node_modules/bun/bin/bun.exe","bun_sha256":"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","package_tree_sha256":"1111111111111111111111111111111111111111111111111111111111111111","npm_tree_sha256":"2222222222222222222222222222222222222222222222222222222222222222","launchers":["/opt/homebrew/bin/ocx"],"node_executable":"/opt/homebrew/bin/node","node_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","npm_cli":"/opt/homebrew/lib/node_modules/npm/bin/npm-cli.js","npm_cli_sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","confidence":"trusted","removal_capability":"homebrew_guarded_npm","removal_authority":"automatic","homebrew_guard_required":true,"user_writable":true,"requires_elevation":false,"fingerprint":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","warnings":["homebrew_guard_required"]}],"coverage":[{"source":"trusted_prefix","root":"/opt/homebrew","state":"scanned"}],"rejected":0,"truncated":false}"#.utf8)
        let result = try JSONDecoder().decode(OpenCodexDiscoveryResult.self, from: source).validated()
        let candidate = try XCTUnwrap(result.candidates.first)
        XCTAssertEqual(candidate.removalCapability, .homebrewGuardedNPM)
        XCTAssertTrue(candidate.requiresHomebrewGuard)
        XCTAssertFalse(candidate.isAutomaticRemovalEligible)

        let schemaFour = String(decoding: source, as: UTF8.self)
            .replacingOccurrences(of: #""schema_version":3"#, with: #""schema_version":4"#)
            .replacingOccurrences(
                of: #""homebrew_guard_required":true"#,
                with: #""homebrew_guard_required":true,"teardown_capability":"relay_preserve_v1","data_capability":"preserve_only","teardown_compatibility_reason":"compatible","teardown_adapter_id":"opencodex_npm_2_22_0_preserve_v1""#
            )
        let schemaFourCandidate = try XCTUnwrap(try JSONDecoder()
            .decode(OpenCodexDiscoveryResult.self, from: Data(schemaFour.utf8))
            .validated()
            .candidates.first)
        XCTAssertTrue(schemaFourCandidate.isAutomaticRemovalEligible)

        let missingGuardFlag = String(decoding: source, as: UTF8.self)
            .replacingOccurrences(of: #","homebrew_guard_required":true"#, with: "")
        XCTAssertThrowsError(try JSONDecoder()
            .decode(OpenCodexDiscoveryResult.self, from: Data(missingGuardFlag.utf8))
            .validated())

        let schemaTwoGuard = String(decoding: source, as: UTF8.self)
            .replacingOccurrences(of: #""schema_version":3"#, with: #""schema_version":2"#)
        XCTAssertThrowsError(try JSONDecoder()
            .decode(OpenCodexDiscoveryResult.self, from: Data(schemaTwoGuard.utf8))
            .validated())
    }

	func testLocalOpenCodexPresentationRequiresVerifiedReadiness() throws {
		let ready = status(
			desiredBackend: .localOpenCodex,
			appliedBackend: .localOpenCodex,
			connection: connection(localOpenCodex: .ready, remoteGateway: .notApplicable)
		)
		let value = try ready.validated()
		XCTAssertEqual(value.presentation, .localOpenCodexReady)
		XCTAssertTrue(value.canRequestLocalOpenCodex)

		let parked = status(
			desiredBackend: .localOpenCodex,
			appliedBackend: .localOpenCodex,
			relayAdmission: .deny,
			catalogRefresh: .pause,
			connection: connection(localOpenCodex: .unavailable, remoteGateway: .notApplicable, catalog: .paused)
		)
		XCTAssertEqual(try parked.validated().presentation, .localOpenCodexUnavailable)
		XCTAssertEqual(parked.appliedBackend, .localOpenCodex)
	}

	func testNativeMayExplicitlyRequestLocalWithoutBackgroundProbe() throws {
		let native = status(
			desiredBackend: .none,
			appliedBackend: .none,
			phase: .nativeActive,
			relayAdmission: .deny,
			catalogRefresh: .pause,
			connection: connection(remoteGateway: .notApplicable, catalog: .paused)
		)
		let validated = try native.validated()
		XCTAssertFalse(validated.canRequestLocalOpenCodex)
		XCTAssertTrue(validated.canAttemptLocalOpenCodex)

		let knownUnavailable = status(
			desiredBackend: .none,
			appliedBackend: .none,
			phase: .nativeActive,
			relayAdmission: .deny,
			catalogRefresh: .pause,
			connection: connection(localOpenCodex: .unavailable, remoteGateway: .notApplicable, catalog: .paused)
		)
		XCTAssertFalse(try knownUnavailable.validated().canAttemptLocalOpenCodex)
	}

	func testBackendSwapNeedsDesktopBoundaryAndHandoffCarriesOnlyExactPath() throws {
		let pending = status(
			desiredBackend: .localOpenCodex,
			appliedBackend: .external,
			phase: .backendPendingRestart,
			desktopRestartRequired: true,
			connection: connection(localOpenCodex: .ready)
		)
		XCTAssertTrue(try pending.validated().needsDesktopApply)
		let executable = try OpenCodexExecutable(path: "/tmp/ocx", sha256: String(repeating: "a", count: 64))
		XCTAssertEqual(
            RelayctlCommand.repairNative(expectedGeneration: 3).arguments,
            [
                "mode", "repair-native",
                "--expected-routing-generation", "3",
                "--confirm-local-development-native-repair",
                "--json",
            ]
        )
		XCTAssertEqual(
			RelayctlCommand.handoff(executable, .retainProxyRemoveShim).arguments,
			[
				"mode", "handoff", "--ocx-executable", "/tmp/ocx",
				"--ocx-sha256", String(repeating: "a", count: 64),
				"--action", "retain_proxy_remove_shim",
				"--confirm-opencodex-handoff", "--confirm-desktop-exited", "--json",
			]
		)
	}

    func testNativeRepairInspectionIsStrictBoundedAndValidatesOwnerContract() throws {
        let source = Data(#"{"schema_version":1,"generation":3,"phase":"recovery_required","kind":"opencodex","openai_base_url":true,"model_catalog_json":true,"reason":"opencodex_owned"}"#.utf8)
        let inspection = try JSONDecoder().decode(NativeRepairInspection.self, from: source).validated()
        XCTAssertEqual(inspection.kind, .openCodex)
        XCTAssertTrue(inspection.openAIBaseURL)
        XCTAssertTrue(inspection.modelCatalogJSON)

        let unknownKey = Data(#"{"schema_version":1,"generation":3,"phase":"recovery_required","kind":"opencodex","openai_base_url":true,"model_catalog_json":true,"reason":"opencodex_owned","value":"private"}"#.utf8)
        XCTAssertThrowsError(try JSONDecoder().decode(NativeRepairInspection.self, from: unknownKey))

        let invalidOwnerReason = Data(#"{"schema_version":1,"generation":3,"phase":"recovery_required","kind":"state_only","openai_base_url":true,"model_catalog_json":false,"reason":"native_routing_clean"}"#.utf8)
        XCTAssertThrowsError(try JSONDecoder().decode(NativeRepairInspection.self, from: invalidOwnerReason).validated())
    }

    func testNativeRepairOwnerArgumentsBindExactRestoreSelection() throws {
        let selection = try OpenCodexNativeRepairSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64),
            nativeRestoreFingerprint: String(repeating: "b", count: 64),
            executable: OpenCodexExecutable(
                path: "/tmp/ocx",
                sha256: String(repeating: "c", count: 64)
            )
        )

        XCTAssertEqual(
            try ProcessNativeRepairClient.ownerInspectionArguments(
                expectedGeneration: 3,
                owner: .openCodex,
                selection: selection
            ),
            [
                "mode", "inspect-native-repair-owner",
                "--expected-routing-generation", "3",
                "--expected-owner", "opencodex",
                "--installation-id", selection.installationID,
                "--installation-fingerprint", selection.installationFingerprint,
                "--native-restore-fingerprint", selection.nativeRestoreFingerprint,
                "--ocx-executable", selection.executable.path,
                "--ocx-sha256", selection.executable.sha256,
                "--json",
            ]
        )
        XCTAssertEqual(
            try ProcessNativeRepairClient.repairArguments(
                expectedGeneration: 3,
                owner: .openCodex,
                selection: selection
            ),
            [
                "mode", "repair-native-routing",
                "--expected-routing-generation", "3",
                "--expected-owner", "opencodex",
                "--confirm-desktop-exited",
                "--confirm-local-development-native-routing-repair",
                "--installation-id", selection.installationID,
                "--installation-fingerprint", selection.installationFingerprint,
                "--native-restore-fingerprint", selection.nativeRestoreFingerprint,
                "--ocx-executable", selection.executable.path,
                "--ocx-sha256", selection.executable.sha256,
                "--json",
            ]
        )
        XCTAssertThrowsError(try ProcessNativeRepairClient.ownerInspectionArguments(
            expectedGeneration: 0,
            owner: .openCodex,
            selection: selection
        ))
        XCTAssertThrowsError(try ProcessNativeRepairClient.repairArguments(
            expectedGeneration: 3,
            owner: .openCodex,
            selection: nil
        ))
    }

    func testNativeRoutingRepairReceiptRequiresNativeStatusAndExactEnvelope() throws {
        let native = status(
            desiredBackend: .none,
            appliedBackend: .none,
            phase: .nativeActive,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            connection: connection(remoteGateway: .notApplicable, catalog: .paused)
        )
        let statusObject = try JSONSerialization.jsonObject(with: JSONEncoder().encode(native))
        let data = try JSONSerialization.data(withJSONObject: [
            "schema_version": 1,
            "status": statusObject,
            "backup_created": true,
            "nonrouting_cleanup_incomplete": false,
        ])
        let receipt = try JSONDecoder().decode(NativeRoutingRepairReceipt.self, from: data).validated()
        XCTAssertEqual(receipt.status.generation, 2)
        XCTAssertTrue(receipt.backupCreated)
        XCTAssertFalse(receipt.nonRoutingCleanupIncomplete)
    }

    func testNativeOwnerInspectionIsStrictAndDistinguishesReadiness() throws {
        for (configuration, integration, reason) in [
            ("valid", "enabled", "owner_ready"),
            ("valid", "disabled", "owner_ready"),
            ("invalid", "unknown", "owner_configuration_invalid"),
            ("unavailable", "unknown", "owner_probe_unavailable"),
        ] {
            let source = Data("""
            {"schema_version":1,"generation":3,"owner":"opencodex","configuration":"\(configuration)","integration":"\(integration)","reason":"\(reason)"}
            """.utf8)
            XCTAssertNoThrow(try JSONDecoder().decode(NativeRepairOwnerInspection.self, from: source).validated())
        }
        let inconsistent = Data(#"{"schema_version":1,"generation":3,"owner":"opencodex","configuration":"invalid","integration":"enabled","reason":"owner_ready"}"#.utf8)
        XCTAssertThrowsError(try JSONDecoder().decode(NativeRepairOwnerInspection.self, from: inconsistent).validated())
        let leaked = Data(#"{"schema_version":1,"generation":3,"owner":"opencodex","configuration":"valid","integration":"enabled","reason":"owner_ready","path":"/private"}"#.utf8)
        XCTAssertThrowsError(try JSONDecoder().decode(NativeRepairOwnerInspection.self, from: leaked))
    }

    func testNativeRoutingRepairReceiptV2BoundsOwnerAttemptsAndResult() throws {
        let native = status(
            desiredBackend: .none,
            appliedBackend: .none,
            phase: .nativeActive,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            connection: connection(remoteGateway: .notApplicable, catalog: .paused)
        )
        let statusObject = try JSONSerialization.jsonObject(with: JSONEncoder().encode(native))
        let valid = try JSONSerialization.data(withJSONObject: [
            "schema_version": 2,
            "status": statusObject,
            "backup_created": true,
            "nonrouting_cleanup_incomplete": false,
            "owner_restore_attempts": 4,
            "owner_restore_result": "already_native",
        ])
        let receipt = try JSONDecoder().decode(NativeRoutingRepairReceipt.self, from: valid).validated()
        XCTAssertEqual(receipt.ownerRestoreAttempts, 4)
        XCTAssertEqual(receipt.ownerRestoreResult, .alreadyNative)

        let invalid = try JSONSerialization.data(withJSONObject: [
            "schema_version": 2,
            "status": statusObject,
            "backup_created": true,
            "nonrouting_cleanup_incomplete": false,
            "owner_restore_attempts": 5,
            "owner_restore_result": "applied",
        ])
        XCTAssertThrowsError(try JSONDecoder().decode(NativeRoutingRepairReceipt.self, from: invalid).validated())
    }

    func testNativeActiveRejectsRemoteAdmission() {
        let unsafe = status(
			desiredBackend: .none,
			appliedBackend: .none,
            phase: .nativeActive,
            relayAdmission: .allow,
            catalogRefresh: .pause,
            connection: connection(remoteGateway: .notApplicable, catalog: .paused)
        )
        XCTAssertThrowsError(try unsafe.validated())
    }

    func testRelayActiveHasReadablePresentationAndAccessibilityLabel() throws {
        let value = try status().validated()
        XCTAssertEqual(value.menuTitle, "External gateway ready")
        XCTAssertEqual(value.presentation, .externalReady)
        XCTAssertEqual(value.presentation.symbolName, "arrow.trianglehead.2.clockwise.rotate.90")
        XCTAssertTrue(value.presentation.accessibilityLabel.contains("external gateway ready"))
    }

    func testConnectionProjectionSupportsNullableActiveRequests() throws {
        let source = """
        {"schema_version":2,"desired_mode":"relay","applied_mode":"relay","desired_backend":"external","applied_backend":"external","phase":"relay_active","relay_admission":"allow","catalog_refresh":"run","relay_running":false,"active_requests":null,"desktop_restart_required":false,"desktop_effective_mode":"unverifiable","generation":2,"connection":{"local_relay":"unreachable","local_opencodex":"unknown","routing_sync":"unreachable","remote_gateway":"unknown","catalog":"unknown"}}
        """.data(using: .utf8)!
        let value = try JSONDecoder().decode(RoutingStatus.self, from: source).validated()
        XCTAssertNil(value.activeRequests)
        XCTAssertEqual(value.presentation, .relayUnavailable)
        XCTAssertEqual(value.presentation.symbolName, "network.slash")
        XCTAssertTrue(value.presentation.accessibilityLabel.contains("local relay is unavailable"))
    }

    func testRecoveryAllowsUnknownModesWithoutClaimingAnEffectiveBackend() throws {
        let recovery = status(
			desiredBackend: .unknown,
			appliedBackend: .unknown,
            phase: .recoveryRequired,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            relayRunning: false,
            activeRequests: nil,
            desktopRestartRequired: true,
            connection: connection(
                localRelay: .unknown,
                routingSync: .invalid,
                remoteGateway: .notApplicable,
                catalog: .paused
            )
        )
        let validated = try recovery.validated()
        XCTAssertEqual(validated.presentation, .recoveryRequired)
        XCTAssertFalse(validated.canRecover(.complete))
        XCTAssertFalse(validated.canRecover(.rollback))
    }

    func testSchemaV3RecoveryCapabilitiesAreExplicitAndFailClosed() throws {
        let completeOnly = status(
            schemaVersion: 3,
            desiredBackend: .unknown,
            appliedBackend: .unknown,
            phase: .recoveryRequired,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            desktopRestartRequired: true,
            connection: connection(routingSync: .invalid, remoteGateway: .notApplicable, catalog: .paused),
            recoveryCapabilities: RecoveryCapabilities(
                canComplete: true,
                canRollback: false,
                completeReason: "observed_state_verified",
                rollbackReason: "journal_missing",
                target: .none,
                targetConfidence: .observed,
                authoritativeJournal: false
            )
        )
        let validated = try completeOnly.validated()
        XCTAssertTrue(validated.canRecover(.complete))
        XCTAssertFalse(validated.canRecover(.rollback))
        XCTAssertEqual(validated.recoveryReason(for: .rollback), "journal_missing")

        let missingEvidence = status(
            schemaVersion: 3,
            desiredBackend: .unknown,
            appliedBackend: .unknown,
            phase: .recoveryRequired,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            desktopRestartRequired: true,
            connection: connection(routingSync: .invalid, remoteGateway: .notApplicable, catalog: .paused)
        )
        XCTAssertThrowsError(try missingEvidence.validated())

        let contradictory = status(
            schemaVersion: 3,
            recoveryCapabilities: RecoveryCapabilities(
                canComplete: true,
                canRollback: true,
                completeReason: "journal_verified",
                rollbackReason: "journal_verified",
                target: .external,
                targetConfidence: .journal,
                authoritativeJournal: true
            )
        )
        XCTAssertThrowsError(try contradictory.validated())
    }

    func testOpaqueGatedZeroGenerationIsRenderableButNeverActionable() throws {
        let unavailable = RecoveryCapabilities(
            canComplete: false,
            canRollback: false,
            completeReason: "observed_state_unavailable",
            rollbackReason: "observed_state_unavailable",
            target: .unknown,
            targetConfidence: .unavailable,
            authoritativeJournal: false
        )

        let opaque = RoutingStatus(
            schemaVersion: 3,
            desiredBackend: .unknown,
            appliedBackend: .unknown,
            phase: .recoveryRequired,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            relayRunning: false,
            activeRequests: nil,
            desktopRestartRequired: true,
            desktopEffectiveMode: .unverifiable,
            generation: 0,
            connection: connection(
                localRelay: .unknown,
                routingSync: .invalid,
                remoteGateway: .unknown,
                catalog: .unknown
            ),
            recoveryCapabilities: unavailable
        )
        let validated = try opaque.validated()
        XCTAssertEqual(validated.presentation, .recoveryRequired)
        XCTAssertFalse(validated.canRecover(.complete))
        XCTAssertFalse(validated.canRecover(.rollback))
        XCTAssertFalse(validated.canReviewSavedOpenCodexRemovalRecovery)

        let relayDown = RoutingStatus(
            schemaVersion: 3,
            desiredBackend: .unknown,
            appliedBackend: .unknown,
            phase: .recoveryRequired,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            relayRunning: false,
            activeRequests: nil,
            desktopRestartRequired: true,
            desktopEffectiveMode: .unverifiable,
            generation: 0,
            connection: connection(
                localRelay: .unreachable,
                routingSync: .unreachable,
                remoteGateway: .unknown,
                catalog: .unknown
            ),
            recoveryCapabilities: unavailable
        )
        let validatedRelayDown = try relayDown.validated()
        XCTAssertEqual(validatedRelayDown.presentation, .relayUnavailable)
        XCTAssertFalse(validatedRelayDown.canReviewSavedOpenCodexRemovalRecovery)

        let actionable = RoutingStatus(
            schemaVersion: 3,
            desiredBackend: .unknown,
            appliedBackend: .unknown,
            phase: .recoveryRequired,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            relayRunning: false,
            activeRequests: nil,
            desktopRestartRequired: true,
            desktopEffectiveMode: .unverifiable,
            generation: 0,
            connection: connection(
                localRelay: .unknown,
                routingSync: .invalid,
                remoteGateway: .notApplicable,
                catalog: .paused
            ),
            recoveryCapabilities: RecoveryCapabilities(
                canComplete: true,
                canRollback: false,
                completeReason: "observed_state_verified",
                rollbackReason: "journal_missing",
                target: .external,
                targetConfidence: .observed,
                authoritativeJournal: false
            )
        )
        XCTAssertThrowsError(try actionable.validated())
    }

    func testSavedRemovalRecoveryAcceptsOnlyUnreachableRelayProjection() throws {
        let status = RoutingStatus(
            schemaVersion: 3,
            desiredBackend: .unknown,
            appliedBackend: .unknown,
            phase: .recoveryRequired,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            relayRunning: false,
            activeRequests: nil,
            desktopRestartRequired: true,
            desktopEffectiveMode: .unverifiable,
            generation: 7,
            connection: connection(
                localRelay: .unreachable,
                routingSync: .unreachable,
                remoteGateway: .unknown,
                catalog: .unknown
            ),
            recoveryCapabilities: RecoveryCapabilities(
                canComplete: true,
                canRollback: false,
                completeReason: "observed_state_verified",
                rollbackReason: "journal_missing",
                target: .external,
                targetConfidence: .observed,
                authoritativeJournal: false
            )
        )
        let validated = try status.validated()
        XCTAssertFalse(validated.canUninstallOpenCodex)
        XCTAssertTrue(validated.canReviewSavedOpenCodexRemovalRecovery)
    }

    func testSavedRoutingRecoveryRequiresACompleteNonLocalTarget() throws {
        func gatedStatus(target: RoutingBackend) throws -> RoutingStatus {
            try RoutingStatus(
                schemaVersion: 3,
                desiredBackend: .unknown,
                appliedBackend: .unknown,
                phase: .recoveryRequired,
                relayAdmission: .deny,
                catalogRefresh: .pause,
                relayRunning: true,
                activeRequests: 0,
                desktopRestartRequired: true,
                desktopEffectiveMode: .unverifiable,
                generation: 7,
                connection: connection(
                    localRelay: .healthy,
                    routingSync: .invalid,
                    remoteGateway: .unknown,
                    catalog: .paused
                ),
                recoveryCapabilities: RecoveryCapabilities(
                    canComplete: true,
                    canRollback: false,
                    completeReason: "observed_state_verified",
                    rollbackReason: "origin_not_authoritative",
                    target: target,
                    targetConfidence: .observed,
                    authoritativeJournal: false
                )
            ).validated()
        }

        let external = try gatedStatus(target: .external)
        let native = try gatedStatus(target: .none)
        let local = try gatedStatus(target: .localOpenCodex)
        XCTAssertTrue(external.canCheckpointSavedOpenCodexRoutingRecovery)
        XCTAssertTrue(external.canReviewSavedOpenCodexRoutingRecovery)
        XCTAssertTrue(native.canCheckpointSavedOpenCodexRoutingRecovery)
        XCTAssertTrue(native.canReviewSavedOpenCodexRoutingRecovery)
        XCTAssertFalse(local.canCheckpointSavedOpenCodexRoutingRecovery)
        XCTAssertFalse(local.canReviewSavedOpenCodexRoutingRecovery)

        let unreachable = RoutingStatus(
            schemaVersion: 3,
            desiredBackend: .unknown,
            appliedBackend: .unknown,
            phase: .recoveryRequired,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            relayRunning: false,
            activeRequests: nil,
            desktopRestartRequired: true,
            desktopEffectiveMode: .unverifiable,
            generation: 8,
            connection: connection(
                localRelay: .unreachable,
                routingSync: .unreachable,
                remoteGateway: .unknown,
                catalog: .unknown
            ),
            recoveryCapabilities: external.recoveryCapabilities
        )
        XCTAssertTrue(try unreachable.validated().canCheckpointSavedOpenCodexRoutingRecovery)
        XCTAssertFalse(try unreachable.validated().canReviewSavedOpenCodexRoutingRecovery)
    }

    func testLocalRelayUnreachableTakesPrecedenceOverRoutingPhase() {
        let pending = status(
			desiredBackend: .none,
			appliedBackend: .external,
            phase: .nativePendingRestart,
            connection: connection(localRelay: .unreachable, routingSync: .acknowledged)
        )
        XCTAssertEqual(pending.presentation, .relayUnavailable)
    }

    func testStatusRejectsUnsupportedSchemaAndLoosePendingState() {
        let unsupported = RoutingStatus(
            schemaVersion: 4,
			desiredBackend: .external,
			appliedBackend: .external,
            phase: .relayActive,
            relayAdmission: .allow,
            catalogRefresh: .run,
            relayRunning: true,
            activeRequests: 0,
            desktopRestartRequired: false,
            desktopEffectiveMode: .unverifiable,
            generation: 1,
            connection: connection()
        )
        XCTAssertThrowsError(try unsupported.validated())

        let loosePending = status(
			desiredBackend: .none,
			appliedBackend: .external,
            phase: .nativePendingRestart,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            desktopRestartRequired: true,
            connection: connection(catalog: .paused)
        )
        XCTAssertThrowsError(try loosePending.validated())
    }

    func testPollingPolicyUsesFastTransitionAndPopoverIntervals() {
        let normal = status()
        XCTAssertEqual(RoutingStatusPolling.intervalSeconds(status: normal, isPopoverVisible: false), 15)
        XCTAssertEqual(RoutingStatusPolling.intervalSeconds(status: normal, isPopoverVisible: true), 2)

		let transitioning = status(desiredBackend: .none, phase: .nativePendingRestart)
        XCTAssertEqual(RoutingStatusPolling.intervalSeconds(status: transitioning, isPopoverVisible: false), 2)

        let unreachable = status(connection: connection(localRelay: .unreachable))
        XCTAssertEqual(RoutingStatusPolling.intervalSeconds(status: unreachable, isPopoverVisible: false), 30)
        XCTAssertEqual(RoutingStatusPolling.intervalSeconds(status: nil, isPopoverVisible: false), 30)
    }

    func testRelayctlErrorsDoNotExposeRawDiagnostics() {
        let rawSentinel = "sensitive stderr from helper"
        let error = RelayctlError.invocationFailed(exitCode: 17)
        XCTAssertEqual(error.safeCode, "relayctl_failed")
        XCTAssertFalse(error.safeMessage.contains(rawSentinel))
        XCTAssertEqual(error.errorDescription, error.safeMessage)
    }

    func testStructuredRelayctlErrorAcceptsOnlyAllowlistedCodes() throws {
        let source = """
        {"schema_version":1,"ok":false,"error":{"code":"routing_recovery_required","message_key":"routing_recovery_required","retryable":true,"recommended_action":"open_recovery"}}
        """.data(using: .utf8)!
        let envelope = try JSONDecoder().decode(RelayctlOperationErrorEnvelope.self, from: source)
        XCTAssertEqual(envelope.reportedCode(), .routingRecoveryRequired)
        let error = RelayctlError.reported(.routingRecoveryRequired)
        XCTAssertEqual(error.safeCode, "routing_recovery_required")
        XCTAssertFalse(error.safeMessage.contains("config.toml"))

        for (code, retryable, action) in [
            ("native_owner_busy", true, "retry_owner_repair"),
            ("native_owner_configuration_invalid", false, "manual_remediation"),
            ("native_owner_restore_failed", true, "refresh_status"),
            ("native_owner_result_invalid", false, "manual_remediation"),
        ] {
            let data = Data("""
            {"schema_version":1,"ok":false,"error":{"code":"\(code)","message_key":"\(code)","retryable":\(retryable),"recommended_action":"\(action)"}}
            """.utf8)
            XCTAssertNotNil(try JSONDecoder().decode(RelayctlOperationErrorEnvelope.self, from: data).reportedCode())
        }

        let unknown = """
        {"schema_version":1,"ok":false,"error":{"code":"raw_sensitive_diagnostic","message_key":"raw_sensitive_diagnostic","retryable":true,"recommended_action":"retry"}}
        """.data(using: .utf8)!
        XCTAssertNil(try JSONDecoder().decode(RelayctlOperationErrorEnvelope.self, from: unknown).reportedCode())

        let spoofedMetadata = """
        {"schema_version":1,"ok":false,"error":{"code":"routing_recovery_required","message_key":"routing_recovery_required","retryable":false,"recommended_action":"open_recovery"}}
        """.data(using: .utf8)!
        XCTAssertNil(try JSONDecoder().decode(RelayctlOperationErrorEnvelope.self, from: spoofedMetadata).reportedCode())

        let unknownField = """
        {"schema_version":1,"ok":false,"unexpected":true,"error":{"code":"operation_failed","message_key":"operation_failed","retryable":true,"recommended_action":"refresh_status"}}
        """.data(using: .utf8)!
        XCTAssertThrowsError(try JSONDecoder().decode(RelayctlOperationErrorEnvelope.self, from: unknownField))
    }

    func testLoginRegistrationCoordinatorUsesOnlySafeResults() {
        let alreadyEnabled = MockLoginRegistration(registrationState: .enabled, stateAfterRegister: .enabled)
        XCTAssertEqual(LoginRegistrationCoordinator.ensureRegistered(alreadyEnabled), .enabled)
        XCTAssertEqual(alreadyEnabled.registerCalls, 0)

        let pending = MockLoginRegistration(registrationState: .pending, stateAfterRegister: .pending)
        XCTAssertEqual(LoginRegistrationCoordinator.ensureRegistered(pending), .pending)
        XCTAssertEqual(pending.registerCalls, 1)

        let failed = MockLoginRegistration(registrationState: .pending, stateAfterRegister: .enabled)
        failed.shouldFail = true
        XCTAssertEqual(LoginRegistrationCoordinator.ensureRegistered(failed), .failed)
        XCTAssertEqual(failed.registerCalls, 1)

        let disabled = MockLoginRegistration(registrationState: .disabled, stateAfterRegister: .enabled)
        XCTAssertEqual(LoginRegistrationCoordinator.unregister(disabled), .disabled)
        XCTAssertEqual(disabled.unregisterCalls, 0)

        let removable = MockLoginRegistration(
            registrationState: .pending,
            stateAfterRegister: .pending,
            stateAfterUnregister: .disabled
        )
        XCTAssertEqual(LoginRegistrationCoordinator.unregister(removable), .disabled)
        XCTAssertEqual(removable.unregisterCalls, 1)

        let failedUnregister = MockLoginRegistration(registrationState: .enabled, stateAfterRegister: .enabled)
        failedUnregister.shouldFailUnregister = true
        XCTAssertEqual(LoginRegistrationCoordinator.unregister(failedUnregister), .failed)
    }

    func testRoutingBindingRequiresCanonicalOwnerOnlyShape() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        let bindingURL = root.appendingPathComponent("routing-binding.json")
        defer { try? FileManager.default.removeItem(at: root) }
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        let payload = #"{"schema":1,"relay_config":"/tmp/relay.json","codex_config":"/tmp/config.toml"}"#
        try payload.data(using: .utf8)!.write(to: bindingURL, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: bindingURL.path)

        let binding = try RoutingBindingReader.load(at: bindingURL)
        XCTAssertEqual(binding.relayctlArguments, ["--config", "/tmp/relay.json", "--codex-config", "/tmp/config.toml"])

        try FileManager.default.setAttributes([.posixPermissions: 0o644], ofItemAtPath: bindingURL.path)
        XCTAssertThrowsError(try RoutingBindingReader.load(at: bindingURL)) { error in
            XCTAssertEqual(error as? RoutingBindingError, .unsafeFile)
        }
    }

    func testCodexConfigurationReaderInspectsAndReadsBoundRegularUTF8File() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        let configURL = root.appendingPathComponent("config.toml", isDirectory: false)
        defer { try? FileManager.default.removeItem(at: root) }
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        try Data("model = \"gpt-5\"\n".utf8).write(to: configURL)
        let bindingURL = try writeRoutingBinding(in: root, codexConfig: configURL)

        let reader = SecureCodexConfigurationReader()
        let metadata = try reader.inspect(bindingURL: bindingURL)
        XCTAssertEqual(metadata.location, configURL.path)
        XCTAssertEqual(metadata.byteCount, 16)
        XCTAssertEqual(try reader.readDocument(bindingURL: bindingURL).contents, "model = \"gpt-5\"\n")
    }

    func testCodexConfigurationReaderRejectsMissingSymlinkAndDirectoryTargets() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        defer { try? FileManager.default.removeItem(at: root) }
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)

        let missingURL = root.appendingPathComponent("missing.toml", isDirectory: false)
        let missingBinding = try writeRoutingBinding(in: root, codexConfig: missingURL)
        XCTAssertThrowsError(try SecureCodexConfigurationReader().inspect(bindingURL: missingBinding)) {
            XCTAssertEqual($0 as? CodexConfigurationFileError, .missing)
        }

        let targetURL = root.appendingPathComponent("target.toml", isDirectory: false)
        let symlinkURL = root.appendingPathComponent("linked.toml", isDirectory: false)
        try Data("native = true\n".utf8).write(to: targetURL)
        try FileManager.default.createSymbolicLink(at: symlinkURL, withDestinationURL: targetURL)
        let symlinkBinding = try writeRoutingBinding(in: root, codexConfig: symlinkURL)
        XCTAssertThrowsError(try SecureCodexConfigurationReader().inspect(bindingURL: symlinkBinding)) {
            XCTAssertEqual($0 as? CodexConfigurationFileError, .unsafeFile)
        }

        let directoryURL = root.appendingPathComponent("config-directory", isDirectory: true)
        try FileManager.default.createDirectory(at: directoryURL, withIntermediateDirectories: false)
        let directoryBinding = try writeRoutingBinding(in: root, codexConfig: directoryURL)
        XCTAssertThrowsError(try SecureCodexConfigurationReader().inspect(bindingURL: directoryBinding)) {
            XCTAssertEqual($0 as? CodexConfigurationFileError, .unsafeFile)
        }
    }

    func testCodexConfigurationPreviewBoundsSizeAndUTF8WithoutBlockingMetadata() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        let configURL = root.appendingPathComponent("config.toml", isDirectory: false)
        defer { try? FileManager.default.removeItem(at: root) }
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        let bindingURL = try writeRoutingBinding(in: root, codexConfig: configURL)
        let reader = SecureCodexConfigurationReader()

        try Data(repeating: 0x61, count: Int(SecureCodexConfigurationReader.maximumPreviewBytes + 1))
            .write(to: configURL)
        XCTAssertEqual(
            try reader.inspect(bindingURL: bindingURL).byteCount,
            SecureCodexConfigurationReader.maximumPreviewBytes + 1
        )
        XCTAssertThrowsError(try reader.readDocument(bindingURL: bindingURL)) {
            XCTAssertEqual($0 as? CodexConfigurationFileError, .previewTooLarge)
        }

        try Data([0xFF, 0xFE, 0xFD]).write(to: configURL)
        XCTAssertEqual(try reader.inspect(bindingURL: bindingURL).byteCount, 3)
        XCTAssertThrowsError(try reader.readDocument(bindingURL: bindingURL)) {
            XCTAssertEqual($0 as? CodexConfigurationFileError, .previewNotUTF8)
        }
    }

    func testCodexConfigurationReaderRejectsAtomicReplacementAfterOpen() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        let configURL = root.appendingPathComponent("config.toml", isDirectory: false)
        let replacementURL = root.appendingPathComponent("replacement.toml", isDirectory: false)
        defer { try? FileManager.default.removeItem(at: root) }
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        try Data("first = true\n".utf8).write(to: configURL)
        try Data("second = true\n".utf8).write(to: replacementURL)
        let bindingURL = try writeRoutingBinding(in: root, codexConfig: configURL)
        let originalPath = configURL.path
        let replacementPath = replacementURL.path
        let reader = SecureCodexConfigurationReader {
            _ = Darwin.rename(replacementPath, originalPath)
        }

        XCTAssertThrowsError(try reader.readDocument(bindingURL: bindingURL)) {
            XCTAssertEqual($0 as? CodexConfigurationFileError, .changedDuringRead)
        }
    }

    func testHelperOverrideMustBeAbsolute() {
        let bundle = Bundle(for: Self.self)
        let relative = RelayctlHelperLocation.resolve(bundle: bundle, environment: ["OPENCODEX_RELAYCTL_PATH": "fake-relayctl"])
        XCTAssertTrue(relative.path.hasSuffix("Contents/Library/Helpers/opencodex-relayctl"))
        let absolute = RelayctlHelperLocation.resolve(bundle: bundle, environment: ["OPENCODEX_RELAYCTL_PATH": "/tmp/relayctl"])
        XCTAssertEqual(absolute.path, "/tmp/relayctl")
    }

    func testSelectedDesktopTargetRequiresAnExactExistingApplicationBundle() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        let application = root.appendingPathComponent("ChosenCodex.app", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: root) }
        try FileManager.default.createDirectory(at: application, withIntermediateDirectories: true)

        let resolved = try DesktopTargetResolver.validate(application)
        let target = DesktopTarget(url: resolved)
        XCTAssertEqual(try DesktopTargetResolver.resolve(target), resolved)
        XCTAssertThrowsError(try DesktopTargetResolver.validate(root))
    }

    func testLocalRequestAndUninstallRequireAcknowledgedRelayState() throws {
        let unacknowledged = status(
            relayRunning: false,
            activeRequests: nil,
            connection: connection(
                localRelay: .unreachable,
                localOpenCodex: .ready,
                routingSync: .unreachable,
                remoteGateway: .unknown,
                catalog: .unknown
            )
        )
        XCTAssertFalse(try unacknowledged.validated().canRequestLocalOpenCodex)
        XCTAssertFalse(unacknowledged.canUninstallOpenCodex)

        let stableNative = status(
            desiredBackend: .none,
            appliedBackend: .none,
            phase: .nativeActive,
            relayAdmission: .deny,
            catalogRefresh: .pause,
            connection: connection(remoteGateway: .notApplicable, catalog: .paused)
        )
        XCTAssertTrue(try stableNative.validated().canUninstallOpenCodex)
    }

    func testModeBackendAndRelayRuntimeContradictionsAreRejected() {
        let mismatchedModes = status(desiredMode: .native, appliedMode: .relay)
        XCTAssertThrowsError(try mismatchedModes.validated())

        let impossibleRuntime = status(relayRunning: false)
        XCTAssertThrowsError(try impossibleRuntime.validated())
    }

    func testOpenCodexRemovalArgumentsAreTypedAndConfirmationExact() throws {
        let selection = try OpenCodexRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64)
        )
        XCTAssertEqual(
            selection.inventoryArguments(relayConfig: "/tmp/relay.json"),
            [
                "mode", "inspect-open-codex-data",
                "--installation-id", "0123456789abcdef01234567",
                "--installation-fingerprint", String(repeating: "a", count: 64),
                "--config", "/tmp/relay.json", "--json",
            ]
        )

        let preserve = try OpenCodexRemovalRequest(
            selection: selection,
            mode: .preserveData,
            dataItemIDs: [],
            expectedRoutingGeneration: UInt64.max,
            confirmsRemoval: true,
            confirmsTrash: false,
            confirmsDesktopExited: true
        )
        XCTAssertEqual(
            preserve.removalArguments(relayConfig: "/tmp/relay.json", codexConfig: "/tmp/config.toml"),
            [
                "mode", "remove-open-codex",
                "--installation-id", "0123456789abcdef01234567",
                "--installation-fingerprint", String(repeating: "a", count: 64),
                "--removal-mode", "preserve_data",
                "--expected-routing-generation", String(UInt64.max),
                "--confirm-opencodex-removal", "--confirm-desktop-exited",
                "--config", "/tmp/relay.json", "--codex-config", "/tmp/config.toml", "--json",
            ]
        )

        let first = "ocx-data-v1:" + String(repeating: "1", count: 32)
        let second = "ocx-data-v1:" + String(repeating: "2", count: 32)
        let trash = try OpenCodexRemovalRequest(
            selection: selection,
            mode: .trashSelected,
            dataItemIDs: [first, second],
            expectedRoutingGeneration: 7,
            expectedInventoryRevision: String(repeating: "d", count: 64),
            confirmsRemoval: true,
            confirmsTrash: true,
            confirmsInterruptedDataRefresh: true,
            confirmsDesktopExited: true
        )
        XCTAssertEqual(
            trash.removalArguments(relayConfig: "/tmp/relay.json", codexConfig: "/tmp/config.toml"),
            [
                "mode", "remove-open-codex",
                "--installation-id", "0123456789abcdef01234567",
                "--installation-fingerprint", String(repeating: "a", count: 64),
                "--removal-mode", "trash_selected", "--expected-routing-generation", "7",
                "--data-item", first, "--data-item", second,
                "--confirm-opencodex-removal", "--confirm-data-trash",
                "--expected-inventory-revision", String(repeating: "d", count: 64),
                "--confirm-interrupted-data-refresh", "--confirm-desktop-exited",
                "--config", "/tmp/relay.json", "--codex-config", "/tmp/config.toml", "--json",
            ]
        )

        XCTAssertThrowsError(try OpenCodexRemovalRequest(
            selection: selection,
            mode: .trashSelected,
            dataItemIDs: [first, first],
            expectedRoutingGeneration: 7,
            confirmsRemoval: true,
            confirmsTrash: true,
            confirmsDesktopExited: true
        ))
    }

    func testOpenCodexInventoryAndRemovalReceiptsFailClosed() throws {
        let selection = try OpenCodexRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64)
        )
        let itemID = "ocx-data-v1:" + String(repeating: "b", count: 32)
        let validInventoryJSON = """
        {"schema_version":2,"operation":"open-codex-data-inventory","status":"verified","installation_id":"0123456789abcdef01234567","installation_fingerprint":"\(String(repeating: "a", count: 64))","inventory_revision":"\(String(repeating: "d", count: 64))","routing_generation":9,"items":[{"id":"\(itemID)","category":"root","scope":"config-root","kind":"directory","relative_path":".","exists":true,"sensitive":false,"trashable":false}]}
        """.data(using: .utf8)!
        let inventory = try JSONDecoder()
            .decode(OpenCodexDataInventoryReceipt.self, from: validInventoryJSON)
            .validated(for: selection)
        XCTAssertEqual(inventory.items.count, 1)

        let unknownKey = String(data: validInventoryJSON, encoding: .utf8)!
            .replacingOccurrences(of: "\"items\":", with: "\"unexpected\":true,\"items\":")
            .data(using: .utf8)!
        XCTAssertThrowsError(try JSONDecoder().decode(OpenCodexDataInventoryReceipt.self, from: unknownKey))

        let unsafeRoot = String(data: validInventoryJSON, encoding: .utf8)!
            .replacingOccurrences(of: "\"trashable\":false", with: "\"trashable\":true")
            .data(using: .utf8)!
        XCTAssertThrowsError(
            try JSONDecoder().decode(OpenCodexDataInventoryReceipt.self, from: unsafeRoot).validated(for: selection)
        )

        let request = try OpenCodexRemovalRequest(
            selection: selection,
            mode: .preserveData,
            dataItemIDs: [],
            expectedRoutingGeneration: 9,
            confirmsRemoval: true,
            confirmsTrash: false,
            confirmsDesktopExited: true
        )
        func decodeReceipt(_ json: String) throws -> OpenCodexRemovalReceipt {
            try JSONDecoder()
                .decode(OpenCodexRemovalReceipt.self, from: Data(json.utf8))
                .validated(for: request)
        }

        let successJSON = """
        {"schema_version":2,"operation":"remove-open-codex","status":"completed","mode":"preserve_data","installation_id":"0123456789abcdef01234567","data_scope":"preserved","selected_data_items":0,"moved_data_items":0,"package_removed":true,"data_movement_unknown":false,"routing_recovery_required":false,"permanent_delete_fallback":false,"stages":[
        {"stage":"candidate_revalidation","status":"completed","code":"candidate_verified","subject_id":"0123456789abcdef01234567"},
        {"stage":"cleanup_journal","status":"completed","code":"cleanup_intent_persisted","subject_id":"0123456789abcdef01234567"},
        {"stage":"routing_pre_teardown","status":"completed","code":"routing_ownership_verified"},
        {"stage":"teardown","status":"completed","code":"teardown_completed","subject_id":"0123456789abcdef01234567"},
        {"stage":"routing_verification","status":"completed","code":"routing_ownership_reverified"},
        {"stage":"data_trash","status":"skipped","code":"data_preserved"},
        {"stage":"routing_reverification","status":"completed","code":"routing_ownership_reverified"},
        {"stage":"cleanup_journal","status":"completed","code":"cleanup_journal_persisted","subject_id":"0123456789abcdef01234567"},
        {"stage":"cleanup_journal","status":"completed","code":"package_execution_in_flight","subject_id":"0123456789abcdef01234567"},
        {"stage":"npm_uninstall","status":"completed","code":"npm_uninstall_completed","subject_id":"0123456789abcdef01234567"},
        {"stage":"cleanup_journal","status":"completed","code":"package_cleanup_verified","subject_id":"0123456789abcdef01234567"},
        {"stage":"routing_post_verification","status":"completed","code":"routing_ownership_reverified"},
        {"stage":"routing_final_verification","status":"completed","code":"routing_ownership_reverified"},
        {"stage":"package_verification","status":"completed","code":"package_absent","subject_id":"0123456789abcdef01234567"},
        {"stage":"relay_cleanup","status":"completed","code":"relay_cleanup_completed"}]}
        """
        let receipt = try decodeReceipt(successJSON)
        XCTAssertTrue(receipt.isSuccessful)

        let resumeJSON = """
        {"schema_version":2,"operation":"remove-open-codex","status":"completed","mode":"preserve_data","installation_id":"0123456789abcdef01234567","data_scope":"preserved","selected_data_items":0,"moved_data_items":0,"package_removed":true,"data_movement_unknown":false,"routing_recovery_required":false,"permanent_delete_fallback":false,"stages":[
        {"stage":"cleanup_journal","status":"completed","code":"cleanup_resume","subject_id":"0123456789abcdef01234567"},
        {"stage":"routing_verification","status":"completed","code":"routing_ownership_verified"},
        {"stage":"candidate_revalidation","status":"completed","code":"candidate_verified","subject_id":"0123456789abcdef01234567"},
        {"stage":"routing_reverification","status":"completed","code":"routing_ownership_reverified"},
        {"stage":"cleanup_journal","status":"completed","code":"package_execution_in_flight","subject_id":"0123456789abcdef01234567"},
        {"stage":"npm_uninstall","status":"completed","code":"npm_uninstall_completed","subject_id":"0123456789abcdef01234567"},
        {"stage":"cleanup_journal","status":"completed","code":"package_cleanup_verified","subject_id":"0123456789abcdef01234567"},
        {"stage":"routing_post_verification","status":"completed","code":"routing_ownership_reverified"},
        {"stage":"routing_final_verification","status":"completed","code":"routing_ownership_reverified"},
        {"stage":"package_verification","status":"completed","code":"package_absent","subject_id":"0123456789abcdef01234567"},
        {"stage":"relay_cleanup","status":"completed","code":"relay_cleanup_completed"}]}
        """
        XCTAssertTrue(try decodeReceipt(resumeJSON).isSuccessful)

        let reconciledJSON = """
        {"schema_version":2,"operation":"remove-open-codex","status":"completed","mode":"preserve_data","installation_id":"0123456789abcdef01234567","data_scope":"preserved","selected_data_items":0,"moved_data_items":0,"package_removed":true,"data_movement_unknown":false,"routing_recovery_required":false,"permanent_delete_fallback":false,"stages":[
        {"stage":"cleanup_journal","status":"completed","code":"cleanup_resume","subject_id":"0123456789abcdef01234567"},
        {"stage":"package_verification","status":"completed","code":"package_absent","subject_id":"0123456789abcdef01234567"},
        {"stage":"routing_final_verification","status":"completed","code":"routing_ownership_reverified"},
        {"stage":"relay_cleanup","status":"completed","code":"relay_cleanup_completed"}]}
        """
        XCTAssertTrue(try decodeReceipt(reconciledJSON).isSuccessful)

        let cleanupFailureJSON = successJSON
            .replacingOccurrences(
                of: "\"status\":\"completed\",\"mode\"",
                with: "\"status\":\"partial\",\"mode\""
            )
            .replacingOccurrences(
                of: "{\"stage\":\"relay_cleanup\",\"status\":\"completed\",\"code\":\"relay_cleanup_completed\"}",
                with: "{\"stage\":\"relay_cleanup\",\"status\":\"failed\",\"code\":\"enrollment_cleanup_failed\"}"
            )
        XCTAssertFalse(try decodeReceipt(cleanupFailureJSON).isSuccessful)

        let routingRecoveryJSON = """
        {"schema_version":2,"operation":"remove-open-codex","status":"partial","mode":"preserve_data","installation_id":"0123456789abcdef01234567","data_scope":"preserved","selected_data_items":0,"moved_data_items":0,"package_removed":true,"data_movement_unknown":false,"routing_recovery_required":true,"permanent_delete_fallback":false,"stages":[
        {"stage":"cleanup_journal","status":"completed","code":"cleanup_resume","subject_id":"0123456789abcdef01234567"},
        {"stage":"routing_post_verification","status":"failed","code":"routing_ownership_changed"},
        {"stage":"routing_recovery","status":"completed","code":"routing_recovery_persisted"},
        {"stage":"routing_final_verification","status":"completed","code":"routing_ownership_reverified"},
        {"stage":"package_verification","status":"completed","code":"package_absent","subject_id":"0123456789abcdef01234567"}]}
        """
        let routingRecoveryReceipt = try decodeReceipt(routingRecoveryJSON)
        XCTAssertTrue(routingRecoveryReceipt.packageRemoved)
        XCTAssertTrue(routingRecoveryReceipt.routingRecoveryRequired)
        XCTAssertFalse(routingRecoveryReceipt.isSuccessful)

        let preStartRoutingRefusalJSON = """
        {"schema_version":2,"operation":"remove-open-codex","status":"partial","mode":"preserve_data","installation_id":"0123456789abcdef01234567","data_scope":"preserved","selected_data_items":0,"moved_data_items":0,"package_removed":false,"data_movement_unknown":false,"routing_recovery_required":true,"permanent_delete_fallback":false,"stages":[
        {"stage":"cleanup_journal","status":"completed","code":"cleanup_resume","subject_id":"0123456789abcdef01234567"},
        {"stage":"teardown","status":"refused","code":"routing_ownership_changed","subject_id":"0123456789abcdef01234567"},
        {"stage":"routing_recovery","status":"completed","code":"routing_recovery_persisted"}]}
        """
        let preStartRoutingRefusal = try decodeReceipt(preStartRoutingRefusalJSON)
        XCTAssertTrue(preStartRoutingRefusal.routingRecoveryRequired)
        XCTAssertFalse(preStartRoutingRefusal.requiresWholeMacReboot)

        for code in ["candidate_changed", "manual_removal_required"] {
            let teardownRefusalJSON = """
            {"schema_version":2,"operation":"remove-open-codex","status":"partial","mode":"preserve_data","installation_id":"0123456789abcdef01234567","data_scope":"preserved","selected_data_items":0,"moved_data_items":0,"package_removed":false,"data_movement_unknown":false,"routing_recovery_required":false,"permanent_delete_fallback":false,"stages":[
            {"stage":"teardown","status":"refused","code":"\(code)","subject_id":"0123456789abcdef01234567"}]}
            """
            XCTAssertFalse(try decodeReceipt(teardownRefusalJSON).isSuccessful)
        }
        let unknownTeardownRefusalJSON = """
        {"schema_version":2,"operation":"remove-open-codex","status":"partial","mode":"preserve_data","installation_id":"0123456789abcdef01234567","data_scope":"preserved","selected_data_items":0,"moved_data_items":0,"package_removed":false,"data_movement_unknown":false,"routing_recovery_required":false,"permanent_delete_fallback":false,"stages":[
        {"stage":"teardown","status":"refused","code":"unknown_teardown_refusal","subject_id":"0123456789abcdef01234567"}]}
        """
        XCTAssertThrowsError(try decodeReceipt(unknownTeardownRefusalJSON))

        let incomplete = OpenCodexRemovalReceipt(
            status: .completed,
            mode: .preserveData,
            installationID: selection.installationID,
            dataScope: "preserved",
            selectedDataItems: 0,
            movedDataItems: 0,
            packageRemoved: true,
            dataMovementUnknown: false,
            routingRecoveryRequired: false,
            permanentDeleteFallback: false,
            stages: [
                OpenCodexRemovalStage(
                    stage: .cleanupJournal,
                    status: .completed,
                    code: "cleanup_resume",
                    subjectID: selection.installationID
                ),
                OpenCodexRemovalStage(
                    stage: .packageVerification,
                    status: .completed,
                    code: "package_absent",
                    subjectID: selection.installationID
                ),
            ]
        )
        XCTAssertFalse(incomplete.isSuccessful)
        XCTAssertThrowsError(try incomplete.validated(for: request))

        let mismatchedStageCode = successJSON.replacingOccurrences(
            of: "{\"stage\":\"relay_cleanup\",\"status\":\"completed\",\"code\":\"relay_cleanup_completed\"}",
            with: "{\"stage\":\"relay_cleanup\",\"status\":\"completed\",\"code\":\"package_absent\"}"
        )
        XCTAssertThrowsError(try decodeReceipt(mismatchedStageCode))

        let duplicateTerminalStage = OpenCodexRemovalReceipt(
            status: receipt.status,
            mode: receipt.mode,
            installationID: receipt.installationID,
            dataScope: receipt.dataScope,
            selectedDataItems: receipt.selectedDataItems,
            movedDataItems: receipt.movedDataItems,
            packageRemoved: receipt.packageRemoved,
            dataMovementUnknown: receipt.dataMovementUnknown,
            routingRecoveryRequired: receipt.routingRecoveryRequired,
            permanentDeleteFallback: receipt.permanentDeleteFallback,
            stages: receipt.stages + [receipt.stages.last!]
        )
        XCTAssertThrowsError(try duplicateTerminalStage.validated(for: request))

        let permanentFallback = OpenCodexRemovalReceipt(
            status: .completed,
            mode: .preserveData,
            installationID: selection.installationID,
            dataScope: "preserved",
            selectedDataItems: 0,
            movedDataItems: 0,
            packageRemoved: true,
            dataMovementUnknown: false,
            routingRecoveryRequired: false,
            permanentDeleteFallback: true,
            stages: receipt.stages
        )
        XCTAssertThrowsError(try permanentFallback.validated(for: request))
    }

    func testUnsupportedTrashReceiptIsCompatibleAndRequiresFreshSelection() throws {
        let selection = try OpenCodexRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64)
        )
        let itemID = "ocx-data-v1:" + String(repeating: "b", count: 32)
        let request = try OpenCodexRemovalRequest(
            selection: selection,
            mode: .trashSelected,
            dataItemIDs: [itemID],
            expectedRoutingGeneration: 7,
            expectedInventoryRevision: String(repeating: "d", count: 64),
            confirmsRemoval: true,
            confirmsTrash: true,
            confirmsDesktopExited: true
        )
        let receipt = OpenCodexRemovalReceipt(
            status: .partial,
            mode: .trashSelected,
            installationID: selection.installationID,
            dataScope: "explicit_items_only",
            selectedDataItems: 1,
            movedDataItems: 0,
            packageRemoved: false,
            dataMovementUnknown: false,
            routingRecoveryRequired: false,
            permanentDeleteFallback: false,
            stages: [
                OpenCodexRemovalStage(
                    stage: .dataTrash,
                    status: .refused,
                    code: "trash_unsupported",
                    subjectID: itemID
                ),
            ]
        )

        let decoded = try JSONDecoder()
            .decode(OpenCodexRemovalReceipt.self, from: JSONEncoder().encode(receipt))
            .validated(for: request)
        XCTAssertFalse(decoded.isSuccessful)
        XCTAssertTrue(decoded.requiresDataSelectionRefresh)
        XCTAssertFalse(decoded.requiresWholeMacReboot)

        let normalized = OpenCodexRemovalReceipt(
            status: .partial,
            mode: .trashSelected,
            installationID: selection.installationID,
            dataScope: "explicit_items_only",
            selectedDataItems: 1,
            movedDataItems: 0,
            packageRemoved: false,
            dataMovementUnknown: true,
            routingRecoveryRequired: false,
            permanentDeleteFallback: false,
            stages: [
                OpenCodexRemovalStage(
                    stage: .dataTrash,
                    status: .refused,
                    code: "data_selection_refresh_required"
                ),
            ]
        )
        let validatedNormalized = try normalized.validated(for: request)
        XCTAssertTrue(validatedNormalized.requiresDataSelectionRefresh)
        XCTAssertFalse(validatedNormalized.requiresWholeMacReboot)

        let unknownCode = OpenCodexRemovalReceipt(
            status: .partial,
            mode: .trashSelected,
            installationID: selection.installationID,
            dataScope: "explicit_items_only",
            selectedDataItems: 1,
            movedDataItems: 0,
            packageRemoved: false,
            dataMovementUnknown: true,
            routingRecoveryRequired: false,
            permanentDeleteFallback: false,
            stages: [
                OpenCodexRemovalStage(
                    stage: .dataTrash,
                    status: .refused,
                    code: "unknown_refresh_code"
                ),
            ]
        )
        XCTAssertThrowsError(try unknownCode.validated(for: request))
    }

    func testVerifiedPreMutationRemovalFailureRequiresExactAllowlistedSequence() throws {
        let selection = try OpenCodexRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64)
        )
        let request = try OpenCodexRemovalRequest(
            selection: selection,
            mode: .preserveData,
            dataItemIDs: [],
            expectedRoutingGeneration: 9,
            confirmsRemoval: true,
            confirmsTrash: false,
            confirmsDesktopExited: true
        )
        let verified = OpenCodexRemovalStage(
            stage: .candidateRevalidation,
            status: .completed,
            code: "candidate_verified",
            subjectID: selection.installationID
        )
        let preflight = OpenCodexRemovalStage(
            stage: .teardownPreflight,
            status: .completed,
            code: "teardown_preflight_verified",
            subjectID: selection.installationID
        )
        func receipt(_ stages: [OpenCodexRemovalStage], status: OpenCodexRemovalStatus = .failed) -> OpenCodexRemovalReceipt {
            OpenCodexRemovalReceipt(
                status: status,
                mode: .preserveData,
                installationID: selection.installationID,
                dataScope: "preserved",
                selectedDataItems: 0,
                movedDataItems: 0,
                packageRemoved: false,
                dataMovementUnknown: false,
                routingRecoveryRequired: false,
                permanentDeleteFallback: false,
                stages: stages
            )
        }

        let allowed: [([OpenCodexRemovalStage], String)] = [
            ([OpenCodexRemovalStage(stage: .requestValidation, status: .failed, code: "coordinator_unavailable")], "coordinator_unavailable"),
            ([OpenCodexRemovalStage(stage: .candidateRevalidation, status: .refused, code: "candidate_not_found", subjectID: selection.installationID)], "candidate_not_found"),
            ([verified, OpenCodexRemovalStage(stage: .dataPolicy, status: .refused, code: "teardown_unsupported", subjectID: selection.installationID)], "teardown_unsupported"),
            ([verified, OpenCodexRemovalStage(stage: .candidateRevalidation, status: .refused, code: "candidate_changed", subjectID: selection.installationID)], "candidate_changed"),
            ([verified, OpenCodexRemovalStage(stage: .teardownPreflight, status: .failed, code: "teardown_preflight_failed", subjectID: selection.installationID)], "teardown_preflight_failed"),
            ([verified, preflight, OpenCodexRemovalStage(stage: .candidateRevalidation, status: .refused, code: "teardown_candidate_changed", subjectID: selection.installationID)], "teardown_candidate_changed"),
        ]
        for (stages, expected) in allowed {
            let checked = try receipt(stages).validated(for: request)
            XCTAssertEqual(checked.verifiedPreMutationFailureCode, expected)
        }

        for stages in [
            [OpenCodexRemovalStage(stage: .cleanupJournal, status: .refused, code: "removal_in_flight")],
            [verified, preflight, OpenCodexRemovalStage(stage: .cleanupJournal, status: .failed, code: "cleanup_intent_unavailable", subjectID: selection.installationID)],
            [OpenCodexRemovalStage(stage: .npmUninstall, status: .failed, code: "process_cleanup_unverified", subjectID: selection.installationID)],
        ] {
            let checked = try receipt(stages).validated(for: request)
            XCTAssertNil(checked.verifiedPreMutationFailureCode)
        }
        XCTAssertNil(receipt(allowed[0].0, status: .partial).verifiedPreMutationFailureCode)
    }

    func testOpenCodexRecoverySessionIsVersionedAndPathFree() throws {
        let selection = try OpenCodexRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64)
        )
        let itemID = "ocx-data-v1:" + String(repeating: "c", count: 32)
        let session = try OpenCodexRemovalRecoverySession(
            selection: selection,
            mode: .trashSelected,
            orderedDataItemIDs: [itemID],
            retiredDataItemIDs: [],
            recoveryKind: .rebootRequired,
            lastCode: "process_cleanup_unverified",
            inventoryRevision: String(repeating: "d", count: 64)
        )
        let encoded = try JSONEncoder().encode(session)
        let text = String(decoding: encoded, as: UTF8.self)
        XCTAssertFalse(text.contains("routing_generation"))
        XCTAssertFalse(text.contains("relative_path"))
        XCTAssertFalse(text.contains("package_root"))
        XCTAssertEqual(try JSONDecoder().decode(OpenCodexRemovalRecoverySession.self, from: encoded), session)
    }

    func testRoutingRecoverySessionRequiresAndRoundTripsDurableGeneration() throws {
        let selection = try OpenCodexRemovalSelection(
            installationID: "0123456789abcdef01234567",
            installationFingerprint: String(repeating: "a", count: 64)
        )
        XCTAssertThrowsError(try OpenCodexRemovalRecoverySession(
            selection: selection,
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .routingRecoveryRequired,
            lastCode: "routing_recovery_required"
        ))

        let session = try OpenCodexRemovalRecoverySession(
            selection: selection,
            mode: .preserveData,
            orderedDataItemIDs: [],
            retiredDataItemIDs: [],
            recoveryKind: .routingRecoveryRequired,
            lastCode: "routing_recovery_required",
            expectedRoutingGeneration: 7
        )
        let encoded = try JSONEncoder().encode(session)
        XCTAssertEqual(
            try JSONDecoder().decode(OpenCodexRemovalRecoverySession.self, from: encoded),
            session
        )
        XCTAssertEqual(session.expectedRoutingGeneration, 7)
    }

    func testAppInformationReaderReportsBoundedBundledHelperVersions() async throws {
        let bundleURL = FileManager.default.temporaryDirectory
            .appendingPathComponent("OpenCodexRelay-AppInformation-\(UUID().uuidString).app")
        defer { try? FileManager.default.removeItem(at: bundleURL) }

        let helperDirectory = bundleURL
            .appendingPathComponent("Contents/Library/Helpers", isDirectory: true)
        try FileManager.default.createDirectory(
            at: helperDirectory,
            withIntermediateDirectories: true
        )
        for kind in BundledRelayComponentKind.allCases {
            let executableURL = helperDirectory.appendingPathComponent(kind.executableName)
            try "#!/bin/sh\nprintf '1.2.3-test.4\\n'\n".write(
                to: executableURL,
                atomically: true,
                encoding: .utf8
            )
            try FileManager.default.setAttributes(
                [.posixPermissions: 0o755],
                ofItemAtPath: executableURL.path
            )
        }

        let snapshot = await AppInformationReader(
            displayName: "PW OpenCodex Relay Dev",
            version: "1.2.3-test.4",
            build: "42",
            bundleIdentifier: DistributionFlavor.developmentBundleIdentifier,
            distributionFlavor: .localDevelopment,
            minimumSystemVersion: "26.0",
            bundleURL: bundleURL
        ).load()

        XCTAssertEqual(snapshot.displayName, "PW OpenCodex Relay Dev")
        XCTAssertEqual(snapshot.version, "1.2.3-test.4")
        XCTAssertEqual(snapshot.build, "42")
        XCTAssertEqual(snapshot.distributionFlavor, .localDevelopment)
        XCTAssertEqual(snapshot.minimumSystemVersion, "26.0")
        XCTAssertEqual(snapshot.components.map(\.kind), [.relay, .relayctl])
        XCTAssertTrue(snapshot.components.allSatisfy { $0.availability == .available })
        XCTAssertTrue(snapshot.components.allSatisfy { $0.version == "1.2.3-test.4" })
        XCTAssertTrue(snapshot.components.allSatisfy { $0.architecture == snapshot.architecture })
    }

    func testAppInformationReaderBoundsInvalidAndMissingHelpers() async throws {
        let bundleURL = FileManager.default.temporaryDirectory
            .appendingPathComponent("OpenCodexRelay-AppInformation-\(UUID().uuidString).app")
        defer { try? FileManager.default.removeItem(at: bundleURL) }

        let helperDirectory = bundleURL
            .appendingPathComponent("Contents/Library/Helpers", isDirectory: true)
        try FileManager.default.createDirectory(
            at: helperDirectory,
            withIntermediateDirectories: true
        )
        let relayURL = helperDirectory.appendingPathComponent(
            BundledRelayComponentKind.relay.executableName
        )
        try "#!/bin/sh\nprintf 'unexpected helper output\\n'\n".write(
            to: relayURL,
            atomically: true,
            encoding: .utf8
        )
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o755],
            ofItemAtPath: relayURL.path
        )

        let reader = AppInformationReader(
            displayName: "PW OpenCodex Relay",
            version: "1.2.3",
            build: "1",
            bundleIdentifier: "io.github.novelkr.opencodex-relay",
            distributionFlavor: .production,
            minimumSystemVersion: "26.0",
            bundleURL: bundleURL
        )
        XCTAssertTrue(reader.loadingSnapshot.components.allSatisfy {
            $0.availability == .loading
        })

        let snapshot = await reader.load()
        XCTAssertEqual(snapshot.components.first(where: { $0.kind == .relay })?.availability, .unverified)
        XCTAssertNil(snapshot.components.first(where: { $0.kind == .relay })?.version)
        XCTAssertEqual(snapshot.components.first(where: { $0.kind == .relayctl })?.availability, .missing)
        XCTAssertNil(snapshot.components.first(where: { $0.kind == .relayctl })?.version)
    }

    func testLocalDevelopmentFlavorUsesAnIsolatedBindingAndWarningFlag() {
        let development = DistributionFlavor.from(
            bundleIdentifier: DistributionFlavor.developmentBundleIdentifier,
            declaredFlavor: "local_development"
        )
        XCTAssertEqual(development, .localDevelopment)
        XCTAssertEqual(development.routingBindingRelativePath, "Library/Application Support/OpenCodexRelayDev/routing-binding.json")
        XCTAssertTrue(development.isLocalDevelopment)

        let production = DistributionFlavor.from(bundleIdentifier: "io.github.novelkr.opencodex-relay", declaredFlavor: "production")
        XCTAssertEqual(production, .production)
        XCTAssertFalse(production.isLocalDevelopment)
    }

    func testRuntimeModeKeepsLegacyBundlesManagedAndUnknownValuesFailClosed() {
        XCTAssertEqual(RelayRuntimeMode.from(declaredMode: nil), .managed)
        XCTAssertEqual(RelayRuntimeMode.from(declaredMode: "managed"), .managed)
        XCTAssertEqual(RelayRuntimeMode.from(declaredMode: "preview"), .preview)
        XCTAssertEqual(RelayRuntimeMode.from(declaredMode: "future-mode"), .preview)
    }

    func testIntegrationInspectorDistinguishesEverySafeAvailability() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("OpenCodexRelay-Integration-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }

        let missingBinding = root.appendingPathComponent("missing.json")
        let missingHelper = root.appendingPathComponent("missing-helper")
        XCTAssertEqual(
            RelayIntegrationInspector.inspect(
                runtimeMode: .preview,
                bindingURL: missingBinding,
                helperURL: missingHelper
            ),
            .preview
        )
        XCTAssertEqual(
            RelayIntegrationInspector.inspect(
                runtimeMode: .managed,
                bindingURL: missingBinding,
                helperURL: missingHelper
            ),
            .missing
        )

        let codexConfig = root.appendingPathComponent("config.toml")
        let bindingURL = try writeRoutingBinding(in: root, codexConfig: codexConfig)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o644],
            ofItemAtPath: bindingURL.path
        )
        XCTAssertEqual(
            RelayIntegrationInspector.inspect(
                runtimeMode: .managed,
                bindingURL: bindingURL,
                helperURL: missingHelper
            ),
            .unsafe
        )

        try Data("{}".utf8).write(to: bindingURL)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o600],
            ofItemAtPath: bindingURL.path
        )
        XCTAssertEqual(
            RelayIntegrationInspector.inspect(
                runtimeMode: .managed,
                bindingURL: bindingURL,
                helperURL: missingHelper
            ),
            .invalid
        )

        _ = try writeRoutingBinding(in: root, codexConfig: codexConfig)
        XCTAssertEqual(
            RelayIntegrationInspector.inspect(
                runtimeMode: .managed,
                bindingURL: bindingURL,
                helperURL: missingHelper
            ),
            .helperUnavailable
        )

        let helper = root.appendingPathComponent("relayctl")
        try Data("#!/bin/sh\n".utf8).write(to: helper)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o755],
            ofItemAtPath: helper.path
        )
        XCTAssertEqual(
            RelayIntegrationInspector.inspect(
                runtimeMode: .managed,
                bindingURL: bindingURL,
                helperURL: helper
            ),
            .ready
        )
    }

    func testPreviewConfigReaderFailsBeforeBindingLookup() {
        let reader = SecureCodexConfigurationReader(runtimeMode: .preview)
        let missingBinding = FileManager.default.temporaryDirectory
            .appendingPathComponent("missing-binding-\(UUID().uuidString).json")

        XCTAssertThrowsError(try reader.inspect(bindingURL: missingBinding)) { error in
            XCTAssertEqual(error as? CodexConfigurationFileError, .previewMode)
        }
        XCTAssertThrowsError(try reader.readDocument(bindingURL: missingBinding)) { error in
            XCTAssertEqual(error as? CodexConfigurationFileError, .previewMode)
        }
    }
}
