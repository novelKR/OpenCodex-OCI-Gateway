import Foundation
import XCTest
@testable import OpenCodexRelay
@testable import OpenCodexRelayCore

final class CodexConfigurationTests: XCTestCase {
    private struct Fixture {
        let root: URL
        let bindingURL: URL
        let configURL: URL
    }

    @MainActor
    private final class MockExternalOpener: CodexConfigurationExternalOpening {
        var destinations: [CodexConfigurationOpenDestination] = [
            .systemDefault,
            .visualStudioCode,
            .other,
        ]
        var result: CodexConfigurationExternalOpenResult = .opened
        private(set) var calls: [(URL, CodexConfigurationOpenDestination)] = []

        func availableDestinations() -> [CodexConfigurationOpenDestination] {
            destinations
        }

        func open(
            fileURL: URL,
            destination: CodexConfigurationOpenDestination
        ) async -> CodexConfigurationExternalOpenResult {
            calls.append((fileURL, destination))
            return result
        }
    }

    private func makeFixture(contents: Data = Data("model = \"gpt-5\"\n".utf8)) throws -> Fixture {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        let configURL = root.appendingPathComponent("config.toml", isDirectory: false)
        try contents.write(to: configURL)

        let bindingURL = root.appendingPathComponent("routing-binding.json", isDirectory: false)
        let binding = RoutingBinding(
            relayConfig: root.appendingPathComponent("relay.json").path,
            codexConfig: configURL.path
        )
        try JSONEncoder().encode(binding).write(to: bindingURL, options: .atomic)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o600],
            ofItemAtPath: bindingURL.path
        )
        return Fixture(root: root, bindingURL: bindingURL, configURL: configURL)
    }

    @MainActor
    func testObservationRefreshesRoutingOncePerDistinctFileState() throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        let opener = MockExternalOpener()
        let activityLog = RelayActivityLogStore(subsystem: "test.codex-config.observation")
        var refreshCount = 0
        let controller = CodexConfigurationController(
            bindingURL: fixture.bindingURL,
            opener: opener,
            activityLog: activityLog
        ) {
            refreshCount += 1
        }

        controller.refreshMetadata()
        XCTAssertEqual(refreshCount, 0)
        XCTAssertFalse(controller.hasChangedSinceReview)

        try Data("model = \"gpt-5\"\nfeatures = true\n".utf8).write(to: fixture.configURL)
        controller.refreshMetadata()
        XCTAssertEqual(refreshCount, 1)
        XCTAssertTrue(controller.hasChangedSinceReview)

        controller.refreshMetadata()
        XCTAssertEqual(refreshCount, 1)

        try FileManager.default.removeItem(at: fixture.configURL)
        controller.refreshMetadata()
        controller.refreshMetadata()
        XCTAssertEqual(refreshCount, 2)
        XCTAssertEqual(controller.metadataFailureCode, "config_file_missing")
        XCTAssertEqual(
            activityLog.events.filter { $0.code == "config_changed" }.count,
            2
        )

        try Data("model = \"gpt-5.1\"\n".utf8).write(to: fixture.configURL)
        controller.refreshMetadata()
        XCTAssertNotNil(controller.metadata)
        XCTAssertNil(controller.metadataFailureCode)
        XCTAssertEqual(refreshCount, 3)
    }

    func testActionAvailabilityKeepsRefreshIndependentFromMetadataAndExternalOpen() {
        XCTAssertEqual(
            CodexConfigurationActionAvailability.resolve(
                hasMetadata: false,
                isOpeningExternally: false
            ),
            .init(canRefresh: true, canOpenExternally: false)
        )
        XCTAssertEqual(
            CodexConfigurationActionAvailability.resolve(
                hasMetadata: true,
                isOpeningExternally: true
            ),
            .init(canRefresh: true, canOpenExternally: false)
        )
        XCTAssertEqual(
            CodexConfigurationActionAvailability.resolve(
                hasMetadata: true,
                isOpeningExternally: false
            ),
            .init(canRefresh: true, canOpenExternally: true)
        )
    }

    @MainActor
    func testPreviewIsSessionBoundAndActivityLogNeverIncludesContentsOrPath() throws {
        let secret = "api_key = \"secret-token-for-test\"\n"
        let fixture = try makeFixture(contents: Data(secret.utf8))
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        let activityLog = RelayActivityLogStore(subsystem: "test.codex-config.preview")
        let controller = CodexConfigurationController(
            bindingURL: fixture.bindingURL,
            opener: MockExternalOpener(),
            activityLog: activityLog,
            onStatusRefreshRequested: {}
        )

        controller.refreshMetadata()
        XCTAssertTrue(controller.revealPreview())
        XCTAssertEqual(controller.previewText, secret)
        XCTAssertTrue(activityLog.events.contains { $0.code == "preview_revealed" })

        let logJSON = activityLog.jsonLines()
        XCTAssertFalse(logJSON.contains("secret-token-for-test"))
        XCTAssertFalse(logJSON.contains(fixture.configURL.path))

        controller.dismissPreview()
        XCTAssertNil(controller.previewText)
        XCTAssertNil(controller.previewFailureCode)
    }

    @MainActor
    func testExternalOpenRevalidatesFileAndEmitsOnlyBoundedFields() async throws {
        let fixture = try makeFixture()
        defer { try? FileManager.default.removeItem(at: fixture.root) }
        let opener = MockExternalOpener()
        let activityLog = RelayActivityLogStore(subsystem: "test.codex-config.open")
        let controller = CodexConfigurationController(
            bindingURL: fixture.bindingURL,
            opener: opener,
            activityLog: activityLog,
            onStatusRefreshRequested: {}
        )

        controller.refreshMetadata()
        await controller.openExternally(.visualStudioCode)
        XCTAssertEqual(opener.calls.count, 1)
        XCTAssertEqual(opener.calls.first?.0, fixture.configURL)
        XCTAssertEqual(opener.calls.first?.1, .visualStudioCode)
        XCTAssertEqual(controller.externalOpenResult, .opened)
        XCTAssertTrue(activityLog.events.contains {
            $0.code == "external_open_finished" &&
                $0.fields["destination"] == "visual_studio_code" &&
                $0.fields["result_code"] == "opened"
        })
        XCTAssertFalse(activityLog.jsonLines().contains(fixture.configURL.path))

        let regularURL = fixture.root.appendingPathComponent("regular.toml")
        try Data("native = true\n".utf8).write(to: regularURL)
        try FileManager.default.removeItem(at: fixture.configURL)
        try FileManager.default.createSymbolicLink(
            at: fixture.configURL,
            withDestinationURL: regularURL
        )
        await controller.openExternally(.systemDefault)
        XCTAssertEqual(opener.calls.count, 1)
        XCTAssertEqual(controller.externalOpenResult, .failed)
        XCTAssertEqual(controller.metadataFailureCode, "config_file_unsafe")
    }

    @MainActor
    func testWorkspaceOpenerListsInstalledTargetsAndHandlesChooserCancellation() async {
        let fakeApplication = URL(fileURLWithPath: "/Applications/Fake Code.app", isDirectory: true)
        let opener = NSWorkspaceCodexConfigurationExternalOpener(
            defaultApplicationURL: { _ in nil },
            applicationURL: { bundleIdentifier in
                bundleIdentifier == "com.microsoft.VSCode" ? fakeApplication : nil
            },
            applicationChooser: { nil },
            applicationOpen: { _, _ in true }
        )

        XCTAssertEqual(
            opener.availableDestinations(),
            [.systemDefault, .visualStudioCode, .other]
        )
        let defaultResult = await opener.open(
            fileURL: URL(fileURLWithPath: "/tmp/config.toml"),
            destination: .systemDefault
        )
        XCTAssertEqual(defaultResult, .applicationUnavailable)

        let otherResult = await opener.open(
            fileURL: URL(fileURLWithPath: "/tmp/config.toml"),
            destination: .other
        )
        XCTAssertEqual(otherResult, .cancelled)
    }
}
