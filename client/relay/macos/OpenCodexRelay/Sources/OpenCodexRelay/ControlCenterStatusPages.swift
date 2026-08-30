import SwiftUI
import OpenCodexRelayCore
import OpenCodexRelayLocalization

enum LocalOpenCodexPageMode: Equatable {
    case integrated
    case standaloneNative
    case blocked

    static func resolve(
        _ availability: RelayIntegrationAvailability,
        recoveryRequired: Bool = false
    ) -> Self {
        if recoveryRequired {
            return .blocked
        }
        return switch availability {
        case .ready: .integrated
        case .missing: .standaloneNative
        case .preview, .unsafe, .invalid, .helperUnavailable: .blocked
        }
    }

    var showsDiscoveryControls: Bool {
        self == .integrated || self == .standaloneNative
    }

    var showsRelaySetupCard: Bool {
        self == .standaloneNative
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
        let mode = LocalOpenCodexPageMode.resolve(
            model.integrationAvailability,
            recoveryRequired: model.hasPendingOpenCodexRemovalRecovery ||
                model.status?.phase == .recoveryRequired
        )
        ControlCenterPage(
            title: title,
            systemImage: systemImage,
            model: model,
            localizer: localizer,
            handlesMissingIntegrationInline: mode == .standaloneNative
        ) {
            localManagementCard(mode: mode)

            if mode.showsRelaySetupCard {
                relaySetupCard
            }
        }
    }

    private func localManagementCard(
        mode: LocalOpenCodexPageMode
    ) -> some View {
        ControlCenterSectionCard(
            localizer.text(.controlCenterLocalOpenCodexManagement),
            systemImage: "shippingbox"
        ) {
            VStack(alignment: .leading, spacing: 12) {
                if mode == .integrated {
                    StatusRow(
                        localizer.text(.viewLocalOpenCodex),
                        value: model.localOpenCodexDisplay,
                        systemImage: "server.rack",
                        showsDivider: true
                    )
                    Divider()
                }

                if mode == .blocked {
                    ControlCenterSupportingText(
                        localizer.text(.controlCenterLocalOpenCodexBlockedDetail),
                        systemImage: "exclamationmark.triangle"
                    )
                } else {
                    ControlCenterSupportingText(
                        localizer.text(.controlCenterLocalOpenCodexManagementDetail),
                        systemImage: "magnifyingglass"
                    )

                    discoveryAction(mode: mode)
                }

                if mode.showsDiscoveryControls {
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

    @ViewBuilder
    private func discoveryAction(mode: LocalOpenCodexPageMode) -> some View {
        switch mode {
        case .standaloneNative:
            ControlCenterActionFooter {
                EmptyView()
            } primary: {
                Button {
                    model.addLocalOpenCodexBackend()
                } label: {
                    Label(
                        localizer.text(.controlCenterLocalOpenCodexInspectAction),
                        systemImage: "magnifyingglass"
                    )
                }
                .buttonStyle(.glassProminent)
                .disabled(!model.canDiscoverOpenCodex)
                .accessibilityHint(
                    localizer.text(.controlCenterLocalOpenCodexInspectHint)
                )
            }

        case .integrated:
            ControlCenterActionFooter {
                EmptyView()
            } primary: {
                Button {
                    model.addLocalOpenCodexBackend()
                } label: {
                    Label(localizer.text(.menuAddLocal), systemImage: "plus.circle")
                }
                .buttonStyle(.glassProminent)
                .disabled(!model.canDiscoverOpenCodex)
                .accessibilityHint(localizer.text(.menuAddLocalHint))
            }

        case .blocked:
            EmptyView()
        }
    }

    private var relaySetupCard: some View {
        ControlCenterSectionCard(
            localizer.text(.controlCenterLocalOpenCodexRelayTitle),
            systemImage: "network"
        ) {
            VStack(alignment: .leading, spacing: 12) {
                ControlCenterSupportingText(
                    localizer.text(.controlCenterLocalOpenCodexRelayDetail),
                    systemImage: "gearshape"
                )
                ControlCenterActionFooter {
                    EmptyView()
                } primary: {
                    Button(action: openSettings) {
                        Label(
                            localizer.text(.controlCenterLocalOpenCodexRelayAction),
                            systemImage: "gearshape"
                        )
                    }
                    .disabled(model.isBusy)
                    .accessibilityHint(
                        localizer.text(.controlCenterLocalOpenCodexRelayHint)
                    )
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.vertical, 4)
        } accessory: {
            ControlCenterStatusBadge(
                text: localizer.text(.controlCenterLocalOpenCodexRelayBadge),
                tone: .warning
            )
        }
    }
}
