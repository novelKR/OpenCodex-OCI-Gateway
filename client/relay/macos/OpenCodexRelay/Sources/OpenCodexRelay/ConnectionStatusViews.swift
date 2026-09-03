import AppKit
import SwiftUI
import OpenCodexRelayCore
import OpenCodexRelayLocalization

enum ControlCenterLayout {
    static let popoverWidth: CGFloat = 320
    static let maximumPopoverHeight: CGFloat = 300
    static let defaultWindowSize = CGSize(width: 980, height: 680)
    static let minimumWindowSize = CGSize(width: 640, height: 440)
    static let visibleFrameInset: CGFloat = 24

    static func initialWindowSize(for visibleSize: CGSize) -> CGSize {
        let availableWidth = max(0, visibleSize.width - (visibleFrameInset * 2))
        let availableHeight = max(0, visibleSize.height - (visibleFrameInset * 2))
        return CGSize(
            width: max(minimumWindowSize.width, min(defaultWindowSize.width, availableWidth)),
            height: max(minimumWindowSize.height, min(defaultWindowSize.height, availableHeight))
        )
    }
}

enum RelayControlCenterSection: String, CaseIterable, Identifiable {
    case overview
    case connection
    case desktop
    case localOpenCodex
    case maintenance
    case activityLog
    case settings
    case appInformation

    var id: String { rawValue }


    static let statusSections: [Self] = [.overview, .connection, .desktop, .localOpenCodex]
    static let managementSections: [Self] = [.maintenance, .activityLog]
    static let appSections: [Self] = [.settings, .appInformation]
    var titleKey: AppStringKey {
        switch self {
        case .overview: .controlCenterOverview
        case .connection: .controlCenterConnection
        case .desktop: .controlCenterDesktop
        case .localOpenCodex: .controlCenterLocalOpenCodex
        case .maintenance: .controlCenterMaintenance
        case .activityLog: .controlCenterActivityLog
        case .settings: .controlCenterSettings
        case .appInformation: .controlCenterAppInformation
        }
    }

    var systemImage: String {
        switch self {
        case .overview: "gauge.with.dots.needle.50percent"
        case .connection: "point.3.connected.trianglepath.dotted"
        case .desktop: "macwindow"
        case .localOpenCodex: "shippingbox"
        case .maintenance: "wrench.and.screwdriver"
        case .activityLog: "list.bullet.rectangle"
        case .settings: "gearshape"
        case .appInformation: "info.circle"
        }
    }
}

struct MenuBarContentView: View {
    @ObservedObject var model: MenuBarModel
    let onOpenControlCenter: () -> Void
    @EnvironmentObject private var localization: LocalizationStore

    private var localizer: AppLocalizer { localization.localizer }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
                Label(model.statusTitle, systemImage: model.menuSymbolName)
                    .font(.headline)
                    .accessibilityLabel(model.menuAccessibilityLabel)

                if model.isLocalDevelopmentBuild {
                    LocalDevelopmentWarningView(localizer: localizer, compact: true)
                }

                VStack(alignment: .leading, spacing: 6) {
                    StatusRow(
                        localizer.text(.viewControlledApp),
                        value: model.selectedDesktopDisplayName,
                        minimumHeight: 0
                    )
                    StatusRow(
                        localizer.text(.viewRegistration),
                        value: model.desktopTargetState.title(using: localizer),
                        minimumHeight: 0
                    )
                    StatusRow(
                        localizer.text(.viewAppliedBackend),
                        value: model.status.map { localizer.displayName($0.appliedBackend) }
                            ?? localizer.text(.genericUnknown),
                        minimumHeight: 0
                    )
                    StatusRow(
                        localizer.text(.viewPhase),
                        value: model.status.map { localizer.displayName($0.phase) }
                            ?? localizer.text(.genericUnknown),
                        minimumHeight: 0
                    )
                }

                if model.status?.phase == .recoveryRequired || model.hasPendingOpenCodexRemovalRecovery {
                    Label(localizer.text(.controlCenterRecoveryNeeded), systemImage: "exclamationmark.triangle")
                        .font(.caption)
                        .foregroundStyle(.orange)
                }

                Divider()

                HStack {
                    Button {
                        onOpenControlCenter()
                    } label: {
                        Label(localizer.text(.menuConnectionDetails), systemImage: "macwindow")
                    }
                    .buttonStyle(.glassProminent)
                    .accessibilityHint(localizer.text(.menuConnectionDetailsHint))
                    Spacer()
                    Button(localizer.text(.menuQuit)) {
                        NSApplication.shared.terminate(nil)
                    }
                }
        }
        .padding(14)
        .frame(width: ControlCenterLayout.popoverWidth)
        .fixedSize(horizontal: false, vertical: true)
    }
}

struct RelayControlCenterView: View {
    @ObservedObject var model: MenuBarModel
    @ObservedObject var windowCoordinator: ControlCenterWindowCoordinator
    @ObservedObject var relocation: ApplicationRelocationController
    @ObservedObject var updates: ReleaseUpdateController
    @ObservedObject var runtimeUpgrade: RuntimeUpgradeController
    @ObservedObject var containerRuntime: ContainerRuntimeController
    @EnvironmentObject private var localization: LocalizationStore
    @StateObject private var codexConfiguration: CodexConfigurationController
    @StateObject private var gatewaySettings: GatewaySettingsController
    @State private var selection: RelayControlCenterSection? = .overview
    @State private var showsNativeRepairConfirmation = false
    @State private var showsCodexConfigPreviewWarning = false
    @State private var showsCodexConfigPreview = false

    private var localizer: AppLocalizer { localization.localizer }

    init(
        model: MenuBarModel,
        windowCoordinator: ControlCenterWindowCoordinator,
        relocation: ApplicationRelocationController,
        updates: ReleaseUpdateController,
        runtimeUpgrade: RuntimeUpgradeController,
        containerRuntime: ContainerRuntimeController
    ) {
        self.model = model
        self.windowCoordinator = windowCoordinator
        self.relocation = relocation
        self.updates = updates
        self.runtimeUpgrade = runtimeUpgrade
        self.containerRuntime = containerRuntime
        _codexConfiguration = StateObject(
            wrappedValue: model.makeCodexConfigurationController()
        )
        _gatewaySettings = StateObject(
            wrappedValue: model.makeGatewaySettingsController()
        )
    }

    var body: some View {
        NavigationSplitView {
            List(selection: $selection) {
                Section(localizer.text(.controlCenterSidebarStatus)) {
                    sidebarRows(RelayControlCenterSection.statusSections)
                }
                Section(localizer.text(.controlCenterSidebarManagement)) {
                    sidebarRows(RelayControlCenterSection.managementSections)
                }
                Section(localizer.text(.controlCenterSidebarApp)) {
                    sidebarRows(RelayControlCenterSection.appSections)
                }
            }
            .navigationSplitViewColumnWidth(min: 160, ideal: 205, max: 250)
            .listStyle(.sidebar)
        } detail: {
            selectedSection
                .toolbar {
                    ToolbarSpacer(.flexible, placement: .primaryAction)
                    ToolbarItem(placement: .primaryAction) {
                        Button {
                            model.refresh()
                            gatewaySettings.load()
                            runtimeUpgrade.refresh()
                            containerRuntime.refresh()
                        } label: {
                            Label {
                                Text(localizer.text(.menuRefresh))
                            } icon: {
                                if model.isRefreshing {
                                    ProgressView().controlSize(.small)
                                } else {
                                    Image(systemName: "arrow.clockwise")
                                }
                            }
                        }
                        .disabled(model.isBusy || model.isRefreshing)
                        .accessibilityHint(localizer.text(.controlCenterRefreshHint))
                    }
                }
        }
        .navigationSplitViewStyle(.balanced)
        .frame(
            minWidth: ControlCenterLayout.minimumWindowSize.width,
            minHeight: ControlCenterLayout.minimumWindowSize.height
        )
        .onAppear {
            model.start()
            model.setControlCenterVisible(true)
            if let requestedSection = windowCoordinator.requestedSection {
                selection = requestedSection
            } else if model.hasPendingOpenCodexRemovalRecovery {
                selection = .maintenance
            } else if model.shouldOpenSelfHostedOnboarding {
                selection = .settings
            }
            model.recordControlCenterSection((selection ?? .overview).rawValue)
        }
        .onDisappear {
            model.setControlCenterVisible(false)
        }
        .onChange(of: selection) { _, section in
            model.recordControlCenterSection((section ?? .overview).rawValue)
        }
        .onChange(of: windowCoordinator.requestedSection) { _, section in
            if let section { selection = section }
        }
        .onChange(of: model.openCodexRemovalFlow?.id) { _, flowID in
            if flowID != nil {
                selection = .maintenance
            }
        }
        .sheet(
            item: Binding(
                get: { model.openCodexRemovalFlow },
                set: { value in
                    if value == nil {
                        model.dismissOpenCodexRemoval()
                    }
                }
            )
        ) { _ in
            OpenCodexRemovalWizardView(model: model)
                .environmentObject(localization)
        }
        .sheet(
            isPresented: $showsCodexConfigPreview,
            onDismiss: {
                codexConfiguration.dismissPreview()
            }
        ) {
            if let contents = codexConfiguration.previewText {
                CodexConfigurationPreviewView(
                    contents: contents,
                    localizer: localizer
                )
            }
        }
        .windowDismissBehavior(model.canDismissOpenCodexRemoval ? .automatic : .disabled)
        .alert(
            localizer.text(nativeRepairConfirmationTitleKey),
            isPresented: $showsNativeRepairConfirmation
        ) {
            Button(localizer.text(.controlCenterNativeRepairCancel), role: .cancel) {}
            Button(localizer.text(nativeRepairConfirmationActionKey)) {
                if model.nativeRepairInspection?.kind == .stateOnly {
                    model.repairNative()
                } else {
                    model.repairNativeRouting()
                }
            }
        } message: {
            Text(localizer.text(nativeRepairConfirmationDetailKey))
        }
        .alert(
            localizer.text(.codexConfigPreviewWarningTitle),
            isPresented: $showsCodexConfigPreviewWarning
        ) {
            Button(localizer.text(.codexConfigPreviewCancel), role: .cancel) {}
            Button(localizer.text(.codexConfigPreviewReveal)) {
                if codexConfiguration.revealPreview() {
                    showsCodexConfigPreview = true
                }
            }
        } message: {
            Text(localizer.text(.codexConfigPreviewWarningDetail))
        }
    }

    @ViewBuilder
    private var selectedSection: some View {
        let section = selection ?? .overview
        switch section {
        case .overview:
            OverviewControlCenterPage(
                model: model,
                localizer: localizer,
                title: localizer.text(section.titleKey),
                systemImage: section.systemImage
            )
        case .connection:
            ConnectionControlCenterPage(
                model: model,
                localizer: localizer,
                title: localizer.text(section.titleKey),
                systemImage: section.systemImage,
                openMaintenance: { selection = .maintenance },
                openSettings: { selection = .settings }
            )
        case .desktop:
            DesktopControlCenterPage(
                model: model,
                codexConfiguration: codexConfiguration,
                localizer: localizer,
                title: localizer.text(section.titleKey),
                systemImage: section.systemImage,
                requestPreview: { showsCodexConfigPreviewWarning = true }
            )
        case .localOpenCodex:
            LocalOpenCodexControlCenterPage(
                model: model,
                localizer: localizer,
                title: localizer.text(section.titleKey),
                systemImage: section.systemImage,
                openMaintenance: { selection = .maintenance },
                openSettings: { selection = .settings }
            )
        case .maintenance:
            MaintenanceControlCenterPage(
                model: model,
                localizer: localizer,
                title: localizer.text(section.titleKey),
                systemImage: section.systemImage,
                confirmNativeRepair: { showsNativeRepairConfirmation = true },
                openLocalOpenCodex: { selection = .localOpenCodex }
            )
        case .activityLog:
            ActivityLogControlCenterPage(
                model: model,
                localizer: localizer,
                title: localizer.text(section.titleKey),
                systemImage: section.systemImage
            )
        case .settings:
            SettingsControlCenterPage(
                model: model,
                gatewaySettings: gatewaySettings,
                relocation: relocation,
                updates: updates,
                runtimeUpgrade: runtimeUpgrade,
                containerRuntime: containerRuntime,
                languageSelection: $localization.selection,
                languageDescriptors: localization.registry.descriptors,
                localizer: localizer,
                title: localizer.text(section.titleKey),
                systemImage: section.systemImage
            )
        case .appInformation:
            AppInformationControlCenterPage(
                model: model,
                updates: updates,
                runtimeUpgrade: runtimeUpgrade,
                relocation: relocation,
                localizer: localizer,
                title: localizer.text(section.titleKey),
                systemImage: section.systemImage
            )
        }
    }

    private var nativeRepairConfirmationTitleKey: AppStringKey {
        model.nativeRepairInspection?.kind == .stateOnly
            ? .controlCenterNativeRepairConfirmTitle

            : .controlCenterNativeOwnerRepairConfirmTitle
    }

    private var nativeRepairConfirmationDetailKey: AppStringKey {
        model.nativeRepairInspection?.kind == .stateOnly
            ? .controlCenterNativeRepairConfirmDetail
            : .controlCenterNativeOwnerRepairConfirmDetail
    }

    private var nativeRepairConfirmationActionKey: AppStringKey {
        model.nativeRepairInspection?.kind == .stateOnly
            ? .controlCenterNativeRepairConfirmAction
            : .controlCenterNativeOwnerRepairConfirmAction
    }
    @ViewBuilder
    private func sidebarRows(_ sections: [RelayControlCenterSection]) -> some View {
        ForEach(sections) { section in
            Label(localizer.text(section.titleKey), systemImage: section.systemImage)
                .tag(section)
        }
    }
}

struct OpenCodexDiscoveryControls: View {
    @ObservedObject var model: MenuBarModel
    let localizer: AppLocalizer
    let onRemovalFlowPresented: () -> Void

    @ViewBuilder
    var body: some View {
        switch model.openCodexDiscoveryState {
        case .idle:
            EmptyView()
        case let .searching(tier):
            HStack(spacing: 8) {
                ProgressView()
                    .controlSize(.small)
                Text(localizer.text(.menuDiscoverySearching, tier.rawValue.uppercased()))
                    .font(.body)
            }
        case .nativeSearching:
            HStack(spacing: 8) {
                ProgressView()
                    .controlSize(.small)
                Text(localizer.text(.menuDiscoveryNativeSearching))
                    .font(.body)
            }
        case let .candidates(result):
            VStack(alignment: .leading, spacing: 10) {
                Text(localizer.text(.menuDiscoveryCandidates))
                    .font(.body.weight(.semibold))
                ForEach(result.candidates) { candidate in
                    if result.context == .standaloneNative && !candidate.automaticRemovalEligible {
                        manualRemovalCandidate(candidate)
                    } else {
                        Button(candidateTitle(candidate, context: result.context)) {
                            if model.chooseDiscoveredOpenCodexCandidate(id: candidate.id) {
                                onRemovalFlowPresented()
                            }
                        }
                        .disabled(model.isBusy)
                        .help(candidate.help ?? "")
                    }
                }
                if result.truncated {
                    ControlCenterSupportingText(
                        localizer.text(.menuDiscoveryTruncated),
                        systemImage: "exclamationmark.triangle"
                    )
                }
                AdaptiveActionRow {
                    if result.context == .integrated {
                        Button {
                            model.selectOpenCodexExecutableManually()
                        } label: {
                            Label(localizer.text(.menuDiscoveryManual), systemImage: "doc.badge.plus")
                        }
                        .disabled(model.isBusy)
                    }
                    Button(role: .cancel) {
                        model.cancelOpenCodexDiscovery()
                    } label: {
                        Label(localizer.text(.menuDiscoveryCancel), systemImage: "xmark.circle")
                    }
                    .disabled(model.isBusy)
                }
            }
        case .broadScanApprovalRequired:
            VStack(alignment: .leading, spacing: 10) {
                ControlCenterSupportingText(
                    localizer.text(.menuDiscoveryBroadDetail),
                    systemImage: "externaldrive"
                )
                AdaptiveActionRow {
                    Button {
                        model.approveBroadOpenCodexDiscovery()
                    } label: {
                        Label(localizer.text(.menuDiscoveryBroadAction), systemImage: "magnifyingglass")
                    }
                    .disabled(model.isBusy)
                    Button {
                        model.selectOpenCodexExecutableManually()
                    } label: {
                        Label(localizer.text(.menuDiscoveryManual), systemImage: "doc.badge.plus")
                    }
                    .disabled(model.isBusy)
                    Button(role: .cancel) {
                        model.cancelOpenCodexDiscovery()
                    } label: {
                        Label(localizer.text(.menuDiscoveryCancel), systemImage: "xmark.circle")
                    }
                    .disabled(model.isBusy)
                }
            }
        case let .notFound(result):
            VStack(alignment: .leading, spacing: 10) {
                Text(localizer.text(.menuDiscoveryNoCandidates))
                    .font(.body)
                if result.truncated {
                    ControlCenterSupportingText(
                        localizer.text(.menuDiscoveryTruncated),
                        systemImage: "exclamationmark.triangle"
                    )
                }
                AdaptiveActionRow {
                    if result.context == .integrated {
                        Button {
                            model.selectOpenCodexExecutableManually()
                        } label: {
                            Label(localizer.text(.menuDiscoveryManual), systemImage: "doc.badge.plus")
                        }
                    }
                    Button(role: .cancel) {
                        model.cancelOpenCodexDiscovery()
                    } label: {
                        Label(localizer.text(.menuDiscoveryCancel), systemImage: "xmark.circle")
                    }
                }
            }
        case .failed:
            if model.integrationAvailability == .ready {
                Button {
                    model.selectOpenCodexExecutableManually()
                } label: {
                    Label(localizer.text(.menuDiscoveryManual), systemImage: "doc.badge.plus")
                }
                .disabled(model.isBusy)
            }
        }
    }

    private func manualRemovalCandidate(_ candidate: OpenCodexDiscoveryCandidatePresentation) -> some View {
        OpenCodexManualRemovalCandidateView(
            candidate: candidate,
            title: candidateTitle(candidate, context: .standaloneNative),
            reasonText: localizer.text(reasonKey(candidate.automaticRemovalReason)),
            model: model,
            localizer: localizer
        )
    }

    private func reasonKey(_ reason: OpenCodexAutomaticRemovalReason?) -> AppStringKey {
        switch reason ?? .verificationUnavailable {
        case .eligible: .menuDiscoveryReasonEligible
        case .unreviewedPackageClosure: .menuDiscoveryReasonUnreviewedPackageClosure
        case .unsupportedPackageVersion: .menuDiscoveryReasonUnsupportedPackageVersion
        case .packageModuleChanged: .menuDiscoveryReasonPackageModuleChanged
        case .executionEvidenceIncomplete: .menuDiscoveryReasonExecutionEvidenceIncomplete
        case .manualPackageManager: .menuDiscoveryReasonManualPackageManager
        case .identityUnverified: .menuDiscoveryReasonIdentityUnverified
        case .verificationUnavailable: .menuDiscoveryReasonVerificationUnavailable
        }
    }

    private func candidateTitle(
        _ candidate: OpenCodexDiscoveryCandidatePresentation,
        context: OpenCodexRemovalContext
    ) -> String {
        switch context {
        case .integrated:
            return localizer.text(
                .menuDiscoveryCandidate,
                candidate.manager.rawValue,
                candidate.version,
                candidate.tier?.rawValue.uppercased() ?? "?"
            )
        case .standaloneNative:
            return localizer.text(
                .menuDiscoveryNativeCandidate,
                candidate.manager.rawValue,
                candidate.version
            )
        }
    }
}

struct OpenCodexManualRemovalCandidateView: View {
    let candidate: OpenCodexDiscoveryCandidatePresentation
    let title: String
    let reasonText: String
    @ObservedObject var model: MenuBarModel
    let localizer: AppLocalizer
    @State private var commandPresentation: OpenCodexManualRemovalCommandPresentation?

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            ViewThatFits(in: .horizontal) {
                HStack(alignment: .firstTextBaseline, spacing: 10) {
                    Text(title)
                        .font(.body.weight(.medium))
                    Spacer(minLength: 8)
                    manualBadge
                }
                VStack(alignment: .leading, spacing: 6) {
                    Text(title)
                        .font(.body.weight(.medium))
                    manualBadge
                }
            }
            Text(reasonText)
                .font(.callout)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
            AdaptiveActionRow {
                if model.openCodexManualRemovalCommand(candidateID: candidate.id) != nil {
                    Button {
                        commandPresentation = model.openCodexManualRemovalCommand(
                            candidateID: candidate.id
                        )
                    } label: {
                        Label(
                            localizer.text(.menuDiscoveryManualRemovalInstructions),
                            systemImage: "terminal"
                        )
                    }
                    .accessibilityLabel(
                        localizer.text(.menuDiscoveryManualRemovalInstructionsAccessibility)
                    )
                    .accessibilityHint(
                        localizer.text(.menuDiscoveryManualRemovalInstructionsHint)
                    )
                }
                Button {
                    model.copyOpenCodexManualRemovalDiagnostics(candidateID: candidate.id)
                } label: {
                    Label(localizer.text(.menuDiscoveryCopyDiagnostics), systemImage: "doc.on.doc")
                }
                .buttonStyle(.borderless)
                .accessibilityLabel(localizer.text(.menuDiscoveryCopyDiagnosticsAccessibility))
                .accessibilityHint(localizer.text(.menuDiscoveryCopyDiagnosticsHint))
            }
            .disabled(model.isBusy)
        }
        .padding(.vertical, 4)
        .accessibilityElement(children: .contain)
        .sheet(item: $commandPresentation) { presentation in
            OpenCodexManualRemovalCommandSheet(
                presentation: presentation,
                model: model,
                localizer: localizer
            )
        }
    }

    private var manualBadge: some View {
        ControlCenterStatusBadge(
            text: localizer.text(.menuDiscoveryManualRemovalBadge),
            tone: .warning
        )
    }
}

struct OpenCodexManualRemovalCommandSheet: View {
    let presentation: OpenCodexManualRemovalCommandPresentation
    @ObservedObject var model: MenuBarModel
    let localizer: AppLocalizer
    @Environment(\.dismiss) private var dismiss
    @State private var visibleFrameSize = CGSize(width: 800, height: 600)

    private var sheetSize: CGSize {
        DevelopmentSetupSheetLayout.size(for: visibleFrameSize)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(alignment: .firstTextBaseline) {
                Label(
                    localizer.text(.menuDiscoveryManualRemovalTitle),
                    systemImage: "terminal"
                )
                .font(.title2.weight(.semibold))
                Spacer()
                Button(localizer.text(.menuDiscoveryManualRemovalClose)) {
                    dismiss()
                }
                .keyboardShortcut(.cancelAction)
            }
            .padding(.horizontal, 24)
            .padding(.vertical, 18)

            Divider()

            ScrollView(.vertical) {
                VStack(alignment: .leading, spacing: 18) {
                    Text(localizer.text(.menuDiscoveryManualRemovalDetail))
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)

                    ControlCenterNotice(tone: presentation.nativeState == .native ? .success : .warning) {
                        Text(localizer.text(nativeNoticeKey))
                            .fixedSize(horizontal: false, vertical: true)
                    }

                    GroupBox(localizer.text(.menuDiscoveryManualRemovalCommand)) {
                        Text(presentation.command)
                            .font(.system(.body, design: .monospaced))
                            .textSelection(.enabled)
                            .lineLimit(nil)
                            .fixedSize(horizontal: false, vertical: true)
                            .padding(.vertical, 6)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }

                    GroupBox(localizer.text(.menuDiscoveryManualRemovalDiagnostics)) {
                        Text(diagnostics)
                            .font(.system(.callout, design: .monospaced))
                            .foregroundStyle(.secondary)
                            .textSelection(.enabled)
                            .lineLimit(nil)
                            .fixedSize(horizontal: false, vertical: true)
                            .padding(.vertical, 6)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                }
                .padding(24)
                .frame(maxWidth: .infinity, alignment: .leading)
            }

            Divider()

            ControlCenterActionFooter {
                Button {
                    model.copyOpenCodexManualRemovalDiagnostics(candidateID: presentation.id)
                } label: {
                    Label(localizer.text(.menuDiscoveryCopyDiagnostics), systemImage: "doc.on.doc")
                }
            } primary: {
                Button {
                    model.copyOpenCodexManualRemovalCommand(candidateID: presentation.id)
                } label: {
                    Label(
                        localizer.text(.menuDiscoveryManualRemovalCopyCommand),
                        systemImage: "doc.on.doc"
                    )
                }
                .buttonStyle(.glassProminent)
                .keyboardShortcut(.defaultAction)
            }
            .disabled(model.isBusy)
            .padding(.horizontal, 24)
            .padding(.vertical, 16)
        }
        .frame(
            minWidth: 320,
            idealWidth: sheetSize.width,
            maxWidth: sheetSize.width,
            minHeight: 320,
            idealHeight: sheetSize.height,
            maxHeight: sheetSize.height
        )
        .background {
            SheetVisibleFrameReader { size in
                visibleFrameSize = size
            }
            .frame(width: 0, height: 0)
        }
    }

    private var nativeNoticeKey: AppStringKey {
        presentation.nativeState == .native
            ? .menuDiscoveryManualRemovalNativeReady
            : .menuDiscoveryManualRemovalNativeWarning
    }

    private var diagnostics: String {
        [
            "version=\(presentation.version)",
            "manager=\(presentation.manager.rawValue)",
            "automatic_removal_reason=\(presentation.reason.rawValue)",
        ].joined(separator: "\n")
    }
}

struct ConnectionRoutingCard: View {
    @ObservedObject var model: MenuBarModel
    let localizer: AppLocalizer

    var body: some View {
        VStack(alignment: .leading, spacing: ControlCenterPresentationMetrics.pageSpacing) {
            ControlCenterSectionCard(
                localizer.text(.controlCenterConnectionStatus),
                systemImage: "network"
            ) {
                StatusRow(
                    localizer.text(.viewLocalRelay),
                    value: model.localRelayDisplay,
                    systemImage: "antenna.radiowaves.left.and.right",
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.viewRemoteObservation),
                    value: model.remoteGatewayDisplay,
                    systemImage: "globe",
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.viewRelayProcess),
                    value: model.status?.relayRunning == true
                        ? localizer.text(.genericRunning)
                        : localizer.text(.genericUnavailable),
                    systemImage: "bolt.horizontal.circle",
                    badgeTone: model.status?.relayRunning == true ? .success : .warning,
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.viewRelayAdmission),
                    value: model.status.map { localizer.displayName($0.relayAdmission) }
                        ?? localizer.text(.genericUnknown),
                    systemImage: "checkmark.shield"
                )
            }

            ControlCenterSectionCard(
                localizer.text(.controlCenterRoutingStatus),
                systemImage: "point.3.connected.trianglepath.dotted"
            ) {
                StatusRow(
                    localizer.text(.viewRoutingSync),
                    value: model.routingSyncDisplay,
                    systemImage: "arrow.triangle.2.circlepath",
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.viewDesiredBackend),
                    value: model.status.map { localizer.displayName($0.desiredBackend) }
                        ?? localizer.text(.genericUnknown),
                    systemImage: "scope",
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.viewAppliedBackend),
                    value: model.status.map { localizer.displayName($0.appliedBackend) }
                        ?? localizer.text(.genericUnknown),
                    systemImage: "checkmark.circle",
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.viewPhase),
                    value: model.status.map { localizer.displayName($0.phase) }
                        ?? localizer.text(.genericUnknown),
                    systemImage: "flag",
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.viewCatalog),
                    value: model.catalogDisplay,
                    systemImage: "books.vertical",
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.viewActiveRequests),
                    value: model.activeRequestsDisplay,
                    systemImage: "arrow.up.arrow.down",
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.viewDrain),
                    value: model.drainDisplay,
                    systemImage: "hourglass",
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.viewLastLocalUpdate),
                    value: model.lastStatusUpdatedDisplay,
                    systemImage: "clock"
                )
            }
        }
        .accessibilityElement(children: .contain)
        .accessibilityLabel(localizer.text(.viewConnectionRoutingAccessibility))
    }
}
