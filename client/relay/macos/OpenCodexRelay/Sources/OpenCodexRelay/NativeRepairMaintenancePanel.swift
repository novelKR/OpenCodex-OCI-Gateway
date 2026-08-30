import SwiftUI
import OpenCodexRelayCore
import OpenCodexRelayLocalization

struct NativeRepairMaintenancePanel: View {
    @ObservedObject var model: MenuBarModel
    let localizer: AppLocalizer
    let confirmStateOnly: () -> Void
    let confirmOwnedRepair: () -> Void
    let openLocalOpenCodex: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            if model.canRepairNative {
                diagnosis
            }
            if let progress = model.nativeRepairProgress {
                progressView(progress)
            }
        }
    }

    @ViewBuilder
    private var diagnosis: some View {
        GroupBox(localizer.text(.controlCenterNativeRepairDiagnosis)) {
            VStack(alignment: .leading, spacing: 12) {
                if let inspection = model.nativeRepairInspection {
                    StatusRow(
                        localizer.text(.controlCenterNativeRepairDetectedOwner),
                        value: localizer.text(ownerKey(inspection.kind))
                    )
                    StatusRow(
                        localizer.text(.controlCenterNativeRepairOpenAIBaseURL),
                        value: localizer.text(inspection.openAIBaseURL
                            ? .controlCenterNativeRepairFieldPresent
                            : .controlCenterNativeRepairFieldAbsent)
                    )
                    StatusRow(
                        localizer.text(.controlCenterNativeRepairModelCatalog),
                        value: localizer.text(inspection.modelCatalogJSON
                            ? .controlCenterNativeRepairFieldPresent
                            : .controlCenterNativeRepairFieldAbsent)
                    )
                    ControlCenterSupportingText(
                        localizer.text(detailKey(inspection.kind)),
                        systemImage: inspection.kind == .unavailable
                            ? "exclamationmark.triangle"
                            : "checkmark.shield"
                    )
                    if inspection.kind == .openCodex {
                        openCodexCandidates
                    }
                    AdaptiveActionRow {
                        switch inspection.kind {
                        case .stateOnly:
                            Button {
                                confirmStateOnly()
                            } label: {
                                Label(localizer.text(.controlCenterNativeRepairAction), systemImage: "wrench.adjustable.fill")
                            }
                            .disabled(model.isBusy)
                        case .localRelay, .openCodex:
                            Button {
                                confirmOwnedRepair()
                            } label: {
                                Label(localizer.text(.controlCenterNativeOwnerRepairAction), systemImage: "arrow.trianglehead.2.clockwise.rotate.90")
                            }
                            .disabled(model.isBusy || !model.canRunOwnedNativeRepair)
                            .accessibilityHint(localizer.text(.controlCenterNativeOwnerRepairHint))
                        case .unavailable:
                            EmptyView()
                        }
                        Button {
                            model.inspectNativeRepair()
                        } label: {
                            Label(localizer.text(.menuRefresh), systemImage: "arrow.clockwise")
                        }
                        .disabled(model.isBusy)
                    }
                } else {
                    ControlCenterSupportingText(
                        localizer.text(.controlCenterNativeRepairDetail),
                        systemImage: "stethoscope"
                    )
                    Button {
                        model.inspectNativeRepair()
                    } label: {
                        Label(localizer.text(.controlCenterNativeRepairInspectAction), systemImage: "doc.text.magnifyingglass")
                    }
                    .disabled(model.isBusy)
                    .accessibilityHint(localizer.text(.controlCenterNativeRepairInspectHint))
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.vertical, 4)
        }
    }

    @ViewBuilder
    private var openCodexCandidates: some View {
        Divider()
        Text(localizer.text(.controlCenterNativeRepairCandidateTitle))
            .font(.body.weight(.semibold))
        ownerPreflight
        switch model.nativeRepairDiscoveryState {
        case .idle:
            Button(localizer.text(.controlCenterNativeRepairRediscover)) {
                model.rediscoverNativeRepairOpenCodex()
            }
            .disabled(model.isBusy)
        case let .searching(tier):
            HStack(spacing: 8) {
                ProgressView().controlSize(.small)
                Text(localizer.text(.menuDiscoverySearching, tier.rawValue.uppercased()))
                    .font(.body)
            }
        case let .candidates(.integrated(result)):
            VStack(alignment: .leading, spacing: 8) {
                ForEach(result.candidates) { candidate in
                    let restoreReady = candidate.nativeRepairSelection != nil
                    Button {
                        model.chooseNativeRepairOpenCodexCandidate(id: candidate.id)
                    } label: {
                        VStack(alignment: .leading, spacing: 3) {
                            HStack {
                                Text(localizer.text(
                                    .controlCenterNativeRepairCandidateAction,
                                    candidate.manager.rawValue,
                                    candidate.version,
                                    candidate.tier.rawValue.uppercased()
                                ))
                                Spacer(minLength: 8)
                                if model.nativeRepairOpenCodexCandidate?.id == candidate.id {
                                    Label(
                                        localizer.text(.controlCenterNativeRepairCandidateSelected),
                                        systemImage: "checkmark.circle.fill"
                                    )
                                    .foregroundStyle(.green)
                                }
                            }
                            if !restoreReady {
                                Text(localizer.text(.controlCenterNativeRepairCandidateRuntimeUnverified))
                                    .font(.caption)
                                    .foregroundStyle(.secondary)
                                    .multilineTextAlignment(.leading)
                            }
                        }
                        .fixedSize(horizontal: false, vertical: true)
                    }
                    .disabled(model.isBusy || !restoreReady)
                    .accessibilityHint(restoreReady
                        ? localizer.text(.controlCenterNativeRepairInspectHint)
                        : localizer.text(.controlCenterNativeRepairCandidateRuntimeUnverified))
                }
                Button(localizer.text(.controlCenterNativeRepairRediscover)) {
                    model.rediscoverNativeRepairOpenCodex()
                }
                .disabled(model.isBusy)
            }
        case .nativeSearching, .candidates(.standaloneNative), .broadScanApprovalRequired, .notFound:
            VStack(alignment: .leading, spacing: 8) {
                ControlCenterSupportingText(
                    localizer.text(.controlCenterNativeRepairCandidateNone),
                    systemImage: "magnifyingglass"
                )
                Button(localizer.text(.controlCenterNativeRepairRediscover)) {
                    model.rediscoverNativeRepairOpenCodex()
                }
                .disabled(model.isBusy)
            }
        case .failed:
            VStack(alignment: .leading, spacing: 8) {
                ControlCenterSupportingText(
                    localizer.text(.controlCenterNativeRepairCandidateNone),
                    systemImage: "exclamationmark.triangle"
                )
                Button(localizer.text(.controlCenterNativeRepairRediscover)) {
                    model.rediscoverNativeRepairOpenCodex()
                }
                .disabled(model.isBusy)
            }
        }
    }


    @ViewBuilder
    private var ownerPreflight: some View {
        if let owner = model.nativeRepairOwnerInspection {
            VStack(alignment: .leading, spacing: 8) {
                StatusRow(
                    localizer.text(.controlCenterNativeRepairOwnerConfiguration),
                    value: localizer.text(configurationKey(owner.configuration))
                )
                StatusRow(
                    localizer.text(.controlCenterNativeRepairOwnerIntegration),
                    value: localizer.text(integrationKey(owner.integration))
                )
                let ready = owner.configuration == .valid &&
                    (owner.integration == .enabled || owner.integration == .disabled)
                StatusRow(
                    localizer.text(.controlCenterNativeRepairOwnerReadiness),
                    value: localizer.text(ready
                        ? .controlCenterNativeRepairOwnerReady
                        : .controlCenterNativeRepairOwnerBlocked)
                )
            }
        } else if model.nativeRepairOpenCodexCandidate != nil, model.isBusy {
            HStack(spacing: 8) {
                ProgressView().controlSize(.small)
                Text(localizer.text(.controlCenterNativeRepairInspectHint))
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
        }
    }

    private func progressView(_ progress: NativeRepairProgress) -> some View {
        GroupBox(localizer.text(.controlCenterNativeRepairProgress)) {
            VStack(alignment: .leading, spacing: 12) {
                if isSuccessfulCompletion(progress) {
                    completedProgressView(progress)
                } else {
                    detailedProgressView(progress)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.vertical, 4)
        }
    }

    @ViewBuilder
    private func detailedProgressView(_ progress: NativeRepairProgress) -> some View {
        ControlCenterSupportingText(
            localizer.text(.controlCenterNativeRepairProgressDetail),
            systemImage: "list.bullet.clipboard"
        )
        if progress.inspection.kind == .openCodex {
            ControlCenterSupportingText(
                localizer.text(.controlCenterNativeRepairOwnerRetryPolicy),
                systemImage: "clock.arrow.trianglehead.counterclockwise.rotate.90"
            )
        }
        progressSteps(progress)
        if let result = progress.result {
            Divider()
            SafeStatusMessageView(message: result, localizer: localizer)
        }
    }

    @ViewBuilder
    private func completedProgressView(_ progress: NativeRepairProgress) -> some View {
        Label {
            Text(localizer.text(.controlCenterNativeRepairCompletedTitle))
                .font(.headline)
                .foregroundStyle(.primary)
        } icon: {
            Image(systemName: "checkmark.circle.fill")
                .foregroundStyle(.green)
        }

        if let result = progress.result {
            SafeStatusMessageView(message: result, localizer: localizer)
        }

        if progress.inspection.kind == .openCodex {
            Button {
                openLocalOpenCodex()
            } label: {
                Label(localizer.text(.controlCenterNativeRepairOpenLocal), systemImage: "shippingbox")
            }
        }

        DisclosureGroup {
            progressSteps(progress)
                .padding(.top, 8)
        } label: {
            Label(
                localizer.text(.controlCenterNativeRepairCompletedSteps),
                systemImage: "list.bullet"
            )
            .font(.body.weight(.medium))
        }
    }

    private func progressSteps(_ progress: NativeRepairProgress) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            ForEach(NativeRepairFlowStep.allCases, id: \.self) { step in
                progressRow(step, state: progress.state(for: step))
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func isSuccessfulCompletion(_ progress: NativeRepairProgress) -> Bool {
        progress.failedStep == nil && progress.receipt != nil && progress.result != nil
    }

    private func progressRow(_ step: NativeRepairFlowStep, state: NativeRepairStepState) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            Image(systemName: stateSymbol(state))
                .foregroundStyle(stateColor(state))
                .frame(width: 18)
            Text(localizer.text(stepKey(step)))
                .font(.body)
                .foregroundStyle(.primary)
            Spacer(minLength: 12)
            Text(localizer.text(stateKey(state)))
                .font(.subheadline.weight(.medium))
                .foregroundStyle(stateColor(state))
        }
        .fixedSize(horizontal: false, vertical: true)
        .accessibilityElement(children: .combine)
    }

    private func configurationKey(_ value: NativeOwnerConfiguration) -> AppStringKey {
        switch value {
        case .valid: .controlCenterNativeRepairOwnerConfigurationValid
        case .invalid: .controlCenterNativeRepairOwnerConfigurationInvalid
        case .unavailable: .controlCenterNativeRepairOwnerConfigurationUnavailable
        }
    }

    private func integrationKey(_ value: NativeOwnerIntegration) -> AppStringKey {
        switch value {
        case .enabled: .controlCenterNativeRepairOwnerIntegrationEnabled
        case .disabled: .controlCenterNativeRepairOwnerIntegrationDisabled
        case .unknown: .controlCenterNativeRepairOwnerIntegrationUnknown
        }
    }

    private func ownerKey(_ kind: NativeRepairKind) -> AppStringKey {
        switch kind {
        case .stateOnly: .controlCenterNativeRepairOwnerStateOnly
        case .localRelay: .controlCenterNativeRepairOwnerLocalRelay
        case .openCodex: .controlCenterNativeRepairOwnerOpenCodex
        case .unavailable: .controlCenterNativeRepairOwnerUnavailable
        }
    }

    private func detailKey(_ kind: NativeRepairKind) -> AppStringKey {
        switch kind {
        case .stateOnly: .controlCenterNativeRepairStateOnlyDetail
        case .localRelay: .controlCenterNativeRepairLocalRelayDetail
        case .openCodex: .controlCenterNativeRepairOpenCodexDetail
        case .unavailable: .controlCenterNativeRepairUnavailableDetail
        }
    }

    private func stepKey(_ step: NativeRepairFlowStep) -> AppStringKey {
        switch step {
        case .preflight: .controlCenterNativeRepairStepPreflight
        case .desktopExit: .controlCenterNativeRepairStepDesktopExit
        case .ownerRepair: .controlCenterNativeRepairStepOwnerRepair
        case .nativeVerification: .controlCenterNativeRepairStepNativeVerification
        case .stateCommit: .controlCenterNativeRepairStepStateCommit
        case .desktopRelaunch: .controlCenterNativeRepairStepDesktopRelaunch
        case .statusRefresh: .controlCenterNativeRepairStepStatusRefresh
        }
    }

    private func stateKey(_ state: NativeRepairStepState) -> AppStringKey {
        switch state {
        case .pending: .controlCenterNativeRepairStepPending
        case .running: .controlCenterNativeRepairStepRunning
        case .completed: .controlCenterNativeRepairStepCompleted
        case .failed: .controlCenterNativeRepairStepFailed
        }
    }

    private func stateSymbol(_ state: NativeRepairStepState) -> String {
        switch state {
        case .pending: "circle"
        case .running: "circle.dotted"
        case .completed: "checkmark.circle.fill"
        case .failed: "exclamationmark.triangle.fill"
        }
    }

    private func stateColor(_ state: NativeRepairStepState) -> Color {
        switch state {
        case .pending: .secondary
        case .running: .accentColor
        case .completed: .green
        case .failed: .orange
        }
    }
}
