import Foundation
import XCTest
@testable import OpenCodexRelayCore

final class ContainerRuntimeTests: XCTestCase {
    private let hexA = String(repeating: "a", count: 64)
    private let hexB = String(repeating: "b", count: 64)
    private let hexC = String(repeating: "c", count: 64)

    func testStrictInspectionAcceptsBoundedExactContract() throws {
        let result = try ContainerRuntimeInspection.decodeStrict(inspectionData())
        XCTAssertEqual(result.state, .healthy)
        XCTAssertEqual(result.active?.artifactVersion, "2.40.0-r1")
        XCTAssertEqual(result.active?.indexDigest, "sha256:\(hexB)")
        XCTAssertFalse(result.recoveryRequired)
    }

    func testUnavailableInspectionPreservesActiveArtifactIdentity() throws {
        var object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: inspectionData()) as? [String: Any]
        )
        object["state"] = "unavailable"
        object["capability"] = [
            "available": false,
            "reason": "apple_container_unavailable",
            "macos_version": "26.5.1",
            "apple_container_version": "",
            "system_service_state": "unavailable",
        ]
        let decoded = try ContainerRuntimeInspection.decodeStrict(
            JSONSerialization.data(withJSONObject: object)
        )
        XCTAssertEqual(decoded.state, .unavailable)
        XCTAssertEqual(decoded.active?.artifactVersion, "2.40.0-r1")
    }

    func testStrictInspectionRejectsUnknownDuplicateAndInconsistentState() throws {
        var object = try XCTUnwrap(
            JSONSerialization.jsonObject(with: inspectionData()) as? [String: Any]
        )
        object["unexpected"] = true
        XCTAssertThrowsError(
            try ContainerRuntimeInspection.decodeStrict(
                JSONSerialization.data(withJSONObject: object)
            )
        ) {
            XCTAssertEqual($0 as? ContainerRuntimeContractError, .unknownField)
        }

        let duplicate = Data(#"{"schema_version":1,"schema_version":1}"#.utf8)
        XCTAssertThrowsError(try ContainerRuntimeInspection.decodeStrict(duplicate)) {
            XCTAssertEqual($0 as? ContainerRuntimeContractError, .duplicateField)
        }

        object.removeValue(forKey: "unexpected")
        object["recovery_required"] = true
        XCTAssertThrowsError(
            try ContainerRuntimeInspection.decodeStrict(
                JSONSerialization.data(withJSONObject: object)
            )
        ) {
            XCTAssertEqual($0 as? ContainerRuntimeContractError, .invalidState)
        }
    }

    func testStrictInspectionRejectsNonCanonicalArtifactVersions() throws {
        for version in [
            "2.40.0-rc.1-r1",
            "2.40.0+build.1-r1",
            "02.40.0-r1",
            "4294967296.40.0-r1",
            "2.40.0-r01",
        ] {
            var object = try XCTUnwrap(
                JSONSerialization.jsonObject(with: inspectionData()) as? [String: Any]
            )
            object["active"] = artifactObject(version: version)
            XCTAssertThrowsError(
                try ContainerRuntimeInspection.decodeStrict(
                    JSONSerialization.data(withJSONObject: object)
                ),
                "accepted non-canonical artifact version \(version)"
            ) {
                XCTAssertEqual($0 as? ContainerRuntimeContractError, .invalidState)
            }
        }
    }

    func testCheckRequiresCandidateOnlyForActionableUpdate() throws {
        let base = try XCTUnwrap(
            JSONSerialization.jsonObject(with: inspectionData()) as? [String: Any]
        )
        var check = base
        check["status"] = "update_available"
        check["candidate"] = artifactObject(version: "2.41.0-r1")
        check["compatible"] = true
        check["reason"] = ""
        let decoded = try ContainerRuntimeCheckReceipt.decodeStrict(
            JSONSerialization.data(withJSONObject: check)
        )
        XCTAssertEqual(decoded.candidate?.artifactVersion, "2.41.0-r1")

        check.removeValue(forKey: "candidate")
        XCTAssertThrowsError(
            try ContainerRuntimeCheckReceipt.decodeStrict(
                JSONSerialization.data(withJSONObject: check)
            )
        ) {
            XCTAssertEqual($0 as? ContainerRuntimeContractError, .invalidState)
        }
    }

    func testCheckWithoutSignedStableManifestIsStrictlyUnavailable() throws {
        var check = try XCTUnwrap(
            JSONSerialization.jsonObject(with: inspectionData()) as? [String: Any]
        )
        check["state"] = "unavailable"
        check.removeValue(forKey: "staged")
        check.removeValue(forKey: "active")
        check["status"] = "unavailable"
        check["compatible"] = false
        check["reason"] = "stable_runtime_manifest_unavailable"
        let data = try JSONSerialization.data(withJSONObject: check)
        let decoded = try ContainerRuntimeCheckReceipt.decodeStrict(data)
        XCTAssertEqual(decoded.state, .unavailable)
        XCTAssertEqual(decoded.status, .unavailable)
        XCTAssertNil(decoded.candidate)
        XCTAssertFalse(decoded.compatible)

        for mutation in [
            { (object: inout [String: Any]) in object["compatible"] = true },
            { (object: inout [String: Any]) in object["candidate"] = self.artifactObject(version: "2.40.0-r1") },
            { (object: inout [String: Any]) in object["reason"] = "hosted_candidate_only" },
        ] {
            var invalid = check
            mutation(&invalid)
            XCTAssertThrowsError(try ContainerRuntimeCheckReceipt.decodeStrict(
                JSONSerialization.data(withJSONObject: invalid)
            )) {
                XCTAssertEqual($0 as? ContainerRuntimeContractError, .invalidState)
            }
        }
    }

    func testOAuthProvidersAndReceiptAreStrictAndBounded() throws {
        let providers = Data(#"{"schema_version":1,"ok":true,"providers":[{"id":"github-copilot","name":"GitHub Copilot","kind":"generic","supports_device_flow":false},{"id":"chatgpt","name":"OpenAI Codex","kind":"codex","supports_device_flow":false}]}"#.utf8)
        let decoded = try ContainerRuntimeOAuthProvidersReceipt.decodeStrict(providers)
        XCTAssertEqual(decoded.providers.map(\.kind), [.generic, .codex])

        for invalidProviders in [
            #"{"schema_version":1,"ok":true,"providers":[]}"#,
            #"{"schema_version":1,"ok":true,"providers":[{"id":"ChatGPT","name":"OpenAI Codex","kind":"codex","supports_device_flow":false}]}"#,
            #"{"schema_version":1,"ok":true,"providers":[{"id":"chatgpt","name":"OpenAI Codex","kind":"generic","supports_device_flow":false}]}"#,
            #"{"schema_version":1,"ok":true,"providers":[{"id":"github-copilot","name":"GitHub Copilot","kind":"codex","supports_device_flow":false}]}"#,
        ] {
            XCTAssertThrowsError(
                try ContainerRuntimeOAuthProvidersReceipt.decodeStrict(Data(invalidProviders.utf8))
            )
        }

        let receipt = Data("""
        {"schema_version":1,"ok":true,"operation_id":"\(hexA)","provider":"chatgpt","kind":"codex","status":"pending","authorization_url":"https://example.test/login","instructions":"Open the page","user_code":null}
        """.utf8)
        XCTAssertEqual(
            try ContainerRuntimeOAuthReceipt.decodeStrict(receipt).provider,
            "chatgpt"
        )

        let mismatchedKind = Data("""
        {"schema_version":1,"ok":true,"operation_id":"\(hexA)","provider":"chatgpt","kind":"generic","status":"pending"}
        """.utf8)
        XCTAssertThrowsError(try ContainerRuntimeOAuthReceipt.decodeStrict(mismatchedKind)) {
            XCTAssertEqual($0 as? ContainerRuntimeContractError, .invalidSchema)
        }

        let secretField = Data("""
        {"schema_version":1,"ok":true,"operation_id":"\(hexA)","provider":"chatgpt","kind":"codex","status":"pending","authorization_url":null,"instructions":null,"user_code":null,"token":"secret"}
        """.utf8)
        XCTAssertThrowsError(try ContainerRuntimeOAuthReceipt.decodeStrict(secretField)) {
            XCTAssertEqual($0 as? ContainerRuntimeContractError, .unknownField)
        }

        for authorizationURL in [
            "https://user@example.test/login",
            "https://example.test/login#secret",
            "https:///missing-host",
            "javascript:alert(1)",
        ] {
            let unsafe = Data("""
            {"schema_version":1,"ok":true,"operation_id":"\(hexA)","provider":"chatgpt","kind":"codex","status":"pending","authorization_url":"\(authorizationURL)"}
            """.utf8)
            XCTAssertThrowsError(
                try ContainerRuntimeOAuthReceipt.decodeStrict(unsafe),
                "accepted unsafe authorization URL \(authorizationURL)"
            ) {
                XCTAssertEqual($0 as? ContainerRuntimeContractError, .invalidState)
            }
        }
    }

    func testProcessOAuthSubmitUsesBoundedStdinNotArguments() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("container-runtime-client.\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let script = directory.appendingPathComponent("relayctl")
        let captured = directory.appendingPathComponent("stdin.json")
        let safeCaptured = captured.path.replacingOccurrences(of: "'", with: "'\\''")
        let relayConfig = directory.appendingPathComponent("bound-relay.json")
        let codexConfig = directory.appendingPathComponent("bound-codex.toml")
        let bindingURL = directory.appendingPathComponent("routing-binding.json")
        try writeBinding(
            at: bindingURL,
            relayConfig: relayConfig,
            codexConfig: codexConfig
        )
        let response = """
        {"schema_version":1,"ok":true,"operation_id":"\(hexA)","provider":"chatgpt","kind":"codex","status":"complete","instructions":"done"}
        """
        let body = """
        #!/bin/sh
        test "$1" = container-runtime || exit 2
        test "$2" = oauth || exit 2
        test "$3" = submit || exit 2
        test "$4" = --operation-id || exit 2
        test "$5" = \(hexA) || exit 2
        test "$6" = --json || exit 2
        test "$7" = --config || exit 2
        test "$8" = '\(relayConfig.path)' || exit 2
        test "$9" = --codex-config || exit 2
        test "${10}" = '\(codexConfig.path)' || exit 2
        test "$#" = 10 || exit 2
        /bin/cat > '\(safeCaptured)'
        /bin/echo '\(response)'
        """
        try Data(body.utf8).write(to: script)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)

        let client = ProcessContainerRuntimeClient(
            executableURL: script,
            bindingURL: bindingURL,
            runtimeMode: .managed,
            distributionFlavor: .production,
            executionPolicy: RelayctlExecutionPolicy(timeout: 2)
        )
        let sensitive = "https://localhost/callback?code=secret&state=opaque"
        let result = try await client.oauthSubmit(
            operationID: hexA,
            redirectURL: sensitive,
            code: nil
        )
        XCTAssertEqual(result.status, .complete)
        let input = try JSONSerialization.jsonObject(with: Data(contentsOf: captured)) as? [String: Any]
        XCTAssertEqual(input?["redirect_url"] as? String, sensitive)
        XCTAssertEqual(input?["schema_version"] as? Int, 1)

        do {
            _ = try await client.oauthSubmit(
                operationID: hexA,
                redirectURL: nil,
                code: String(repeating: "x", count: 4 * 1_024 + 1)
            )
            XCTFail("oversized OAuth input was accepted")
        } catch {
            XCTAssertEqual(error as? ContainerRuntimeContractError, .invalidArgument)
        }
    }

    func testProcessStopAndRecoveryRejectUnverifiedDesktopExitBeforeInvocation() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("container-runtime-desktop-gate.\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let script = directory.appendingPathComponent("relayctl")
        let marker = directory.appendingPathComponent("invoked")
        try Data("#!/bin/sh\ntouch '\(marker.path)'\n".utf8).write(to: script)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)
        let client = ProcessContainerRuntimeClient(
            executableURL: script,
            bindingURL: directory.appendingPathComponent("missing-binding.json"),
            runtimeMode: .managed,
            distributionFlavor: .production
        )

        do {
            _ = try await client.stop(
                expectedStateDigest: hexA,
                expectedRoutingGeneration: 9,
                confirmDesktopExited: false
            )
            XCTFail("unverified stop reached relayctl")
        } catch {
            XCTAssertEqual(error as? ContainerRuntimeContractError, .invalidArgument)
        }
        do {
            _ = try await client.recover(
                expectedStateDigest: hexA,
                confirmDesktopExited: false
            )
            XCTFail("unverified recovery reached relayctl")
        } catch {
            XCTAssertEqual(error as? ContainerRuntimeContractError, .invalidArgument)
        }
        XCTAssertFalse(FileManager.default.fileExists(atPath: marker.path))
    }

    func testProcessStopAndRecoveryCarryFreshDesktopExitConfirmation() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("container-runtime-desktop-argv.\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let script = directory.appendingPathComponent("relayctl")
        let captured = directory.appendingPathComponent("arguments.txt")
        let response = String(decoding: inspectionData(), as: UTF8.self)
        let body = """
        #!/bin/sh
        printf '%s\\n' "$@" >> '\(captured.path)'
        echo '\(response)'
        """
        try Data(body.utf8).write(to: script)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)
        let bindingURL = directory.appendingPathComponent("routing-binding.json")
        let relayConfig = directory.appendingPathComponent("relay.json")
        let codexConfig = directory.appendingPathComponent("config.toml")
        try writeBinding(at: bindingURL, relayConfig: relayConfig, codexConfig: codexConfig)
        let client = ProcessContainerRuntimeClient(
            executableURL: script,
            bindingURL: bindingURL,
            runtimeMode: .managed,
            distributionFlavor: .production,
            executionPolicy: RelayctlExecutionPolicy(timeout: 2)
        )

        _ = try await client.stop(
            expectedStateDigest: hexA,
            expectedRoutingGeneration: 9,
            confirmDesktopExited: true
        )
        _ = try await client.recover(
            expectedStateDigest: hexA,
            confirmDesktopExited: true
        )

        let arguments = String(decoding: try Data(contentsOf: captured), as: UTF8.self)
            .split(separator: "\n")
            .map(String.init)
        XCTAssertEqual(arguments, [
            "container-runtime", "stop",
            "--expected-state-digest", hexA,
            "--expected-routing-generation", "9",
            "--confirm-desktop-exited", "--json",
            "--config", relayConfig.path,
            "--codex-config", codexConfig.path,
            "container-runtime", "recover",
            "--expected-state-digest", hexA,
            "--confirm-desktop-exited", "--json",
            "--config", relayConfig.path,
            "--codex-config", codexConfig.path,
        ])
    }

    func testProcessClientReloadsBindingForEveryInvocation() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("container-runtime-binding.\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let script = directory.appendingPathComponent("relayctl")
        let captured = directory.appendingPathComponent("arguments.txt")
        let body = """
        #!/bin/sh
        printf '%s\\n' "$@" >> '\(captured.path)'
        echo '{"schema_version":1,"ok":true,"state":"healthy","capability":{"available":true,"reason":"","macos_version":"26.5.1","apple_container_version":"1.3.1","system_service_state":"running"},"active":{"artifact_version":"2.40.0-r1","release_sequence":7,"manifest_sha256":"\(hexA)","index_digest":"sha256:\(hexB)","arm64_digest":"sha256:\(hexC)"},"state_digest":"\(hexA)","routing_generation":9,"recovery_required":false}'
        """
        try Data(body.utf8).write(to: script)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)

        let bindingURL = directory.appendingPathComponent("routing-binding.json")
        let firstRelay = directory.appendingPathComponent("first-relay.json")
        let firstCodex = directory.appendingPathComponent("first-codex.toml")
        try writeBinding(at: bindingURL, relayConfig: firstRelay, codexConfig: firstCodex)
        let client = ProcessContainerRuntimeClient(
            executableURL: script,
            bindingURL: bindingURL,
            runtimeMode: .managed,
            distributionFlavor: .production,
            executionPolicy: RelayctlExecutionPolicy(timeout: 2)
        )

        _ = try await client.inspect()

        let secondRelay = directory.appendingPathComponent("second-relay.json")
        let secondCodex = directory.appendingPathComponent("second-codex.toml")
        try writeBinding(at: bindingURL, relayConfig: secondRelay, codexConfig: secondCodex)
        _ = try await client.inspect()

        let arguments = String(decoding: try Data(contentsOf: captured), as: UTF8.self)
            .split(separator: "\n")
            .map(String.init)
        XCTAssertEqual(arguments, [
            "container-runtime", "inspect", "--json",
            "--config", firstRelay.path,
            "--codex-config", firstCodex.path,
            "container-runtime", "inspect", "--json",
            "--config", secondRelay.path,
            "--codex-config", secondCodex.path,
        ])
    }

    func testProcessClientFailsClosedOutsideManagedProduction() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("container-runtime-scope.\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let script = directory.appendingPathComponent("relayctl")
        let marker = directory.appendingPathComponent("invoked")
        try Data("#!/bin/sh\ntouch '\(marker.path)'\n".utf8).write(to: script)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)
        let bindingURL = directory.appendingPathComponent("routing-binding.json")
        try writeBinding(
            at: bindingURL,
            relayConfig: directory.appendingPathComponent("relay.json"),
            codexConfig: directory.appendingPathComponent("config.toml")
        )

        for (runtimeMode, distributionFlavor, expected) in [
            (RelayRuntimeMode.preview, DistributionFlavor.production, ContainerRuntimeClientError.previewMode),
            (RelayRuntimeMode.managed, DistributionFlavor.localDevelopment, ContainerRuntimeClientError.unsupportedDistribution),
        ] {
            let client = ProcessContainerRuntimeClient(
                executableURL: script,
                bindingURL: bindingURL,
                runtimeMode: runtimeMode,
                distributionFlavor: distributionFlavor
            )
            do {
                _ = try await client.inspect()
                XCTFail("unsupported runtime context executed relayctl")
            } catch {
                XCTAssertEqual(error as? ContainerRuntimeClientError, expected)
            }
        }
        XCTAssertFalse(FileManager.default.fileExists(atPath: marker.path))
    }

    func testProcessClientFailsClosedWhenBindingIsMissing() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("container-runtime-missing-binding.\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let script = directory.appendingPathComponent("relayctl")
        let marker = directory.appendingPathComponent("invoked")
        try Data("#!/bin/sh\ntouch '\(marker.path)'\n".utf8).write(to: script)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)
        let client = ProcessContainerRuntimeClient(
            executableURL: script,
            bindingURL: directory.appendingPathComponent("missing-binding.json"),
            runtimeMode: .managed,
            distributionFlavor: .production
        )

        do {
            _ = try await client.inspect()
            XCTFail("missing binding executed relayctl")
        } catch {
            XCTAssertEqual(error as? RoutingBindingError, .missing)
        }
        XCTAssertFalse(FileManager.default.fileExists(atPath: marker.path))
    }

    private func writeBinding(
        at bindingURL: URL,
        relayConfig: URL,
        codexConfig: URL
    ) throws {
        let binding = RoutingBinding(
            relayConfig: relayConfig.path,
            codexConfig: codexConfig.path
        )
        try JSONEncoder().encode(binding).write(to: bindingURL, options: .atomic)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o600],
            ofItemAtPath: bindingURL.path
        )
    }

    private func inspectionData() -> Data {
        let object: [String: Any] = [
            "schema_version": 1,
            "ok": true,
            "state": "healthy",
            "capability": [
                "available": true,
                "reason": "",
                "macos_version": "26.5.1",
                "apple_container_version": "1.3.1",
                "system_service_state": "running",
            ],
            "active": artifactObject(version: "2.40.0-r1"),
            "state_digest": hexA,
            "routing_generation": 9,
            "recovery_required": false,
        ]
        return try! JSONSerialization.data(withJSONObject: object, options: [.sortedKeys])
    }

    private func artifactObject(version: String) -> [String: Any] {
        [
            "artifact_version": version,
            "release_sequence": 7,
            "manifest_sha256": hexA,
            "index_digest": "sha256:\(hexB)",
            "arm64_digest": "sha256:\(hexC)",
        ]
    }
}
