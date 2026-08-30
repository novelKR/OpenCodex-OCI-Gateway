import SwiftUI
import OpenCodexRelayCore
import OpenCodexRelayLocalization

enum GatewaySettingsActionMode: Equatable {
    case prepare
    case recover
    case testAndApply

    static func resolve(
        state: GatewaySettingsState,
        integrationState: SelfHostedIntegrationState?
    ) -> Self {
        switch state {
        case .integrationRequired:
            return .prepare
        case .recoveryRequired:
            return .recover
        case .applying:
            switch integrationState {
            case .integrationRequired:
                return .prepare
            case .recoveryRequired:
                return .recover
            case .ready, .none:
                return .testAndApply
            }
        default:
            return .testAndApply
        }
    }
}

struct ExternalGatewaySettingsCard: View {
    @ObservedObject var controller: GatewaySettingsController
    let localizer: AppLocalizer
    let permitsConfiguration: Bool
    let canSwitchCodex: Bool
    let onSwitchCodex: (String, UInt64) -> Void

    @State private var cloudflareClientID = ""
    @State private var cloudflareClientSecret = ""
    @State private var gatewayAPIKey = ""

    var body: some View {
        ControlCenterSectionCard(
            localizer.text(.gatewaySettingsTitle),
            systemImage: "network"
        ) {
            VStack(alignment: .leading, spacing: 14) {
                VStack(alignment: .leading, spacing: 6) {
                    Text(localizer.text(.gatewayAddressLabel))
                        .font(.subheadline.weight(.medium))
                    TextField(
                        localizer.text(.gatewayAddressPlaceholder),
                        text: $controller.draftURL
                    )
                    .textFieldStyle(.roundedBorder)
                    .privacySensitive()
                    .disabled(controller.isBusy || !canEditConnection)
                }

                VStack(alignment: .leading, spacing: 6) {
                    Text(localizer.text(.gatewayAuthenticationLabel))
                        .font(.subheadline.weight(.medium))
                    Picker(
                        localizer.text(.gatewayAuthenticationLabel),
                        selection: $controller.authenticationProfile
                    ) {
                        Text(localizer.text(.gatewayAuthenticationNone))
                            .tag(RemoteAuthenticationProfile.none)
                        Text(localizer.text(.gatewayAuthenticationAPIKey))
                            .tag(RemoteAuthenticationProfile.gatewayAPIKey)
                        Text(localizer.text(.gatewayAuthenticationCloudflare))
                            .tag(RemoteAuthenticationProfile.cloudflareAccessAndGatewayAPIKey)
                    }
                    .pickerStyle(.menu)
                    .labelsHidden()
                    .disabled(controller.isBusy || !canEditConnection)
                    Text(localizer.text(authenticationDetailKey))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                    if controller.hasTransportProfileConflict {
                        ControlCenterSupportingText(
                            localizer.text(.gatewayAuthenticationHTTPSRequired),
                            systemImage: "exclamationmark.triangle.fill"
                        )
                    }
                }

                if controller.requiresInsecureTransportConfirmation {
                    Toggle(
                        localizer.text(.gatewayInsecurePrivateIPConfirmation),
                        isOn: $controller.allowInsecurePrivateIP
                    )
                    .toggleStyle(.checkbox)
                    .foregroundStyle(.orange)
                    .disabled(controller.isBusy)
                }

                HStack(spacing: 8) {
                    ControlCenterStatusBadge(
                        text: localizer.text(statusKey),
                        tone: statusTone
                    )
                    if controller.isBusy {
                        ProgressView().controlSize(.small)
                    }
                }

                ControlCenterSupportingText(
                    localizer.text(statusDetailKey),
                    systemImage: statusTone.systemImage
                )

                ControlCenterActionFooter {
                    if actionMode == .testAndApply {
                        Button(localizer.text(.gatewayConnectionTest)) {
                            controller.test()
                        }
                        .disabled(!controller.canTest)
                    }
                } primary: {
                    switch actionMode {
                    case .prepare:
                        Button(localizer.text(.gatewayIntegrationPrepare)) {
                            controller.prepareIntegration()
                        }
                        .buttonStyle(.glassProminent)
                        .disabled(!controller.canPrepareIntegration)
                    case .recover:
                        Button(localizer.text(.gatewayIntegrationRecover)) {
                            controller.recoverIntegration()
                        }
                        .buttonStyle(.glassProminent)
                        .disabled(!controller.canRecoverIntegration)
                    case .testAndApply:
                        Button(localizer.text(.gatewayApply)) {
                            controller.apply()
                        }
                        .buttonStyle(.glassProminent)
                        .disabled(!controller.canApply)
                    }
                }

                Divider()

                VStack(alignment: .leading, spacing: 8) {
                    Text(localizer.text(.gatewaySwitchCodexDetail))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    HStack {
                        Spacer()
                        Button(localizer.text(.gatewaySwitchCodex)) {
                            if let inspection = controller.inspection {
                                onSwitchCodex(
                                    inspection.configDigest,
                                    inspection.routingGeneration
                                )
                            }
                        }
                        .buttonStyle(.glassProminent)
                        .disabled(
                            !canSwitchCodex ||
                                !controller.canSwitchCodexToVerifiedConfiguration
                        )
                    }
                }

                Divider()

                if controller.canEditCredentials {
                    ForEach(controller.requiredCredentialKinds, id: \.rawValue) { kind in
                        credentialEditor(kind: kind, value: credentialBinding(kind))
                        if kind != controller.requiredCredentialKinds.last {
                            Divider()
                        }
                    }
                    ControlCenterSupportingText(
                        localizer.text(.gatewayCredentialsPrivacy),
                        systemImage: "key.fill"
                    )
                } else {
                    ControlCenterSupportingText(
                        localizer.text(.gatewayCredentialsManagedExternally),
                        systemImage: "lock.fill"
                    )
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.vertical, 6)
        } accessory: {
            ControlCenterStatusBadge(
                text: localizer.text(statusKey),
                tone: statusTone
            )
        }
        .onAppear { controller.load() }
        .onChange(of: controller.draftURL) { _, _ in
            controller.addressDidChange()
        }
        .onChange(of: controller.authenticationProfile) { _, _ in
            controller.authenticationProfileDidChange()
        }
        .onChange(of: controller.allowInsecurePrivateIP) { _, _ in
            controller.draftDidChange()
        }
        .disabled(!permitsConfiguration)
    }

    @ViewBuilder
    private func credentialEditor(
        kind: GatewayCredentialKind,
        value: Binding<String>
    ) -> some View {
        let status = credentialStatus(kind)
        let configured = controller.credentialMetadata[kind]?.configured == true
        VStack(alignment: .leading, spacing: 7) {
            HStack {
                Text(localizer.text(credentialLabelKey(kind)))
                    .font(.subheadline.weight(.medium))
                Spacer(minLength: 8)
                ControlCenterStatusBadge(
                    text: localizer.text(status.key),
                    tone: status.tone
                )
            }
            HStack(spacing: 8) {
                SecureField(
                    localizer.text(.gatewayCredentialNewValue),
                    text: value
                )
                .textFieldStyle(.roundedBorder)
                .privacySensitive()
                Button(
                    localizer.text(
                        configured
                            ? .gatewayCredentialReplace
                            : .gatewayCredentialAdd
                    )
                ) {
                    replaceCredential(kind, value: value)
                }
                .disabled(
                    controller.isBusy ||
                        controller.credentialMetadataState != .ready ||
                        value.wrappedValue.isEmpty
                )
            }
        }
    }

    private func replaceCredential(
        _ kind: GatewayCredentialKind,
        value: Binding<String>
    ) {
        let candidate = value.wrappedValue
        Task {
            _ = await controller.replaceCredential(kind, value: candidate)
            value.wrappedValue = ""
        }
    }

    private func credentialLabelKey(_ kind: GatewayCredentialKind) -> AppStringKey {
        switch kind {
        case .cloudflareClientID: .gatewayCloudflareClientID
        case .cloudflareClientSecret: .gatewayCloudflareClientSecret
        case .gatewayAPIKey: .gatewayAPIKey
        }
    }

    private func credentialStatus(
        _ kind: GatewayCredentialKind
    ) -> (key: AppStringKey, tone: ControlCenterStatusTone) {
        switch controller.credentialMetadataState {
        case .idle, .loading:
            (.gatewayCredentialChecking, .info)
        case .failed:
            (.gatewayCredentialUnavailable, .error)
        case .ready:
            if controller.credentialMetadata[kind]?.configured == true {
                (.gatewayCredentialConfigured, .success)
            } else {
                (.gatewayCredentialMissing, .warning)
            }
        }
    }

    private func credentialBinding(_ kind: GatewayCredentialKind) -> Binding<String> {
        switch kind {
        case .cloudflareClientID:
            $cloudflareClientID
        case .cloudflareClientSecret:
            $cloudflareClientSecret
        case .gatewayAPIKey:
            $gatewayAPIKey
        }
    }

    private var authenticationDetailKey: AppStringKey {
        switch controller.authenticationProfile {
        case .none:
            .gatewayAuthenticationNoneDetail
        case .gatewayAPIKey:
            .gatewayAuthenticationAPIKeyDetail
        case .cloudflareAccessAndGatewayAPIKey:
            .gatewayAuthenticationCloudflareDetail
        }
    }

    private var actionMode: GatewaySettingsActionMode {
        GatewaySettingsActionMode.resolve(
            state: controller.state,
            integrationState: controller.integrationInspection?.state
        )
    }

    private var statusKey: AppStringKey {
        switch controller.state {
        case .loading: .gatewayStatusLoading
        case .needsValidation: .gatewayStatusNeedsValidation
        case .testing: .gatewayStatusTesting
        case .applying: .gatewayStatusApplying
        case .connected: .gatewayStatusConnected
        case .authenticationMismatch: .gatewayStatusAuthenticationMismatch
        case .unreachable: .gatewayStatusUnreachable
        case .catalogInvalid: .gatewayStatusCatalogInvalid
        case .integrationRequired: .gatewayStatusIntegrationRequired
        case .recoveryRequired: .gatewayStatusRecoveryRequired
        case .appLocationInvalid: .gatewayStatusAppLocationInvalid
        case .integrationArtifactInvalid: .gatewayStatusArtifactInvalid
        case .bindingUnsafe: .gatewayStatusBindingUnsafe
        case .bindingInvalid: .gatewayStatusBindingInvalid
        case .helperUnavailable: .gatewayStatusHelperUnavailable
        case .unsupported: .gatewayStatusUnsupported
        case .failed: .gatewayStatusFailed
        }
    }

    private var statusDetailKey: AppStringKey {
        switch controller.state {
        case .loading, .testing, .applying:
            .gatewayDetailWorking
        case .needsValidation:
            .gatewayDetailNeedsValidation
        case .connected:
            .gatewayDetailConnected
        case .authenticationMismatch:
            .gatewayDetailAuthenticationMismatch
        case .unreachable:
            .gatewayDetailUnreachable
        case .catalogInvalid:
            .gatewayDetailCatalogInvalid
        case .integrationRequired:
            .gatewayDetailIntegrationRequired
        case .recoveryRequired:
            .gatewayDetailRecoveryRequired
        case .appLocationInvalid:
            .gatewayDetailAppLocationInvalid
        case .integrationArtifactInvalid:
            .gatewayDetailArtifactInvalid
        case .bindingUnsafe:
            .gatewayDetailBindingUnsafe
        case .bindingInvalid:
            .gatewayDetailBindingInvalid
        case .helperUnavailable:
            .gatewayDetailHelperUnavailable
        case .unsupported:
            .gatewayDetailUnsupported
        case .failed:
            .gatewayDetailFailed
        }
    }

    private var statusTone: ControlCenterStatusTone {
        switch controller.state {
        case .loading, .testing, .applying: .info
        case .connected: .success
        case .needsValidation: .warning
        case .unsupported: .neutral
        case .integrationRequired: .warning
        case .recoveryRequired: .error
        case .appLocationInvalid, .integrationArtifactInvalid: .error
        case .authenticationMismatch, .unreachable, .catalogInvalid,
             .bindingUnsafe, .bindingInvalid, .helperUnavailable, .failed:
            .error
        }
    }

    private var canEditConnection: Bool {
        permitsConfiguration && (
            controller.inspection != nil ||
                controller.integrationInspection?.state == .integrationRequired
        )
    }
}
