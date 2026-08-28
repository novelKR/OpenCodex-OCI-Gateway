import Darwin
import Foundation
import XCTest
@testable import OpenCodexRelayCore

final class GatewayClientTests: XCTestCase {
    func testCandidateInputIsOnlyBoundedWhileRelayOwnsURLValidation() {
        XCTAssertTrue(GatewayInspection.isCandidateInput("not-yet-a-valid-url"))
        XCTAssertFalse(GatewayInspection.isCandidateInput(""))
        XCTAssertFalse(GatewayInspection.isCandidateInput("value\0suffix"))
        XCTAssertFalse(GatewayInspection.isCandidateInput(String(repeating: "x", count: 4_097)))
    }

    func testGatewayWireModelsRejectMalformedWitnesses() throws {
        let invalidDigest = Data("""
        {"schema_version":1,"upstream_base_url":"https://gateway.example.test/v1","config_digest":"raw-value","routing_generation":7,"credential_source":"keychain","credential_account":"account","credentials_editable":true}
        """.utf8)
        let inspection = try JSONDecoder().decode(GatewayInspection.self, from: invalidDigest)
        XCTAssertThrowsError(try inspection.validated())

        let invalidCredentialContract = Data("""
        {"schema_version":1,"upstream_base_url":"https://gateway.example.test/v1","config_digest":"\(String(repeating: "a", count: 64))","routing_generation":7,"credential_source":"file","credential_account":null,"credentials_editable":true}
        """.utf8)
        let mismatch = try JSONDecoder().decode(GatewayInspection.self, from: invalidCredentialContract)
        XCTAssertThrowsError(try mismatch.validated())
    }

    func testProcessGatewayClientSendsAddressOnlyThroughJSONStdin() async throws {
        let directory = URL(fileURLWithPath: NSTemporaryDirectory(), isDirectory: true)
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let script = directory.appendingPathComponent("relayctl-test")
        try Data("""
        #!/bin/sh
        /usr/bin/printf '%s\\n' "$@" > "${0}.args"
        /bin/cat > "${0}.stdin"
        /usr/bin/printf '%s' '{"schema_version":1,"ok":true,"config_digest":"\(String(repeating: "a", count: 64))","routing_generation":7,"model_count":2}'
        """.utf8).write(to: script)
        XCTAssertEqual(Darwin.chmod(script.path, 0o700), 0)

        let candidate = "https://private-gateway.example.test/v1"
        let client = ProcessGatewayClient(
            executableURL: script,
            additionalArguments: ["--config", "/tmp/relay.json"]
        )
        let result = try await client.test(upstreamBaseURL: candidate)

        XCTAssertEqual(result.modelCount, 2)
        let arguments = try String(contentsOf: URL(fileURLWithPath: script.path + ".args"), encoding: .utf8)
        XCTAssertFalse(arguments.contains(candidate))
        XCTAssertTrue(arguments.contains("gateway\ntest\n--json"))
        let stdin = try Data(contentsOf: URL(fileURLWithPath: script.path + ".stdin"))
        let object = try JSONDecoder().decode(GatewayCandidate.self, from: stdin)
        XCTAssertEqual(object.upstreamBaseURL, candidate)
        XCTAssertEqual(object.authenticationProfile, .cloudflareAccessAndGatewayAPIKey)
        XCTAssertFalse(object.allowInsecurePrivateIP)
    }

    func testGatewayReportedErrorsRemainBounded() {
        let cases: [(RelayctlReportedErrorCode, String)] = [
            (.authenticationFailed, "authentication_failed"),
            (.gatewayUnreachable, "gateway_unreachable"),
            (.catalogInvalid, "catalog_invalid"),
            (.configChanged, "config_changed"),
            (.routingChanged, "routing_changed"),
        ]
        for (code, safeCode) in cases {
            let error = RelayctlError.reported(code)
            XCTAssertEqual(error.safeCode, safeCode)
            XCTAssertFalse(error.safeMessage.contains("https://"))
        }
    }
}
