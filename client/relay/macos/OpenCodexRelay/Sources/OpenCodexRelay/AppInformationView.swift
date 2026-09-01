import AppKit
import SwiftUI
import OpenCodexRelayCore
import OpenCodexRelayLocalization

struct AppInformationView: View {
    @ObservedObject var model: MenuBarModel
    @ObservedObject var updates: ReleaseUpdateController
    let localizer: AppLocalizer
    private let reader: AppInformationReader
    @State private var information: AppInformationSnapshot
    @State private var showsUpdateQuitConfirmation = false

    init(
        model: MenuBarModel,
        updates: ReleaseUpdateController,
        localizer: AppLocalizer,
        reader: AppInformationReader = AppInformationReader()
    ) {
        self.model = model
        self.updates = updates
        self.localizer = localizer
        self.reader = reader
        _information = State(initialValue: reader.loadingSnapshot)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: ControlCenterPresentationMetrics.pageSpacing) {
            ControlCenterSectionCard(
                localizer.text(.appInformationApplication),
                systemImage: "app"
            ) {
                VStack(alignment: .leading, spacing: 14) {
                    HStack(spacing: 14) {
                        Image(nsImage: NSApplication.shared.applicationIconImage)
                            .resizable()
                            .scaledToFit()
                            .frame(width: 56, height: 56)
                            .accessibilityHidden(true)

                        VStack(alignment: .leading, spacing: 3) {
                            Text(information.displayName)
                                .font(.title3.weight(.semibold))
                                .foregroundStyle(.primary)
                            Text(versionSummary)
                                .font(.body)
                                .foregroundStyle(.secondary)
                        }
                    }

                    Divider()

                    StatusRow(
                        localizer.text(.appInformationVersion),
                        value: information.version ?? localizer.text(.genericUnknown),
                        showsDivider: true
                    )
                    StatusRow(
                        localizer.text(.appInformationBuild),
                        value: information.build ?? localizer.text(.genericUnknown),
                        showsDivider: true
                    )
                    StatusRow(
                        localizer.text(.appInformationBundleIdentifier),
                        value: information.bundleIdentifier ?? localizer.text(.genericUnknown),
                        showsDivider: true
                    )
                    StatusRow(
                        localizer.text(.appInformationDistribution),
                        value: distributionTitle,
                        showsDivider: true
                    )
                    StatusRow(
                        localizer.text(.appInformationRuntimeMode),
                        value: model.isPreviewRuntime
                            ? localizer.text(.appInformationRuntimePreview)
                            : localizer.text(.appInformationRuntimeManaged),
                        showsDivider: true
                    )
                    StatusRow(
                        localizer.text(.appInformationMinimumSystem),
                        value: information.minimumSystemVersion ?? localizer.text(.genericUnknown),
                        showsDivider: true
                    )
                    StatusRow(
                        localizer.text(.appInformationArchitecture),
                        value: information.architecture
                    )
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.vertical, 4)
            }

            ControlCenterSectionCard(
                localizer.text(.appInformationComponents),
                systemImage: "shippingbox"
            ) {
                VStack(alignment: .leading, spacing: 14) {
                    ForEach(Array(information.components.enumerated()), id: \.element.id) { index, component in
                        if index > 0 {
                            Divider()
                        }
                        componentView(component)
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.vertical, 4)
            }

            updateCard

            ControlCenterSupportingText(
                localizer.text(.appInformationAdHocDistributionNotice),
                systemImage: "checkmark.shield"
            )

            HomebrewGuardStatusCard(
                model: model,
                localizer: localizer
            )

            ControlCenterSupportingText(
                localizer.text(.appInformationPrivacy),
                systemImage: "lock.shield"
            )
        }
        .task {
            information = await reader.load()
        }
    }

    @ViewBuilder
    private var updateCard: some View {
        ControlCenterSectionCard(
            localizer.text(.updateTitle),
            systemImage: "arrow.triangle.2.circlepath"
        ) {
            VStack(alignment: .leading, spacing: 12) {
                HStack(alignment: .firstTextBaseline, spacing: 10) {
                    Text(updateStatusTitle)
                        .font(.headline)
                    if updates.isUpdateBadgeVisible, let version = updates.candidateVersion {
                        ControlCenterStatusBadge(
                            text: localizer.text(.updateAvailableBadge, version),
                            tone: .info
                        )
                    }
                    Spacer(minLength: 12)
                    if updates.isChecking {
                        ProgressView().controlSize(.small)
                    }
                }

                StatusRow(
                    localizer.text(.updateChannel),
                    value: updates.channel == .stable
                        ? localizer.text(.updateChannelStable)
                        : localizer.text(.updateChannelPreview),
                    showsDivider: true
                )
                StatusRow(
                    localizer.text(.updateLastChecked),
                    value: updates.lastCheckedAt?.formatted(date: .abbreviated, time: .shortened)
                        ?? localizer.text(.genericNever),
                    showsDivider: updates.candidateVersion != nil
                )
                if let candidateVersion = updates.candidateVersion {
                    StatusRow(
                        localizer.text(.updateSelectedVersion),
                        value: candidateVersion
                    )
                }

                AdaptiveActionRow {
                    Button {
                        updates.checkNow()
                    } label: {
                        Label(localizer.text(.updateCheckNow), systemImage: "arrow.clockwise")
                    }
                    .disabled(updates.isChecking)
                    .accessibilityHint(localizer.text(.updateCheckNowHint))

                    if let releaseURL = updates.releaseURL {
                        Button {
                            NSWorkspace.shared.open(releaseURL)
                        } label: {
                            Label(localizer.text(.updateOpenRelease), systemImage: "safari")
                        }
                        .accessibilityHint(localizer.text(.updateOpenReleaseHint))
                    }

                    if updates.isUpdateBadgeVisible {
                        Button(localizer.text(.updateDismissBadge)) {
                            updates.dismissBadge()
                        }
                    }

                    if updates.canDownloadUpdate {
                        Button {
                            updates.downloadUpdate()
                        } label: {
                            Label(localizer.text(.updateDownload), systemImage: "arrow.down.circle")
                        }
                        .accessibilityHint(localizer.text(.updateDownloadHint))
                    }

                    if updates.stageState == .ready {
                        Button {
                            updates.prepareFinderHandoff()
                        } label: {
                            Label(localizer.text(.updateRevealInFinder), systemImage: "folder")
                        }
                        .accessibilityHint(localizer.text(.updateRevealInFinderHint))
                    }

                    if updates.stageState == .awaitingQuit {
                        Button(role: .destructive) {
                            showsUpdateQuitConfirmation = true
                        } label: {
                            Label(localizer.text(.updateQuitForInstall), systemImage: "power")
                        }
                    }
                }

                if updates.stageState == .staging || updates.stageState == .preparingFinderHandoff {
                    HStack(spacing: 8) {
                        ProgressView().controlSize(.small)
                        Text(
                            updates.stageState == .staging
                                ? localizer.text(.updateStageDownloading)
                                : localizer.text(.updateStageRevalidating)
                        )
                    }
                    .foregroundStyle(.secondary)
                } else if updates.stageState == .ready {
                    ControlCenterSupportingText(
                        localizer.text(.updateStageReady),
                        systemImage: "checkmark.shield"
                    )
                } else if updates.stageState == .awaitingQuit {
                    ControlCenterSupportingText(
                        localizer.text(.updateFinderHandoffReady),
                        systemImage: "folder.badge.gearshape"
                    )
                } else if updates.stageState == .failed {
                    ControlCenterSupportingText(
                        localizer.text(.updateStageFailed),
                        systemImage: "exclamationmark.triangle"
                    )
                    .foregroundStyle(.orange)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.vertical, 4)
        }
        .confirmationDialog(
            localizer.text(.updateQuitConfirmationTitle),
            isPresented: $showsUpdateQuitConfirmation,
            titleVisibility: .visible
        ) {
            Button(localizer.text(.updateQuitConfirmationAction), role: .destructive) {
                updates.confirmQuitForFinderInstall()
            }
            Button(localizer.text(.updateQuitConfirmationCancel), role: .cancel) {}
        } message: {
            Text(localizer.text(.updateQuitConfirmationDetail))
        }
    }

    private var updateStatusTitle: String {
        if updates.lastCheckFailed { return localizer.text(.updateStatusFailed) }
        guard let status = updates.status else { return localizer.text(.updateStatusNotChecked) }
        switch status {
        case .current: return localizer.text(.updateStatusCurrent)
        case .newerThanSelectedChannel: return localizer.text(.updateStatusNewerThanChannel)
        case .updateAvailable: return localizer.text(.updateStatusAvailable)
        case .offline: return localizer.text(.updateStatusOffline)
        case .rateLimited: return localizer.text(.updateStatusRateLimited)
        case .invalidRelease: return localizer.text(.updateStatusInvalidRelease)
        case .updaterTooOld: return localizer.text(.updateStatusUpdaterTooOld)
        case .unsupportedSystem: return localizer.text(.updateStatusUnsupportedSystem)
        }
    }

    private var versionSummary: String {
        localizer.text(
            .appInformationVersionSummary,
            information.version ?? localizer.text(.genericUnknown),
            information.build ?? localizer.text(.genericUnknown)
        )
    }

    private var distributionTitle: String {
        switch information.distributionFlavor {
        case .production:
            localizer.text(.appInformationDistributionProduction)
        case .localDevelopment:
            localizer.text(.appInformationDistributionLocalDevelopment)
        }
    }

    @ViewBuilder
    private func componentView(_ component: BundledRelayComponentInformation) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(alignment: .firstTextBaseline, spacing: 10) {
                Label(component.kind.executableName, systemImage: componentSymbol(component.kind))
                    .font(.headline)
                    .textSelection(.enabled)
                Spacer(minLength: 12)
                availabilityView(component.availability)
            }

            Text(componentRole(component.kind))
                .font(.body)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)

            StatusRow(
                localizer.text(.appInformationComponentVersion),
                value: component.version ?? localizer.text(.genericUnknown),
                showsDivider: true
            )
            StatusRow(
                localizer.text(.appInformationArchitecture),
                value: component.architecture,
                showsDivider: true
            )
            StatusRow(
                localizer.text(.appInformationVersionMatch),
                value: versionMatchTitle(component)
            )

            if versionMatch(component) == .different {
                ControlCenterSupportingText(
                    localizer.text(.appInformationVersionMismatch),
                    systemImage: "exclamationmark.triangle"
                )
                .foregroundStyle(.orange)
            }
        }
    }

    @ViewBuilder
    private func availabilityView(
        _ availability: BundledRelayComponentAvailability
    ) -> some View {
        switch availability {
        case .loading:
            HStack(spacing: 6) {
                ProgressView()
                    .controlSize(.small)
                Text(localizer.text(.appInformationStatusLoading))
            }
            .foregroundStyle(.secondary)
        case .available:
            ControlCenterStatusBadge(text: localizer.text(.appInformationStatusAvailable), tone: .success)
        case .missing:
            ControlCenterStatusBadge(text: localizer.text(.appInformationStatusMissing), tone: .warning)
        case .unverified:
            ControlCenterStatusBadge(text: localizer.text(.appInformationStatusUnverified), tone: .warning)
        }
    }

    private func componentRole(_ kind: BundledRelayComponentKind) -> String {
        switch kind {
        case .relay:
            localizer.text(.appInformationRelayRole)
        case .relayctl:
            localizer.text(.appInformationRelayctlRole)
        }
    }

    private func componentSymbol(_ kind: BundledRelayComponentKind) -> String {
        switch kind {
        case .relay: "network"
        case .relayctl: "terminal"
        }
    }

    private enum VersionMatch {
        case matched
        case different
        case unknown
    }

    private func versionMatch(
        _ component: BundledRelayComponentInformation
    ) -> VersionMatch {
        guard component.availability == .available,
              let appVersion = information.version,
              let componentVersion = component.version else {
            return .unknown
        }
        return appVersion == componentVersion ? .matched : .different
    }

    private func versionMatchTitle(
        _ component: BundledRelayComponentInformation
    ) -> String {
        switch versionMatch(component) {
        case .matched:
            localizer.text(.appInformationVersionMatchMatched)
        case .different:
            localizer.text(.appInformationVersionMatchDifferent)
        case .unknown:
            localizer.text(.genericUnknown)
        }
    }
}
