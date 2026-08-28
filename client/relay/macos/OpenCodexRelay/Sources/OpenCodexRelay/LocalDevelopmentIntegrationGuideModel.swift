import AppKit
import Foundation

@MainActor
protocol LocalDevelopmentIntegrationFileSelecting {
    func chooseDirectory(title: String, message: String) -> URL?
    func chooseFile(title: String, message: String) -> URL?
}

@MainActor
struct SystemLocalDevelopmentIntegrationFileSelector: LocalDevelopmentIntegrationFileSelecting {
    func chooseDirectory(title: String, message: String) -> URL? {
        let panel = NSOpenPanel()
        panel.title = title
        panel.message = message
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.canCreateDirectories = false
        return panel.runModal() == .OK ? panel.url : nil
    }

    func chooseFile(title: String, message: String) -> URL? {
        let panel = NSOpenPanel()
        panel.title = title
        panel.message = message
        panel.canChooseDirectories = false
        panel.canChooseFiles = true
        panel.allowsMultipleSelection = false
        panel.allowedContentTypes = [.data]
        return panel.runModal() == .OK ? panel.url : nil
    }
}

@MainActor
final class LocalDevelopmentIntegrationGuideModel: ObservableObject {
    @Published private(set) var currentStep: LocalDevelopmentIntegrationStep = .source
    @Published private(set) var repositoryRoot = ""
    @Published private(set) var version: String
    @Published private(set) var outputDirectory = ""
    @Published private(set) var upstreamURL = ""
    @Published private(set) var signingSource: LocalDevelopmentSigningSource = .keychainService
    @Published private(set) var keychainService = LocalDevelopmentIntegrationCommandBuilder.defaultKeychainService
    @Published private(set) var pemFilePath = ""
    @Published var prepareKeychainSigningKey = true
    @Published var acknowledgedSource = false
    @Published var isAdvancedExpanded = false

    private var outputParentDirectory: URL?

    init(bundleVersion: String? = Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String) {
        if let bundleVersion,
           LocalDevelopmentIntegrationCommandBuilder.isExplicitVersion(bundleVersion) {
            version = bundleVersion
        } else {
            version = ""
        }
    }

    var input: LocalDevelopmentIntegrationInput {
        LocalDevelopmentIntegrationInput(
            repositoryRoot: repositoryRoot,
            version: version,
            outputDirectory: outputDirectory,
            upstreamURL: upstreamURL,
            signingSource: signingSource,
            signingValue: signingSource == .keychainService ? keychainService : pemFilePath,
            prepareKeychainSigningKey: signingSource == .keychainService && prepareKeychainSigningKey,
            acknowledgedSource: acknowledgedSource
        )
    }

    var validation: LocalDevelopmentIntegrationValidation {
        LocalDevelopmentIntegrationCommandBuilder.validation(for: input)
    }

    var commands: LocalDevelopmentIntegrationCommands? {
        try? LocalDevelopmentIntegrationCommandBuilder.commands(for: input)
    }

    var canGoBack: Bool { currentStep != .source }

    var canAdvance: Bool {
        switch currentStep {
        case .source:
            validation.repository.isValid
        case .package:
            validation.version.isValid && validation.output.isValid
        case .gateway:
            validation.gateway.isValid
        case .signing:
            validation.signing.isValid
        case .review:
            false
        }
    }

    func advance() {
        guard canAdvance,
              let next = LocalDevelopmentIntegrationStep(rawValue: currentStep.rawValue + 1) else {
            return
        }
        currentStep = next
    }

    func goBack() {
        guard let previous = LocalDevelopmentIntegrationStep(rawValue: currentStep.rawValue - 1) else {
            return
        }
        currentStep = previous
    }

    func go(to step: LocalDevelopmentIntegrationStep) {
        guard step.rawValue <= currentStep.rawValue else { return }
        currentStep = step
    }

    func setRepositoryRoot(_ value: String) {
        repositoryRoot = value
    }

    func setVersion(_ value: String) {
        version = value
        recomputeOutputDirectory()
    }

    func setOutputDirectory(_ value: String) {
        outputParentDirectory = nil
        outputDirectory = value
    }

    func setOutputParent(_ url: URL) {
        outputParentDirectory = url
        recomputeOutputDirectory()
    }

    func setUpstreamURL(_ value: String) {
        upstreamURL = value
    }

    func setSigningSource(_ value: LocalDevelopmentSigningSource) {
        signingSource = value
    }

    func setKeychainService(_ value: String) {
        keychainService = value
    }

    func setPEMFilePath(_ value: String) {
        pemFilePath = value
    }

    func chooseRepository(
        using selector: LocalDevelopmentIntegrationFileSelecting,
        title: String,
        message: String
    ) {
        guard let url = selector.chooseDirectory(title: title, message: message) else { return }
        setRepositoryRoot(url.path)
    }

    func chooseOutputParent(
        using selector: LocalDevelopmentIntegrationFileSelecting,
        title: String,
        message: String
    ) {
        guard let url = selector.chooseDirectory(title: title, message: message) else { return }
        setOutputParent(url)
    }

    func choosePEMFile(
        using selector: LocalDevelopmentIntegrationFileSelecting,
        title: String,
        message: String
    ) {
        guard let url = selector.chooseFile(title: title, message: message) else { return }
        setPEMFilePath(url.path)
    }

    private func recomputeOutputDirectory() {
        guard let outputParentDirectory else { return }
        outputDirectory = LocalDevelopmentIntegrationCommandBuilder.suggestedOutputDirectory(
            parent: outputParentDirectory,
            version: version
        )
    }
}
