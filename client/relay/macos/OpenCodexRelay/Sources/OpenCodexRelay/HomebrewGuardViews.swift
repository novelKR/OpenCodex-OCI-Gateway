import AppKit
import SwiftUI
import OpenCodexRelayLocalization

enum HomebrewGuardPrimaryAction: Equatable {
    case register
    case developmentSetup
    case openSettings
    case recover

    static func resolve(_ state: HomebrewGuardRegistrationState) -> Self? {
        switch state {
        case .preview:
            nil
        case .notRegistered:
            .register
        case .approvalRequired:
            .openSettings
        case .manualInstallRequired, .manualUpdateRequired,
             .manualInstallerRecoveryRequired, .daemonLaunchFailed:
            .developmentSetup
        case .recoveryRequired:
            .recover
        case .notRequired, .ready, .busy, .unavailable:
            nil
        }
    }
}

enum DevelopmentSetupSheetLayout {
    static let visibleFrameInset: CGFloat = 48

    static func size(for visibleFrame: CGSize) -> CGSize {
        CGSize(
            width: min(720, max(320, min(640, visibleFrame.width - visibleFrameInset))),
            height: min(680, max(320, min(560, visibleFrame.height - visibleFrameInset)))
        )
    }
}

enum DevelopmentSetupSheetPresentation: Equatable {
    case command
    case checking
    case unchanged
    case ready
    case stateChanged

    static func resolve(
        initial: HomebrewGuardRegistrationState,
        current: HomebrewGuardRegistrationState,
        didCheck: Bool,
        isChecking: Bool
    ) -> Self {
        if isChecking {
            return .checking
        }
        if current == .ready {
            return .ready
        }
        if current != initial {
            return .stateChanged
        }
        return didCheck ? .unchanged : .command
    }

    var showsCommand: Bool {
        switch self {
        case .command, .checking, .unchanged:
            true
        case .ready, .stateChanged:
            false
        }
    }
}

struct HomebrewGuardStatusCard: View {
    @ObservedObject var model: MenuBarModel
    let localizer: AppLocalizer
    var showsActions = true
    @Environment(\.scenePhase) private var scenePhase
    @State private var setupCommand: String?
    @State private var developmentSetupInitialRegistration: HomebrewGuardRegistrationState?
    @State private var developmentSetupIsRecovery = false
    @State private var showsDevelopmentSetup = false

    var body: some View {
        ControlCenterSectionCard(
            localizer.text(.homebrewGuardTitle),
            systemImage: "lock.shield"
        ) {
            VStack(alignment: .leading, spacing: 0) {
                StatusRow(
                    localizer.text(.homebrewGuardBackend),
                    value: backendTitle,
                    systemImage: "server.rack",
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.homebrewGuardVersion),
                    value: model.homebrewGuardAvailability.helperVersion
                        ?? localizer.text(.genericUnknown),
                    systemImage: "number",
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.homebrewGuardProtocol),
                    value: localizer.formattedNumber(
                        model.homebrewGuardAvailability.protocolVersion
                    ),
                    systemImage: "arrow.left.arrow.right"
                )

                ControlCenterNotice(tone: detailTone) {
                    Text(detail)
                        .foregroundStyle(.primary)
                        .fixedSize(horizontal: false, vertical: true)
                }
                .padding(.top, 10)

                if showsActions, primaryAction != nil {
                    Divider()
                        .padding(.vertical, 12)
                    ControlCenterActionFooter {
                        EmptyView()
                    } primary: {
                        primaryActionButton
                    }
                }

            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.vertical, 4)
        } accessory: {
            HStack(spacing: 8) {
                ControlCenterStatusBadge(text: stateTitle, tone: stateTone)
                Button {
                    Task {
                        await model.refreshHomebrewGuardAvailability()
                    }
                } label: {
                    Image(systemName: "arrow.clockwise")
                }
                .buttonStyle(.borderless)
                .disabled(model.isBusy)
                .help(localizer.text(.homebrewGuardRefresh))
                .accessibilityLabel(localizer.text(.homebrewGuardRefresh))
            }
        }
        .accessibilityElement(children: .contain)
        .accessibilityLabel(localizer.text(.homebrewGuardTitle))
        .task {
            await model.refreshHomebrewGuardAvailability()
        }
        .onChange(of: scenePhase) { _, phase in
            guard phase == .active else { return }
            Task {
                await model.refreshHomebrewGuardAvailability()
            }
        }
        .sheet(isPresented: $showsDevelopmentSetup, onDismiss: {
            setupCommand = nil
            developmentSetupInitialRegistration = nil
            developmentSetupIsRecovery = false
        }) {
            if let setupCommand, let developmentSetupInitialRegistration {
                DevelopmentHomebrewGuardSetupView(
                    command: setupCommand,
                    initialRegistration: developmentSetupInitialRegistration,
                    isRecovery: developmentSetupIsRecovery,
                    model: model,
                    localizer: localizer,
                    isPresented: $showsDevelopmentSetup
                )
            }
        }
    }

    private var stateTitle: String {
        return switch model.homebrewGuardAvailability.registration {
        case .preview: localizer.text(.homebrewGuardStatePreview)
        case .notRequired: localizer.text(.homebrewGuardStateNotRequired)
        case .notRegistered: localizer.text(.homebrewGuardStateNotRegistered)
        case .approvalRequired: localizer.text(.homebrewGuardStateApprovalRequired)
        case .manualInstallRequired: localizer.text(.homebrewGuardStateManualInstallRequired)
        case .manualUpdateRequired: localizer.text(.homebrewGuardStateManualUpdateRequired)
        case .manualInstallerRecoveryRequired:
            localizer.text(.homebrewGuardStateManualInstallerRecoveryRequired)
        case .daemonLaunchFailed: localizer.text(.homebrewGuardStateDaemonLaunchFailed)
        case .ready: localizer.text(.homebrewGuardStateReady)
        case .busy: localizer.text(.homebrewGuardStateBusy)
        case .recoveryRequired: localizer.text(.homebrewGuardStateRecoveryRequired)
        case .unavailable: localizer.text(.homebrewGuardStateUnavailable)
        }

    }
    private var stateTone: ControlCenterStatusTone {
        switch model.homebrewGuardAvailability.registration {
        case .ready:
            .success
        case .preview, .notRequired:
            .neutral
        case .busy:
            .info
        case .unavailable:
            .error
        case .notRegistered, .approvalRequired, .manualInstallRequired,
             .manualUpdateRequired, .manualInstallerRecoveryRequired,
             .daemonLaunchFailed, .recoveryRequired:
            .warning
        }
    }

    private var detailTone: ControlCenterStatusTone {
        stateTone
    }

    private var primaryAction: HomebrewGuardPrimaryAction? {
        guard let action = HomebrewGuardPrimaryAction.resolve(
            model.homebrewGuardAvailability.registration
        ) else {
            return nil
        }
        switch action {
        case .recover where model.canRecoverHomebrewGuard:
            return .recover
        case .register where model.canRegisterHomebrewGuard:
            return .register
        case .developmentSetup where model.canShowDevelopmentHomebrewGuardSetup:
            return .developmentSetup
        case .openSettings where model.canOpenHomebrewGuardSystemSettings:
            return .openSettings
        default:
            return nil
        }
    }

    @ViewBuilder
    private var primaryActionButton: some View {
        switch primaryAction {
        case .register:
            Button {
                model.registerHomebrewGuard()
            } label: {
                Label(localizer.text(.homebrewGuardRegister), systemImage: "person.badge.key")
            }
            .buttonStyle(.glassProminent)
            .disabled(model.isBusy)
            .accessibilityHint(localizer.text(.homebrewGuardDetailNotRegistered))

        case .developmentSetup:
            Button {
                guard let command = model.developmentHomebrewGuardSetupCommand() else {
                    return
                }
                let registration = model.homebrewGuardAvailability.registration
                setupCommand = command
                developmentSetupInitialRegistration = registration
                developmentSetupIsRecovery =
                    registration == .manualInstallerRecoveryRequired
                showsDevelopmentSetup = true
            } label: {
                Label(localizer.text(developmentSetupLabelKey), systemImage: "terminal")
            }
            .buttonStyle(.glassProminent)
            .disabled(model.isBusy || model.developmentHomebrewGuardSetupFailureCode != nil)
            .accessibilityHint(localizer.text(developmentSetupHintKey))

        case .openSettings:
            Button {
                model.openHomebrewGuardSystemSettings()
            } label: {
                Label(localizer.text(.homebrewGuardOpenSettings), systemImage: "gearshape")
            }
            .buttonStyle(.glassProminent)
            .disabled(model.isBusy)
            .accessibilityHint(localizer.text(.homebrewGuardOpenSettingsHint))

        case .recover:
            Button {
                model.recoverHomebrewGuardProtection()
            } label: {
                Label(localizer.text(.homebrewGuardRecover), systemImage: "lock.open.rotation")
            }
            .buttonStyle(.glassProminent)
            .disabled(model.isBusy)
            .accessibilityHint(localizer.text(.homebrewGuardDetailRecoveryRequired))

        case nil:
            EmptyView()
        }
    }

    private var developmentSetupLabelKey: AppStringKey {
        model.homebrewGuardAvailability.registration == .manualInstallerRecoveryRequired
            ? .homebrewGuardDevelopmentRecovery
            : .homebrewGuardDevelopmentSetup
    }

    private var developmentSetupHintKey: AppStringKey {
        model.homebrewGuardAvailability.registration == .manualInstallerRecoveryRequired
            ? .homebrewGuardDevelopmentRecoveryHint
            : .homebrewGuardDevelopmentSetupHint
    }

    private var backendTitle: String {
        switch model.homebrewGuardBackend {
        case .smAppService:
            localizer.text(.homebrewGuardBackendSMAppService)
        case .manualAdmin:
            localizer.text(.homebrewGuardBackendManualDevelopment)
        }
    }

    private var detail: String {
        if model.developmentHomebrewGuardSetupFailureCode == "artifact_invalid" {
            return localizer.text(.homebrewGuardDetailArtifactInvalid)
        }
        return switch model.homebrewGuardAvailability.registration {
        case .preview:
            localizer.text(.homebrewGuardDetailPreview)
        case .notRequired:
            localizer.text(.homebrewGuardStateNotRequired)
        case .notRegistered:
            localizer.text(.homebrewGuardDetailNotRegistered)
        case .approvalRequired:
            localizer.text(.homebrewGuardDetailApprovalRequired)
        case .manualInstallRequired:
            localizer.text(.homebrewGuardDetailManualInstallRequired)
        case .manualUpdateRequired:
            localizer.text(.homebrewGuardDetailManualUpdateRequired)
        case .manualInstallerRecoveryRequired:
            localizer.text(.homebrewGuardDetailManualInstallerRecoveryRequired)
        case .daemonLaunchFailed:
            localizer.text(.homebrewGuardDetailDaemonLaunchFailed)
        case .ready:
            localizer.text(.homebrewGuardDetailReady)
        case .recoveryRequired:
            localizer.text(.homebrewGuardDetailRecoveryRequired)
        case .busy, .unavailable:
            localizer.text(.homebrewGuardDetailUnavailable)
        }
    }

    private var detailSymbol: String {
        switch model.homebrewGuardAvailability.registration {
        case .ready: "checkmark.shield.fill"
        case .recoveryRequired, .manualInstallerRecoveryRequired:
            "lock.trianglebadge.exclamationmark.fill"
        case .notRegistered, .approvalRequired, .manualInstallRequired,
             .manualUpdateRequired, .daemonLaunchFailed, .busy, .unavailable:
            "exclamationmark.triangle.fill"
        case .preview, .notRequired: "info.circle"
        }
    }

    private var detailColor: Color {
        switch model.homebrewGuardAvailability.registration {
        case .ready: .green
        case .preview, .notRequired: .secondary
        case .notRegistered, .approvalRequired, .manualInstallRequired,
             .manualUpdateRequired, .daemonLaunchFailed, .busy,
             .manualInstallerRecoveryRequired, .recoveryRequired, .unavailable:
            .orange
        }
    }
}

struct DevelopmentHomebrewGuardSetupView: View {
    let command: String
    let initialRegistration: HomebrewGuardRegistrationState
    let isRecovery: Bool
    @ObservedObject var model: MenuBarModel
    let localizer: AppLocalizer
    @Binding var isPresented: Bool
    @State private var visibleFrameSize = CGSize(width: 800, height: 600)
    @State private var didCheck = false
    @State private var isChecking = false

    private var sheetSize: CGSize {
        DevelopmentSetupSheetLayout.size(for: visibleFrameSize)
    }

    private var presentation: DevelopmentSetupSheetPresentation {
        DevelopmentSetupSheetPresentation.resolve(
            initial: initialRegistration,
            current: model.homebrewGuardAvailability.registration,
            didCheck: didCheck,
            isChecking: isChecking
        )
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(alignment: .firstTextBaseline) {
                Label(
                    localizer.text(titleKey),
                    systemImage: "lock.shield"
                )
                .font(.title2.weight(.semibold))
                Spacer()
                Button(localizer.text(.homebrewGuardDevelopmentSetupClose)) {
                    isPresented = false
                }
                .keyboardShortcut(.cancelAction)
            }
            .padding(.horizontal, 24)
            .padding(.vertical, 18)

            Divider()

            ScrollView(.vertical) {
                sheetContent
                .padding(24)
                .frame(maxWidth: .infinity, alignment: .leading)
            }

            Divider()

            sheetFooter
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

    @ViewBuilder
    private var sheetContent: some View {
        VStack(alignment: .leading, spacing: 18) {
            switch presentation {
            case .ready:
                ControlCenterNotice(tone: .success) {
                    VStack(alignment: .leading, spacing: 6) {
                        Text(localizer.text(.homebrewGuardDevelopmentSetupSuccessTitle))
                            .font(.headline)
                        Text(localizer.text(.homebrewGuardDevelopmentSetupSuccessDetail))
                            .foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
                StatusRow(
                    localizer.text(.homebrewGuardVersion),
                    value: model.homebrewGuardAvailability.helperVersion
                        ?? localizer.text(.genericUnknown),
                    systemImage: "number"
                )

            case .stateChanged:
                ControlCenterNotice(tone: .warning) {
                    VStack(alignment: .leading, spacing: 6) {
                        Text(localizer.text(.homebrewGuardDevelopmentSetupStateChangedTitle))
                            .font(.headline)
                        Text(localizer.text(.homebrewGuardDevelopmentSetupStateChangedDetail))
                            .foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }

            case .command, .checking, .unchanged:
                if presentation == .checking {
                    ControlCenterNotice(tone: .info) {
                        HStack(spacing: 8) {
                            ProgressView()
                                .controlSize(.small)
                            Text(localizer.text(.homebrewGuardDevelopmentSetupChecking))
                        }
                    }
                } else if presentation == .unchanged {
                    ControlCenterNotice(tone: .warning) {
                        Text(localizer.text(.homebrewGuardDevelopmentSetupUnchanged))
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }

                Text(localizer.text(detailKey))
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)

                GroupBox(localizer.text(.homebrewGuardDevelopmentSetupCommand)) {
                    Text(command)
                        .font(.system(.body, design: .monospaced))
                        .textSelection(.enabled)
                        .lineLimit(nil)
                        .fixedSize(horizontal: false, vertical: true)
                        .padding(.vertical, 6)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }

                HomebrewGuardDiagnosticGuidanceBlock(
                    isRecovery: isRecovery,
                    localizer: localizer
                )
            }
        }
    }

    @ViewBuilder
    private var sheetFooter: some View {
        switch presentation {
        case .ready:
            ControlCenterActionFooter {
                EmptyView()
            } primary: {
                Button(localizer.text(.homebrewGuardDevelopmentSetupDone)) {
                    isPresented = false
                }
                .buttonStyle(.glassProminent)
                .keyboardShortcut(.defaultAction)
            }

        case .stateChanged:
            ControlCenterActionFooter {
                EmptyView()
            } primary: {
                Button(localizer.text(.homebrewGuardDevelopmentSetupReviewUpdatedState)) {
                    isPresented = false
                }
                .buttonStyle(.glassProminent)
                .keyboardShortcut(.defaultAction)
            }

        case .command, .checking, .unchanged:
            ControlCenterActionFooter {
                Button {
                    checkHelperStatus()
                } label: {
                    if isChecking {
                        HStack(spacing: 6) {
                            ProgressView()
                                .controlSize(.small)
                            Text(localizer.text(.homebrewGuardDevelopmentSetupChecking))
                        }
                    } else {
                        Label(
                            localizer.text(.homebrewGuardRefresh),
                            systemImage: "arrow.clockwise"
                        )
                    }
                }
                .disabled(isChecking || model.isBusy)
            } primary: {
                Button {
                    model.copyDevelopmentHomebrewGuardSetupCommand(command)
                } label: {
                    Label(
                        localizer.text(.homebrewGuardDevelopmentSetupCopy),
                        systemImage: "doc.on.doc"
                    )
                }
                .buttonStyle(.glassProminent)
                .keyboardShortcut(.defaultAction)
                .disabled(isChecking || model.isBusy)
            }
        }
    }

    private func checkHelperStatus() {
        guard !isChecking else { return }
        isChecking = true
        Task { @MainActor in
            await model.refreshHomebrewGuardAvailability()
            didCheck = true
            isChecking = false
        }
    }

    private var titleKey: AppStringKey {
        switch presentation {
        case .ready:
            .homebrewGuardDevelopmentSetupSuccessTitle
        case .stateChanged:
            .homebrewGuardDevelopmentSetupStateChangedTitle
        case .command, .checking, .unchanged:
            isRecovery
                ? .homebrewGuardDevelopmentRecoveryTitle
                : .homebrewGuardDevelopmentSetupTitle
        }
    }

    private var detailKey: AppStringKey {
        isRecovery ? .homebrewGuardDevelopmentRecoveryDetail : .homebrewGuardDevelopmentSetupDetail
    }
}

struct SheetVisibleFrameReader: NSViewRepresentable {
    let onChange: @MainActor (CGSize) -> Void

    func makeNSView(context: Context) -> SheetVisibleFrameObservationView {
        SheetVisibleFrameObservationView(onChange: onChange)
    }

    func updateNSView(_ nsView: SheetVisibleFrameObservationView, context: Context) {
        nsView.onChange = onChange
        nsView.report()
    }
}

@MainActor
final class SheetVisibleFrameObservationView: NSView {
    var onChange: @MainActor (CGSize) -> Void

    init(onChange: @escaping @MainActor (CGSize) -> Void) {
        self.onChange = onChange
        super.init(frame: .zero)
    }

    @available(*, unavailable)
    required init?(coder: NSCoder) {
        fatalError("init(coder:) has not been implemented")
    }

    override func viewDidMoveToWindow() {
        super.viewDidMoveToWindow()
        report()
    }

    func report() {
        guard let size = window?.screen?.visibleFrame.size else { return }
        onChange(size)
    }
}

struct HomebrewGuardDiagnosticGuidanceBlock: View {
    let isRecovery: Bool
    let localizer: AppLocalizer

    static func codeGroups(isRecovery: Bool) -> [[String]] {
        if isRecovery {
            return [["rollback_result=failed"]]
        }
        return [
            ["daemon_start_rejected"],
            ["probe_timeout", "xpc_rejected", "invalid_response"],
            ["owner_or_mode_mismatch"],
            ["rollback_result=failed"],
        ]
    }

    var body: some View {
        ControlCenterSectionCard(
            localizer.text(.homebrewGuardDevelopmentSetupResultHint),
            systemImage: "stethoscope"
        ) {
            if !isRecovery {
                HomebrewGuardDiagnosticGuidanceRow(
                    codes: Self.codeGroups(isRecovery: false)[0],
                    text: localizer.text(.homebrewGuardDevelopmentGuidanceDaemonStart)
                )
                Divider()
                HomebrewGuardDiagnosticGuidanceRow(
                    codes: Self.codeGroups(isRecovery: false)[1],
                    text: localizer.text(.homebrewGuardDevelopmentGuidanceProbe)
                )
                Divider()
                HomebrewGuardDiagnosticGuidanceRow(
                    codes: Self.codeGroups(isRecovery: false)[2],
                    text: localizer.text(.homebrewGuardDevelopmentGuidanceOwnership)
                )
                Divider()
            }
            HomebrewGuardDiagnosticGuidanceRow(
                codes: ["rollback_result=failed"],
                text: localizer.text(.homebrewGuardDevelopmentGuidanceRollback),
                tone: .error
            )
        }
    }
}

struct HomebrewGuardDiagnosticGuidanceRow: View {
    let codes: [String]
    let text: String
    var tone: ControlCenterStatusTone = .warning

    var body: some View {
        VStack(alignment: .leading, spacing: 7) {
            ViewThatFits(in: .horizontal) {
                HStack(spacing: 5) {
                    codeBadges
                }
                VStack(alignment: .leading, spacing: 5) {
                    codeBadges
                }
            }

            Text(text)
                .font(.callout)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(.vertical, 5)
    }

    @ViewBuilder
    private var codeBadges: some View {
        ForEach(codes, id: \.self) { code in
            ControlCenterStatusBadge(
                text: code,
                tone: tone,
                monospaced: true
            )
        }
    }
}

struct OpenCodexRemovalExecutionProgressView: View {
    let progress: OpenCodexRemovalExecutionProgress
    let localizer: AppLocalizer

    private enum StepState {
        case pending
        case active
        case completed
        case blocked
        case failed
    }

    private struct Step: Identifiable {
        let phase: OpenCodexRemovalExecutionPhase
        let titleKey: AppStringKey
        var id: Int { phase.rawValue }
    }

    var body: some View {
        GroupBox(localizer.text(.removalExecutionProgressTitle)) {
            VStack(alignment: .leading, spacing: 12) {
                Text(localizer.text(.removalExecutionProgressDetail))
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)

                ForEach(steps) { step in
                    stepRow(step)
                }

                if let result = progress.result {
                    Divider()
                    Label(
                        result.text(using: localizer),
                        systemImage: progress.phase == .completed
                            ? "checkmark.shield.fill"
                            : "exclamationmark.triangle.fill"
                    )
                    .foregroundStyle(
                        progress.phase == .completed ? Color.green : Color.orange
                    )
                    .fixedSize(horizontal: false, vertical: true)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.vertical, 4)
        }
    }

    private var steps: [Step] {
        var values = [
            Step(phase: .preflight, titleKey: .removalExecutionStepPreflight),
            Step(phase: .desktopExit, titleKey: .removalExecutionStepDesktopExit),
            Step(
                phase: .homebrewProtection,
                titleKey: .removalExecutionStepHomebrewProtection
            ),
            Step(
                phase: .candidateRevalidation,
                titleKey: .removalExecutionStepCandidateRevalidation
            ),
            Step(phase: .teardown, titleKey: .removalExecutionStepTeardown),
            Step(phase: .packageRemoval, titleKey: .removalExecutionStepPackageRemoval),
            Step(
                phase: .resultVerification,
                titleKey: .removalExecutionStepResultVerification
            ),
            Step(
                phase: .permissionRestore,
                titleKey: .removalExecutionStepPermissionRestore
            ),
            Step(
                phase: .desktopRelaunch,
                titleKey: .removalExecutionStepDesktopRelaunch
            ),
            Step(phase: .statusRefresh, titleKey: .removalExecutionStepStatusRefresh),
        ]
        if !progress.usesHomebrewGuard {
            values.removeAll {
                $0.phase == .homebrewProtection ||
                    $0.phase == .candidateRevalidation ||
                    $0.phase == .permissionRestore
            }
        }
        values.removeAll { $0.phase == .candidateRevalidation }
        return values
    }

    @ViewBuilder
    private func stepRow(_ step: Step) -> some View {
        let state = state(for: step.phase)
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            statusIcon(state)
                .frame(width: 18)
            Text(localizer.text(step.titleKey))
                .frame(maxWidth: .infinity, alignment: .leading)
            Text(statusText(state))
                .font(.caption)
                .foregroundStyle(
                    state == .failed || state == .blocked ? Color.orange : Color.secondary
                )
        }
        .accessibilityElement(children: .combine)
    }

    @ViewBuilder
    private func statusIcon(_ state: StepState) -> some View {
        switch state {
        case .pending:
            Image(systemName: "circle").foregroundStyle(.tertiary)
        case .active:
            ProgressView().controlSize(.small)
        case .completed:
            Image(systemName: "checkmark.circle.fill").foregroundStyle(.green)
        case .blocked:
            Image(systemName: "lock.trianglebadge.exclamationmark.fill")
                .foregroundStyle(.orange)
        case .failed:
            Image(systemName: "exclamationmark.triangle.fill")
                .foregroundStyle(.orange)
        }
    }

    private func state(for phase: OpenCodexRemovalExecutionPhase) -> StepState {
        if progress.phase == .completed { return .completed }
        if progress.phase == .failed {
            let failed = progress.failedPhase ?? .preflight
            if phase.rawValue < failed.rawValue { return .completed }
            if phase == failed { return failed == .preflight ? .blocked : .failed }
            return .pending
        }
        if phase.rawValue < progress.phase.rawValue { return .completed }
        if phase == progress.phase { return .active }
        return .pending
    }

    private func statusText(_ state: StepState) -> String {
        switch state {
        case .pending: localizer.text(.removalHandoffStatusPending)
        case .active: localizer.text(.removalHandoffStatusRunning)
        case .completed: localizer.text(.removalHandoffStatusCompleted)
        case .blocked: localizer.text(.removalHandoffStatusBlocked)
        case .failed: localizer.text(.removalHandoffStatusFailed)
        }
    }
}
