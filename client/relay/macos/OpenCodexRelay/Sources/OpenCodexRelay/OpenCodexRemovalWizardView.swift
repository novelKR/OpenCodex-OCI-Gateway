import SwiftUI
import OpenCodexRelayCore
import OpenCodexRelayLocalization

struct OpenCodexRemovalWizardView: View {
    @ObservedObject var model: MenuBarModel
    @EnvironmentObject private var localization: LocalizationStore

    private var localizer: AppLocalizer { localization.localizer }

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack {
                Text(localizer.text(.removalTitle))
                    .font(.title2.bold())
                Spacer()
                Button(localizer.text(.removalClose)) {
                    model.dismissOpenCodexRemoval()
                }
                .disabled(!model.canDismissOpenCodexRemoval || model.isBusy)
            }
            Divider()
            ScrollView {
                if let flow = model.openCodexRemovalFlow {
                    phaseView(flow)
                        .frame(maxWidth: .infinity, minHeight: 320, alignment: .topLeading)
                }
            }
        }
        .padding(20)
        .frame(
            minWidth: 560,
            idealWidth: 620,
            minHeight: 380,
            idealHeight: 500
        )
        .interactiveDismissDisabled(!model.canDismissOpenCodexRemoval)
    }

    @ViewBuilder
    private func phaseView(_ flow: OpenCodexRemovalFlow) -> some View {
        switch flow.phase {
        case .actions:
            actionsView(flow)
        case .handoff:
            if let progress = flow.handoffProgress {
                OpenCodexHandoffProgressView(
                    progress: progress,
                    status: model.status,
                    automaticRemovalEligible: flow.automaticRemovalEligible,
                    localizer: localizer
                )
            }
        case .loadingInventory:
            progressView(
                title: localizer.text(.removalInventoryLoading),
                detail: localizer.text(.removalInventoryLoadingDetail)
            )
        case .confirmTrash:
            trashConfirmationView(flow)
        case .dataRefreshRequired:
            recoveryView(
                title: localizer.text(.removalDataRefreshTitle),
                detail: localizer.text(.removalDataRefreshDetail),
                actionTitle: localizer.text(.removalDataRefreshAction),
                action: model.refreshInterruptedOpenCodexInventory
            )
        case .options:
            optionsView(flow)
        case .confirmRemoval:
            packageConfirmationView(flow)
        case .quittingDesktop:
            if let progress = flow.removalProgress {
                OpenCodexRemovalExecutionProgressView(
                    progress: progress,
                    localizer: localizer
                )
            } else {
                progressView(
                    title: localizer.text(.removalQuittingDesktop),
                    detail: localizer.text(.removalQuittingDesktopDetail)
                )
            }
        case .removing:
            if let progress = flow.removalProgress {
                OpenCodexRemovalExecutionProgressView(
                    progress: progress,
                    localizer: localizer
                )
            } else {
                progressView(
                    title: localizer.text(.removalRunning),
                    detail: localizer.text(.removalRunningDetail)
                )
            }
        case .rebootRequired:
            recoveryView(
                title: localizer.text(.removalRebootTitle),
                detail: localizer.text(.removalRebootDetail),
                actionTitle: localizer.text(.removalRebootAction),
                action: model.prepareRebootedOpenCodexRecovery
            )
        case .routingRecoveryRequired:
            recoveryView(
                title: localizer.text(.removalRoutingRecoveryTitle),
                detail: localizer.text(.removalRoutingRecoveryDetail),
                actionTitle: localizer.text(.removalRoutingRecoveryAction),
                action: model.checkOpenCodexRoutingRecovery
            )
        case .nativeRecoveryRequired:
            recoveryView(
                title: localizer.text(.removalNativeRecoveryTitle),
                detail: localizer.text(.removalNativeRecoveryDetail),
                actionTitle: localizer.text(.removalNativeRecoveryAction),
                action: model.checkOpenCodexNativeRecovery
            )
        case .nativeTerminalCleanupPending:
            recoveryView(
                title: localizer.text(.removalNativeCleanupTitle),
                detail: localizer.text(.removalNativeCleanupDetail),
                actionTitle: localizer.text(.removalNativeCleanupAction),
                action: model.checkOpenCodexNativeTerminalCleanup
            )
        case .result:
            resultView(flow)
        case .failed:
            failedView(flow)
        }
    }

    private func actionsView(_ flow: OpenCodexRemovalFlow) -> some View {
        VStack(alignment: .leading, spacing: 14) {
            candidateSummary(flow)
            Text(localizer.text(
                flow.context == .standaloneNative
                    ? .removalNativeActionsDetail
                    : .removalActionsDetail
            ))
                .foregroundStyle(.secondary)

            if flow.requiresHomebrewGuard || model.canRecoverHomebrewGuard {
                HomebrewGuardStatusCard(model: model, localizer: localizer)
            }

            if flow.context == .integrated, let progress = flow.handoffProgress {
                OpenCodexHandoffProgressView(
                    progress: progress,
                    status: model.status,
                    automaticRemovalEligible: flow.automaticRemovalEligible,
                    localizer: localizer
                )
            }

            if flow.context == .integrated {
                GroupBox(localizer.text(.removalHandoffSection)) {
                VStack(alignment: .leading, spacing: 10) {
                    Button(localizer.displayName(OpenCodexHandoffAction.retainProxyRemoveShim)) {
                        model.chooseOpenCodexHandoffAction(.retainProxyRemoveShim)
                    }
                    .disabled(model.isBusy || !model.canStartOpenCodexHandoff)
                    Text(localizer.text(.removalHandoffRemoveShimDetail))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Button(localizer.displayName(OpenCodexHandoffAction.retainProxyKeepShim)) {
                        model.chooseOpenCodexHandoffAction(.retainProxyKeepShim)
                    }
                    .disabled(model.isBusy || !model.canStartOpenCodexHandoff)
                    Text(localizer.text(.removalHandoffKeepShimDetail))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    if let blocked = model.openCodexHandoffAvailabilityMessage {
                        Label(
                            blocked.text(using: localizer),
                            systemImage: "exclamationmark.triangle.fill"
                        )
                        .font(.caption)
                        .foregroundStyle(.orange)
                        .fixedSize(horizontal: false, vertical: true)
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.vertical, 4)
                }
            }

            GroupBox(localizer.text(
                flow.context == .standaloneNative
                    ? .removalNativeSafeSection
                    : .removalSafeSection
            )) {
                VStack(alignment: .leading, spacing: 10) {
                    Text(localizer.text(
                        flow.context == .standaloneNative
                            ? .removalNativeSafeDetail
                            : .removalSafeDetail
                    ))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Button(localizer.text(
                        flow.context == .standaloneNative
                            ? .removalNativeSafeAction
                            : .removalSafeAction
                    ), role: .destructive) {
                        model.beginOpenCodexRemoval()
                    }
                    .disabled(
                        !model.canBeginOpenCodexRemoval || model.isBusy
                    )
                    if model.canBeginOpenCodexRemoval {
                        Label(
                            localizer.text(.removalHandoffResultRemovalAvailable),
                            systemImage: "checkmark.shield.fill"
                        )
                        .font(.caption)
                        .foregroundStyle(.green)
                    } else if !flow.automaticRemovalEligible {
                        Label(localizer.text(.removalManualOnly), systemImage: "lock.shield")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    } else if flow.context == .integrated,
                              model.status?.phase == .recoveryRequired {
                        Label(localizer.text(.removalHandoffResultRecoveryRequired), systemImage: "arrow.trianglehead.2.clockwise.rotate.90")
                            .font(.caption)
                            .foregroundStyle(.orange)
                    } else if flow.context == .integrated,
                              model.status?.canUninstallOpenCodex != true {
                        Label(localizer.text(.removalRouteUnsafe), systemImage: "arrow.trianglehead.branch")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.vertical, 4)
            }
            Spacer()
        }
    }

    private var homebrewGuardBlockingTitle: String {
        switch model.homebrewGuardAvailability.registration {
        case .preview: localizer.text(.homebrewGuardStatePreview)
        case .notRegistered: localizer.text(.homebrewGuardStateNotRegistered)
        case .approvalRequired: localizer.text(.homebrewGuardStateApprovalRequired)
        case .manualInstallRequired: localizer.text(.homebrewGuardStateManualInstallRequired)
        case .manualUpdateRequired: localizer.text(.homebrewGuardStateManualUpdateRequired)
        case .manualInstallerRecoveryRequired:
            localizer.text(.homebrewGuardStateManualInstallerRecoveryRequired)
        case .daemonLaunchFailed: localizer.text(.homebrewGuardStateDaemonLaunchFailed)
        case .busy: localizer.text(.homebrewGuardStateBusy)
        case .recoveryRequired: localizer.text(.homebrewGuardStateRecoveryRequired)
        case .unavailable: localizer.text(.homebrewGuardStateUnavailable)
        case .ready: localizer.text(.homebrewGuardStateReady)
        case .notRequired: localizer.text(.homebrewGuardStateNotRequired)
        }
    }

    private func optionsView(_ flow: OpenCodexRemovalFlow) -> some View {
        VStack(alignment: .leading, spacing: 14) {
            Text(localizer.text(.removalOptionsTitle))
                .font(.headline)
            Text(localizer.text(.removalOptionsDetail))
                .foregroundStyle(.secondary)

            Picker(
                localizer.text(.removalDataMode),
                selection: Binding(
                    get: { flow.mode },
                    set: { mode in model.setOpenCodexRemovalMode(mode) }
                )
            ) {
                Text(localizer.text(.removalModePreserve))
                    .tag(OpenCodexRemovalMode.preserveData)
                if flow.supportsSelectiveTrash {
                    Text(localizer.text(.removalModeTrash))
                        .tag(OpenCodexRemovalMode.trashSelected)
                }
            }
            .pickerStyle(.radioGroup)

            Text(localizer.text(
                flow.mode == .preserveData
                    ? .removalPreserveDetail
                    : .removalTrashDetail
            ))
            .font(.caption)
            .foregroundStyle(.secondary)

            if flow.mode == .preserveData {
                preservationSummary
            } else if let inventory = flow.inventory {
                GroupBox(localizer.text(.removalInventoryTitle)) {
                    VStack(alignment: .leading, spacing: 8) {
                        ForEach(inventory.items) { item in
                            Toggle(
                                isOn: Binding(
                                    get: { flow.selectedDataItemIDs.contains(item.id) },
                                    set: { _ in model.toggleOpenCodexDataItem(id: item.id) }
                                )
                            ) {
                                VStack(alignment: .leading, spacing: 2) {
                                    Text(item.relativePath)
                                        .privacySensitive()
                                    Text(item.trashable
                                        ? localizer.displayName(item.category)
                                        : localizer.text(.removalProtectedItem))
                                        .font(.caption)
                                        .foregroundStyle(.secondary)
                                }
                            }
                            .toggleStyle(.checkbox)
                            .disabled(!flow.isSelectable(item))
                        }
                    }
                    .padding(.vertical, 4)
                }
            }

            Spacer()
            HStack {
                Button(localizer.text(.removalCancel)) {
                    model.dismissOpenCodexRemoval()
                }
                Spacer()
                Button(localizer.text(.removalReviewAction)) {
                    model.reviewOpenCodexRemoval()
                }
                .keyboardShortcut(.defaultAction)
                .disabled(!flow.canContinueFromOptions || model.isBusy)
            }
        }
    }

    private var preservationSummary: some View {
        VStack(alignment: .leading, spacing: 12) {
            GroupBox(localizer.text(.removalPreservedTitle)) {
                Label(localizer.text(.removalPreservedDetail), systemImage: "externaldrive.badge.checkmark")
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.vertical, 4)
            }
            GroupBox(localizer.text(.removalRemovedTitle)) {
                Label(localizer.text(.removalRemovedDetail), systemImage: "shippingbox.and.arrow.backward")
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.vertical, 4)
            }
        }
    }

    private func packageConfirmationView(_ flow: OpenCodexRemovalFlow) -> some View {
        VStack(alignment: .leading, spacing: 14) {
            Label(localizer.text(.removalConfirmPackageTitle), systemImage: "exclamationmark.shield")
                .font(.headline)
            Text(localizer.text(.removalConfirmPackageDetail))
            if flow.mode == .preserveData {
                preservationSummary
            } else {
                Text(localizer.text(.removalSecondConfirmationNotice))
                    .font(.caption)
                    .foregroundStyle(.orange)
            }
            confirmationSummary(flow)
            if flow.requiresHomebrewGuard {
                HomebrewGuardStatusCard(model: model, localizer: localizer)
            }
            Spacer()
            HStack {
                Button(localizer.text(.removalBack)) {
                    model.returnToOpenCodexRemovalOptions()
                }
                Spacer()
                Button(localizer.text(.removalConfirmPackageAction), role: .destructive) {
                    model.confirmOpenCodexPackageRemoval()
                }
                .keyboardShortcut(.defaultAction)
                .disabled(model.isBusy)
            }
        }
    }

    private func trashConfirmationView(_ flow: OpenCodexRemovalFlow) -> some View {
        VStack(alignment: .leading, spacing: 14) {
            Label(localizer.text(.removalConfirmTrashTitle), systemImage: "trash")
                .font(.headline)
            Text(localizer.text(
                .removalConfirmTrashDetail,
                localizer.formattedNumber(flow.selectedDataItemIDs.count)
            ))
            Label(
                localizer.text(.removalTrashNoPermanentDelete),
                systemImage: "hand.raised.fill"
            )
            .font(.caption)
            .foregroundStyle(.secondary)
            confirmationSummary(flow)
            Spacer()
            HStack {
                Button(localizer.text(.removalBack)) {
                    model.returnToOpenCodexRemovalOptions()
                }
                Spacer()
                Button(localizer.text(.removalConfirmTrashAction), role: .destructive) {
                    model.confirmOpenCodexDataTrash()
                }
                .keyboardShortcut(.defaultAction)
                .disabled(model.isBusy)
            }
        }
    }

    private func resultView(_ flow: OpenCodexRemovalFlow) -> some View {
        VStack(alignment: .leading, spacing: 14) {
            if let progress = flow.removalProgress {
                OpenCodexRemovalExecutionProgressView(
                    progress: progress,
                    localizer: localizer
                )
            }
            if let receipt = flow.receipt {
                Label(
                    receipt.isSuccessful
                        ? localizer.text(.removalResultSuccess)
                        : localizer.text(.removalResultPartial),
                    systemImage: receipt.isSuccessful ? "checkmark.shield" : "exclamationmark.triangle"
                )
                .font(.headline)
                if receipt.mode == .preserveData {
                    Label(localizer.text(.removalPreservedDetail), systemImage: "externaldrive.badge.checkmark")
                        .foregroundStyle(.secondary)
                } else {
                    Label(
                        localizer.text(
                            .removalResultCounts,
                            String(receipt.movedDataItems),
                            String(receipt.selectedDataItems)
                        ),
                        systemImage: "trash"
                    )
                    .foregroundStyle(.secondary)
                }
                Text(receipt.packageRemoved
                    ? localizer.text(.removalPackageRemoved)
                    : localizer.text(.removalPackageNotVerified))
                GroupBox(localizer.text(.removalStagesTitle)) {
                    ScrollView {
                        VStack(alignment: .leading, spacing: 6) {
                            ForEach(Array(receipt.stages.enumerated()), id: \.offset) { _, stage in
                                Text(localizer.text(
                                    .removalStageRow,
                                    localizer.displayName(stage.stage),
                                    localizer.displayName(stage.status)
                                ))
                            }
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    .frame(maxHeight: 220)
                }
                Spacer()
                HStack {
                    if receipt.isSuccessful {
                        Button(localizer.text(.menuRelaunchDesktop)) {
                            model.relaunchSelectedDesktop()
                        }
                        Spacer()
                        Button(localizer.text(.removalClose)) {
                            model.dismissOpenCodexRemoval()
                        }
                        .keyboardShortcut(.defaultAction)
                    } else {
                        Text(localizer.text(.removalResultRecoveryDetail))
                            .font(.caption)
                            .foregroundStyle(.secondary)
                        Spacer()
                        Button(localizer.text(.removalRequireRebootAction)) {
                            model.requireRebootForOpenCodexResultRecovery()
                        }
                    }
                }
            } else if let receipt = flow.nativeReceipt {
                Label(
                    receipt.isSuccessful
                        ? localizer.text(.removalResultSuccess)
                        : localizer.text(.removalResultPartial),
                    systemImage: receipt.isSuccessful ? "checkmark.shield" : "exclamationmark.triangle"
                )
                .font(.headline)
                if receipt.mode == .preserveData {
                    Label(
                        localizer.text(.removalPreservedDetail),
                        systemImage: "externaldrive.badge.checkmark"
                    )
                    .foregroundStyle(.secondary)
                } else {
                    Label(
                        localizer.text(
                            .removalResultCounts,
                            String(receipt.movedDataItems),
                            String(receipt.selectedDataItems)
                        ),
                        systemImage: "trash"
                    )
                    .foregroundStyle(.secondary)
                }
                Text(receipt.packageRemoved
                    ? localizer.text(.removalPackageRemoved)
                    : localizer.text(.removalPackageNotVerified))
                GroupBox(localizer.text(.removalStagesTitle)) {
                    ScrollView {
                        VStack(alignment: .leading, spacing: 6) {
                            ForEach(Array(receipt.stages.enumerated()), id: \.offset) { _, stage in
                                Text(localizer.text(
                                    .removalStageRow,
                                    localizer.displayName(stage.stage),
                                    localizer.displayName(stage.status)
                                ))
                            }
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    .frame(maxHeight: 220)
                }
                Spacer()
                HStack {
                    if receipt.isSuccessful {
                        Spacer()
                        Button(localizer.text(.removalClose)) {
                            model.dismissOpenCodexRemoval()
                        }
                        .keyboardShortcut(.defaultAction)
                    } else {
                        Text(localizer.text(.removalResultRecoveryDetail))
                            .font(.caption)
                            .foregroundStyle(.secondary)
                        Spacer()
                        Button(localizer.text(.removalRequireRebootAction)) {
                            model.requireRebootForOpenCodexResultRecovery()
                        }
                    }
                }
            } else {
                Text(localizer.text(.removalResultInvalid))
            }
        }
    }

    private func failedView(_ flow: OpenCodexRemovalFlow) -> some View {
        VStack(alignment: .leading, spacing: 14) {
            if let progress = flow.removalProgress {
                OpenCodexRemovalExecutionProgressView(
                    progress: progress,
                    localizer: localizer
                )
            }
            if model.canRecoverHomebrewGuard {
                HomebrewGuardStatusCard(model: model, localizer: localizer)
            }
            Label(localizer.text(.removalFailedTitle), systemImage: "xmark.shield")
                .font(.headline)
            if let failure = flow.failure {
                Text(failure.text(using: localizer))
            } else {
                Text(localizer.text(.removalFailedDetail))
            }
            Text(localizer.text(.removalFailedSafetyDetail))
                .font(.caption)
                .foregroundStyle(.secondary)
            Spacer()
            HStack {
                Spacer()
                Button(localizer.text(.removalClose)) {
                    model.dismissOpenCodexRemoval()
                }
                .keyboardShortcut(.defaultAction)
            }
        }
    }

    private func progressView(title: String, detail: String) -> some View {
        VStack(alignment: .leading, spacing: 14) {
            ProgressView()
            Text(title)
                .font(.headline)
            Text(detail)
                .foregroundStyle(.secondary)
            Spacer()
        }
    }

    private func recoveryView(
        title: String,
        detail: String,
        actionTitle: String,
        action: @escaping () -> Void
    ) -> some View {
        VStack(alignment: .leading, spacing: 14) {
            Label(title, systemImage: "arrow.trianglehead.2.clockwise.rotate.90")
                .font(.headline)
            Text(detail)
            Text(localizer.text(.removalRecoveryNoPIDProof))
                .font(.caption)
                .foregroundStyle(.secondary)
            Spacer()
            HStack {
                Button(localizer.text(.removalClose)) {
                    model.dismissOpenCodexRemoval()
                }
                Spacer()
                Button(actionTitle, action: action)
                    .keyboardShortcut(.defaultAction)
                    .disabled(model.isBusy)
            }
        }
    }

    private func candidateSummary(_ flow: OpenCodexRemovalFlow) -> some View {
        HStack(spacing: 12) {
            Image(systemName: "shippingbox")
                .font(.title2)
            VStack(alignment: .leading, spacing: 2) {
                Text("OpenCodex \(flow.displayVersion ?? localizer.text(.genericUnknown))")
                    .font(.headline)
                if let manager = flow.displayManager, let tier = flow.displayTier {
                    Text(localizer.text(
                        .removalCandidateSummary,
                        manager.rawValue,
                        tier.rawValue.uppercased()
                    ))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                }
            }
        }
    }

    private func confirmationSummary(_ flow: OpenCodexRemovalFlow) -> some View {
        GroupBox {
            VStack(alignment: .leading, spacing: 6) {
                Text(localizer.text(.removalReviewMode, localizer.displayName(flow.mode)))
                Text(localizer.text(
                    flow.context == .standaloneNative
                        ? .removalReviewNativeBoundary
                        : .removalReviewGeneration,
                    flow.context == .standaloneNative
                        ? String((flow.expectedBoundaryRevision ?? localizer.text(.genericUnknown)).prefix(12))
                        : flow.expectedRoutingGeneration.map(localizer.formattedNumber) ?? localizer.text(.genericUnknown)
                ))
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}
