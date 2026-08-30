import SwiftUI
import OpenCodexRelayCore
import OpenCodexRelayLocalization

enum LocalOpenCodexPrimaryAction: Equatable {
    case find
    case openSettings

    static func resolve(_ availability: RelayIntegrationAvailability) -> Self {
        availability == .missing ? .openSettings : .find
    }

    static func showsDiscoveryControls(_ availability: RelayIntegrationAvailability) -> Bool {
        availability == .ready
    }
}

struct OverviewControlCenterPage: View {
    @ObservedObject var model: MenuBarModel
    let localizer: AppLocalizer
    let title: String
    let systemImage: String

    var body: some View {
        ControlCenterPage(title: title, systemImage: systemImage, model: model, localizer: localizer) {
            if model.isLocalDevelopmentBuild {
                LocalDevelopmentWarningView(localizer: localizer)
            }

            ControlCenterSectionCard(localizer.text(.viewConnectionRouting), systemImage: "waveform.path.ecg") {
                StatusRow(
                    localizer.text(.viewRoutingSync),
                    value: model.routingSyncDisplay,
                    systemImage: "arrow.triangle.2.circlepath",
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.viewAppliedBackend),
                    value: model.status.map { localizer.displayName($0.appliedBackend) }
                        ?? localizer.text(.genericUnknown),
                    systemImage: "point.3.connected.trianglepath.dotted",
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.viewRegistration),
                    value: model.desktopTargetState.title(using: localizer),
                    systemImage: "macwindow",
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.viewLastLocalUpdate),
                    value: model.lastStatusUpdatedDisplay,
                    systemImage: "clock"
                )
            }
        }
    }
}

struct ConnectionControlCenterPage: View {
    @ObservedObject var model: MenuBarModel
    let localizer: AppLocalizer
    let title: String
    let systemImage: String
    let openMaintenance: () -> Void
    let openSettings: () -> Void

    var body: some View {
        ControlCenterPage(title: title, systemImage: systemImage, model: model, localizer: localizer) {
            ConnectionRoutingCard(model: model, localizer: localizer)

            ControlCenterSectionCard(
                localizer.text(.controlCenterRouteSelection),
                systemImage: "arrow.triangle.branch"
            ) {
                HStack {
                    Spacer(minLength: 0)
                    Menu {
                        Button {
                            model.requestMode(.external)
                        } label: {
                            Label(localizer.text(.menuUseExternal), systemImage: "network")
                        }
                        .disabled(model.isBusy || !model.desktopTargetState.canControl || !model.canRequestRouting)

                        Button {
                            model.requestMode(.localOpenCodex)
                        } label: {
                            Label(localizer.text(.menuUseLocal), systemImage: "shippingbox")
                        }
                        .disabled(
                            model.isBusy ||
                            !model.desktopTargetState.canControl ||
                            !model.canRequestRouting ||
                            model.status?.canAttemptLocalOpenCodex != true
                        )
                        .accessibilityHint(localizer.text(.menuUseLocalHint))

                        Button {
                            model.requestMode(.native)
                        } label: {
                            Label(localizer.text(.menuUseNative), systemImage: "macwindow")
                        }
                        .disabled(model.isBusy || !model.desktopTargetState.canControl || !model.canRequestRouting)
                    } label: {
                        Label(localizer.text(.controlCenterChangeRoute), systemImage: "arrow.left.arrow.right")
                    }
                    .disabled(model.isBusy || !model.desktopTargetState.canControl || !model.canRequestRouting)
                }
            }

            Button(action: openSettings) {
                Label(
                    localizer.text(.gatewayEditSettings),
                    systemImage: "slider.horizontal.3"
                )
            }

            if model.status?.needsDesktopApply == true {
                ControlCenterSectionCard(
                    localizer.text(.controlCenterPendingChange),
                    systemImage: "clock.badge"
                ) {
                    ControlCenterActionFooter {
                        Button {
                            model.cancelPendingTransition()
                        } label: {
                            Label(localizer.text(.menuCancelPending), systemImage: "xmark.circle")
                        }
                        .disabled(model.isBusy)
                    } primary: {
                        Button {
                            model.completePendingTransition()
                        } label: {
                            Label(localizer.text(.menuApplyPending), systemImage: "checkmark.circle")
                        }
                        .buttonStyle(.glassProminent)
                        .disabled(model.isBusy || !model.desktopTargetState.canControl || !model.canRequestRouting)
                    }
                }
            }

            if model.status?.phase == .recoveryRequired {
                Button(action: openMaintenance) {
                    Label(localizer.text(.controlCenterOpenMaintenance), systemImage: "wrench.and.screwdriver")
                }
            }
        }
    }
}

struct DesktopControlCenterPage: View {
    @ObservedObject var model: MenuBarModel
    @ObservedObject var codexConfiguration: CodexConfigurationController
    let localizer: AppLocalizer
    let title: String
    let systemImage: String
    let requestPreview: () -> Void

    var body: some View {
        ControlCenterPage(title: title, systemImage: systemImage, model: model, localizer: localizer) {
            ControlCenterSectionCard(localizer.text(.viewCodexDesktop), systemImage: "macwindow") {
                VStack(alignment: .leading, spacing: 12) {
                    StatusRow(
                        localizer.text(.viewControlledApp),
                        value: model.selectedDesktopDisplayName,
                        systemImage: "app",
                        showsDivider: true
                    )
                    StatusRow(
                        localizer.text(.viewRegistration),
                        value: model.desktopTargetState.title(using: localizer),
                        systemImage: "checkmark.circle",
                        badgeTone: model.desktopTargetState.canControl ? .success : .warning,
                        showsDivider: true
                    )
                    ControlCenterSupportingText(
                        localizer.text(.viewDesktopBackendUnverifiable),
                        systemImage: "info.circle"
                    )

                    Divider()

                    ControlCenterActionFooter {
                        Menu {
                            Button {
                                model.selectDesktopApplication()
                            } label: {
                                Label(localizer.text(.menuChooseDesktop), systemImage: "macwindow.badge.plus")
                            }
                            .disabled(model.isBusy)
                            .accessibilityHint(localizer.text(.menuChooseDesktopHint))

                            if case let .registered(running) = model.desktopTargetState, !running {
                                Divider()
                                Button {
                                    model.relaunchSelectedDesktop()
                                } label: {
                                    Label(localizer.text(.menuRelaunchDesktop), systemImage: "arrow.clockwise")
                                }
                                .disabled(model.isBusy)
                                .accessibilityHint(localizer.text(.menuRelaunchDesktopHint))
                            }
                        } label: {
                            Label(localizer.text(.controlCenterMoreActions), systemImage: "ellipsis.circle")
                        }
                    } primary: {
                        Button {
                            model.discoverCodexDesktopApplication()
                        } label: {
                            Label(localizer.text(.menuFindDesktop), systemImage: "magnifyingglass")
                        }
                        .buttonStyle(.glassProminent)
                        .disabled(model.isBusy)
                        .accessibilityHint(localizer.text(.menuFindDesktopHint))
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.vertical, 4)
            }

            CodexConfigurationCard(
                model: model,
                controller: codexConfiguration,
                requestPreview: requestPreview
            )
        }
    }
}

struct LocalOpenCodexControlCenterPage: View {
    @ObservedObject var model: MenuBarModel
    let localizer: AppLocalizer
    let title: String
    let systemImage: String
    let openMaintenance: () -> Void
    let openSettings: () -> Void

    var body: some View {
        ControlCenterPage(title: title, systemImage: systemImage, model: model, localizer: localizer) {
            ControlCenterSectionCard(localizer.text(.viewLocalOpenCodex), systemImage: "shippingbox") {
                VStack(alignment: .leading, spacing: 12) {
                    StatusRow(
                        localizer.text(.viewLocalOpenCodex),
                        value: model.localOpenCodexDisplay,
                        systemImage: "server.rack",
                        showsDivider: true
                    )
                    Divider()

                    localOpenCodexPrimaryAction

                    if LocalOpenCodexPrimaryAction.showsDiscoveryControls(
                        model.integrationAvailability
                    ) {
                        OpenCodexDiscoveryControls(
                            model: model,
                            localizer: localizer,
                            onRemovalFlowPresented: openMaintenance
                        )
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.vertical, 4)
            }
        }
    }

    @ViewBuilder
    private var localOpenCodexPrimaryAction: some View {
        switch LocalOpenCodexPrimaryAction.resolve(model.integrationAvailability) {
        case .openSettings:
            ControlCenterSupportingText(
                localizer.text(.controlCenterLocalOpenCodexSetupRequiredDetail),
                systemImage: "gearshape"
            )
            ControlCenterActionFooter {
                EmptyView()
            } primary: {
                Button(action: openSettings) {
                    Label(
                        localizer.text(.controlCenterLocalOpenCodexOpenSettings),
                        systemImage: "gearshape"
                    )
                }
                .buttonStyle(.glassProminent)
                .disabled(model.isBusy)
                .accessibilityHint(
                    localizer.text(.controlCenterLocalOpenCodexOpenSettingsHint)
                )
            }

        case .find:
            ControlCenterActionFooter {
                EmptyView()
            } primary: {
                Button {
                    model.addLocalOpenCodexBackend()
                } label: {
                    Label(localizer.text(.menuAddLocal), systemImage: "plus.circle")
                }
                .buttonStyle(.glassProminent)
                .disabled(model.isBusy || !model.desktopTargetState.canControl || !model.canRequestRouting)
                .accessibilityHint(localizer.text(.menuAddLocalHint))
            }
        }
    }
}
