import Foundation
import XCTest
@testable import OpenCodexRelay

@MainActor
final class LocalDevelopmentIntegrationGuideTests: XCTestCase {
    func testReviewedPEMInputsGenerateOnlyShellQuotedOfficialCommands() throws {
        let fixture = try makeFixture(repositoryName: "repo's checkout")
        defer { fixture.remove() }

        let commands = try LocalDevelopmentIntegrationCommandBuilder.commands(
            for: fixture.input(signingSource: .pemFile, signingValue: fixture.signingKey.path)
        )

        XCTAssertNil(commands.signingSetup)
        XCTAssertTrue(commands.build.hasPrefix("cd '/tmp/"))
        XCTAssertTrue(commands.build.contains("repo'\"'\"'s checkout"))
        XCTAssertTrue(commands.build.contains("./client/relay/scripts/build-local-dev.sh"))
        XCTAssertTrue(commands.build.contains("--signing-key '/tmp/"))
        XCTAssertTrue(commands.install.contains("./client/relay/scripts/install-local-dev.sh"))
        XCTAssertTrue(commands.install.contains("--acknowledge-local-development-source"))
        XCTAssertTrue(commands.install.contains("--acknowledge-local-source"))
        for command in [commands.build, commands.install] {
            XCTAssertFalse(command.contains("sudo"))
            XCTAssertFalse(command.contains("open -a"))
        }
    }

    func testMissingKeychainKeyAddsPreparationCommandBeforeBuildAndInstall() throws {
        let fixture = try makeFixture()
        defer { fixture.remove() }

        let commands = try LocalDevelopmentIntegrationCommandBuilder.commands(
            for: fixture.input(
                signingSource: .keychainService,
                signingValue: LocalDevelopmentIntegrationCommandBuilder.defaultKeychainService,
                prepareKeychainSigningKey: true
            )
        )

        let signingSetup = try XCTUnwrap(commands.signingSetup)
        XCTAssertTrue(signingSetup.contains("./client/relay/scripts/bootstrap-keychain-signing-key.sh"))
        XCTAssertTrue(signingSetup.contains("--service 'io.github.novelkr.opencodex-relay.local-dev-signing'"))
        XCTAssertTrue(signingSetup.contains("--public-key-out '/tmp/"))
        XCTAssertTrue(commands.build.contains("--signing-key-keychain-service"))
        XCTAssertFalse(signingSetup.contains("sudo"))
        XCTAssertFalse(signingSetup.contains("open -a"))

        let existingKeyCommands = try LocalDevelopmentIntegrationCommandBuilder.commands(
            for: fixture.input(
                signingSource: .keychainService,
                signingValue: LocalDevelopmentIntegrationCommandBuilder.defaultKeychainService,
                prepareKeychainSigningKey: false
            )
        )
        XCTAssertNil(existingKeyCommands.signingSetup)
    }

    func testValidationReturnsAFieldSpecificSafeCodeForEveryInput() throws {
        let fixture = try makeFixture()
        defer { fixture.remove() }

        var input = fixture.input(signingSource: .pemFile, signingValue: fixture.signingKey.path)
        XCTAssertTrue(
            LocalDevelopmentIntegrationCommandBuilder.validation(for: input)
                .allRequiredFieldsAreValid
        )

        input.repositoryRoot = "relative/repository"
        XCTAssertEqual(
            LocalDevelopmentIntegrationCommandBuilder.validation(for: input).repository,
            .invalid(.repositoryUnsafe)
        )

        let incompleteRepository = fixture.root.appending(path: "incomplete-repository")
        try FileManager.default.createDirectory(at: incompleteRepository, withIntermediateDirectories: true)
        input.repositoryRoot = incompleteRepository.path
        XCTAssertEqual(
            LocalDevelopmentIntegrationCommandBuilder.validation(for: input).repository,
            .invalid(.repositoryScriptsMissing)
        )

        input = fixture.input(signingSource: .pemFile, signingValue: fixture.signingKey.path)
        input.version = "latest"
        XCTAssertEqual(
            LocalDevelopmentIntegrationCommandBuilder.validation(for: input).version,
            .invalid(.versionInvalid)
        )

        input = fixture.input(signingSource: .pemFile, signingValue: fixture.signingKey.path)
        input.outputDirectory = fixture.root.appending(path: "missing/child").path
        XCTAssertEqual(
            LocalDevelopmentIntegrationCommandBuilder.validation(for: input).output,
            .invalid(.outputUnsafe)
        )

        input = fixture.input(signingSource: .pemFile, signingValue: fixture.signingKey.path)
        input.upstreamURL = "https://gateway.example.test/v1?secret=value"
        XCTAssertEqual(
            LocalDevelopmentIntegrationCommandBuilder.validation(for: input).gateway,
            .invalid(.gatewayInvalid)
        )

        input = fixture.input(signingSource: .keychainService, signingValue: "service with spaces")
        XCTAssertEqual(
            LocalDevelopmentIntegrationCommandBuilder.validation(for: input).signing,
            .invalid(.keychainServiceInvalid)
        )

        input = fixture.input(
            signingSource: .pemFile,
            signingValue: fixture.root.appending(path: "missing.pem").path
        )
        XCTAssertEqual(
            LocalDevelopmentIntegrationCommandBuilder.validation(for: input).signing,
            .invalid(.pemFileUnsafe)
        )
    }

    func testStepsBlockUntilTheirFieldsAreValidAndCommandsRequireReview() throws {
        let fixture = try makeFixture()
        defer { fixture.remove() }
        let selector = TestFileSelector(directoryURL: fixture.repository)
        let model = LocalDevelopmentIntegrationGuideModel(bundleVersion: "1.2.3-dev.1")

        model.advance()
        XCTAssertEqual(model.currentStep, .source)
        model.go(to: .review)
        XCTAssertEqual(model.currentStep, .source)

        model.chooseRepository(using: selector, title: "", message: "")
        XCTAssertTrue(model.canAdvance)
        model.advance()
        XCTAssertEqual(model.currentStep, .package)

        XCTAssertFalse(model.canAdvance)
        selector.directoryURL = fixture.outputParent
        model.chooseOutputParent(using: selector, title: "", message: "")
        XCTAssertEqual(
            model.outputDirectory,
            fixture.outputParent.appending(path: "OpenCodexRelay-1.2.3-dev.1").path
        )
        XCTAssertTrue(model.canAdvance)
        model.advance()

        model.setUpstreamURL("https://gateway.example.test/v1")
        model.advance()
        XCTAssertEqual(model.currentStep, .signing)
        XCTAssertEqual(
            model.keychainService,
            LocalDevelopmentIntegrationCommandBuilder.defaultKeychainService
        )
        XCTAssertTrue(model.canAdvance)
        model.advance()
        XCTAssertEqual(model.currentStep, .review)
        XCTAssertNil(model.commands)

        model.acknowledgedSource = true
        XCTAssertNotNil(model.commands?.signingSetup)
        model.setUpstreamURL("https://gateway.example.test/v1?changed=true")
        XCTAssertNil(model.commands)
        model.setUpstreamURL("https://gateway.example.test/v1")
        XCTAssertNotNil(model.commands)
        model.goBack()
        XCTAssertEqual(model.currentStep, .signing)
    }

    func testOutputParentRecomputesWithVersionButRawAdvancedValueRemainsAuthoritative() throws {
        let fixture = try makeFixture()
        defer { fixture.remove() }
        let model = LocalDevelopmentIntegrationGuideModel(bundleVersion: nil)

        model.setOutputParent(fixture.outputParent)
        XCTAssertEqual(
            model.outputDirectory,
            fixture.outputParent.appending(path: "OpenCodexRelay-local-dev").path
        )
        model.setVersion("2.3.4-dev.5")
        XCTAssertEqual(
            model.outputDirectory,
            fixture.outputParent.appending(path: "OpenCodexRelay-2.3.4-dev.5").path
        )

        let rawOutput = fixture.outputParent.appending(path: "manually-selected-output").path
        model.setOutputDirectory(rawOutput)
        model.setVersion("2.3.4-dev.6")
        XCTAssertEqual(model.outputDirectory, rawOutput)
        XCTAssertEqual(
            LocalDevelopmentIntegrationGuideModel(bundleVersion: "not-a-version").version,
            ""
        )
    }

    func testSigningModeAndCancelledSelectionsPreserveSharedState() throws {
        let fixture = try makeFixture()
        defer { fixture.remove() }
        let model = LocalDevelopmentIntegrationGuideModel(bundleVersion: nil)
        let selector = TestFileSelector(directoryURL: fixture.repository, fileURL: fixture.signingKey)

        model.chooseRepository(using: selector, title: "", message: "")
        selector.directoryURL = nil
        model.chooseRepository(using: selector, title: "", message: "")
        XCTAssertEqual(model.repositoryRoot, fixture.repository.path)

        model.setSigningSource(.pemFile)
        model.choosePEMFile(using: selector, title: "", message: "")
        XCTAssertEqual(model.pemFilePath, fixture.signingKey.path)
        selector.fileURL = nil
        model.choosePEMFile(using: selector, title: "", message: "")
        XCTAssertEqual(model.pemFilePath, fixture.signingKey.path)

        model.setSigningSource(.keychainService)
        model.setKeychainService("custom.signing.service")
        model.setSigningSource(.pemFile)
        XCTAssertEqual(model.pemFilePath, fixture.signingKey.path)
        model.setSigningSource(.keychainService)
        XCTAssertEqual(model.keychainService, "custom.signing.service")
    }

    func testShellQuoteDoesNotPermitArgumentSplitting() {
        XCTAssertEqual(
            LocalDevelopmentIntegrationCommandBuilder.shellQuote("alpha' beta; $(unsafe)"),
            "'alpha'\"'\"' beta; $(unsafe)'"
        )
    }

    private func makeFixture(repositoryName: String = "repository") throws -> Fixture {
        let root = URL(fileURLWithPath: "/tmp/OpenCodexRelay-Guide-\(UUID().uuidString)")
        let repository = root.appending(path: repositoryName)
        let scripts = repository.appending(path: "client/relay/scripts")
        let signingKey = root.appending(path: "manifest key.pem")
        let outputParent = root.appending(path: "packages")
        try FileManager.default.createDirectory(at: scripts, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(at: outputParent, withIntermediateDirectories: true)

        for name in LocalDevelopmentIntegrationCommandBuilder.requiredScripts.map({
            URL(fileURLWithPath: $0).lastPathComponent
        }) {
            let script = scripts.appending(path: name)
            try Data("#!/bin/sh\n".utf8).write(to: script)
            try FileManager.default.setAttributes(
                [.posixPermissions: 0o755],
                ofItemAtPath: script.path
            )
        }
        try Data("test fixture".utf8).write(to: signingKey)
        return Fixture(
            root: root,
            repository: repository,
            signingKey: signingKey,
            outputParent: outputParent
        )
    }
}

private struct Fixture {
    let root: URL
    let repository: URL
    let signingKey: URL
    let outputParent: URL

    func input(
        signingSource: LocalDevelopmentSigningSource,
        signingValue: String,
        prepareKeychainSigningKey: Bool = false
    ) -> LocalDevelopmentIntegrationInput {
        LocalDevelopmentIntegrationInput(
            repositoryRoot: repository.path,
            version: "1.2.3-dev.4",
            outputDirectory: outputParent.appending(path: "OpenCodexRelay-1.2.3-dev.4").path,
            upstreamURL: "https://gateway.example.test/v1",
            signingSource: signingSource,
            signingValue: signingValue,
            prepareKeychainSigningKey: prepareKeychainSigningKey,
            acknowledgedSource: true
        )
    }

    func remove() {
        try? FileManager.default.removeItem(at: root)
    }
}

@MainActor
private final class TestFileSelector: LocalDevelopmentIntegrationFileSelecting {
    var directoryURL: URL?
    var fileURL: URL?

    init(directoryURL: URL? = nil, fileURL: URL? = nil) {
        self.directoryURL = directoryURL
        self.fileURL = fileURL
    }

    func chooseDirectory(title _: String, message _: String) -> URL? {
        directoryURL
    }

    func chooseFile(title _: String, message _: String) -> URL? {
        fileURL
    }
}
