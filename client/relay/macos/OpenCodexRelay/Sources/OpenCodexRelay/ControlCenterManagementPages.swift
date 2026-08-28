import SwiftUI
import OpenCodexRelayLocalization

struct MaintenanceControlCenterPage: View {
    @ObservedObject var model: MenuBarModel
    let localizer: AppLocalizer
    let title: String
    let systemImage: String
    let confirmNativeRepair: () -> Void
    let openLocalOpenCodex: () -> Void

    var body: some View {
        ControlCenterPage(title: title, systemImage: systemImage, model: model, localizer: localizer) {
            if model.status?.phase == .recoveryRequired {
                GroupBox(localizer.text(.controlCenterRoutingRecovery)) {
                    VStack(alignment: .leading, spacing: 12) {
                        AdaptiveActionRow {
                            Button {
                                model.recover(.rollback)
                            } label: {
                                Label(localizer.text(.menuRollbackRecovery), systemImage: "arrow.uturn.backward")
                            }
                            .disabled(model.isBusy || !model.canRecover(.rollback))

                            Button {
                                model.recover(.complete)
                            } label: {
                                Label(localizer.text(.menuCompleteRecovery), systemImage: "checkmark.shield")
                            }
                            .disabled(model.isBusy || !model.canRecover(.complete))
                        }

                        if !model.canRecover(.rollback) {
                            ControlCenterSupportingText(
                                localizer.text(model.recoveryReasonKey(for: .rollback)),
                                systemImage: "lock.shield"
                            )
                        }
                        if !model.canRecover(.complete) {
                            ControlCenterSupportingText(
                                localizer.text(model.recoveryReasonKey(for: .complete)),
                                systemImage: "lock.shield"
                            )
                        }

                        if model.canRepairNative {
                            Divider()
                            NativeRepairMaintenancePanel(
                                model: model,
                                localizer: localizer,
                                confirmStateOnly: confirmNativeRepair,
                                confirmOwnedRepair: confirmNativeRepair,
                                openLocalOpenCodex: openLocalOpenCodex
                            )
                        }
                    }
                    .padding(.vertical, 4)
                }
            }

            if model.status?.phase != .recoveryRequired,
               model.nativeRepairProgress != nil {
                NativeRepairMaintenancePanel(
                    model: model,
                    localizer: localizer,
                    confirmStateOnly: {},
                    confirmOwnedRepair: {},
                    openLocalOpenCodex: openLocalOpenCodex
                )
            }

            if model.homebrewGuardAvailability.registration != .notRequired {
                HomebrewGuardStatusCard(model: model, localizer: localizer)
            }

            if model.hasPendingOpenCodexRemovalRecovery && model.openCodexRemovalFlow == nil {
                GroupBox(localizer.text(.controlCenterOpenCodexMaintenance)) {
                    AdaptiveActionRow {
                        Button {
                            model.resumePendingOpenCodexRemoval()
                        } label: {
                            Label(
                                localizer.text(.menuResumeRemovalRecovery),
                                systemImage: "arrow.clockwise.circle"
                            )
                        }
                        .disabled(model.isBusy)
                        .accessibilityHint(localizer.text(.menuResumeRemovalRecoveryHint))
                    }
                    .padding(.vertical, 4)
                }
            }

            if model.openCodexRemovalFlow != nil {
                ControlCenterSupportingText(
                    localizer.text(.controlCenterRemovalInProgress),
                    systemImage: "shippingbox.and.arrow.backward"
                )
            }

            if model.status?.phase != .recoveryRequired &&
                !model.hasPendingOpenCodexRemovalRecovery &&
                model.openCodexRemovalFlow == nil &&
                model.nativeRepairProgress == nil &&
                model.homebrewGuardAvailability.registration == .notRequired {
                ContentUnavailableView(
                    localizer.text(.controlCenterNoMaintenance),
                    systemImage: "checkmark.shield"
                )
            }
        }
        .id(presentationIdentity)
    }

    private var presentationIdentity: String {
        let generation = model.status.map { String($0.generation) } ?? "unknown"
        let phase = model.status?.phase.rawValue ?? "unknown"
        return "\(generation):\(phase)"
    }
}

struct ActivityLogControlCenterPage: View {
    @ObservedObject var model: MenuBarModel
    let localizer: AppLocalizer
    let title: String
    let systemImage: String

    var body: some View {
        ControlCenterPage(title: title, systemImage: systemImage, model: model, localizer: localizer) {
            RelayActivityLogView(store: model.activityLog, localizer: localizer)
        }
    }
}
