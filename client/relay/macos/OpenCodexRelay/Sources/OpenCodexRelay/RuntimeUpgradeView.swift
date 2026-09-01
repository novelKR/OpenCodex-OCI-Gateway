import SwiftUI
import OpenCodexRelayCore
import OpenCodexRelayLocalization

struct RuntimeUpgradeCard: View {
    @ObservedObject var controller: RuntimeUpgradeController
    @ObservedObject var model: MenuBarModel
    @ObservedObject var relocation: ApplicationRelocationController
    let localizer: AppLocalizer
    @State private var showsApplyConfirmation = false
    @State private var showsRecoveryConfirmation = false

    var body: some View {
        ControlCenterSectionCard(
            localizer.text(.runtimeUpgradeTitle),
            systemImage: "server.rack"
        ) {
            VStack(alignment: .leading, spacing: 12) {
                HStack(spacing: 8) {
                    ControlCenterStatusBadge(
                        text: localizer.text(stateKey),
                        tone: stateTone
                    )
                    if controller.isBusy {
                        ProgressView().controlSize(.small)
                    }
                }

                if let inspection = controller.inspection {
                    if !inspection.installedRuntimeVersion.isEmpty {
                        StatusRow(
                            localizer.text(.runtimeUpgradeInstalledVersion),
                            value: inspection.installedRuntimeVersion,
                            showsDivider: true
                        )
                    }
                    StatusRow(
                        localizer.text(.runtimeUpgradeBundledVersion),
                        value: inspection.bundledRuntimeVersion
                    )
                }

                ControlCenterSupportingText(
                    localizer.text(detailKey),
                    systemImage: stateTone.systemImage
                )

                if let code = controller.lastErrorCode {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(localizer.text(.runtimeUpgradeFailedDetail))
                            .foregroundStyle(.orange)
                        Text(code)
                            .font(.caption.monospaced())
                            .foregroundStyle(.secondary)
                    }
                }

                AdaptiveActionRow {
                    Button {
                        controller.refresh()
                    } label: {
                        Label(localizer.text(.runtimeUpgradeRefresh), systemImage: "arrow.clockwise")
                    }
                    .disabled(controller.isBusy)

                    if controller.inspection?.state == .upgradeAvailable {
                        Button {
                            showsApplyConfirmation = true
                        } label: {
                            Label(localizer.text(.runtimeUpgradeApply), systemImage: "restart.circle")
                        }
                        .buttonStyle(.glassProminent)
                        .disabled(mutationBlocked || !controller.canApply)
                    }

                    if controller.inspection?.state == .recoveryRequired {
                        Button {
                            showsRecoveryConfirmation = true
                        } label: {
                            Label(localizer.text(.runtimeUpgradeRecover), systemImage: "arrow.uturn.backward.circle")
                        }
                        .buttonStyle(.glassProminent)
                        .disabled(mutationBlocked || !controller.canRecover)
                    }
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.vertical, 4)
        }
        .confirmationDialog(
            localizer.text(.runtimeUpgradeConfirmationTitle),
            isPresented: $showsApplyConfirmation,
            titleVisibility: .visible
        ) {
            Button(localizer.text(.runtimeUpgradeConfirmationAction), role: .destructive) {
                controller.applyConfirmed()
            }
            Button(localizer.text(.runtimeUpgradeCancel), role: .cancel) {}
        } message: {
            Text(localizer.text(.runtimeUpgradeConfirmationDetail))
        }
        .confirmationDialog(
            localizer.text(.runtimeUpgradeRecoveryConfirmationTitle),
            isPresented: $showsRecoveryConfirmation,
            titleVisibility: .visible
        ) {
            Button(localizer.text(.runtimeUpgradeRecover), role: .destructive) {
                controller.recoverConfirmed()
            }
            Button(localizer.text(.runtimeUpgradeCancel), role: .cancel) {}
        } message: {
            Text(localizer.text(.runtimeUpgradeRecoveryConfirmationDetail))
        }
    }

    private var mutationBlocked: Bool {
        model.isBusy ||
            model.status?.phase == .recoveryRequired ||
            model.hasPendingOpenCodexRemovalRecovery ||
            !relocation.permitsGatewayConfiguration
    }

    private var stateKey: AppStringKey {
        switch controller.inspection?.state {
        case .notIntegrated: .runtimeUpgradeStateNotIntegrated
        case .current: .runtimeUpgradeStateCurrent
        case .upgradeAvailable: .runtimeUpgradeStateAvailable
        case .recoveryRequired: .runtimeUpgradeStateRecoveryRequired
        case .incompatible: .runtimeUpgradeStateIncompatible
        case nil: .runtimeUpgradeStateChecking
        }
    }

    private var detailKey: AppStringKey {
        switch controller.inspection?.state {
        case .notIntegrated: .runtimeUpgradeDetailNotIntegrated
        case .current: .runtimeUpgradeDetailCurrent
        case .upgradeAvailable: .runtimeUpgradeDetailAvailable
        case .recoveryRequired: .runtimeUpgradeDetailRecoveryRequired
        case .incompatible: .runtimeUpgradeDetailIncompatible
        case nil: .runtimeUpgradeDetailChecking
        }
    }

    private var stateTone: ControlCenterStatusTone {
        switch controller.inspection?.state {
        case .current: .success
        case .upgradeAvailable: .info
        case .notIntegrated, nil: .neutral
        case .recoveryRequired: .warning
        case .incompatible: .error
        }
    }
}
