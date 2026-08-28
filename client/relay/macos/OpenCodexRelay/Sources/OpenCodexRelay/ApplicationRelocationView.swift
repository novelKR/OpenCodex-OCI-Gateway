import SwiftUI
import OpenCodexRelayLocalization

struct ApplicationRelocationView: View {
    @ObservedObject var controller: ApplicationRelocationController
    let localizer: AppLocalizer

    var body: some View {
        ControlCenterSectionCard(
            localizer.text(.relocationTitle),
            systemImage: "arrow.forward.app"
        ) {
            VStack(alignment: .leading, spacing: 12) {
                ControlCenterSupportingText(
                    localizer.text(detailKey),
                    systemImage: detailSymbol
                )

                if showsProgress {
                    ProgressView()
                        .controlSize(.small)
                }

                if controller.canStart {
                    ControlCenterActionFooter {} primary: {
                        Button {
                            controller.begin()
                        } label: {
                            Label(
                                localizer.text(Self.primaryActionKey(for: controller.state)),
                                systemImage: controller.state == .failed(.sourceProcessInvalid)
                                    ? "arrow.clockwise"
                                    : "arrow.forward.app"
                            )
                        }
                        .buttonStyle(.glassProminent)
                    }
                } else if controller.state == .sourceExitRequired ||
                            controller.state == .backupCleanupFailed {
                    ControlCenterActionFooter {} primary: {
                        Button {
                            controller.retryHandoff()
                        } label: {
                            Label(
                                localizer.text(.relocationRetryHandoff),
                                systemImage: "arrow.clockwise"
                            )
                        }
                        .buttonStyle(.glassProminent)
                    }
                } else if controller.state == .failed(.sourceChanged) ||
                            controller.state == .failed(.trashFailed) {
                    ControlCenterActionFooter {} primary: {
                        Button(localizer.text(.relocationCleanupKeep)) {
                            controller.keepOriginal()
                        }
                        .buttonStyle(.glassProminent)
                    }
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.vertical, 4)
        }
        .alert(
            localizer.text(.relocationFallbackTitle),
            isPresented: confirmationBinding(for: .fallback)
        ) {
            Button(localizer.text(.relocationCancel), role: .cancel) {
                controller.cancelConfirmation()
            }
            Button(localizer.text(.relocationFallbackAction)) {
                controller.approveUserApplicationsFallback()
            }
        } message: {
            Text(localizer.text(.relocationFallbackDetail))
        }
        .alert(
            localizer.text(.relocationReplacementTitle),
            isPresented: confirmationBinding(for: .replacement)
        ) {
            Button(localizer.text(.relocationCancel), role: .cancel) {
                controller.cancelConfirmation()
            }
            Button(localizer.text(.relocationReplacementAction)) {
                controller.approveReplacement()
            }
        } message: {
            Text(localizer.text(.relocationReplacementDetail))
        }
        .alert(
            localizer.text(.relocationCleanupTitle),
            isPresented: confirmationBinding(for: .cleanup)
        ) {
            Button(localizer.text(.relocationCleanupKeep), role: .cancel) {
                controller.keepOriginal()
            }
            Button(localizer.text(.relocationCleanupTrash), role: .destructive) {
                Task { await controller.moveOriginalToTrash() }
            }
        } message: {
            Text(localizer.text(.relocationCleanupDetail))
        }
    }

    private var showsProgress: Bool {
        controller.state == .preparing || controller.state == .waitingForDestination
    }

    private var detailKey: AppStringKey {
        Self.detailKey(for: controller.state)
    }

    static func detailKey(for state: ApplicationRelocationState) -> AppStringKey {
        switch state {
        case .preview:
            .relocationPreviewDetail
        case .preparing:
            .relocationPreparing
        case .waitingForDestination:
            .relocationWaitingForDestination
        case .sourceExitRequired:
            .relocationSourceExitRequired
        case .backupCleanupFailed:
            .relocationBackupCleanupFailed
        case .completed:
            .relocationCompleted
        case .recoveryRequired:
            .relocationRecoveryDetail
        case .failed(.sourceBundleInvalid):
            .relocationSourceBundleInvalid
        case .failed(.sourceProcessInvalid):
            .relocationSourceProcessInvalid
        case .failed(.sourceLocationInvalid):
            .relocationSourceLocationInvalid
        case .failed(.destinationUnavailable), .failed(.copyFailed),
             .failed(.sourceChanged), .failed(.trashFailed):
            .relocationManualDetail
        case .unavailable, .available, .fallbackConfirmationRequired,
             .replacementConfirmationRequired, .sourceCleanupRequired,
             .failed:
            .relocationDetail
        }
    }

    static func primaryActionKey(for state: ApplicationRelocationState) -> AppStringKey {
        state == .failed(.sourceProcessInvalid)
            ? .relocationRetryValidation
            : .relocationMoveAction
    }

    private var detailSymbol: String {
        switch controller.state {
        case .completed: "checkmark.circle.fill"
        case .failed, .sourceExitRequired, .backupCleanupFailed, .recoveryRequired:
            "exclamationmark.triangle.fill"
        case .preview: "eye"
        default: "info.circle"
        }
    }

    private enum Confirmation {
        case fallback
        case replacement
        case cleanup
    }

    private func confirmationBinding(for confirmation: Confirmation) -> Binding<Bool> {
        Binding(
            get: {
                switch confirmation {
                case .fallback:
                    return controller.state == .fallbackConfirmationRequired
                case .replacement:
                    if case .replacementConfirmationRequired = controller.state { return true }
                    return false
                case .cleanup:
                    return controller.state == .sourceCleanupRequired
                }
            },
            set: { value in
                if !value && controller.state != .sourceCleanupRequired {
                    controller.cancelConfirmation()
                }
            }
        )
    }
}
