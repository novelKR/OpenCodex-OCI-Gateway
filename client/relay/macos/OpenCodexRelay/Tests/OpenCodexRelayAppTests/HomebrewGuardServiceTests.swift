import Foundation
import XCTest
@testable import OpenCodexRelay
import OpenCodexRelayHelperInstallerCore

final class HomebrewGuardServiceTests: XCTestCase {
    func testProductionUsesManualAdminBackend() throws {
        let bundle = try makeBundle(info: [
            "CFBundleIdentifier": "test.production",
            "OpenCodexHomebrewGuardBackend": "manual_admin",
            "OpenCodexHomebrewGuardMachService": "io.github.novelkr.opencodex-relay.homebrew-guard",
            "OpenCodexHomebrewGuardInstallerExecutable": HelperInstallerConstants.installerExecutableName,
            "OpenCodexHomebrewGuardHelperRequirement": "cdhash H\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"",
            "OpenCodexHomebrewGuardHelperVersion": "1.2.3",
        ])
        defer { try? FileManager.default.removeItem(at: bundle.bundleURL) }

        let configuration = try XCTUnwrap(
            HomebrewGuardServiceConfiguration(bundle: bundle, flavor: .production)
        )

        XCTAssertEqual(configuration.backend, .manualAdmin)
        XCTAssertEqual(configuration.machServiceName, "io.github.novelkr.opencodex-relay.homebrew-guard")
        XCTAssertEqual(configuration.installerProfile.distribution, .production)
    }

    func testLocalDevelopmentUsesManualDaemonBackend() throws {
        let bundle = try makeBundle(info: localDevelopmentInfo())
        defer { try? FileManager.default.removeItem(at: bundle.bundleURL) }

        let configuration = try XCTUnwrap(
            HomebrewGuardServiceConfiguration(bundle: bundle, flavor: .localDevelopment)
        )

        XCTAssertEqual(configuration.backend, .manualAdmin)
        XCTAssertEqual(
            configuration.machServiceName,
            HelperInstallerConstants.manualServiceName
        )
        XCTAssertEqual(
            configuration.installerExecutableName,
            HelperInstallerConstants.installerExecutableName
        )
    }

    func testLocalDevelopmentRejectsLegacySMAppServiceAsActiveBackend() throws {
        var info = localDevelopmentInfo()
        info["OpenCodexHomebrewGuardBackend"] = "sm_app_service"
        info["OpenCodexHomebrewGuardMachService"] = HelperInstallerProfile.production.serviceName
        let bundle = try makeBundle(info: info)
        defer { try? FileManager.default.removeItem(at: bundle.bundleURL) }

        XCTAssertNil(
            HomebrewGuardServiceConfiguration(bundle: bundle, flavor: .localDevelopment)
        )
    }

    @MainActor
    func testMissingBundledInstallerReturnsExplicitArtifactInvalid() throws {
        let bundle = try makeBundle(info: localDevelopmentInfo())
        defer { try? FileManager.default.removeItem(at: bundle.bundleURL) }
        let configuration = try XCTUnwrap(
            HomebrewGuardServiceConfiguration(bundle: bundle, flavor: .localDevelopment)
        )
        let manager = SystemHomebrewGuardManager(configuration: configuration)

        XCTAssertEqual(
            manager.setupCommand(for: .install),
            .unavailable("artifact_invalid")
        )
    }

    @MainActor
    func testInterruptedInstallerTransactionPreemptsReadyArtifactInspection() async throws {
        let bundle = try makeBundle(info: localDevelopmentInfo())
        let root = FileManager.default.temporaryDirectory
            .appending(path: "pw-relay-manual-guard-state-\(UUID().uuidString)")
        defer {
            try? FileManager.default.removeItem(at: bundle.bundleURL)
            try? FileManager.default.removeItem(at: root)
        }
        let transaction = root
            .appending(path: HelperInstallerConstants.stateDirectoryRelativePath)
            .appending(path: HelperInstallerConstants.transactionDirectoryName)
        try FileManager.default.createDirectory(at: transaction, withIntermediateDirectories: true)
        let configuration = try XCTUnwrap(
            HomebrewGuardServiceConfiguration(bundle: bundle, flavor: .localDevelopment)
        )
        let manager = SystemHomebrewGuardManager(
            configuration: configuration,
            manualSystemRootURL: root
        )

        let availability = await manager.availability(candidate: nil)

        XCTAssertEqual(availability.registration, .manualInstallerRecoveryRequired)
        XCTAssertEqual(availability.errorCode, .recoveryRequired)
        XCTAssertNil(availability.operationID)
    }

    private func localDevelopmentInfo() -> [String: Any] {
        [
            "CFBundleIdentifier": "test.local",
            "OpenCodexHomebrewGuardBackend": "manual_admin",
            "OpenCodexHomebrewGuardMachService": HelperInstallerConstants.manualServiceName,
            "OpenCodexHomebrewGuardInstallerExecutable": HelperInstallerConstants.installerExecutableName,
            "OpenCodexHomebrewGuardHelperRequirement": "cdhash H\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\"",
            "OpenCodexHomebrewGuardHelperVersion": "1.2.3-dev",
        ]
    }

    private func makeBundle(info: [String: Any]) throws -> Bundle {
        let root = FileManager.default.temporaryDirectory
            .appending(path: "pw-relay-helper-config-\(UUID().uuidString).bundle")
        let contents = root.appending(path: "Contents")
        try FileManager.default.createDirectory(at: contents, withIntermediateDirectories: true)
        let data = try PropertyListSerialization.data(
            fromPropertyList: info,
            format: .xml,
            options: 0
        )
        try data.write(to: contents.appending(path: "Info.plist"))
        return try XCTUnwrap(Bundle(url: root))
    }
}
