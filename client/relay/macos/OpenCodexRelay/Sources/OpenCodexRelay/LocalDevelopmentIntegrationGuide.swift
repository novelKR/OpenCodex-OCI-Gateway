import Darwin
import Foundation

enum LocalDevelopmentSigningSource: String, CaseIterable, Identifiable {
    case keychainService
    case pemFile

    var id: String { rawValue }
}

enum LocalDevelopmentIntegrationStep: Int, CaseIterable, Identifiable {
    case source
    case package
    case gateway
    case signing
    case review

    var id: Int { rawValue }
}

enum LocalDevelopmentIntegrationField: Hashable {
    case repository
    case version
    case output
    case gateway
    case signing
}

enum LocalDevelopmentIntegrationValidationCode: String, Equatable {
    case repositoryUnsafe = "repository_unsafe"
    case repositoryScriptsMissing = "repository_scripts_missing"
    case versionInvalid = "version_invalid"
    case outputUnsafe = "output_unsafe"
    case gatewayInvalid = "gateway_invalid"
    case keychainServiceInvalid = "keychain_service_invalid"
    case pemFileUnsafe = "pem_file_unsafe"
}

enum LocalDevelopmentIntegrationFieldValidation: Equatable {
    case empty
    case valid
    case invalid(LocalDevelopmentIntegrationValidationCode)

    var isValid: Bool { self == .valid }
}

struct LocalDevelopmentIntegrationValidation: Equatable {
    let repository: LocalDevelopmentIntegrationFieldValidation
    let version: LocalDevelopmentIntegrationFieldValidation
    let output: LocalDevelopmentIntegrationFieldValidation
    let gateway: LocalDevelopmentIntegrationFieldValidation
    let signing: LocalDevelopmentIntegrationFieldValidation

    func state(for field: LocalDevelopmentIntegrationField) -> LocalDevelopmentIntegrationFieldValidation {
        switch field {
        case .repository: repository
        case .version: version
        case .output: output
        case .gateway: gateway
        case .signing: signing
        }
    }

    var allRequiredFieldsAreValid: Bool {
        repository.isValid && version.isValid && output.isValid && gateway.isValid && signing.isValid
    }
}

struct LocalDevelopmentIntegrationInput: Equatable {
    var repositoryRoot: String
    var version: String
    var outputDirectory: String
    var upstreamURL: String
    var signingSource: LocalDevelopmentSigningSource
    var signingValue: String
    var prepareKeychainSigningKey: Bool
    var acknowledgedSource: Bool

    init(
        repositoryRoot: String,
        version: String,
        outputDirectory: String,
        upstreamURL: String,
        signingSource: LocalDevelopmentSigningSource,
        signingValue: String,
        prepareKeychainSigningKey: Bool = false,
        acknowledgedSource: Bool
    ) {
        self.repositoryRoot = repositoryRoot
        self.version = version
        self.outputDirectory = outputDirectory
        self.upstreamURL = upstreamURL
        self.signingSource = signingSource
        self.signingValue = signingValue
        self.prepareKeychainSigningKey = prepareKeychainSigningKey
        self.acknowledgedSource = acknowledgedSource
    }
}

struct LocalDevelopmentIntegrationCommands: Equatable {
    let signingSetup: String?
    let build: String
    let install: String
}

enum LocalDevelopmentIntegrationGuideError: Error, Equatable {
    case invalidInput
}

enum LocalDevelopmentIntegrationCommandBuilder {
    static let defaultKeychainService = "io.github.novelkr.opencodex-relay.local-dev-signing"
    static let requiredScripts = [
        "client/relay/scripts/build-local-dev.sh",
        "client/relay/scripts/install-local-dev.sh",
        "client/relay/scripts/bootstrap-keychain-signing-key.sh",
    ]

    static func validation(
        for input: LocalDevelopmentIntegrationInput,
        fileManager: FileManager = .default
    ) -> LocalDevelopmentIntegrationValidation {
        LocalDevelopmentIntegrationValidation(
            repository: validateRepository(input.repositoryRoot, fileManager: fileManager),
            version: validateVersion(input.version),
            output: validateOutput(input.outputDirectory, fileManager: fileManager),
            gateway: validateGateway(input.upstreamURL),
            signing: validateSigningSource(input, fileManager: fileManager)
        )
    }

    static func commands(
        for input: LocalDevelopmentIntegrationInput,
        fileManager: FileManager = .default
    ) throws -> LocalDevelopmentIntegrationCommands {
        let validation = validation(for: input, fileManager: fileManager)
        guard input.acknowledgedSource,
              validation.allRequiredFieldsAreValid,
              let repository = safeAbsoluteURL(input.repositoryRoot),
              let output = safeOutputURL(input.outputDirectory, fileManager: fileManager) else {
            throw LocalDevelopmentIntegrationGuideError.invalidInput
        }

        let repositoryArgument = shellQuote(repository.path)
        let versionArgument = shellQuote(input.version)
        let outputArgument = shellQuote(output.path)
        let upstreamArgument = shellQuote(input.upstreamURL)
        let signingArgument: String
        let signingSetup: String?
        switch input.signingSource {
        case .pemFile:
            signingArgument = "--signing-key \(shellQuote(input.signingValue))"
            signingSetup = nil
        case .keychainService:
            signingArgument = "--signing-key-keychain-service \(shellQuote(input.signingValue))"
            if input.prepareKeychainSigningKey {
                let publicKeyOutput = output
                    .deletingLastPathComponent()
                    .appending(
                        path: "OpenCodexRelay-\(input.version)-bootstrap-public-key.pem",
                        directoryHint: .notDirectory
                    )
                signingSetup = "cd \(repositoryArgument) && ./client/relay/scripts/bootstrap-keychain-signing-key.sh --service \(shellQuote(input.signingValue)) --public-key-out \(shellQuote(publicKeyOutput.path))"
            } else {
                signingSetup = nil
            }
        }

        return LocalDevelopmentIntegrationCommands(
            signingSetup: signingSetup,
            build: "cd \(repositoryArgument) && ./client/relay/scripts/build-local-dev.sh \(versionArgument) \(signingArgument) --output \(outputArgument)",
            install: "cd \(repositoryArgument) && ./client/relay/scripts/install-local-dev.sh install \(versionArgument) --source-dir \(outputArgument) --upstream \(upstreamArgument) --acknowledge-local-development-source --acknowledge-local-source"
        )
    }

    static func suggestedOutputDirectory(parent: URL, version: String) -> String {
        let suffix = isExplicitVersion(version) ? version : "local-dev"
        return parent.appending(path: "OpenCodexRelay-\(suffix)", directoryHint: .isDirectory).path
    }

    static func shellQuote(_ value: String) -> String {
        "'\(value.replacingOccurrences(of: "'", with: "'\"'\"'"))'"
    }

    static func isExplicitVersion(_ value: String) -> Bool {
        value.range(
            of: #"^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$"#,
            options: .regularExpression
        ) != nil
    }

    private static func validateRepository(
        _ value: String,
        fileManager: FileManager
    ) -> LocalDevelopmentIntegrationFieldValidation {
        guard !value.isEmpty else { return .empty }
        guard let repository = safeAbsoluteURL(value),
              isSafeDirectory(repository, fileManager: fileManager) else {
            return .invalid(.repositoryUnsafe)
        }
        guard requiredScripts.allSatisfy({ relativePath in
            isExecutableRegularFile(
                repository.appending(path: relativePath),
                fileManager: fileManager
            )
        }) else {
            return .invalid(.repositoryScriptsMissing)
        }
        return .valid
    }

    private static func validateVersion(_ value: String) -> LocalDevelopmentIntegrationFieldValidation {
        guard !value.isEmpty else { return .empty }
        return isExplicitVersion(value) ? .valid : .invalid(.versionInvalid)
    }

    private static func validateOutput(
        _ value: String,
        fileManager: FileManager
    ) -> LocalDevelopmentIntegrationFieldValidation {
        guard !value.isEmpty else { return .empty }
        return safeOutputURL(value, fileManager: fileManager) == nil
            ? .invalid(.outputUnsafe)
            : .valid
    }

    private static func validateGateway(_ value: String) -> LocalDevelopmentIntegrationFieldValidation {
        guard !value.isEmpty else { return .empty }
        return isHTTPSV1URL(value) ? .valid : .invalid(.gatewayInvalid)
    }

    private static func validateSigningSource(
        _ input: LocalDevelopmentIntegrationInput,
        fileManager: FileManager
    ) -> LocalDevelopmentIntegrationFieldValidation {
        guard !input.signingValue.isEmpty else { return .empty }
        switch input.signingSource {
        case .pemFile:
            guard let url = safeAbsoluteURL(input.signingValue),
                  isRegularFile(url),
                  fileManager.isReadableFile(atPath: url.path) else {
                return .invalid(.pemFileUnsafe)
            }
            return .valid
        case .keychainService:
            return input.signingValue.range(
                of: #"^[0-9A-Za-z._-]{1,160}$"#,
                options: .regularExpression
            ) == nil ? .invalid(.keychainServiceInvalid) : .valid
        }
    }

    private static func safeAbsoluteURL(_ value: String) -> URL? {
        guard !value.isEmpty,
              value.hasPrefix("/"),
              !value.contains("\0"),
              !value.contains("\n"),
              !value.contains("\r") else {
            return nil
        }
        let url = URL(fileURLWithPath: value).standardizedFileURL
        guard url.path == value,
              url.resolvingSymlinksInPath().path == value else {
            return nil
        }
        return url
    }

    private static func safeOutputURL(_ value: String, fileManager: FileManager) -> URL? {
        guard let output = safeAbsoluteURL(value) else { return nil }
        if fileManager.fileExists(atPath: output.path) {
            return isSafeDirectory(output, fileManager: fileManager) ? output : nil
        }
        return isSafeDirectory(output.deletingLastPathComponent(), fileManager: fileManager)
            ? output
            : nil
    }

    private static func isSafeDirectory(_ url: URL, fileManager: FileManager) -> Bool {
        var isDirectory: ObjCBool = false
        return fileManager.fileExists(atPath: url.path, isDirectory: &isDirectory) &&
            isDirectory.boolValue &&
            url.resolvingSymlinksInPath().path == url.path
    }

    private static func isExecutableRegularFile(_ url: URL, fileManager: FileManager) -> Bool {
        guard isRegularFile(url), fileManager.isExecutableFile(atPath: url.path) else { return false }
        return url.resolvingSymlinksInPath().path == url.path
    }

    private static func isRegularFile(_ url: URL) -> Bool {
        var metadata = stat()
        guard url.path.withCString({ lstat($0, &metadata) }) == 0 else { return false }
        return metadata.st_mode & S_IFMT == S_IFREG
    }

    private static func isHTTPSV1URL(_ value: String) -> Bool {
        guard !value.contains("\n"), !value.contains("\r"),
              let components = URLComponents(string: value),
              components.scheme == "https",
              components.host?.isEmpty == false,
              components.user == nil,
              components.password == nil,
              components.query == nil,
              components.fragment == nil,
              components.path == "/v1" else {
            return false
        }
        return true
    }
}
