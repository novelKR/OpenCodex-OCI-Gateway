import SwiftUI
import OpenCodexRelayLocalization

enum LocalDevelopmentProducerTools {
    static let sceneID = "local-development-producer-tools"
}

struct LocalDevelopmentProducerCommands: Commands {
    @Environment(\.openWindow) private var openWindow
    let title: String
    let actionTitle: String

    var body: some Commands {
        CommandMenu(title) {
            Button(actionTitle) {
                openWindow(id: LocalDevelopmentProducerTools.sceneID)
            }
        }
    }
}

struct LocalDevelopmentIntegrationCard: View {
    @ObservedObject var model: MenuBarModel
    let localizer: AppLocalizer

    @State private var isPresentingGuide = false

    var body: some View {
        if model.canShowLocalDevelopmentIntegrationGuide {
            ControlCenterSectionCard(
                localizer.text(.integrationGuideCardTitle),
                systemImage: "wrench.and.screwdriver"
            ) {
                VStack(alignment: .leading, spacing: 12) {
                    ControlCenterSupportingText(
                        localizer.text(.integrationGuideCardDetail),
                        systemImage: "hand.raised"
                    )
                    HStack {
                        Spacer()
                        Button(localizer.text(.integrationGuideOpen)) {
                            model.recordIntegrationGuidePresented()
                            isPresentingGuide = true
                        }
                    }
                }
                .padding(.vertical, 6)
            }
            .sheet(isPresented: $isPresentingGuide) {
                LocalDevelopmentIntegrationGuideView(model: model, localizer: localizer)
            }
        }
    }
}

struct LocalDevelopmentIntegrationGuideView: View {
    @ObservedObject private var model: MenuBarModel
    let localizer: AppLocalizer

    @StateObject private var guide: LocalDevelopmentIntegrationGuideModel
    private let fileSelector: any LocalDevelopmentIntegrationFileSelecting
    @Environment(\.dismiss) private var dismiss

    init(
        model: MenuBarModel,
        localizer: AppLocalizer,
        fileSelector: any LocalDevelopmentIntegrationFileSelecting =
            SystemLocalDevelopmentIntegrationFileSelector()
    ) {
        self.model = model
        self.localizer = localizer
        self.fileSelector = fileSelector
        _guide = StateObject(wrappedValue: LocalDevelopmentIntegrationGuideModel())
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            Label(localizer.text(.integrationGuideTitle), systemImage: "wrench.and.screwdriver")
                .font(.title2.weight(.bold))
                .accessibilityAddTraits(.isHeader)

            Text(localizer.text(.integrationGuideDetail))
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)

            stepIndicator

            Divider()

            ScrollView {
                VStack(alignment: .leading, spacing: 18) {
                    stepContent
                    advancedEditor
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.trailing, 8)
            }

            Divider()

            footer
        }
        .padding(24)
        .frame(minWidth: 720, idealWidth: 780, minHeight: 520, idealHeight: 640)
    }

    private var stepIndicator: some View {
        HStack(spacing: 6) {
            ForEach(LocalDevelopmentIntegrationStep.allCases) { step in
                Button {
                    guide.go(to: step)
                } label: {
                    HStack(spacing: 5) {
                        Image(systemName: step.rawValue < guide.currentStep.rawValue
                            ? "checkmark.circle.fill"
                            : "\(step.rawValue + 1).circle.fill")
                        Text(localizer.text(stepTitleKey(step)))
                            .lineLimit(1)
                    }
                    .font(.caption.weight(step == guide.currentStep ? .semibold : .regular))
                    .foregroundStyle(step == guide.currentStep ? Color.accentColor : .secondary)
                    .padding(.horizontal, 8)
                    .padding(.vertical, 6)
                    .background(
                        step == guide.currentStep ? Color.accentColor.opacity(0.12) : Color.clear,
                        in: Capsule()
                    )
                }
                .buttonStyle(.plain)
                .disabled(step.rawValue > guide.currentStep.rawValue)
            }
        }
        .accessibilityElement(children: .contain)
    }

    @ViewBuilder
    private var stepContent: some View {
        switch guide.currentStep {
        case .source:
            sourceStep
        case .package:
            packageStep
        case .gateway:
            gatewayStep
        case .signing:
            signingStep
        case .review:
            reviewStep
        }
    }

    private var sourceStep: some View {
        stepSection(title: .integrationGuideStepSource, detail: .integrationGuideSourceDetail) {
            labeledValue(localizer.text(.integrationGuideRepository), value: guide.repositoryRoot)
            Button(localizer.text(.integrationGuideChooseRepository)) {
                guide.chooseRepository(
                    using: fileSelector,
                    title: localizer.text(.integrationGuideRepositorySelectionTitle),
                    message: localizer.text(.integrationGuideRepositorySelectionMessage)
                )
            }
            validationFeedback(.repository)
        }
    }

    private var packageStep: some View {
        stepSection(title: .integrationGuideStepPackage, detail: .integrationGuidePackageDetail) {
            labeledTextField(
                label: localizer.text(.integrationGuideVersion),
                text: Binding(get: { guide.version }, set: { guide.setVersion($0) }),
                prompt: "1.2.3-dev.1"
            )
            supportingText(.integrationGuideVersionHelp)
            validationFeedback(.version)

            Divider()

            labeledValue(localizer.text(.integrationGuideOutput), value: guide.outputDirectory)
            supportingText(.integrationGuideOutputHelp)
            Button(localizer.text(.integrationGuideChooseOutput)) {
                guide.chooseOutputParent(
                    using: fileSelector,
                    title: localizer.text(.integrationGuideOutputSelectionTitle),
                    message: localizer.text(.integrationGuideOutputSelectionMessage)
                )
            }
            validationFeedback(.output)
        }
    }

    private var gatewayStep: some View {
        stepSection(title: .integrationGuideStepGateway, detail: .integrationGuideGatewayDetail) {
            labeledTextField(
                label: localizer.text(.integrationGuideUpstream),
                text: Binding(get: { guide.upstreamURL }, set: { guide.setUpstreamURL($0) }),
                prompt: "https://gateway.example.com/v1"
            )
            supportingText(.integrationGuideGatewayHelp)
            validationFeedback(.gateway)
        }
    }

    private var signingStep: some View {
        stepSection(title: .integrationGuideStepSigning, detail: .integrationGuideSigningDetail) {
            ControlCenterNotice(tone: .info) {
                Text(localizer.text(.integrationGuideSigningNotCredential))
                    .fixedSize(horizontal: false, vertical: true)
            }

            Picker(
                localizer.text(.integrationGuideSigningSource),
                selection: Binding(get: { guide.signingSource }, set: { guide.setSigningSource($0) })
            ) {
                Text(localizer.text(.integrationGuideSigningKeychain))
                    .tag(LocalDevelopmentSigningSource.keychainService)
                Text(localizer.text(.integrationGuideSigningPEM))
                    .tag(LocalDevelopmentSigningSource.pemFile)
            }
            .pickerStyle(.segmented)

            if guide.signingSource == .keychainService {
                labeledTextField(
                    label: localizer.text(.integrationGuideKeychainService),
                    text: Binding(get: { guide.keychainService }, set: { guide.setKeychainService($0) }),
                    prompt: ""
                )
                supportingText(.integrationGuideKeychainHelp)
                Picker(
                    localizer.text(.integrationGuideKeychainChoice),
                    selection: $guide.prepareKeychainSigningKey
                ) {
                    Text(localizer.text(.integrationGuideKeychainPrepare)).tag(true)
                    Text(localizer.text(.integrationGuideKeychainExisting)).tag(false)
                }
                .pickerStyle(.radioGroup)
            } else {
                labeledValue(localizer.text(.integrationGuidePEMFile), value: guide.pemFilePath)
                supportingText(.integrationGuidePEMHelp)
                Button(localizer.text(.integrationGuideChoosePEM)) {
                    guide.choosePEMFile(
                        using: fileSelector,
                        title: localizer.text(.integrationGuidePEMSelectionTitle),
                        message: localizer.text(.integrationGuidePEMSelectionMessage)
                    )
                }
            }
            validationFeedback(.signing)
        }
    }

    private var reviewStep: some View {
        stepSection(title: .integrationGuideStepReview, detail: .integrationGuideReviewDetail) {
            VStack(alignment: .leading, spacing: 10) {
                reviewRow(
                    .integrationGuideRepository,
                    value: guide.repositoryRoot,
                    field: .repository
                )
                reviewRow(.integrationGuideVersion, value: guide.version, field: .version)
                reviewRow(.integrationGuideOutput, value: guide.outputDirectory, field: .output)
                reviewRow(.integrationGuideUpstream, value: guide.upstreamURL, field: .gateway)
                reviewRow(
                    guide.signingSource == .keychainService
                        ? .integrationGuideKeychainService
                        : .integrationGuidePEMFile,
                    value: guide.signingSource == .keychainService
                        ? guide.keychainService
                        : guide.pemFilePath,
                    field: .signing
                )
            }

            if guide.validation.allRequiredFieldsAreValid {
                ControlCenterNotice(tone: .success) {
                    Text(localizer.text(.integrationGuideReviewReady))
                }
            } else {
                ControlCenterNotice(tone: .warning) {
                    Text(localizer.text(.integrationGuideReviewCorrectionRequired))
                }
            }

            Toggle(localizer.text(.integrationGuideAcknowledge), isOn: $guide.acknowledgedSource)
                .fixedSize(horizontal: false, vertical: true)

            if let commands = guide.commands {
                if let signingSetup = commands.signingSetup {
                    commandBlock(
                        position: 1,
                        title: .integrationGuideSigningSetupCommand,
                        copyTitle: .integrationGuideCopySigningSetup,
                        command: signingSetup,
                        kind: "signing_setup"
                    )
                }
                commandBlock(
                    position: commands.signingSetup == nil ? 1 : 2,
                    title: .integrationGuideBuildCommand,
                    copyTitle: .integrationGuideCopyBuild,
                    command: commands.build,
                    kind: "build"
                )
                commandBlock(
                    position: commands.signingSetup == nil ? 2 : 3,
                    title: .integrationGuideInstallCommand,
                    copyTitle: .integrationGuideCopyInstall,
                    command: commands.install,
                    kind: "install"
                )
            } else if guide.validation.allRequiredFieldsAreValid {
                ControlCenterNotice(tone: .warning) {
                    Text(localizer.text(.integrationGuideReviewAcknowledgeRequired))
                }
            }
        }
    }

    private var advancedEditor: some View {
        DisclosureGroup(isExpanded: $guide.isAdvancedExpanded) {
            VStack(alignment: .leading, spacing: 12) {
                labeledTextField(
                    label: localizer.text(.integrationGuideRepository),
                    text: Binding(get: { guide.repositoryRoot }, set: { guide.setRepositoryRoot($0) }),
                    prompt: ""
                )
                validationFeedback(.repository)
                labeledTextField(
                    label: localizer.text(.integrationGuideVersion),
                    text: Binding(get: { guide.version }, set: { guide.setVersion($0) }),
                    prompt: "1.2.3-dev.1"
                )
                validationFeedback(.version)
                labeledTextField(
                    label: localizer.text(.integrationGuideOutput),
                    text: Binding(get: { guide.outputDirectory }, set: { guide.setOutputDirectory($0) }),
                    prompt: ""
                )
                validationFeedback(.output)
                labeledTextField(
                    label: localizer.text(.integrationGuideUpstream),
                    text: Binding(get: { guide.upstreamURL }, set: { guide.setUpstreamURL($0) }),
                    prompt: "https://gateway.example.com/v1"
                )
                validationFeedback(.gateway)
                labeledTextField(
                    label: localizer.text(
                        guide.signingSource == .keychainService
                            ? .integrationGuideKeychainService
                            : .integrationGuidePEMFile
                    ),
                    text: Binding(
                        get: {
                            guide.signingSource == .keychainService
                                ? guide.keychainService
                                : guide.pemFilePath
                        },
                        set: {
                            if guide.signingSource == .keychainService {
                                guide.setKeychainService($0)
                            } else {
                                guide.setPEMFilePath($0)
                            }
                        }
                    ),
                    prompt: ""
                )
                validationFeedback(.signing)
            }
            .padding(.top, 12)
        } label: {
            Text(localizer.text(.integrationGuideAdvanced))
                .font(.headline)
        }
    }

    private var footer: some View {
        HStack {
            Button(localizer.text(.integrationGuideClose)) { dismiss() }
                .keyboardShortcut(.cancelAction)
            Spacer()
            Button(localizer.text(.integrationGuideBack)) { guide.goBack() }
                .disabled(!guide.canGoBack)
            if guide.currentStep != .review {
                Button(localizer.text(.integrationGuideNext)) { guide.advance() }
                    .buttonStyle(.glassProminent)
                    .disabled(!guide.canAdvance)
                    .keyboardShortcut(.defaultAction)
            }
        }
    }

    private func stepSection<Content: View>(
        title: AppStringKey,
        detail: AppStringKey,
        @ViewBuilder content: () -> Content
    ) -> some View {
        VStack(alignment: .leading, spacing: 14) {
            Text(localizer.text(title))
                .font(.title3.weight(.semibold))
                .accessibilityAddTraits(.isHeader)
            Text(localizer.text(detail))
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            content()
        }
    }

    private func labeledTextField(label: String, text: Binding<String>, prompt: String) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(label).font(.subheadline.weight(.medium))
            TextField(prompt, text: text)
                .textFieldStyle(.roundedBorder)
                .privacySensitive()
                .accessibilityLabel(label)
        }
    }

    private func labeledValue(_ label: String, value: String) -> some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(label).font(.subheadline.weight(.medium))
            Text(value.isEmpty ? localizer.text(.integrationGuideValidationRequired) : value)
                .font(.callout.monospaced())
                .foregroundStyle(value.isEmpty ? .tertiary : .primary)
                .textSelection(.enabled)
                .privacySensitive()
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(8)
                .background(.quaternary, in: RoundedRectangle(cornerRadius: 6))
        }
    }

    private func supportingText(_ key: AppStringKey) -> some View {
        Text(localizer.text(key))
            .font(.caption)
            .foregroundStyle(.secondary)
            .fixedSize(horizontal: false, vertical: true)
    }

    private func reviewRow(
        _ key: AppStringKey,
        value: String,
        field: LocalDevelopmentIntegrationField
    ) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline, spacing: 12) {
                Text(localizer.text(key))
                    .foregroundStyle(.secondary)
                    .frame(width: 150, alignment: .leading)
                Text(value)
                    .font(.callout.monospaced())
                    .textSelection(.enabled)
                    .privacySensitive()
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            validationFeedback(field)
        }
    }

    private func commandBlock(
        position: Int,
        title: AppStringKey,
        copyTitle: AppStringKey,
        command: String,
        kind: String
    ) -> some View {
        VStack(alignment: .leading, spacing: 8) {
            Text("\(position). \(localizer.text(title))").font(.headline)
            ScrollView(.horizontal) {
                Text(command)
                    .font(.caption.monospaced())
                    .textSelection(.enabled)
                    .privacySensitive()
                    .padding(10)
            }
            .background(.quaternary, in: RoundedRectangle(cornerRadius: 8))
            Button(localizer.text(copyTitle)) {
                model.copyIntegrationGuideCommand(command, kind: kind)
            }
        }
        .padding(.top, 4)
    }

    private func validationFeedback(_ field: LocalDevelopmentIntegrationField) -> some View {
        let state = guide.validation.state(for: field)
        return Label(
            localizer.text(validationKey(state)),
            systemImage: state.isValid ? "checkmark.circle.fill" : "exclamationmark.circle.fill"
        )
        .font(.caption)
        .foregroundStyle(state.isValid ? Color.green : Color.orange)
        .fixedSize(horizontal: false, vertical: true)
        .accessibilityElement(children: .combine)
    }

    private func validationKey(
        _ state: LocalDevelopmentIntegrationFieldValidation
    ) -> AppStringKey {
        switch state {
        case .empty:
            .integrationGuideValidationRequired
        case .valid:
            .integrationGuideValidationValid
        case let .invalid(code):
            switch code {
            case .repositoryUnsafe: .integrationGuideValidationRepositoryUnsafe
            case .repositoryScriptsMissing: .integrationGuideValidationRepositoryScriptsMissing
            case .versionInvalid: .integrationGuideValidationVersionInvalid
            case .outputUnsafe: .integrationGuideValidationOutputUnsafe
            case .gatewayInvalid: .integrationGuideValidationGatewayInvalid
            case .keychainServiceInvalid: .integrationGuideValidationKeychainServiceInvalid
            case .pemFileUnsafe: .integrationGuideValidationPEMFileUnsafe
            }
        }
    }

    private func stepTitleKey(_ step: LocalDevelopmentIntegrationStep) -> AppStringKey {
        switch step {
        case .source: .integrationGuideStepSource
        case .package: .integrationGuideStepPackage
        case .gateway: .integrationGuideStepGateway
        case .signing: .integrationGuideStepSigning
        case .review: .integrationGuideStepReview
        }
    }
}
