import SwiftUI
import OpenCodexRelayCore
import OpenCodexRelayLocalization

struct SettingsControlCenterPage: View {
    @ObservedObject var model: MenuBarModel
    @ObservedObject var gatewaySettings: GatewaySettingsController
    @ObservedObject var relocation: ApplicationRelocationController
    @ObservedObject var updates: ReleaseUpdateController
    @ObservedObject var runtimeUpgrade: RuntimeUpgradeController
    @ObservedObject var containerRuntime: ContainerRuntimeController
    @Binding var languageSelection: AppLanguageSelection
    let languageDescriptors: [AppLanguageDescriptor]
    let localizer: AppLocalizer
    let title: String
    let systemImage: String

    var body: some View {
        ControlCenterPage(title: title, systemImage: systemImage, model: model, localizer: localizer) {
            if gatewaySettings.state == .appLocationInvalid || relocation.state != .unavailable {
                ApplicationRelocationView(controller: relocation, localizer: localizer)
            }

            ExternalGatewaySettingsCard(
                controller: gatewaySettings,
                localizer: localizer,
                permitsConfiguration: relocation.permitsGatewayConfiguration,
                canSwitchCodex: model.desktopTargetState.canControl &&
                    model.canRequestRouting &&
                    model.status?.appliedBackend != .external,
                onSwitchCodex: { digest, generation in
                    model.switchCodexToExternalGateway(
                        expectedConfigDigest: digest,
                        expectedRoutingGeneration: generation
                    )
                }
            )

            ControlCenterSectionCard(localizer.text(.controlCenterLanguage), systemImage: "globe") {
                Picker(localizer.text(.languageLabel), selection: $languageSelection) {
                    ForEach(languageDescriptors) { descriptor in
                        Text(localizer.languageName(descriptor)).tag(descriptor.selection)
                    }
                }
                .pickerStyle(.menu)
                .labelsHidden()
                .accessibilityLabel(localizer.text(.languageLabel))
                .frame(maxWidth: 260, alignment: .leading)
                .padding(.vertical, 4)
            }

            ControlCenterSectionCard(localizer.text(.updateSettingsTitle), systemImage: "arrow.triangle.2.circlepath") {
                VStack(alignment: .leading, spacing: 12) {
                    Toggle(
                        localizer.text(.updateAutomaticChecks),
                        isOn: Binding(
                            get: { updates.automaticChecksEnabled },
                            set: { updates.setAutomaticChecksEnabled($0) }
                        )
                    )
                    .disabled(model.isLocalDevelopmentBuild)
                    .accessibilityHint(localizer.text(.updateAutomaticChecksHint))

                    Picker(
                        localizer.text(.updateChannel),
                        selection: Binding(
                            get: { updates.channel },
                            set: { updates.setChannel($0) }
                        )
                    ) {
                        Text(localizer.text(.updateChannelStable)).tag(ReleaseUpdateChannel.stable)
                        Text(localizer.text(.updateChannelPreview)).tag(ReleaseUpdateChannel.preview)
                    }
                    .pickerStyle(.menu)
                    .frame(maxWidth: 280, alignment: .leading)

                    ControlCenterSupportingText(
                        model.isLocalDevelopmentBuild
                            ? localizer.text(.updateLocalDevelopmentAutomaticDisabled)
                            : localizer.text(.updateAutomaticChecksDetail),
                        systemImage: model.isLocalDevelopmentBuild ? "network.slash" : "clock"
                    )
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.vertical, 4)
            }

            RuntimeUpgradeCard(
                controller: runtimeUpgrade,
                model: model,
                relocation: relocation,
                localizer: localizer
            )

            ContainerRuntimeCard(
                controller: containerRuntime,
                model: model,
                relocation: relocation,
                localizer: localizer
            )

            ControlCenterSectionCard(localizer.text(.controlCenterLoginItem), systemImage: "power") {
                VStack(alignment: .leading, spacing: 10) {
                    if let loginItemMessage = model.loginItemMessage {
                        ControlCenterSupportingText(
                            localizer.text(loginItemMessage),
                            systemImage: "info.circle"
                        )
                    }
                    if model.isLocalDevelopmentBuild {
                        AdaptiveActionRow {
                            Button {
                                model.enableLaunchAtLogin()
                            } label: {
                                Label(localizer.text(.menuEnableLogin), systemImage: "power")
                            }
                            .disabled(model.isBusy)
                            .accessibilityHint(localizer.text(.menuEnableLoginHint))
                        }
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.vertical, 4)
            }
        }
    }
}

struct AppInformationControlCenterPage: View {
    @ObservedObject var model: MenuBarModel
    @ObservedObject var updates: ReleaseUpdateController
    @ObservedObject var runtimeUpgrade: RuntimeUpgradeController
    @ObservedObject var relocation: ApplicationRelocationController
    let localizer: AppLocalizer
    let title: String
    let systemImage: String

    var body: some View {
        ControlCenterPage(title: title, systemImage: systemImage, model: model, localizer: localizer) {
            AppInformationView(
                model: model,
                updates: updates,
                runtimeUpgrade: runtimeUpgrade,
                relocation: relocation,
                localizer: localizer
            )
        }
    }
}
