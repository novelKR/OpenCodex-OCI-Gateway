import AppKit
import SwiftUI
import UniformTypeIdentifiers
import OpenCodexRelayCore
import OpenCodexRelayLocalization

struct CodexConfigurationActionAvailability: Equatable {
    let canRefresh: Bool
    let canOpenExternally: Bool

    static func resolve(
        hasMetadata: Bool,
        isOpeningExternally: Bool
    ) -> Self {
        Self(
            canRefresh: true,
            canOpenExternally: hasMetadata && !isOpeningExternally
        )
    }
}

enum CodexConfigurationOpenDestination: String, CaseIterable, Identifiable, Sendable {
    case systemDefault = "system_default"
    case visualStudioCode = "visual_studio_code"
    case xcode
    case textEdit = "text_edit"
    case other

    var id: String { rawValue }

    var bundleIdentifier: String? {
        switch self {
        case .visualStudioCode: "com.microsoft.VSCode"
        case .xcode: "com.apple.dt.Xcode"
        case .textEdit: "com.apple.TextEdit"
        case .systemDefault, .other: nil
        }
    }
}

enum CodexConfigurationExternalOpenResult: String, Equatable, Sendable {
    case opened
    case cancelled
    case applicationUnavailable = "application_unavailable"
    case failed
}

@MainActor
protocol CodexConfigurationExternalOpening: AnyObject {
    func availableDestinations() -> [CodexConfigurationOpenDestination]
    func open(
        fileURL: URL,
        destination: CodexConfigurationOpenDestination
    ) async -> CodexConfigurationExternalOpenResult
}

@MainActor
final class NSWorkspaceCodexConfigurationExternalOpener: CodexConfigurationExternalOpening {
    private let defaultApplicationURL: (URL) -> URL?
    private let applicationURL: (String) -> URL?
    private let applicationChooser: () -> URL?
    private let applicationOpen: (URL, URL) async -> Bool

    init(workspace: NSWorkspace = .shared) {
        defaultApplicationURL = { workspace.urlForApplication(toOpen: $0) }
        applicationURL = { workspace.urlForApplication(withBundleIdentifier: $0) }
        applicationChooser = Self.chooseApplication
        applicationOpen = { fileURL, applicationURL in
            await Self.open(
                fileURL: fileURL,
                applicationURL: applicationURL,
                workspace: workspace
            )
        }
    }

    init(
        defaultApplicationURL: @escaping (URL) -> URL?,
        applicationURL: @escaping (String) -> URL?,
        applicationChooser: @escaping () -> URL?,
        applicationOpen: @escaping (URL, URL) async -> Bool
    ) {
        self.defaultApplicationURL = defaultApplicationURL
        self.applicationURL = applicationURL
        self.applicationChooser = applicationChooser
        self.applicationOpen = applicationOpen
    }

    func availableDestinations() -> [CodexConfigurationOpenDestination] {
        var destinations: [CodexConfigurationOpenDestination] = [.systemDefault]
        destinations.append(contentsOf: [
            .visualStudioCode,
            .xcode,
            .textEdit,
        ].filter { destination in
            destination.bundleIdentifier.flatMap(applicationURL) != nil
        })
        destinations.append(.other)
        return destinations
    }

    func open(
        fileURL: URL,
        destination: CodexConfigurationOpenDestination
    ) async -> CodexConfigurationExternalOpenResult {
        let selectedApplication: URL?
        switch destination {
        case .systemDefault:
            selectedApplication = defaultApplicationURL(fileURL)
        case .visualStudioCode, .xcode, .textEdit:
            selectedApplication = destination.bundleIdentifier.flatMap(applicationURL)
        case .other:
            guard let chosenApplication = applicationChooser() else {
                return .cancelled
            }
            selectedApplication = chosenApplication
        }

        guard let selectedApplication,
              selectedApplication.pathExtension.caseInsensitiveCompare("app") == .orderedSame,
              Bundle(url: selectedApplication)?.bundleIdentifier != nil else {
            return .applicationUnavailable
        }
        return await applicationOpen(fileURL, selectedApplication) ? .opened : .failed
    }

    private static func chooseApplication() -> URL? {
        let panel = NSOpenPanel()
        panel.allowedContentTypes = [.applicationBundle]
        panel.allowsMultipleSelection = false
        panel.canChooseDirectories = false
        panel.canChooseFiles = true
        panel.directoryURL = URL(fileURLWithPath: "/Applications", isDirectory: true)
        panel.treatsFilePackagesAsDirectories = false
        return panel.runModal() == .OK ? panel.url : nil
    }

    private static func open(
        fileURL: URL,
        applicationURL: URL,
        workspace: NSWorkspace
    ) async -> Bool {
        let configuration = NSWorkspace.OpenConfiguration()
        configuration.activates = true
        configuration.addsToRecentItems = false
        configuration.allowsRunningApplicationSubstitution = false
        configuration.promptsUserIfNeeded = true
        return await withCheckedContinuation { continuation in
            workspace.open(
                [fileURL],
                withApplicationAt: applicationURL,
                configuration: configuration
            ) { _, error in
                continuation.resume(returning: error == nil)
            }
        }
    }
}

@MainActor
final class CodexConfigurationController: ObservableObject {
    private enum ObservationSignature: Equatable {
        case available(CodexConfigurationFileIdentity)
        case unavailable(String)
    }

    @Published private(set) var metadata: CodexConfigurationMetadata?
    @Published private(set) var metadataFailureCode: String?
    @Published private(set) var previewText: String?
    @Published private(set) var previewFailureCode: String?
    @Published private(set) var externalOpenResult: CodexConfigurationExternalOpenResult?
    @Published private(set) var hasChangedSinceReview = false
    @Published private(set) var isOpeningExternally = false
    @Published private(set) var availableOpenDestinations: [CodexConfigurationOpenDestination]

    private let bindingURL: URL
    private let reader: any CodexConfigurationReading
    private let opener: any CodexConfigurationExternalOpening
    private let activityLog: RelayActivityLogStore
    private let observationIntervalNanoseconds: UInt64
    private let onStatusRefreshRequested: () -> Void
    private var observationTask: Task<Void, Never>?
    private var lastObservationSignature: ObservationSignature?

    init(
        bindingURL: URL,
        reader: any CodexConfigurationReading = SecureCodexConfigurationReader(),
        opener: any CodexConfigurationExternalOpening = NSWorkspaceCodexConfigurationExternalOpener(),
        activityLog: RelayActivityLogStore,
        observationIntervalNanoseconds: UInt64 = 2_000_000_000,
        onStatusRefreshRequested: @escaping () -> Void
    ) {
        self.bindingURL = bindingURL
        self.reader = reader
        self.opener = opener
        self.activityLog = activityLog
        self.observationIntervalNanoseconds = observationIntervalNanoseconds
        self.onStatusRefreshRequested = onStatusRefreshRequested
        self.availableOpenDestinations = opener.availableDestinations()
    }

    deinit {
        observationTask?.cancel()
    }

    var canPreview: Bool {
        guard let metadata else { return false }
        return metadata.byteCount <= SecureCodexConfigurationReader.maximumPreviewBytes
    }

    func startObserving() {
        guard observationTask == nil else { return }
        refreshMetadata()
        observationTask = Task { [weak self] in
            guard let self else { return }
            while !Task.isCancelled {
                do {
                    try await Task.sleep(nanoseconds: observationIntervalNanoseconds)
                } catch {
                    return
                }
                if Task.isCancelled { return }
                refreshMetadata()
            }
        }
    }

    func stopObserving() {
        observationTask?.cancel()
        observationTask = nil
    }

    func refreshMetadata() {
        do {
            apply(metadata: try reader.inspect(bindingURL: bindingURL))
        } catch {
            applyFailure(code: safeCode(for: error))
        }
        availableOpenDestinations = opener.availableDestinations()
    }

    @discardableResult
    func revealPreview() -> Bool {
        previewText = nil
        previewFailureCode = nil
        do {
            let document = try reader.readDocument(bindingURL: bindingURL)
            apply(metadata: document.metadata)
            previewText = document.contents
            hasChangedSinceReview = false
            activityLog.record(
                category: .operation,
                code: "preview_revealed",
                fields: ["result_code": "opened"]
            )
            return true
        } catch {
            previewFailureCode = safeCode(for: error)
            return false
        }
    }

    func dismissPreview() {
        previewText = nil
        previewFailureCode = nil
    }

    func openExternally(_ destination: CodexConfigurationOpenDestination) async {
        guard !isOpeningExternally else { return }
        isOpeningExternally = true
        externalOpenResult = nil
        activityLog.record(
            category: .operation,
            code: "external_open_requested",
            fields: ["destination": destination.rawValue]
        )
        defer { isOpeningExternally = false }

        let result: CodexConfigurationExternalOpenResult
        do {
            let currentMetadata = try reader.inspect(bindingURL: bindingURL)
            apply(metadata: currentMetadata)
            result = await opener.open(
                fileURL: currentMetadata.fileURL,
                destination: destination
            )
        } catch {
            applyFailure(code: safeCode(for: error))
            result = .failed
        }
        externalOpenResult = result
        activityLog.record(
            result == .opened || result == .cancelled ? .info : .error,
            category: .operation,
            code: "external_open_finished",
            fields: [
                "destination": destination.rawValue,
                "result_code": result.rawValue,
            ]
        )
    }

    private func apply(metadata newMetadata: CodexConfigurationMetadata) {
        let nextSignature = ObservationSignature.available(newMetadata.identity)
        recordChangeIfNeeded(nextSignature, resultCode: "available")
        metadata = newMetadata
        metadataFailureCode = nil
    }

    private func applyFailure(code: String) {
        let nextSignature = ObservationSignature.unavailable(code)
        recordChangeIfNeeded(nextSignature, resultCode: code)
        metadata = nil
        metadataFailureCode = code
    }

    private func recordChangeIfNeeded(
        _ nextSignature: ObservationSignature,
        resultCode: String
    ) {
        defer { lastObservationSignature = nextSignature }
        guard let previous = lastObservationSignature, previous != nextSignature else {
            return
        }
        hasChangedSinceReview = true
        activityLog.record(
            category: .status,
            code: "config_changed",
            fields: [
                "changed": "true",
                "result_code": resultCode,
            ]
        )
        onStatusRefreshRequested()
    }

    private func safeCode(for error: Error) -> String {
        (error as? CodexConfigurationFileError)?.safeCode ?? "config_read_failed"
    }
}

struct CodexConfigurationCard: View {
    @ObservedObject var model: MenuBarModel
    @ObservedObject var controller: CodexConfigurationController
    let requestPreview: () -> Void

    private var actionAvailability: CodexConfigurationActionAvailability {
        .resolve(
            hasMetadata: controller.metadata != nil,
            isOpeningExternally: controller.isOpeningExternally
        )
    }
    @EnvironmentObject private var localization: LocalizationStore

    private var localizer: AppLocalizer { localization.localizer }

    var body: some View {
        ControlCenterSectionCard(
            localizer.text(.codexConfigTitle),
            systemImage: "doc.text"
        ) {
            VStack(alignment: .leading, spacing: 12) {
                if let metadata = controller.metadata {
                    CodexConfigurationLocationRow(
                        label: localizer.text(.codexConfigLocation),
                        value: (metadata.location as NSString).abbreviatingWithTildeInPath,
                        localizer: localizer
                    )
                    StatusRow(
                        localizer.text(.codexConfigExists),
                        value: localizer.text(.codexConfigExistsYes),
                        showsDivider: true
                    )
                    StatusRow(
                        localizer.text(.codexConfigSize),
                        value: ByteCountFormatter.string(
                            fromByteCount: Int64(metadata.byteCount),
                            countStyle: .file
                        ),
                        showsDivider: true
                    )
                    StatusRow(
                        localizer.text(.codexConfigModified),
                        value: localizer.formattedDate(metadata.modifiedAt),
                        showsDivider: true
                    )
                } else {
                    StatusRow(
                        localizer.text(.codexConfigExists),
                        value: localizer.text(.codexConfigExistsNo),
                        showsDivider: true
                    )
                }

                StatusRow(
                    localizer.text(.codexConfigChanged),
                    value: localizer.text(
                        controller.hasChangedSinceReview
                            ? .codexConfigChangedYes
                            : .codexConfigChangedNo
                    ),
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.viewPhase),
                    value: model.status.map { localizer.displayName($0.phase) }
                        ?? localizer.text(.genericUnknown),
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.viewAppliedBackend),
                    value: model.status.map { localizer.displayName($0.appliedBackend) }
                        ?? localizer.text(.genericUnknown),
                    showsDivider: true
                )

                if let failureCode = controller.metadataFailureCode {
                    ControlCenterSupportingText(
                        localizer.text(errorKey(for: failureCode)),
                        systemImage: "exclamationmark.triangle"
                    )
                } else if !controller.canPreview {
                    ControlCenterSupportingText(
                        localizer.text(.codexConfigPreviewTooLarge),
                        systemImage: "doc.badge.ellipsis"
                    )
                }

                if let failureCode = controller.previewFailureCode {
                    ControlCenterSupportingText(
                        localizer.text(errorKey(for: failureCode)),
                        systemImage: "exclamationmark.triangle"
                    )
                }
                if let externalOpenResult = controller.externalOpenResult {
                    ControlCenterSupportingText(
                        localizer.text(openResultKey(externalOpenResult)),
                        systemImage: externalOpenResult == .opened
                            ? "checkmark.circle"
                            : "exclamationmark.triangle"
                    )
                }

                Divider()
                ControlCenterSupportingText(
                    localizer.text(.codexConfigReadOnlyDetail),
                    systemImage: "lock.open.display"
                )

                ControlCenterActionFooter {
                    Menu {
                        Button {
                            controller.refreshMetadata()
                        } label: {
                            Label(localizer.text(.codexConfigRefresh), systemImage: "arrow.clockwise")
                        }
                        .accessibilityHint(localizer.text(.codexConfigRefreshHint))
                        .disabled(!actionAvailability.canRefresh)

                        Divider()
                        ForEach(controller.availableOpenDestinations) { destination in
                            Button(localizer.text(titleKey(for: destination))) {
                                Task {
                                    await controller.openExternally(destination)
                                }
                            }
                            .disabled(!actionAvailability.canOpenExternally)
                        }
                    } label: {
                        Label(localizer.text(.controlCenterMoreActions), systemImage: "ellipsis.circle")
                    }
                } primary: {
                    Button {
                        requestPreview()
                    } label: {
                        Label(localizer.text(.codexConfigPreview), systemImage: "doc.text.magnifyingglass")
                    }
                    .buttonStyle(.glassProminent)
                    .disabled(!controller.canPreview)
                    .accessibilityHint(localizer.text(.codexConfigPreviewHint))
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.vertical, 4)
        }
        .onAppear {
            controller.startObserving()
        }
        .onDisappear {
            controller.stopObserving()
        }
    }

    private func titleKey(for destination: CodexConfigurationOpenDestination) -> AppStringKey {
        switch destination {
        case .systemDefault: .codexConfigOpenDefault
        case .visualStudioCode: .codexConfigOpenVSCode
        case .xcode: .codexConfigOpenXcode
        case .textEdit: .codexConfigOpenTextEdit
        case .other: .codexConfigOpenOther
        }
    }

    private func errorKey(for code: String) -> AppStringKey {
        switch code {
        case "config_preview_mode": .codexConfigPreviewMode
        case "config_binding_missing": .codexConfigBindingMissing
        case "config_binding_unsafe": .codexConfigBindingUnsafe
        case "config_binding_invalid": .codexConfigBindingInvalid
        case "config_file_missing": .codexConfigFileMissing
        case "config_file_unsafe": .codexConfigFileUnsafe
        case "config_file_changed": .codexConfigFileChanged
        case "config_preview_too_large": .codexConfigPreviewTooLarge
        case "config_preview_not_utf8": .codexConfigPreviewNotUTF8
        default: .codexConfigReadFailed
        }
    }

    private func openResultKey(
        _ result: CodexConfigurationExternalOpenResult
    ) -> AppStringKey {
        switch result {
        case .opened: .codexConfigOpenSucceeded
        case .cancelled: .codexConfigOpenCancelled
        case .applicationUnavailable: .codexConfigOpenApplicationUnavailable
        case .failed: .codexConfigOpenFailed
        }
    }
}

private struct CodexConfigurationLocationRow: View {
    let label: String
    let value: String
    let localizer: AppLocalizer

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(label)
                .font(.subheadline.weight(.medium))
                .foregroundStyle(.secondary)
            Text(value)
                .font(.system(.body, design: .monospaced))
                .lineLimit(3)
                .truncationMode(.middle)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel(localizer.text(.viewStatusRowAccessibility, label, value))
    }
}

struct CodexConfigurationPreviewView: View {
    let contents: String
    let localizer: AppLocalizer
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack(alignment: .firstTextBaseline) {
                Label(localizer.text(.codexConfigPreviewTitle), systemImage: "doc.text")
                    .font(.title2.weight(.semibold))
                Spacer()
                Button(localizer.text(.codexConfigPreviewClose)) {
                    dismiss()
                }
                .keyboardShortcut(.cancelAction)
            }

            ControlCenterSupportingText(
                localizer.text(.codexConfigPreviewSessionDetail),
                systemImage: "eye"
            )

            TOMLPreviewTextView(contents: contents)
                .background(.background.secondary, in: RoundedRectangle(cornerRadius: 10))
        }
        .padding(20)
        .frame(minWidth: 520, idealWidth: 760, minHeight: 320, idealHeight: 520)
    }
}
