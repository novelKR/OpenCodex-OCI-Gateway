import AppKit
import OpenCodexRelayCore
import OpenCodexRelayLocalization
import SwiftUI

struct ContainerRuntimeCard: View {
    @ObservedObject var controller: ContainerRuntimeController
    @ObservedObject var model: MenuBarModel
    @ObservedObject var relocation: ApplicationRelocationController
    let localizer: AppLocalizer

    @State private var showsActivationConfirmation = false
    @State private var showsStopConfirmation = false
    @State private var oauthInput = ""

    var body: some View {
        ControlCenterSectionCard(
            localizer.text(.containerRuntimeTitle),
            systemImage: "shippingbox.and.arrow.backward"
        ) {
            VStack(alignment: .leading, spacing: 14) {
                Toggle(
                    localizer.text(.containerRuntimeOptIn),
                    isOn: Binding(
                        get: { controller.optedIn },
                        set: { controller.setOptedIn($0) }
                    )
                )
                ControlCenterSupportingText(
                    localizer.text(.containerRuntimeOptInDetail),
                    systemImage: "clock.badge.checkmark"
                )

                Divider()
                runtimeStatus

                if let code = controller.lastErrorCode {
                    VStack(alignment: .leading, spacing: 4) {
                        Text(localizer.text(.containerRuntimeFailedDetail))
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
                        Label(localizer.text(.menuRefresh), systemImage: "arrow.clockwise")
                    }
                    .disabled(controller.isBusy)

                    Button {
                        controller.checkNow()
                    } label: {
                        Label(localizer.text(.containerRuntimeCheck), systemImage: "network")
                    }
                    .disabled(!controller.canCheck)

                    Button {
                        controller.stageConfirmed()
                    } label: {
                        Label(localizer.text(.containerRuntimeStage), systemImage: "arrow.down.circle")
                    }
                    .disabled(mutationBlocked || !controller.canStage)

                    Button {
                        showsActivationConfirmation = true
                    } label: {
                        Label(localizer.text(.containerRuntimeActivate), systemImage: "play.circle")
                    }
                    .buttonStyle(.glassProminent)
                    .disabled(mutationBlocked || !controller.canActivate)

                    Button {
                        showsStopConfirmation = true
                    } label: {
                        Label(localizer.text(.containerRuntimeStop), systemImage: "stop.circle")
                    }
                    .disabled(mutationBlocked || !controller.canStop)

                    Button {
                        model.recoverContainerRuntime(controller)
                    } label: {
                        Label(localizer.text(.containerRuntimeRecover), systemImage: "arrow.uturn.backward.circle")
                    }
                    .buttonStyle(.glassProminent)
                    .disabled(mutationBlocked || !controller.canRecover)
                }

                if controller.canManageOAuth {
                    Divider()
                    oauthControls
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.vertical, 4)
        }
        .confirmationDialog(
            localizer.text(.containerRuntimeActivateTitle),
            isPresented: $showsActivationConfirmation,
            titleVisibility: .visible
        ) {
            Button(localizer.text(.containerRuntimeActivateConfirm), role: .destructive) {
                model.activateContainerRuntime(controller)
            }
            Button(localizer.text(.runtimeUpgradeCancel), role: .cancel) {}
        } message: {
            Text(localizer.text(.containerRuntimeActivateDetail))
        }
        .confirmationDialog(
            localizer.text(.containerRuntimeStopTitle),
            isPresented: $showsStopConfirmation,
            titleVisibility: .visible
        ) {
            Button(localizer.text(.containerRuntimeStop), role: .destructive) {
                model.stopContainerRuntime(controller)
            }
            Button(localizer.text(.runtimeUpgradeCancel), role: .cancel) {}
        } message: {
            Text(localizer.text(.containerRuntimeStopDetail))
        }
    }

    @ViewBuilder
    private var runtimeStatus: some View {
        HStack(spacing: 8) {
            ControlCenterStatusBadge(
                text: localizer.text(stateKey),
                tone: stateTone
            )
            if controller.isBusy { ProgressView().controlSize(.small) }
        }
        if let capability = controller.inspection?.capability {
            StatusRow(
                localizer.text(.containerRuntimeCapability),
                value: capability.available
                    ? localizer.text(.genericReady)
                    : capability.reason,
                showsDivider: true
            )
            if !capability.appleContainerVersion.isEmpty {
                StatusRow(
                    localizer.text(.containerRuntimeAppleVersion),
                    value: capability.appleContainerVersion
                )
            }
        }
        if let candidate = controller.checkReceipt?.candidate {
            artifactRows(candidate, title: .containerRuntimeStagedVersion)
        }
        if let staged = controller.inspection?.staged {
            artifactRows(staged, title: .containerRuntimeStagedVersion)
        }
        if let active = controller.inspection?.active {
            artifactRows(active, title: .containerRuntimeActiveVersion)
        }
    }

    @ViewBuilder
    private func artifactRows(
        _ artifact: ContainerRuntimeArtifactSummary,
        title: AppStringKey
    ) -> some View {
        StatusRow(localizer.text(title), value: artifact.artifactVersion, showsDivider: true)
        StatusRow(
            localizer.text(.containerRuntimeIndexDigest),
            value: Self.shortDigest(artifact.indexDigest)
        )
        StatusRow(
            localizer.text(.containerRuntimeArm64Digest),
            value: Self.shortDigest(artifact.arm64Digest)
        )
    }

    @ViewBuilder
    private var oauthControls: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(localizer.text(.containerRuntimeOAuthTitle))
                .font(.headline)
            ControlCenterSupportingText(
                localizer.text(.containerRuntimeOAuthDetail),
                systemImage: "person.badge.key"
            )
            AdaptiveActionRow {
                Button(localizer.text(.containerRuntimeOAuthLoad)) {
                    controller.loadOAuthProviders()
                }
                .disabled(controller.isOAuthBusy)
                ForEach(controller.providers) { provider in
                    Button("\(localizer.text(.containerRuntimeOAuthStart)): \(provider.name)") {
                        controller.startOAuth(provider: provider)
                    }
                    .disabled(controller.isOAuthBusy)
                }
            }

            if let receipt = controller.oauthReceipt {
                StatusRow(
                    localizer.text(.containerRuntimeOAuthStatus),
                    value: receipt.status.rawValue
                )
                if let instructions = receipt.instructions, !instructions.isEmpty {
                    Text(instructions).font(.callout).textSelection(.enabled)
                }
                if let userCode = receipt.userCode, !userCode.isEmpty {
                    Text(userCode)
                        .font(.title3.monospaced().weight(.semibold))
                        .textSelection(.enabled)
                        .privacySensitive()
                }
                AdaptiveActionRow {
                    if let rawURL = receipt.authorizationURL,
                       let url = URL(string: rawURL) {
                        Button(localizer.text(.containerRuntimeOAuthOpen)) {
                            NSWorkspace.shared.open(url)
                        }
                    }
                    Button(localizer.text(.containerRuntimeOAuthStatus)) {
                        controller.refreshOAuth()
                    }
                    .disabled(controller.isOAuthBusy)
                    Button(localizer.text(.containerRuntimeOAuthCancel)) {
                        controller.cancelOAuth()
                    }
                    .disabled(controller.isOAuthBusy)
                }
                if receipt.status == .pending || receipt.status == .awaitingUser {
                    HStack(spacing: 8) {
                        SecureField(localizer.text(.containerRuntimeOAuthInput), text: $oauthInput)
                            .textFieldStyle(.roundedBorder)
                            .privacySensitive()
                        Button(localizer.text(.containerRuntimeOAuthSubmit)) {
                            let submitted = oauthInput
                            oauthInput = ""
                            controller.submitOAuth(submitted)
                        }
                        .disabled(oauthInput.isEmpty || controller.isOAuthBusy)
                    }
                }
            }
            if let code = controller.oauthErrorCode {
                Text(code)
                    .font(.caption.monospaced())
                    .foregroundStyle(.orange)
            }
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
        case .unavailable, nil: .containerRuntimeStateUnavailable
        case .stopped: .containerRuntimeStateStopped
        case .staging: .containerRuntimeStateStaging
        case .healthy: .containerRuntimeStateHealthy
        case .updating: .containerRuntimeStateUpdating
        case .recoveryRequired: .containerRuntimeStateRecovery
        }
    }

    private var stateTone: ControlCenterStatusTone {
        switch controller.inspection?.state {
        case .healthy: .success
        case .staging, .updating: .info
        case .stopped, nil: .neutral
        case .recoveryRequired: .warning
        case .unavailable: .error
        }
    }

    private static func shortDigest(_ value: String) -> String {
        guard value.hasPrefix("sha256:"), value.count > 23 else { return value }
        return "sha256:\(value.dropFirst(7).prefix(16))…"
    }
}
