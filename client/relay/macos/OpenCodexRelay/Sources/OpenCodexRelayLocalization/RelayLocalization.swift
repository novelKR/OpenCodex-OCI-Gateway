import Combine
import Foundation
import OpenCodexRelayCore

/// The persisted UI-language choice is deliberately separate from relay
/// routing. A raw value rather than a closed enum lets a registry add a new
/// supported language without changing persisted-selection decoding.
public struct AppLanguageSelection: RawRepresentable, Hashable, Codable, Sendable, Identifiable {
    public let rawValue: String

    public init(rawValue: String) {
        self.rawValue = rawValue
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.singleValueContainer()
        self.rawValue = try container.decode(String.self)
    }

    public func encode(to encoder: Encoder) throws {
        var container = encoder.singleValueContainer()
        try container.encode(rawValue)
    }

    public var id: String { rawValue }

    public static let system = AppLanguageSelection(rawValue: "system")
    public static let english = AppLanguageSelection(rawValue: "english")
    public static let korean = AppLanguageSelection(rawValue: "korean")
}

/// A catalog locale is open-ended for the same reason as a selection ID: a
/// descriptor plus its `.lproj` catalog is the extension point for a language.
public struct ResolvedAppLanguage: RawRepresentable, Hashable, Sendable {
    public let rawValue: String

    public init(rawValue: String) {
        self.rawValue = rawValue
    }

    public static let english = ResolvedAppLanguage(rawValue: "en")
    public static let korean = ResolvedAppLanguage(rawValue: "ko")

    fileprivate var locale: Locale { Locale(identifier: rawValue) }
}

/// The registry owns every language-specific policy. Adding a descriptor and
/// its catalog does not require updates to a selection enum, resolver switch,
/// static catalog table, or picker switch.
public struct AppLanguageDescriptor: Identifiable, Hashable, Sendable {
    public let selection: AppLanguageSelection
    public let catalogLanguage: ResolvedAppLanguage?
    public let systemLanguageCodes: Set<String>
    public let pickerFallbackName: String

    public var id: AppLanguageSelection { selection }
    public var isSystemDefault: Bool { catalogLanguage == nil }

    public init(
        selection: AppLanguageSelection,
        catalogLanguage: ResolvedAppLanguage?,
        systemLanguageCodes: Set<String> = [],
        pickerFallbackName: String
    ) {
        self.selection = selection
        self.catalogLanguage = catalogLanguage
        self.systemLanguageCodes = Set(systemLanguageCodes.map { $0.lowercased() })
        self.pickerFallbackName = pickerFallbackName
    }
}

public struct AppLanguageRegistry: Sendable {
    public let descriptors: [AppLanguageDescriptor]

    public init(descriptors: [AppLanguageDescriptor]) {
        precondition(!descriptors.isEmpty, "at least one language descriptor is required")
        precondition(Set(descriptors.map(\.selection)).count == descriptors.count, "language selection IDs must be unique")
        precondition(descriptors.contains(where: { $0.selection == .system && $0.isSystemDefault }), "system descriptor is required")
        precondition(descriptors.contains(where: { $0.selection == .korean && $0.catalogLanguage == .korean }), "Korean fallback descriptor is required")
        precondition(descriptors.contains(where: { $0.selection == .english && $0.catalogLanguage == .english }), "English descriptor is required")
        self.descriptors = descriptors
    }

    public static let standard = AppLanguageRegistry(descriptors: [
        AppLanguageDescriptor(
            selection: .system,
            catalogLanguage: nil,
            pickerFallbackName: "System Default"
        ),
        AppLanguageDescriptor(
            selection: .korean,
            catalogLanguage: .korean,
            systemLanguageCodes: ["ko"],
            pickerFallbackName: "한국어"
        ),
        AppLanguageDescriptor(
            selection: .english,
            catalogLanguage: .english,
            systemLanguageCodes: ["en"],
            pickerFallbackName: "English"
        ),
    ])

    public var systemDescriptor: AppLanguageDescriptor {
        descriptor(for: .system)!
    }

    public var koreanDescriptor: AppLanguageDescriptor {
        descriptor(for: .korean)!
    }

    public var englishDescriptor: AppLanguageDescriptor {
        descriptor(for: .english)!
    }

    public func descriptor(for selection: AppLanguageSelection) -> AppLanguageDescriptor? {
        descriptors.first { $0.selection == selection }
    }

    public func normalized(_ selection: AppLanguageSelection) -> AppLanguageSelection {
        descriptor(for: selection) == nil ? .system : selection
    }

    /// System language preference is ordered. Only the first preference is
    /// authoritative; a later Korean fallback must not override English.
    public func resolvedDescriptor(
        for selection: AppLanguageSelection,
        preferredLanguageIdentifiers: [String]
    ) -> AppLanguageDescriptor {
        guard normalized(selection) == .system else {
            return descriptor(for: normalized(selection)) ?? koreanDescriptor
        }
        guard let firstIdentifier = preferredLanguageIdentifiers.first,
              let code = Locale(identifier: firstIdentifier).language.languageCode?.identifier.lowercased() else {
            return koreanDescriptor
        }
        return descriptors.first {
            !$0.isSystemDefault && $0.systemLanguageCodes.contains(code)
        } ?? koreanDescriptor
    }
}

/// Stable, typed keys make adding a language a resource-only change: add the
/// new .lproj catalog and a supported-language descriptor without changing
/// relayctl, routing state, or the user-visible call sites.
public enum AppStringKey: String, CaseIterable, Sendable {
    case languageLabel = "language.label"
    case languageSystem = "language.system"
    case languageEnglish = "language.english"
    case languageKorean = "language.korean"

    case genericUnknown = "generic.unknown"
    case genericUnavailable = "generic.unavailable"
    case genericNotApplicable = "generic.not_applicable"
    case genericRunning = "generic.running"
    case genericPaused = "generic.paused"
    case genericPending = "generic.pending"
    case genericInvalid = "generic.invalid"
    case genericHealthy = "generic.healthy"
    case genericDegraded = "generic.degraded"
    case genericAcknowledged = "generic.acknowledged"
    case genericReady = "generic.ready"
    case genericForeignListener = "generic.foreign_listener"
    case genericInvalidCatalog = "generic.invalid_catalog"
    case genericAllow = "generic.allow"
    case genericDeny = "generic.deny"
    case genericNever = "generic.never"
    case genericDraining = "generic.draining"
    case genericWaitingForRequests = "generic.waiting_for_requests"
    case genericNotDraining = "generic.not_draining"

    case backendUnknown = "backend.unknown"
    case backendExternal = "backend.external"
    case backendLocal = "backend.local"
    case backendNative = "backend.native"

    case phaseRelayActive = "phase.relay_active"
    case phaseNativePending = "phase.native_pending"
    case phaseRelayPending = "phase.relay_pending"
    case phaseBackendPending = "phase.backend_pending"
    case phaseApplying = "phase.applying"
    case phaseNativeActive = "phase.native_active"
    case phaseRecovery = "phase.recovery_required"

    case presentationExternalReadyTitle = "presentation.external_ready.title"
    case presentationLocalReadyTitle = "presentation.local_ready.title"
    case presentationNativePendingTitle = "presentation.native_pending.title"
    case presentationExternalPendingTitle = "presentation.external_pending.title"
    case presentationLocalPendingTitle = "presentation.local_pending.title"
    case presentationSyncPendingTitle = "presentation.sync_pending.title"
    case presentationDegradedTitle = "presentation.degraded.title"
    case presentationSwitchingTitle = "presentation.switching.title"
    case presentationNativeParkedTitle = "presentation.native_parked.title"
    case presentationRecoveryTitle = "presentation.recovery.title"
    case presentationLocalUnavailableTitle = "presentation.local_unavailable.title"
    case presentationRelayUnavailableTitle = "presentation.relay_unavailable.title"

    case presentationExternalReadyCompact = "presentation.external_ready.compact"
    case presentationLocalReadyCompact = "presentation.local_ready.compact"
    case presentationNativePendingCompact = "presentation.native_pending.compact"
    case presentationExternalPendingCompact = "presentation.external_pending.compact"
    case presentationLocalPendingCompact = "presentation.local_pending.compact"
    case presentationSyncPendingCompact = "presentation.sync_pending.compact"
    case presentationDegradedCompact = "presentation.degraded.compact"
    case presentationSwitchingCompact = "presentation.switching.compact"
    case presentationNativeParkedCompact = "presentation.native_parked.compact"
    case presentationRecoveryCompact = "presentation.recovery.compact"
    case presentationLocalUnavailableCompact = "presentation.local_unavailable.compact"
    case presentationRelayUnavailableCompact = "presentation.relay_unavailable.compact"

    case presentationExternalReadyAccessibility = "presentation.external_ready.accessibility"
    case presentationLocalReadyAccessibility = "presentation.local_ready.accessibility"
    case presentationNativePendingAccessibility = "presentation.native_pending.accessibility"
    case presentationExternalPendingAccessibility = "presentation.external_pending.accessibility"
    case presentationLocalPendingAccessibility = "presentation.local_pending.accessibility"
    case presentationSyncPendingAccessibility = "presentation.sync_pending.accessibility"
    case presentationDegradedAccessibility = "presentation.degraded.accessibility"
    case presentationSwitchingAccessibility = "presentation.switching.accessibility"
    case presentationNativeParkedAccessibility = "presentation.native_parked.accessibility"
    case presentationRecoveryAccessibility = "presentation.recovery.accessibility"
    case presentationLocalUnavailableAccessibility = "presentation.local_unavailable.accessibility"
    case presentationRelayUnavailableAccessibility = "presentation.relay_unavailable.accessibility"

    case bindingMissing = "binding.missing"
    case bindingUnsafe = "binding.unsafe"
    case bindingInvalid = "binding.invalid"
    case integrationPreview = "integration.preview"
    case integrationHelperUnavailable = "integration.helper_unavailable"
    case relayctlUnavailable = "relayctl.unavailable"
    case relayctlFailed = "relayctl.failed"
    case relayctlInvalidStatus = "relayctl.invalid_status"
    case relayctlLaunchFailed = "relayctl.launch_failed"
    case relayctlTimedOut = "relayctl.timed_out"
    case relayctlCancelled = "relayctl.cancelled"
    case relayctlOutputTooLarge = "relayctl.output_too_large"
    case ocxInvalid = "ocx.invalid"
    case ocxUnavailable = "ocx.unavailable"
    case ocxTooLarge = "ocx.too_large"
    case ocxChanged = "ocx.changed"

    case handoffRemoveShim = "handoff.remove_shim"
    case handoffKeepShim = "handoff.keep_shim"
    case handoffAlertTitle = "handoff.alert.title"
    case handoffAlertDetail = "handoff.alert.detail"
    case handoffCancel = "handoff.cancel"

    case menuConnectionDetails = "menu.connection_details"
    case menuConnectionDetailsHint = "menu.connection_details.hint"
    case menuOpenControlCenter = "menu.open_control_center"
    case menuFindDesktop = "menu.find_desktop"
    case menuFindDesktopHint = "menu.find_desktop.hint"
    case menuChooseDesktop = "menu.choose_desktop"
    case menuChooseDesktopHint = "menu.choose_desktop.hint"
    case menuRelaunchDesktop = "menu.relaunch_desktop"
    case menuRelaunchDesktopHint = "menu.relaunch_desktop.hint"
    case menuUseExternal = "menu.use_external"
    case menuUseLocal = "menu.use_local"
    case menuUseLocalHint = "menu.use_local.hint"
    case menuUseNative = "menu.use_native"
    case menuAddLocal = "menu.add_local"
    case menuAddLocalHint = "menu.add_local.hint"
    case menuDiscoverySearching = "menu.discovery.searching"
    case menuDiscoveryNativeSearching = "menu.discovery.native_searching"
    case menuDiscoveryCandidates = "menu.discovery.candidates"
    case menuDiscoveryCandidate = "menu.discovery.candidate"
    case menuDiscoveryNativeCandidate = "menu.discovery.native_candidate"
    case menuDiscoveryManualRemovalBadge = "menu.discovery.manual_removal.badge"
    case menuDiscoveryCopyDiagnostics = "menu.discovery.copy_diagnostics"
    case menuDiscoveryCopyDiagnosticsAccessibility = "menu.discovery.copy_diagnostics.accessibility"
    case menuDiscoveryCopyDiagnosticsHint = "menu.discovery.copy_diagnostics.hint"
    case menuDiscoveryReasonEligible = "menu.discovery.reason.eligible"
    case menuDiscoveryReasonUnreviewedPackageClosure = "menu.discovery.reason.unreviewed_package_closure"
    case menuDiscoveryReasonUnsupportedPackageVersion = "menu.discovery.reason.unsupported_package_version"
    case menuDiscoveryReasonPackageModuleChanged = "menu.discovery.reason.package_module_changed"
    case menuDiscoveryReasonExecutionEvidenceIncomplete = "menu.discovery.reason.execution_evidence_incomplete"
    case menuDiscoveryReasonManualPackageManager = "menu.discovery.reason.manual_package_manager"
    case menuDiscoveryReasonIdentityUnverified = "menu.discovery.reason.identity_unverified"
    case menuDiscoveryReasonVerificationUnavailable = "menu.discovery.reason.verification_unavailable"
    case menuDiscoveryBroadDetail = "menu.discovery.broad_detail"
    case menuDiscoveryBroadAction = "menu.discovery.broad_action"
    case menuDiscoveryManual = "menu.discovery.manual"
    case menuDiscoveryNoCandidates = "menu.discovery.no_candidates"
    case menuDiscoveryCancel = "menu.discovery.cancel"
    case menuDiscoveryTruncated = "menu.discovery.truncated"
    case menuApplyPending = "menu.apply_pending"
    case menuCancelPending = "menu.cancel_pending"
    case menuCompleteRecovery = "menu.complete_recovery"
    case menuRollbackRecovery = "menu.rollback_recovery"
    case menuEnableLogin = "menu.enable_login"
    case menuEnableLoginHint = "menu.enable_login.hint"
    case menuRefresh = "menu.refresh"
    case menuOpenLoginItemsSettings = "menu.open_login_items_settings"
    case menuQuit = "menu.quit"
    case menuLocalDevelopmentPrefix = "menu.local_development_prefix"
    case menuLocalDevelopmentAccessibility = "menu.local_development_accessibility"

    case viewConnectionDetails = "view.connection_details"
    case viewControlCenter = "view.control_center"
    case controlCenterOverview = "control_center.overview"
    case controlCenterConnection = "control_center.connection"
    case controlCenterDesktop = "control_center.desktop"
    case controlCenterLocalOpenCodex = "control_center.local_opencodex"
    case controlCenterLocalOpenCodexManagement = "control_center.local_opencodex.management"
    case controlCenterLocalOpenCodexManagementDetail = "control_center.local_opencodex.management.detail"
    case controlCenterLocalOpenCodexInspectAction = "control_center.local_opencodex.inspect.action"
    case controlCenterLocalOpenCodexInspectHint = "control_center.local_opencodex.inspect.hint"
    case controlCenterLocalOpenCodexBlockedDetail = "control_center.local_opencodex.blocked.detail"
    case controlCenterLocalOpenCodexRelayTitle = "control_center.local_opencodex.relay.title"
    case controlCenterLocalOpenCodexRelayBadge = "control_center.local_opencodex.relay.badge"
    case controlCenterLocalOpenCodexRelayDetail = "control_center.local_opencodex.relay.detail"
    case controlCenterLocalOpenCodexRelayAction = "control_center.local_opencodex.relay.action"
    case controlCenterLocalOpenCodexRelayHint = "control_center.local_opencodex.relay.hint"
    case controlCenterMaintenance = "control_center.maintenance"
    case controlCenterActivityLog = "control_center.activity_log"
    case controlCenterSettings = "control_center.settings"
    case controlCenterAppInformation = "control_center.app_information"
    case codexConfigTitle = "codex_config.title"
    case codexConfigLocation = "codex_config.location"
    case codexConfigExists = "codex_config.exists"
    case codexConfigExistsYes = "codex_config.exists.yes"
    case controlCenterSidebarStatus = "control_center.sidebar.status"
    case controlCenterSidebarManagement = "control_center.sidebar.management"
    case controlCenterSidebarApp = "control_center.sidebar.app"
    case codexConfigExistsNo = "codex_config.exists.no"
    case codexConfigSize = "codex_config.size"
    case codexConfigModified = "codex_config.modified"
    case codexConfigChanged = "codex_config.changed"
    case codexConfigChangedYes = "codex_config.changed.yes"
    case codexConfigChangedNo = "codex_config.changed.no"
    case codexConfigReadOnlyDetail = "codex_config.read_only_detail"
    case codexConfigRefresh = "codex_config.refresh"
    case codexConfigRefreshHint = "codex_config.refresh.hint"
    case codexConfigPreview = "codex_config.preview"
    case codexConfigPreviewHint = "codex_config.preview.hint"
    case codexConfigOpenWith = "codex_config.open_with"
    case codexConfigOpenWithHint = "codex_config.open_with.hint"
    case codexConfigOpenDefault = "codex_config.open.default"
    case codexConfigOpenVSCode = "codex_config.open.vscode"
    case codexConfigOpenXcode = "codex_config.open.xcode"
    case codexConfigOpenTextEdit = "codex_config.open.textedit"
    case codexConfigOpenOther = "codex_config.open.other"
    case codexConfigPreviewWarningTitle = "codex_config.preview.warning.title"
    case codexConfigPreviewWarningDetail = "codex_config.preview.warning.detail"
    case codexConfigPreviewCancel = "codex_config.preview.cancel"
    case codexConfigPreviewReveal = "codex_config.preview.reveal"
    case codexConfigPreviewTitle = "codex_config.preview.title"
    case codexConfigPreviewClose = "codex_config.preview.close"
    case codexConfigPreviewSessionDetail = "codex_config.preview.session_detail"
    case codexConfigBindingMissing = "codex_config.error.binding_missing"
    case codexConfigPreviewMode = "codex_config.error.preview_mode"
    case codexConfigBindingUnsafe = "codex_config.error.binding_unsafe"
    case codexConfigBindingInvalid = "codex_config.error.binding_invalid"
    case codexConfigFileMissing = "codex_config.error.file_missing"
    case codexConfigFileUnsafe = "codex_config.error.file_unsafe"
    case codexConfigFileChanged = "codex_config.error.file_changed"
    case codexConfigReadFailed = "codex_config.error.read_failed"
    case codexConfigPreviewTooLarge = "codex_config.error.preview_too_large"
    case codexConfigPreviewNotUTF8 = "codex_config.error.preview_not_utf8"
    case codexConfigOpenSucceeded = "codex_config.open.succeeded"
    case codexConfigOpenCancelled = "codex_config.open.cancelled"
    case codexConfigOpenApplicationUnavailable = "codex_config.open.application_unavailable"
    case codexConfigOpenFailed = "codex_config.open.failed"
    case appInformationApplication = "app_information.application"
    case appInformationVersionSummary = "app_information.version_summary"
    case appInformationVersion = "app_information.version"
    case appInformationBuild = "app_information.build"
    case appInformationBundleIdentifier = "app_information.bundle_identifier"
    case appInformationDistribution = "app_information.distribution"
    case appInformationDistributionProduction = "app_information.distribution.production"
    case appInformationDistributionLocalDevelopment = "app_information.distribution.local_development"
    case appInformationAdHocDistributionNotice = "app_information.distribution.adhoc_notice"
    case appInformationRuntimeMode = "app_information.runtime_mode"
    case appInformationRuntimePreview = "app_information.runtime_mode.preview"
    case appInformationRuntimeManaged = "app_information.runtime_mode.managed"
    case appInformationMinimumSystem = "app_information.minimum_system"
    case appInformationArchitecture = "app_information.architecture"
    case appInformationComponents = "app_information.components"
    case appInformationRelayRole = "app_information.component.relay.role"
    case appInformationRelayctlRole = "app_information.component.relayctl.role"
    case appInformationComponentVersion = "app_information.component.version"
    case appInformationStatusLoading = "app_information.status.loading"
    case appInformationStatusAvailable = "app_information.status.available"
    case appInformationStatusMissing = "app_information.status.missing"
    case appInformationStatusUnverified = "app_information.status.unverified"
    case appInformationVersionMatch = "app_information.version_match"
    case appInformationVersionMatchMatched = "app_information.version_match.matched"
    case appInformationVersionMatchDifferent = "app_information.version_match.different"
    case appInformationVersionMismatch = "app_information.version_mismatch"
    case appInformationPrivacy = "app_information.privacy"
    case activityLogAbout = "activity_log.about"
    case activityLogCurrentSession = "activity_log.current_session"
    case activityLogPrivacy = "activity_log.privacy"
    case activityLogUnifiedQuery = "activity_log.unified_query"
    case activityLogEvents = "activity_log.events"
    case activityLogFilter = "activity_log.filter"
    case activityLogFilterAll = "activity_log.filter.all"
    case activityLogSearch = "activity_log.search"
    case activityLogEventCount = "activity_log.event_count"
    case activityLogExport = "activity_log.export"
    case activityLogClearConfirmationTitle = "activity_log.clear.confirmation.title"
    case activityLogClearConfirmationDetail = "activity_log.clear.confirmation.detail"
    case activityLogCopyJSONL = "activity_log.copy_jsonl"
    case activityLogCopyQuery = "activity_log.copy_query"
    case activityLogClear = "activity_log.clear"
    case activityLogEmpty = "activity_log.empty"
    case activityLogLevelDebug = "activity_log.level.debug"
    case activityLogLevelInfo = "activity_log.level.info"
    case activityLogLevelWarning = "activity_log.level.warning"
    case activityLogLevelError = "activity_log.level.error"
    case controlCenterRecoveryNeeded = "control_center.recovery_needed"
    case controlCenterRefreshHint = "control_center.refresh.hint"
    case controlCenterRouteSelection = "control_center.route_selection"
    case controlCenterConnectionStatus = "control_center.connection_status"
    case controlCenterRoutingStatus = "control_center.routing_status"
    case controlCenterChangeRoute = "control_center.change_route"
    case controlCenterMoreActions = "control_center.more_actions"
    case controlCenterPendingChange = "control_center.pending_change"
    case controlCenterOpenMaintenance = "control_center.open_maintenance"
    case controlCenterRoutingRecovery = "control_center.routing_recovery"
    case controlCenterRecoveryObservedUnavailable = "control_center.recovery.observed_unavailable"
    case controlCenterRecoveryJournalMissing = "control_center.recovery.journal_missing"
    case controlCenterRecoveryJournalMalformed = "control_center.recovery.journal_malformed"
    case controlCenterRecoveryEvidenceMismatch = "control_center.recovery.evidence_mismatch"
    case controlCenterRecoveryOriginUnavailable = "control_center.recovery.origin_unavailable"
    case controlCenterRecoveryUnavailable = "control_center.recovery.unavailable"
    case controlCenterNativeRepairAction = "control_center.native_repair.action"
    case controlCenterNativeRepairHint = "control_center.native_repair.hint"
    case controlCenterNativeRepairDetail = "control_center.native_repair.detail"
    case controlCenterNativeRepairConfirmTitle = "control_center.native_repair.confirm.title"
    case controlCenterNativeRepairConfirmDetail = "control_center.native_repair.confirm.detail"
    case controlCenterNativeRepairConfirmAction = "control_center.native_repair.confirm.action"
    case controlCenterNativeRepairCancel = "control_center.native_repair.cancel"
    case controlCenterNativeRepairInspectAction = "control_center.native_repair.inspect.action"
    case controlCenterNativeRepairInspectHint = "control_center.native_repair.inspect.hint"
    case controlCenterNativeRepairDiagnosis = "control_center.native_repair.diagnosis"
    case controlCenterNativeRepairDetectedOwner = "control_center.native_repair.detected_owner"
    case controlCenterNativeRepairOwnerStateOnly = "control_center.native_repair.owner.state_only"
    case controlCenterNativeRepairOwnerLocalRelay = "control_center.native_repair.owner.local_relay"
    case controlCenterNativeRepairOwnerOpenCodex = "control_center.native_repair.owner.opencodex"
    case controlCenterNativeRepairOwnerUnavailable = "control_center.native_repair.owner.unavailable"
    case controlCenterNativeRepairOpenAIBaseURL = "control_center.native_repair.field.openai_base_url"
    case controlCenterNativeRepairModelCatalog = "control_center.native_repair.field.model_catalog"
    case controlCenterNativeRepairFieldPresent = "control_center.native_repair.field.present"
    case controlCenterNativeRepairFieldAbsent = "control_center.native_repair.field.absent"
    case controlCenterNativeRepairStateOnlyDetail = "control_center.native_repair.state_only.detail"
    case controlCenterNativeRepairLocalRelayDetail = "control_center.native_repair.local_relay.detail"
    case controlCenterNativeRepairOpenCodexDetail = "control_center.native_repair.opencodex.detail"
    case controlCenterNativeRepairUnavailableDetail = "control_center.native_repair.unavailable.detail"
    case controlCenterNativeRepairCandidateTitle = "control_center.native_repair.candidate.title"
    case controlCenterNativeRepairCandidateAction = "control_center.native_repair.candidate.action"
    case controlCenterNativeRepairCandidateSelected = "control_center.native_repair.candidate.selected"
    case controlCenterNativeRepairCandidateRuntimeUnverified = "control_center.native_repair.candidate.runtime_unverified"
    case controlCenterNativeRepairCandidateNone = "control_center.native_repair.candidate.none"
    case controlCenterNativeRepairOwnerConfiguration = "control_center.native_repair.owner.configuration"
    case controlCenterNativeRepairOwnerIntegration = "control_center.native_repair.owner.integration"
    case controlCenterNativeRepairOwnerReadiness = "control_center.native_repair.owner.readiness"
    case controlCenterNativeRepairOwnerConfigurationValid = "control_center.native_repair.owner.configuration.valid"
    case controlCenterNativeRepairOwnerConfigurationInvalid = "control_center.native_repair.owner.configuration.invalid"
    case controlCenterNativeRepairOwnerConfigurationUnavailable = "control_center.native_repair.owner.configuration.unavailable"
    case controlCenterNativeRepairOwnerIntegrationEnabled = "control_center.native_repair.owner.integration.enabled"
    case controlCenterNativeRepairOwnerIntegrationDisabled = "control_center.native_repair.owner.integration.disabled"
    case controlCenterNativeRepairOwnerIntegrationUnknown = "control_center.native_repair.owner.integration.unknown"
    case controlCenterNativeRepairOwnerReady = "control_center.native_repair.owner.ready"
    case controlCenterNativeRepairOwnerBlocked = "control_center.native_repair.owner.blocked"
    case controlCenterNativeRepairOwnerRetryPolicy = "control_center.native_repair.owner.retry_policy"
    case controlCenterNativeRepairRediscover = "control_center.native_repair.rediscover"
    case controlCenterNativeOwnerRepairAction = "control_center.native_repair.owner.action"
    case controlCenterNativeOwnerRepairHint = "control_center.native_repair.owner.hint"
    case controlCenterNativeOwnerRepairConfirmTitle = "control_center.native_repair.owner.confirm.title"
    case controlCenterNativeOwnerRepairConfirmDetail = "control_center.native_repair.owner.confirm.detail"
    case controlCenterNativeOwnerRepairConfirmAction = "control_center.native_repair.owner.confirm.action"
    case controlCenterNativeRepairProgress = "control_center.native_repair.progress"
    case controlCenterNativeRepairProgressDetail = "control_center.native_repair.progress.detail"
    case controlCenterNativeRepairCompletedTitle = "control_center.native_repair.completed.title"
    case controlCenterNativeRepairCompletedSteps = "control_center.native_repair.completed.steps"
    case controlCenterNativeRepairStepPreflight = "control_center.native_repair.step.preflight"
    case controlCenterNativeRepairStepDesktopExit = "control_center.native_repair.step.desktop_exit"
    case controlCenterNativeRepairStepOwnerRepair = "control_center.native_repair.step.owner_repair"
    case controlCenterNativeRepairStepNativeVerification = "control_center.native_repair.step.native_verification"
    case controlCenterNativeRepairStepStateCommit = "control_center.native_repair.step.state_commit"
    case controlCenterNativeRepairStepDesktopRelaunch = "control_center.native_repair.step.desktop_relaunch"
    case controlCenterNativeRepairStepStatusRefresh = "control_center.native_repair.step.status_refresh"
    case controlCenterNativeRepairStepPending = "control_center.native_repair.step.pending"
    case controlCenterNativeRepairStepRunning = "control_center.native_repair.step.running"
    case controlCenterNativeRepairStepCompleted = "control_center.native_repair.step.completed"
    case controlCenterNativeRepairStepFailed = "control_center.native_repair.step.failed"
    case controlCenterNativeRepairIntegrationDisabled = "control_center.native_repair.integration_disabled"
    case controlCenterNativeRepairOpenLocal = "control_center.native_repair.open_local"
    case controlCenterOpenCodexMaintenance = "control_center.opencodex_maintenance"
    case controlCenterRemovalInProgress = "control_center.removal_in_progress"
    case controlCenterNoMaintenance = "control_center.no_maintenance"
    case controlCenterLanguage = "control_center.language"
    case controlCenterLoginItem = "control_center.login_item"
    case gatewaySettingsTitle = "gateway.settings.title"
    case gatewayAddressLabel = "gateway.address.label"
    case gatewayAddressPlaceholder = "gateway.address.placeholder"
    case gatewayAuthenticationLabel = "gateway.authentication.label"
    case gatewayAuthenticationNone = "gateway.authentication.none"
    case gatewayAuthenticationAPIKey = "gateway.authentication.api_key"
    case gatewayAuthenticationCloudflare = "gateway.authentication.cloudflare"
    case gatewayAuthenticationNoneDetail = "gateway.authentication.none.detail"
    case gatewayAuthenticationAPIKeyDetail = "gateway.authentication.api_key.detail"
    case gatewayAuthenticationCloudflareDetail = "gateway.authentication.cloudflare.detail"
    case gatewayAuthenticationHTTPSRequired = "gateway.authentication.https_required"
    case gatewayInsecurePrivateIPConfirmation = "gateway.insecure_private_ip.confirmation"
    case gatewayConnectionTest = "gateway.connection_test"
    case gatewayIntegrationPrepare = "gateway.integration.prepare"
    case gatewayIntegrationRecover = "gateway.integration.recover"
    case gatewayApply = "gateway.apply"
    case gatewaySwitchCodex = "gateway.switch_codex"
    case gatewaySwitchCodexDetail = "gateway.switch_codex.detail"
    case gatewayEditSettings = "gateway.edit_settings"
    case gatewayCredentialConfigured = "gateway.credential.configured"
    case gatewayCredentialMissing = "gateway.credential.missing"
    case gatewayCredentialChecking = "gateway.credential.checking"
    case gatewayCredentialUnavailable = "gateway.credential.unavailable"
    case gatewayCredentialNewValue = "gateway.credential.new_value"
    case gatewayCredentialReplace = "gateway.credential.replace"
    case gatewayCredentialAdd = "gateway.credential.add"
    case gatewayCloudflareClientID = "gateway.credential.cloudflare_client_id"
    case gatewayCloudflareClientSecret = "gateway.credential.cloudflare_client_secret"
    case gatewayAPIKey = "gateway.credential.api_key"
    case gatewayCredentialsPrivacy = "gateway.credentials.privacy"
    case gatewayCredentialsManagedExternally = "gateway.credentials.managed_externally"
    case gatewayStatusLoading = "gateway.status.loading"
    case gatewayStatusNeedsValidation = "gateway.status.needs_validation"
    case gatewayStatusTesting = "gateway.status.testing"
    case gatewayStatusApplying = "gateway.status.applying"
    case gatewayStatusConnected = "gateway.status.connected"
    case gatewayStatusAuthenticationMismatch = "gateway.status.authentication_mismatch"
    case gatewayStatusUnreachable = "gateway.status.unreachable"
    case gatewayStatusCatalogInvalid = "gateway.status.catalog_invalid"
    case gatewayStatusIntegrationRequired = "gateway.status.integration_required"
    case gatewayStatusRecoveryRequired = "gateway.status.recovery_required"
    case gatewayStatusAppLocationInvalid = "gateway.status.app_location_invalid"
    case gatewayStatusArtifactInvalid = "gateway.status.artifact_invalid"
    case gatewayStatusBindingUnsafe = "gateway.status.binding_unsafe"
    case gatewayStatusBindingInvalid = "gateway.status.binding_invalid"
    case gatewayStatusHelperUnavailable = "gateway.status.helper_unavailable"
    case gatewayStatusUnsupported = "gateway.status.unsupported"
    case gatewayStatusFailed = "gateway.status.failed"
    case gatewayDetailWorking = "gateway.detail.working"
    case gatewayDetailNeedsValidation = "gateway.detail.needs_validation"
    case gatewayDetailConnected = "gateway.detail.connected"
    case gatewayDetailAuthenticationMismatch = "gateway.detail.authentication_mismatch"
    case gatewayDetailUnreachable = "gateway.detail.unreachable"
    case gatewayDetailCatalogInvalid = "gateway.detail.catalog_invalid"
    case gatewayDetailIntegrationRequired = "gateway.detail.integration_required"
    case gatewayDetailRecoveryRequired = "gateway.detail.recovery_required"
    case gatewayDetailAppLocationInvalid = "gateway.detail.app_location_invalid"
    case gatewayDetailArtifactInvalid = "gateway.detail.artifact_invalid"
    case gatewayDetailBindingUnsafe = "gateway.detail.binding_unsafe"
    case gatewayDetailBindingInvalid = "gateway.detail.binding_invalid"
    case gatewayDetailHelperUnavailable = "gateway.detail.helper_unavailable"
    case gatewayDetailUnsupported = "gateway.detail.unsupported"
    case gatewayDetailFailed = "gateway.detail.failed"
    case relocationTitle = "relocation.title"
    case relocationMoveAction = "relocation.move_action"
    case relocationRetryValidation = "relocation.retry_validation"
    case relocationDetail = "relocation.detail"
    case relocationSourceBundleInvalid = "relocation.source_bundle_invalid"
    case relocationSourceProcessInvalid = "relocation.source_process_invalid"
    case relocationSourceLocationInvalid = "relocation.source_location_invalid"
    case relocationPreviewDetail = "relocation.preview_detail"
    case relocationPreparing = "relocation.preparing"
    case relocationWaitingForDestination = "relocation.waiting_for_destination"
    case relocationSourceExitRequired = "relocation.source_exit_required"
    case relocationBackupCleanupFailed = "relocation.backup_cleanup_failed"
    case relocationRetryHandoff = "relocation.retry_handoff"
    case relocationFallbackTitle = "relocation.fallback.title"
    case relocationFallbackDetail = "relocation.fallback.detail"
    case relocationFallbackAction = "relocation.fallback.action"
    case relocationReplacementTitle = "relocation.replacement.title"
    case relocationReplacementDetail = "relocation.replacement.detail"
    case relocationReplacementAction = "relocation.replacement.action"
    case relocationCleanupTitle = "relocation.cleanup.title"
    case relocationCleanupDetail = "relocation.cleanup.detail"
    case relocationCleanupKeep = "relocation.cleanup.keep"
    case relocationCleanupTrash = "relocation.cleanup.trash"
    case relocationCompleted = "relocation.completed"
    case relocationManualDetail = "relocation.manual_detail"
    case relocationRecoveryDetail = "relocation.recovery_detail"
    case relocationCancel = "relocation.cancel"
    case viewConnectionRouting = "view.connection_routing"
    case viewCodexDesktop = "view.codex_desktop"
    case viewStatusUpdate = "view.status_update"
    case viewControlledApp = "view.controlled_app"
    case viewRegistration = "view.registration"
    case viewDesktopBackendUnverifiable = "view.desktop_backend_unverifiable"
    case viewDesktopRegistrationAccessibility = "view.desktop_registration_accessibility"
    case viewUnsignedWarning = "view.unsigned_warning"
    case viewUnsignedWarningAccessibility = "view.unsigned_warning.accessibility"
    case viewLocalRelay = "view.local_relay"
    case viewRoutingSync = "view.routing_sync"
    case viewRemoteObservation = "view.remote_observation"
    case viewLocalOpenCodex = "view.local_opencodex"
    case viewCatalog = "view.catalog"
    case viewActiveRequests = "view.active_requests"
    case viewDrain = "view.drain"
    case viewLastLocalUpdate = "view.last_local_update"
    case viewDesiredBackend = "view.desired_backend"
    case viewAppliedBackend = "view.applied_backend"
    case viewPhase = "view.phase"
    case viewRelayProcess = "view.relay_process"
    case viewRelayAdmission = "view.relay_admission"
    case viewCatalogRefresh = "view.catalog_refresh"
    case viewGeneration = "view.generation"
    case viewConnectionRoutingAccessibility = "view.connection_routing.accessibility"
    case viewStatusRowAccessibility = "view.status_row.accessibility"
    case viewStatusCode = "view.status_code"
    case viewStatusMessageAccessibility = "view.status_message.accessibility"

    case desktopNotRegistered = "desktop.not_registered"
    case desktopRegisteredRunning = "desktop.registered_running"
    case desktopRegisteredStopped = "desktop.registered_stopped"
    case desktopUnavailable = "desktop.unavailable"
    case desktopTrustConfigurationMissing = "desktop.trust_configuration_missing"
    case desktopUntrusted = "desktop.untrusted"
    case desktopAmbiguous = "desktop.ambiguous"
    case desktopNotRegisteredAccessibility = "desktop.not_registered.accessibility"
    case desktopRegisteredRunningAccessibility = "desktop.registered_running.accessibility"
    case desktopRegisteredStoppedAccessibility = "desktop.registered_stopped.accessibility"
    case desktopUnavailableAccessibility = "desktop.unavailable.accessibility"
    case desktopTrustConfigurationMissingAccessibility = "desktop.trust_configuration_missing.accessibility"
    case desktopUntrustedAccessibility = "desktop.untrusted.accessibility"
    case desktopAmbiguousAccessibility = "desktop.ambiguous.accessibility"
    case desktopNoneSelected = "desktop.none_selected"

    case panelDesktopTitle = "panel.desktop.title"
    case panelDesktopMessage = "panel.desktop.message"
    case panelDesktopPrompt = "panel.desktop.prompt"
    case panelOpenCodexTitle = "panel.ocx.title"
    case panelOpenCodexMessage = "panel.ocx.message"
    case panelOpenCodexPrompt = "panel.ocx.prompt"

    case messageDesktopSelected = "message.desktop_selected"
    case messageDesktopDiscovered = "message.desktop_discovered"
    case messageDesktopNotFound = "message.desktop_not_found"
    case messageDesktopDiscoveryAmbiguous = "message.desktop_discovery_ambiguous"
    case messageDesktopTrustConfigurationMissing = "message.desktop_trust_configuration_missing"
    case messageDesktopTrustRejected = "message.desktop_trust_rejected"
    case messageDesktopSelectionInvalid = "message.desktop_selection_invalid"
    case messageDesktopNotSelectedHandoff = "message.desktop_not_selected.handoff"
    case messageOCXSelectionInvalid = "message.ocx_selection_invalid"
    case messageOpenCodexUninstallUnsafe = "message.ocx_uninstall_unsafe"
    case messageOpenCodexRemoved = "message.ocx_removed"
    case messageOpenCodexNativeRemoved = "message.ocx_native_removed"
    case messageOpenCodexHandoffCompleted = "message.ocx_handoff_completed"
    case messageOpenCodexHandoffCandidateRefreshRequired = "message.ocx_handoff_candidate_refresh_required"
    case messageStatusRefreshed = "message.status_refreshed"
    case messageStatusUnchanged = "message.status_unchanged"
    case messageRoutingRecoveryRequired = "message.routing_recovery_required"
    case messageRoutingStatusUnavailable = "message.routing_status_unavailable"
    case messageRoutingGenerationChanged = "message.routing_generation_changed"
    case messageNativeRoutingUnverified = "message.native_routing_unverified"
    case messageNativeRepairUnavailable = "message.native_repair_unavailable"
    case messageNativeRepairRunning = "message.native_repair_running"
    case messageNativeRepairCompleted = "message.native_repair_completed"
    case messageNativeRepairInspected = "message.native_repair_inspected"
    case messageNativeRepairCandidateSelected = "message.native_repair_candidate_selected"
    case messageNativeRepairOwnerInspected = "message.native_repair_owner_inspected"
    case messageNativeOwnerRepairRunning = "message.native_owner_repair_running"
    case messageNativeOwnerRepairCompleted = "message.native_owner_repair_completed"
    case messageNativeOwnerRepairCompletedWithWarning = "message.native_owner_repair_completed_with_warning"
    case messageNativeRepairOwnerChanged = "message.native_repair_owner_changed"
    case messageNativeOwnerRepairFailed = "message.native_owner_repair_failed"
    case messageNativeOwnerBusy = "message.native_owner_busy"
    case messageNativeOwnerConfigurationInvalid = "message.native_owner_configuration_invalid"
    case messageNativeOwnerRestoreFailed = "message.native_owner_restore_failed"
    case messageNativeOwnerResultInvalid = "message.native_owner_result_invalid"
    case messageNativeStateRepairPending = "message.native_state_repair_pending"
    case messageLocalUnavailable = "message.local_unavailable"
    case messageDesktopNotSelectedRouting = "message.desktop_not_selected.routing"
    case messageRequesting = "message.requesting"
    case messageNoPendingTransition = "message.no_pending_transition"
    case messageRoutingApplied = "message.routing_applied"
    case messageCanceling = "message.canceling"
    case messageRecoveryNotRequired = "message.recovery_not_required"
    case messageRecoveryCompleted = "message.recovery_completed"
    case messageRecoveryRolledBack = "message.recovery_rolled_back"
    case messageDesktopRelaunched = "message.desktop_relaunched"
    case messageDesktopRelaunchFailed = "message.desktop_relaunch_failed"
    case messageDesktopQuitRequested = "message.desktop_quit_requested"
    case messageDesktopQuitDeclined = "message.desktop_quit_declined"
    case messageDesktopQuitTimeout = "message.desktop_quit_timeout"
    case messageDesktopRestarted = "message.desktop_restarted"
    case messageRoutingAppliedRefreshPending = "message.routing_applied_refresh_pending"
    case messageRoutingCommandRunning = "message.routing_command_running"
    case messageRoutingBindingInvalid = "message.routing_binding_invalid"
    case messageDesktopNotSelectedApply = "message.desktop_not_selected.apply"
    case messageDesktopUnavailable = "message.desktop_unavailable"
    case messageRoutingOperationFailed = "message.routing_operation_failed"
    case messageLoginOptional = "message.login_optional"
    case messageLoginEnabled = "message.login_enabled"
    case messageLoginPending = "message.login_pending"
    case messageLoginDisabled = "message.login_disabled"
    case messageLoginFailed = "message.login_failed"

    case menuResumeRemovalRecovery = "menu.removal.resume"
    case menuResumeRemovalRecoveryHint = "menu.removal.resume.hint"

    case homebrewGuardTitle = "homebrew_guard.title"
    case homebrewGuardRegistration = "homebrew_guard.registration"
    case homebrewGuardBackend = "homebrew_guard.backend"
    case homebrewGuardBackendSMAppService = "homebrew_guard.backend.sm_app_service"
    case homebrewGuardBackendManualDevelopment = "homebrew_guard.backend.manual_development"
    case homebrewGuardVersion = "homebrew_guard.version"
    case homebrewGuardProtocol = "homebrew_guard.protocol"
    case homebrewGuardRegister = "homebrew_guard.register"
    case homebrewGuardDevelopmentSetup = "homebrew_guard.development_setup"
    case homebrewGuardDevelopmentSetupTitle = "homebrew_guard.development_setup.title"
    case homebrewGuardDevelopmentSetupDetail = "homebrew_guard.development_setup.detail"
    case homebrewGuardDevelopmentSetupResultHint = "homebrew_guard.development_setup.result_hint"
    case homebrewGuardDevelopmentGuidanceDaemonStart =
        "homebrew_guard.development_setup.guidance.daemon_start"
    case homebrewGuardDevelopmentGuidanceProbe =
        "homebrew_guard.development_setup.guidance.probe"
    case homebrewGuardDevelopmentGuidanceOwnership =
        "homebrew_guard.development_setup.guidance.ownership"
    case homebrewGuardDevelopmentGuidanceRollback =
        "homebrew_guard.development_setup.guidance.rollback"
    case homebrewGuardDevelopmentSetupCommand = "homebrew_guard.development_setup.command"
    case homebrewGuardDevelopmentSetupCopy = "homebrew_guard.development_setup.copy"
    case homebrewGuardDevelopmentSetupClose = "homebrew_guard.development_setup.close"
    case homebrewGuardDevelopmentSetupChecking = "homebrew_guard.development_setup.checking"
    case homebrewGuardDevelopmentSetupUnchanged = "homebrew_guard.development_setup.unchanged"
    case homebrewGuardDevelopmentSetupSuccessTitle = "homebrew_guard.development_setup.success.title"
    case homebrewGuardDevelopmentSetupSuccessDetail = "homebrew_guard.development_setup.success.detail"
    case homebrewGuardDevelopmentSetupStateChangedTitle = "homebrew_guard.development_setup.state_changed.title"
    case homebrewGuardDevelopmentSetupStateChangedDetail = "homebrew_guard.development_setup.state_changed.detail"
    case homebrewGuardDevelopmentSetupDone = "homebrew_guard.development_setup.done"
    case homebrewGuardDevelopmentSetupReviewUpdatedState =
        "homebrew_guard.development_setup.review_updated_state"
    case homebrewGuardDevelopmentSetupHint = "homebrew_guard.development_setup.hint"
    case homebrewGuardDevelopmentRecovery = "homebrew_guard.development_recovery"
    case homebrewGuardDevelopmentRecoveryTitle = "homebrew_guard.development_recovery.title"
    case homebrewGuardDevelopmentRecoveryDetail = "homebrew_guard.development_recovery.detail"
    case homebrewGuardDevelopmentRecoveryHint = "homebrew_guard.development_recovery.hint"
    case homebrewGuardOpenSettings = "homebrew_guard.open_settings"
    case homebrewGuardOpenSettingsHint = "homebrew_guard.open_settings.hint"
    case homebrewGuardRecover = "homebrew_guard.recover"
    case homebrewGuardRefresh = "homebrew_guard.refresh"
    case homebrewGuardStateNotRequired = "homebrew_guard.state.not_required"
    case homebrewGuardStatePreview = "homebrew_guard.state.preview"
    case homebrewGuardStateNotRegistered = "homebrew_guard.state.not_registered"
    case homebrewGuardStateApprovalRequired = "homebrew_guard.state.approval_required"
    case homebrewGuardStateManualInstallRequired = "homebrew_guard.state.manual_install_required"
    case homebrewGuardStateManualUpdateRequired = "homebrew_guard.state.manual_update_required"
    case homebrewGuardStateManualInstallerRecoveryRequired = "homebrew_guard.state.manual_installer_recovery_required"
    case homebrewGuardStateDaemonLaunchFailed = "homebrew_guard.state.daemon_launch_failed"
    case homebrewGuardStateReady = "homebrew_guard.state.ready"
    case homebrewGuardStateBusy = "homebrew_guard.state.busy"
    case homebrewGuardStateRecoveryRequired = "homebrew_guard.state.recovery_required"
    case homebrewGuardStateUnavailable = "homebrew_guard.state.unavailable"
    case homebrewGuardDetailNotRegistered = "homebrew_guard.detail.not_registered"
    case homebrewGuardDetailPreview = "homebrew_guard.detail.preview"
    case homebrewGuardDetailArtifactInvalid = "homebrew_guard.detail.artifact_invalid"
    case homebrewGuardDetailApprovalRequired = "homebrew_guard.detail.approval_required"
    case homebrewGuardDetailManualInstallRequired = "homebrew_guard.detail.manual_install_required"
    case homebrewGuardDetailManualUpdateRequired = "homebrew_guard.detail.manual_update_required"
    case homebrewGuardDetailManualInstallerRecoveryRequired = "homebrew_guard.detail.manual_installer_recovery_required"
    case homebrewGuardDetailDaemonLaunchFailed = "homebrew_guard.detail.daemon_launch_failed"
    case homebrewGuardDetailReady = "homebrew_guard.detail.ready"
    case homebrewGuardDetailRecoveryRequired = "homebrew_guard.detail.recovery_required"
    case homebrewGuardDetailUnavailable = "homebrew_guard.detail.unavailable"

    case integrationGuideCardTitle = "integration_guide.card.title"
    case producerToolsMenu = "producer_tools.menu"
    case integrationGuideCardDetail = "integration_guide.card.detail"
    case integrationGuideOpen = "integration_guide.open"
    case integrationGuideTitle = "integration_guide.title"
    case integrationGuideDetail = "integration_guide.detail"
    case integrationGuideRepository = "integration_guide.repository"
    case integrationGuideVersion = "integration_guide.version"
    case integrationGuideOutput = "integration_guide.output"
    case integrationGuideUpstream = "integration_guide.upstream"
    case integrationGuideSigningSource = "integration_guide.signing_source"
    case integrationGuideSigningPEM = "integration_guide.signing_pem"
    case integrationGuideSigningKeychain = "integration_guide.signing_keychain"
    case integrationGuideSigningValue = "integration_guide.signing_value"
    case integrationGuideAcknowledge = "integration_guide.acknowledge"
    case integrationGuideBuildCommand = "integration_guide.build_command"
    case integrationGuideInstallCommand = "integration_guide.install_command"
    case integrationGuideCopyBuild = "integration_guide.copy_build"
    case integrationGuideCopyInstall = "integration_guide.copy_install"
    case integrationGuideClose = "integration_guide.close"
    case integrationGuideInvalid = "integration_guide.invalid"
    case integrationGuideBack = "integration_guide.back"
    case integrationGuideNext = "integration_guide.next"
    case integrationGuideAdvanced = "integration_guide.advanced"
    case integrationGuideStepSource = "integration_guide.step.source"
    case integrationGuideStepPackage = "integration_guide.step.package"
    case integrationGuideStepGateway = "integration_guide.step.gateway"
    case integrationGuideStepSigning = "integration_guide.step.signing"
    case integrationGuideStepReview = "integration_guide.step.review"
    case integrationGuideSourceDetail = "integration_guide.source.detail"
    case integrationGuideChooseRepository = "integration_guide.source.choose"
    case integrationGuideRepositorySelectionTitle = "integration_guide.source.selection.title"
    case integrationGuideRepositorySelectionMessage = "integration_guide.source.selection.message"
    case integrationGuidePackageDetail = "integration_guide.package.detail"
    case integrationGuideVersionHelp = "integration_guide.package.version_help"
    case integrationGuideOutputHelp = "integration_guide.package.output_help"
    case integrationGuideChooseOutput = "integration_guide.package.output_choose"
    case integrationGuideOutputSelectionTitle = "integration_guide.package.output_selection.title"
    case integrationGuideOutputSelectionMessage = "integration_guide.package.output_selection.message"
    case integrationGuideGatewayDetail = "integration_guide.gateway.detail"
    case integrationGuideGatewayHelp = "integration_guide.gateway.help"
    case integrationGuideSigningDetail = "integration_guide.signing.detail"
    case integrationGuideSigningNotCredential = "integration_guide.signing.not_credential"
    case integrationGuideKeychainService = "integration_guide.signing.keychain_service"
    case integrationGuideKeychainHelp = "integration_guide.signing.keychain_help"
    case integrationGuideKeychainChoice = "integration_guide.signing.keychain_choice"
    case integrationGuideKeychainPrepare = "integration_guide.signing.keychain_prepare"
    case integrationGuideKeychainExisting = "integration_guide.signing.keychain_existing"
    case integrationGuidePEMFile = "integration_guide.signing.pem_file"
    case integrationGuidePEMHelp = "integration_guide.signing.pem_help"
    case integrationGuideChoosePEM = "integration_guide.signing.pem_choose"
    case integrationGuidePEMSelectionTitle = "integration_guide.signing.pem_selection.title"
    case integrationGuidePEMSelectionMessage = "integration_guide.signing.pem_selection.message"
    case integrationGuideReviewDetail = "integration_guide.review.detail"
    case integrationGuideReviewReady = "integration_guide.review.ready"
    case integrationGuideReviewAcknowledgeRequired = "integration_guide.review.acknowledge_required"
    case integrationGuideReviewCorrectionRequired = "integration_guide.review.correction_required"
    case integrationGuideSigningSetupCommand = "integration_guide.signing_setup_command"
    case integrationGuideCopySigningSetup = "integration_guide.copy_signing_setup"
    case integrationGuideValidationRequired = "integration_guide.validation.required"
    case integrationGuideValidationValid = "integration_guide.validation.valid"
    case integrationGuideValidationRepositoryUnsafe = "integration_guide.validation.repository_unsafe"
    case integrationGuideValidationRepositoryScriptsMissing = "integration_guide.validation.repository_scripts_missing"
    case integrationGuideValidationVersionInvalid = "integration_guide.validation.version_invalid"
    case integrationGuideValidationOutputUnsafe = "integration_guide.validation.output_unsafe"
    case integrationGuideValidationGatewayInvalid = "integration_guide.validation.gateway_invalid"
    case integrationGuideValidationKeychainServiceInvalid = "integration_guide.validation.keychain_service_invalid"
    case integrationGuideValidationPEMFileUnsafe = "integration_guide.validation.pem_file_unsafe"
    case messageHomebrewGuardNotRegistered = "message.homebrew_guard_not_registered"
    case messageHomebrewGuardApprovalRequired = "message.homebrew_guard_approval_required"
    case messageHomebrewGuardBusy = "message.homebrew_guard_busy"
    case messageHomebrewGuardCandidateChanged = "message.homebrew_guard_candidate_changed"
    case messageHomebrewGuardProtectionFailed = "message.homebrew_guard_protection_failed"
    case messageHomebrewGuardRecoveryRequired = "message.homebrew_guard_recovery_required"
    case messageHomebrewGuardRestoreFailed = "message.homebrew_guard_restore_failed"
    case removalExecutionProgressTitle = "removal.execution.progress.title"
    case removalExecutionProgressDetail = "removal.execution.progress.detail"
    case removalExecutionStepPreflight = "removal.execution.step.preflight"
    case removalExecutionStepDesktopExit = "removal.execution.step.desktop_exit"
    case removalExecutionStepHomebrewProtection = "removal.execution.step.homebrew_protection"
    case removalExecutionStepCandidateRevalidation = "removal.execution.step.candidate_revalidation"
    case removalExecutionStepTeardown = "removal.execution.step.teardown"
    case removalExecutionStepPackageRemoval = "removal.execution.step.package_removal"
    case removalExecutionStepResultVerification = "removal.execution.step.result_verification"
    case removalExecutionStepPermissionRestore = "removal.execution.step.permission_restore"
    case removalExecutionStepDesktopRelaunch = "removal.execution.step.desktop_relaunch"
    case removalExecutionStepStatusRefresh = "removal.execution.step.status_refresh"

    case removalTitle = "removal.title"
    case removalClose = "removal.close"
    case removalCancel = "removal.cancel"
    case removalBack = "removal.back"
    case removalActionsDetail = "removal.actions.detail"
    case removalNativeActionsDetail = "removal.native.actions.detail"
    case removalHandoffSection = "removal.handoff.section"
    case removalHandoffRemoveShimDetail = "removal.handoff.remove_shim.detail"
    case removalHandoffKeepShimDetail = "removal.handoff.keep_shim.detail"
    case removalHandoffProgressTitle = "removal.handoff.progress.title"
    case removalHandoffProgressDetail = "removal.handoff.progress.detail"
    case removalHandoffStepPreflight = "removal.handoff.step.preflight"
    case removalHandoffStepDesktopExit = "removal.handoff.step.desktop_exit"
    case removalHandoffStepRemoveShim = "removal.handoff.step.remove_shim"
    case removalHandoffStepKeepShim = "removal.handoff.step.keep_shim"
    case removalHandoffStepDesktopRelaunch = "removal.handoff.step.desktop_relaunch"
    case removalHandoffStepStatusRefresh = "removal.handoff.step.status_refresh"
    case removalHandoffStatusPending = "removal.handoff.status.pending"
    case removalHandoffStatusRunning = "removal.handoff.status.running"
    case removalHandoffStatusCompleted = "removal.handoff.status.completed"
    case removalHandoffStatusBlocked = "removal.handoff.status.blocked"
    case removalHandoffStatusFailed = "removal.handoff.status.failed"
    case removalHandoffResultRemovalAvailable = "removal.handoff.result.removal_available"
    case removalHandoffResultRemovalBlocked = "removal.handoff.result.removal_blocked"
    case removalHandoffResultRecoveryRequired = "removal.handoff.result.recovery_required"
    case removalHandoffResultUnverified = "removal.handoff.result.unverified"
    case removalSafeSection = "removal.safe.section"
    case removalSafeDetail = "removal.safe.detail"
    case removalSafeAction = "removal.safe.action"
    case removalNativeSafeSection = "removal.native.safe.section"
    case removalNativeSafeDetail = "removal.native.safe.detail"
    case removalNativeSafeAction = "removal.native.safe.action"
    case removalManualOnly = "removal.manual_only"
    case removalRouteUnsafe = "removal.route_unsafe"
    case removalCandidateSummary = "removal.candidate.summary"
    case removalInventoryLoading = "removal.inventory.loading"
    case removalInventoryLoadingDetail = "removal.inventory.loading.detail"
    case removalInventoryTitle = "removal.inventory.title"
    case removalOptionsTitle = "removal.options.title"
    case removalOptionsDetail = "removal.options.detail"
    case removalPreservedTitle = "removal.preserve.kept.title"
    case removalPreservedDetail = "removal.preserve.kept.detail"
    case removalRemovedTitle = "removal.preserve.removed.title"
    case removalRemovedDetail = "removal.preserve.removed.detail"
    case removalDataMode = "removal.data_mode"
    case removalModePreserve = "removal.mode.preserve"
    case removalModeTrash = "removal.mode.trash"
    case removalPreserveDetail = "removal.mode.preserve.detail"
    case removalTrashDetail = "removal.mode.trash.detail"
    case removalSensitive = "removal.item.sensitive"
    case removalRetiredItem = "removal.item.retired"
    case removalProtectedItem = "removal.item.protected"
    case removalSelectedCount = "removal.selected_count"
    case removalReviewAction = "removal.review.action"
    case removalConfirmPackageTitle = "removal.confirm_package.title"
    case removalConfirmPackageDetail = "removal.confirm_package.detail"
    case removalSecondConfirmationNotice = "removal.confirm_package.second_notice"
    case removalConfirmPackageAction = "removal.confirm_package.action"
    case removalConfirmTrashTitle = "removal.confirm_trash.title"
    case removalConfirmTrashDetail = "removal.confirm_trash.detail"
    case removalConfirmTrashAction = "removal.confirm_trash.action"
    case removalTrashNoPermanentDelete = "removal.confirm_trash.no_permanent_delete"
    case removalReviewMode = "removal.review.mode"
    case removalReviewSelected = "removal.review.selected"
    case removalReviewGeneration = "removal.review.generation"
    case removalReviewNativeBoundary = "removal.review.native_boundary"
    case removalQuittingDesktop = "removal.progress.quitting_desktop"
    case removalQuittingDesktopDetail = "removal.progress.quitting_desktop.detail"
    case removalRunning = "removal.progress.running"
    case removalRunningDetail = "removal.progress.running.detail"
    case removalDataRefreshTitle = "removal.recovery.data_refresh.title"
    case removalDataRefreshDetail = "removal.recovery.data_refresh.detail"
    case removalDataRefreshAction = "removal.recovery.data_refresh.action"
    case removalRebootTitle = "removal.recovery.reboot.title"
    case removalRebootDetail = "removal.recovery.reboot.detail"
    case removalRebootAction = "removal.recovery.reboot.action"
    case removalRoutingRecoveryTitle = "removal.recovery.routing.title"
    case removalRoutingRecoveryDetail = "removal.recovery.routing.detail"
    case removalRoutingRecoveryAction = "removal.recovery.routing.action"
    case removalNativeRecoveryTitle = "removal.recovery.native.title"
    case removalNativeRecoveryDetail = "removal.recovery.native.detail"
    case removalNativeRecoveryAction = "removal.recovery.native.action"
    case removalNativeCleanupTitle = "removal.cleanup.native.title"
    case removalNativeCleanupDetail = "removal.cleanup.native.detail"
    case removalNativeCleanupAction = "removal.cleanup.native.action"
    case removalRecoveryNoPIDProof = "removal.recovery.no_pid_proof"
    case removalResultSuccess = "removal.result.success"
    case removalResultPartial = "removal.result.partial"
    case removalResultCounts = "removal.result.counts"
    case removalPackageRemoved = "removal.result.package_removed"
    case removalPackageNotVerified = "removal.result.package_not_verified"
    case removalStagesTitle = "removal.result.stages"
    case removalStageRow = "removal.result.stage_row"
    case removalResultRecoveryDetail = "removal.result.recovery_detail"
    case removalRequireRebootAction = "removal.result.require_reboot"
    case removalResultInvalid = "removal.result.invalid"
    case removalFailedTitle = "removal.failed.title"
    case removalFailedDetail = "removal.failed.detail"
    case removalFailedSafetyDetail = "removal.failed.safety_detail"

    case removalCategoryCredentials = "removal.category.credentials"
    case removalCategoryConfiguration = "removal.category.configuration"
    case removalCategoryIntegrationBackups = "removal.category.integration_backups"
    case removalCategoryLogs = "removal.category.logs"
    case removalCategoryRuntime = "removal.category.runtime"
    case removalCategoryArtifacts = "removal.category.artifacts"
    case removalCategoryOwnershipMetadata = "removal.category.ownership_metadata"
    case removalCategoryRoot = "removal.category.root"
    case removalCategoryOther = "removal.category.other"
    case removalKindAbsent = "removal.kind.absent"
    case removalKindFile = "removal.kind.file"
    case removalKindDirectory = "removal.kind.directory"
    case removalKindSymlink = "removal.kind.symlink"
    case removalKindOther = "removal.kind.other"

    case removalStageRequestValidation = "removal.stage.request_validation"
    case removalStageCandidateRevalidation = "removal.stage.candidate_revalidation"
    case removalStageDataPolicy = "removal.stage.data_policy"
    case removalStageTeardownPreflight = "removal.stage.teardown_preflight"
    case removalStageCleanupJournal = "removal.stage.cleanup_journal"
    case removalStageRoutingPreTeardown = "removal.stage.routing_pre_teardown"
    case removalStageTeardown = "removal.stage.teardown"
    case removalStageRoutingVerification = "removal.stage.routing_verification"
    case removalStageRoutingPreTrash = "removal.stage.routing_pre_trash"
    case removalStageDataTrash = "removal.stage.data_trash"
    case removalStageRoutingPostTrash = "removal.stage.routing_post_trash"
    case removalStageRoutingReverification = "removal.stage.routing_reverification"
    case removalStageNPMUninstall = "removal.stage.npm_uninstall"
    case removalStageRoutingPostVerification = "removal.stage.routing_post_verification"
    case removalStagePackageVerification = "removal.stage.package_verification"
    case removalStageRoutingFinalVerification = "removal.stage.routing_final_verification"
    case removalStageRoutingRecovery = "removal.stage.routing_recovery"
    case removalStageRelayCleanup = "removal.stage.relay_cleanup"
    case removalStageNativeBoundaryPreTeardown = "removal.stage.native_boundary_pre_teardown"
    case removalStageNativeRestore = "removal.stage.native_restore"
    case removalStageNativeBoundaryVerification = "removal.stage.native_boundary_verification"
    case removalStageNativeBoundaryPreTrash = "removal.stage.native_boundary_pre_trash"
    case removalStageNativeBoundaryPostTrash = "removal.stage.native_boundary_post_trash"
    case removalStageNativeBoundaryReverification = "removal.stage.native_boundary_reverification"
    case removalStageNativeBoundaryFinalVerification = "removal.stage.native_boundary_final_verification"
    case removalStageNativeRecovery = "removal.stage.native_recovery"
    case removalStageCleanupJournalRetained = "removal.stage.cleanup_journal_retained"
    case removalStageStatusCompleted = "removal.stage_status.completed"
    case removalStageStatusSkipped = "removal.stage_status.skipped"
    case removalStageStatusRefused = "removal.stage_status.refused"
    case removalStageStatusFailed = "removal.stage_status.failed"

    case messageRemovalRecoveryUnavailable = "message.removal.recovery_unavailable"
    case messageRemovalManualOnly = "message.removal.manual_only"
    case messageRemovalInventoryAbsent = "message.removal.inventory_absent"
    case messageRemovalInventoryRefused = "message.removal.inventory_refused"
    case messageRemovalInventoryInvalid = "message.removal.inventory_invalid"
    case messageRemovalRequestInvalid = "message.removal.request_invalid"
    case messageRemovalReceiptInvalid = "message.removal.receipt_invalid"
    case messageRemovalCandidateChanged = "message.removal.candidate_changed"
    case messageRemovalGenerationChanged = "message.removal.generation_changed"
    case messageRemovalRunning = "message.removal.running"
    case messageRemovalNativeRunning = "message.removal.native_running"
    case messageRemovalDataRefreshRequired = "message.removal.data_refresh_required"
    case messageRemovalRebootRequired = "message.removal.reboot_required"
    case messageRemovalRoutingRecoveryRequired = "message.removal.routing_recovery_required"
    case messageRemovalNativeRecoveryRequired = "message.removal.native_recovery_required"
    case messageRemovalNativeCleanupPending = "message.removal.native_cleanup_pending"
    case messageRemovalNativeBoundaryChanged = "message.removal.native_boundary_changed"
    case messageRemovalNativeUnavailable = "message.removal.native_unavailable"
    case messageRemovalPartial = "message.removal.partial"
    case messageRemovalFailed = "message.removal.failed"
    case messageRemovalLegacyDataRecoveryBlocked = "message.removal.legacy_data_recovery_blocked"
    case messageRemovalTeardownUnsupported = "message.removal.teardown_unsupported"
    case messageRemovalTeardownCandidateChanged = "message.removal.teardown_candidate_changed"
    case messageRemovalTeardownPreflightFailed = "message.removal.teardown_preflight_failed"
    case messageRemovalTeardownRefused = "message.removal.teardown_refused"
    case messageRemovalTeardownResultInvalid = "message.removal.teardown_result_invalid"
    case messageRemovalTeardownVerificationFailed = "message.removal.teardown_verification_failed"
}

/// SwiftPM's generated `Bundle.module` accessor looks next to the executable,
/// while a conventional macOS app stores resource bundles in
/// `Contents/Resources`. Prefer the latter for installed apps and reserve the
/// generated accessor for debug SwiftPM execution only.
enum AppLocalizationResourceBundle {
    static let name = "OpenCodexRelay_OpenCodexRelayLocalization.bundle"

    static func resolve(
        mainBundle: Bundle,
        swiftPMFallback: () -> Bundle?
    ) -> Bundle? {
        if let resourceURL = mainBundle.resourceURL,
           let bundle = Bundle(url: resourceURL.appendingPathComponent(name, isDirectory: true)) {
            return bundle
        }
        return swiftPMFallback()
    }

    static func resolveForRuntime() -> Bundle? {
        resolve(mainBundle: .main) {
            #if DEBUG
            return Bundle.module
            #else
            return nil
            #endif
        }
    }
}

@MainActor
public struct AppLocalizer {
    public let selection: AppLanguageSelection
    public let preferredLanguageIdentifiers: [String]
    public let registry: AppLanguageRegistry
    private let resourceBundle: Bundle?

    public init(
        selection: AppLanguageSelection,
        preferredLanguageIdentifiers: [String] = Locale.preferredLanguages,
        registry: AppLanguageRegistry = .standard
    ) {
        self.init(
            selection: selection,
            preferredLanguageIdentifiers: preferredLanguageIdentifiers,
            registry: registry,
            resourceBundle: AppLocalizationResourceBundle.resolveForRuntime()
        )
    }

    init(
        selection: AppLanguageSelection,
        preferredLanguageIdentifiers: [String],
        registry: AppLanguageRegistry,
        resourceBundle: Bundle?
    ) {
        self.selection = selection
        self.preferredLanguageIdentifiers = preferredLanguageIdentifiers
        self.registry = registry
        self.resourceBundle = resourceBundle
    }

    public var resolvedDescriptor: AppLanguageDescriptor {
        registry.resolvedDescriptor(
            for: selection,
            preferredLanguageIdentifiers: preferredLanguageIdentifiers
        )
    }

    public var resolvedLanguage: ResolvedAppLanguage {
        resolvedDescriptor.catalogLanguage ?? registry.koreanDescriptor.catalogLanguage!
    }

    public var locale: Locale { resolvedLanguage.locale }

    public func text(_ key: AppStringKey, _ arguments: CVarArg...) -> String {
        text(key, arguments: arguments)
    }

    public func text(_ key: AppStringKey, arguments: [CVarArg]) -> String {
        let selected = template(key, language: resolvedLanguage)
        let fallback = template(key, language: .korean)
        let format = selected == key.rawValue ? fallback : selected
        guard !arguments.isEmpty else { return format }
        return String(format: format, locale: locale, arguments: arguments)
    }

    public func formattedDate(_ date: Date) -> String {
        date.formatted(Date.FormatStyle(date: .abbreviated, time: .standard).locale(locale))
    }

    public func formattedNumber(_ value: Int) -> String {
        formattedNumber(NSNumber(value: value))
    }

    public func formattedNumber(_ value: UInt64) -> String {
        formattedNumber(NSNumber(value: value))
    }

    private func formattedNumber(_ value: NSNumber) -> String {
        let formatter = NumberFormatter()
        formatter.locale = locale
        formatter.numberStyle = .decimal
        return formatter.string(from: value) ?? value.stringValue
    }

    private func template(_ key: AppStringKey, language: ResolvedAppLanguage) -> String {
        table(for: language)[key.rawValue] ?? key.rawValue
    }

    private func table(for language: ResolvedAppLanguage) -> [String: String] {
        guard let resourceBundle else { return [:] }
        let cacheKey = "\(resourceBundle.bundleURL.path)|\(language.rawValue)" as NSString
        if let cached = Self.catalogCache.object(forKey: cacheKey) as? [String: String] {
            return cached
        }
        let url = resourceBundle.bundleURL
            .appendingPathComponent("\(language.rawValue).lproj", isDirectory: true)
            .appendingPathComponent("Localizable.strings", isDirectory: false)
        let dictionary = NSDictionary(contentsOf: url) as? [String: String] ?? [:]
        Self.catalogCache.setObject(dictionary as NSDictionary, forKey: cacheKey)
        return dictionary
    }

    private static let catalogCache = NSCache<NSString, NSDictionary>()
}

@MainActor
public final class LocalizationStore: ObservableObject {
    public static let preferenceKey = "OpenCodexRelay.ui-language.v1"

    @Published public var selection: AppLanguageSelection {
        didSet {
            let normalized = registry.normalized(selection)
            guard normalized == selection else {
                selection = normalized
                return
            }
            defaults.set(selection.rawValue, forKey: preferenceKey)
        }
    }

    private let defaults: UserDefaults
    private let preferenceKey: String
    private let preferredLanguages: () -> [String]
    public let registry: AppLanguageRegistry

    public init(
        defaults: UserDefaults = .standard,
        preferenceKey: String = LocalizationStore.preferenceKey,
        preferredLanguages: @escaping () -> [String] = { Locale.preferredLanguages },
        registry: AppLanguageRegistry = .standard
    ) {
        self.defaults = defaults
        self.preferenceKey = preferenceKey
        self.preferredLanguages = preferredLanguages
        self.registry = registry
        self.selection = registry.normalized(
            AppLanguageSelection(rawValue: defaults.string(forKey: preferenceKey) ?? AppLanguageSelection.system.rawValue)
        )
    }

    public var localizer: AppLocalizer {
        AppLocalizer(
            selection: selection,
            preferredLanguageIdentifiers: preferredLanguages(),
            registry: registry
        )
    }

    public var locale: Locale { localizer.locale }

    public func text(_ key: AppStringKey, _ arguments: CVarArg...) -> String {
        localizer.text(key, arguments: arguments)
    }
}

public extension AppLocalizer {
    func languageName(_ descriptor: AppLanguageDescriptor) -> String {
        guard !descriptor.isSystemDefault else {
            return text(.languageSystem)
        }
        guard let languageCode = descriptor.catalogLanguage?.rawValue else {
            return descriptor.pickerFallbackName
        }
        return locale.localizedString(forLanguageCode: languageCode) ?? descriptor.pickerFallbackName
    }

    func displayName(_ value: RoutingBackend) -> String {
        switch value {
        case .unknown: text(.backendUnknown)
        case .external: text(.backendExternal)
        case .localOpenCodex: text(.backendLocal)
        case .none: text(.backendNative)
        }
    }

    func displayName(_ value: RoutingRequestTarget) -> String {
        displayName(value.backend)
    }

    func displayName(_ value: LocalRelayConnection) -> String {
        switch value {
        case .healthy: text(.genericHealthy)
        case .degraded: text(.genericDegraded)
        case .unreachable: text(.genericUnavailable)
        case .unknown: text(.genericUnknown)
        }
    }

    func displayName(_ value: LocalOpenCodexAvailability) -> String {
        switch value {
        case .ready: text(.genericReady)
        case .unavailable: text(.genericUnavailable)
        case .foreign: text(.genericForeignListener)
        case .invalid: text(.genericInvalidCatalog)
        case .unknown: text(.genericUnknown)
        }
    }

    func displayName(_ value: RoutingSync) -> String {
        switch value {
        case .acknowledged: text(.genericAcknowledged)
        case .pending: text(.genericPending)
        case .unreachable: text(.genericUnavailable)
        case .invalid: text(.genericInvalid)
        }
    }

    func displayName(_ value: RemoteGatewayConnection) -> String {
        switch value {
        case .reachable: text(.genericReady)
        case .unreachable: text(.genericUnavailable)
        case .unknown: text(.genericUnknown)
        case .notApplicable: text(.genericNotApplicable)
        }
    }

    func displayName(_ value: CatalogConnection) -> String {
        switch value {
        case .running: text(.genericRunning)
        case .paused: text(.genericPaused)
        case .unknown: text(.genericUnknown)
        }
    }

    func displayName(_ value: RoutingPhase) -> String {
        switch value {
        case .relayActive: text(.phaseRelayActive)
        case .nativePendingRestart: text(.phaseNativePending)
        case .relayPendingRestart: text(.phaseRelayPending)
        case .backendPendingRestart: text(.phaseBackendPending)
        case .applying: text(.phaseApplying)
        case .nativeActive: text(.phaseNativeActive)
        case .recoveryRequired: text(.phaseRecovery)
        }
    }

    func displayName(_ value: RelayAdmission) -> String {
        value == .allow ? text(.genericAllow) : text(.genericDeny)
    }

    func displayName(_ value: CatalogRefresh) -> String {
        value == .run ? text(.genericRunning) : text(.genericPaused)
    }

    func title(_ value: RoutingPresentation) -> String { text(presentationKey(value, suffix: "title")) }
    func compactLabel(_ value: RoutingPresentation) -> String { text(presentationKey(value, suffix: "compact")) }
    func accessibilityLabel(_ value: RoutingPresentation) -> String { text(presentationKey(value, suffix: "accessibility")) }

    func displayName(_ value: OpenCodexHandoffAction) -> String {
        switch value {
        case .retainProxyRemoveShim: text(.handoffRemoveShim)
        case .retainProxyKeepShim: text(.handoffKeepShim)
        }
    }

    func displayName(_ value: OpenCodexRemovalMode) -> String {
        switch value {
        case .preserveData: text(.removalModePreserve)
        case .trashSelected: text(.removalModeTrash)
        }
    }

    func displayName(_ value: OpenCodexInventoryCategory) -> String {
        switch value {
        case .credentials: text(.removalCategoryCredentials)
        case .configuration: text(.removalCategoryConfiguration)
        case .integrationBackups: text(.removalCategoryIntegrationBackups)
        case .logs: text(.removalCategoryLogs)
        case .runtime: text(.removalCategoryRuntime)
        case .artifacts: text(.removalCategoryArtifacts)
        case .ownershipMetadata: text(.removalCategoryOwnershipMetadata)
        case .root: text(.removalCategoryRoot)
        case .other: text(.removalCategoryOther)
        }
    }

    func displayName(_ value: OpenCodexInventoryKind) -> String {
        switch value {
        case .absent: text(.removalKindAbsent)
        case .file: text(.removalKindFile)
        case .directory: text(.removalKindDirectory)
        case .symlink: text(.removalKindSymlink)
        case .other: text(.removalKindOther)
        }
    }

    func displayName(_ value: OpenCodexRemovalStageName) -> String {
        switch value {
        case .requestValidation: text(.removalStageRequestValidation)
        case .candidateRevalidation: text(.removalStageCandidateRevalidation)
        case .dataPolicy: text(.removalStageDataPolicy)
        case .teardownPreflight: text(.removalStageTeardownPreflight)
        case .cleanupJournal: text(.removalStageCleanupJournal)
        case .routingPreTeardown: text(.removalStageRoutingPreTeardown)
        case .teardown: text(.removalStageTeardown)
        case .routingVerification: text(.removalStageRoutingVerification)
        case .routingPreTrash: text(.removalStageRoutingPreTrash)
        case .dataTrash: text(.removalStageDataTrash)
        case .routingPostTrash: text(.removalStageRoutingPostTrash)
        case .routingReverification: text(.removalStageRoutingReverification)
        case .npmUninstall: text(.removalStageNPMUninstall)
        case .routingPostVerification: text(.removalStageRoutingPostVerification)
        case .packageVerification: text(.removalStagePackageVerification)
        case .routingFinalVerification: text(.removalStageRoutingFinalVerification)
        case .routingRecovery: text(.removalStageRoutingRecovery)
        case .relayCleanup: text(.removalStageRelayCleanup)
        }
    }

    func displayName(_ value: OpenCodexNativeRemovalStageName) -> String {
        switch value {
        case .requestValidation: text(.removalStageRequestValidation)
        case .candidateRevalidation: text(.removalStageCandidateRevalidation)
        case .dataPolicy: text(.removalStageDataPolicy)
        case .teardownPreflight: text(.removalStageTeardownPreflight)
        case .cleanupJournal: text(.removalStageCleanupJournal)
        case .nativeBoundaryPreTeardown: text(.removalStageNativeBoundaryPreTeardown)
        case .teardown: text(.removalStageTeardown)
        case .nativeRestore: text(.removalStageNativeRestore)
        case .nativeBoundaryVerification: text(.removalStageNativeBoundaryVerification)
        case .nativeBoundaryPreTrash: text(.removalStageNativeBoundaryPreTrash)
        case .dataTrash: text(.removalStageDataTrash)
        case .nativeBoundaryPostTrash: text(.removalStageNativeBoundaryPostTrash)
        case .nativeBoundaryReverification: text(.removalStageNativeBoundaryReverification)
        case .npmUninstall: text(.removalStageNPMUninstall)
        case .packageVerification: text(.removalStagePackageVerification)
        case .nativeBoundaryFinalVerification: text(.removalStageNativeBoundaryFinalVerification)
        case .nativeRecovery: text(.removalStageNativeRecovery)
        case .cleanupJournalRetained: text(.removalStageCleanupJournalRetained)
        }
    }

    func displayName(_ value: OpenCodexRemovalStageStatus) -> String {
        switch value {
        case .completed: text(.removalStageStatusCompleted)
        case .skipped: text(.removalStageStatusSkipped)
        case .refused: text(.removalStageStatusRefused)
        case .failed: text(.removalStageStatusFailed)
        }
    }

    func message(_ error: RoutingBindingError) -> String {
        switch error {
        case .missing: text(.bindingMissing)
        case .unsafeFile: text(.bindingUnsafe)
        case .malformed: text(.bindingInvalid)
        }
    }

    func message(_ error: RelayctlError) -> String {
        switch error {
        case .helperUnavailable: text(.relayctlUnavailable)
        case .invocationFailed: text(.relayctlFailed)
        case .reported: text(.relayctlFailed)
        case .invalidJSON, .invalidStatus: text(.relayctlInvalidStatus)
        case .launchFailed: text(.relayctlLaunchFailed)
        case .timedOut: text(.relayctlTimedOut)
        case .cancelled: text(.relayctlCancelled)
        case .outputTooLarge: text(.relayctlOutputTooLarge)
        }
    }

    func message(_ error: OpenCodexExecutableError) -> String {
        switch error {
        case .invalidSelection: text(.ocxInvalid)
        case .unavailable: text(.ocxUnavailable)
        case .tooLarge: text(.ocxTooLarge)
        case .changed: text(.ocxChanged)
        }
    }

    private func presentationKey(_ value: RoutingPresentation, suffix: String) -> AppStringKey {
        switch (value, suffix) {
        case (.externalReady, "title"): .presentationExternalReadyTitle
        case (.localOpenCodexReady, "title"): .presentationLocalReadyTitle
        case (.nativePending, "title"): .presentationNativePendingTitle
        case (.externalPending, "title"): .presentationExternalPendingTitle
        case (.localOpenCodexPending, "title"): .presentationLocalPendingTitle
        case (.routingSyncPending, "title"): .presentationSyncPendingTitle
        case (.relayDegraded, "title"): .presentationDegradedTitle
        case (.switching, "title"): .presentationSwitchingTitle
        case (.nativeParked, "title"): .presentationNativeParkedTitle
        case (.recoveryRequired, "title"): .presentationRecoveryTitle
        case (.localOpenCodexUnavailable, "title"): .presentationLocalUnavailableTitle
        case (.relayUnavailable, "title"): .presentationRelayUnavailableTitle
        case (.externalReady, "compact"): .presentationExternalReadyCompact
        case (.localOpenCodexReady, "compact"): .presentationLocalReadyCompact
        case (.nativePending, "compact"): .presentationNativePendingCompact
        case (.externalPending, "compact"): .presentationExternalPendingCompact
        case (.localOpenCodexPending, "compact"): .presentationLocalPendingCompact
        case (.routingSyncPending, "compact"): .presentationSyncPendingCompact
        case (.relayDegraded, "compact"): .presentationDegradedCompact
        case (.switching, "compact"): .presentationSwitchingCompact
        case (.nativeParked, "compact"): .presentationNativeParkedCompact
        case (.recoveryRequired, "compact"): .presentationRecoveryCompact
        case (.localOpenCodexUnavailable, "compact"): .presentationLocalUnavailableCompact
        case (.relayUnavailable, "compact"): .presentationRelayUnavailableCompact
        case (.externalReady, "accessibility"): .presentationExternalReadyAccessibility
        case (.localOpenCodexReady, "accessibility"): .presentationLocalReadyAccessibility
        case (.nativePending, "accessibility"): .presentationNativePendingAccessibility
        case (.externalPending, "accessibility"): .presentationExternalPendingAccessibility
        case (.localOpenCodexPending, "accessibility"): .presentationLocalPendingAccessibility
        case (.routingSyncPending, "accessibility"): .presentationSyncPendingAccessibility
        case (.relayDegraded, "accessibility"): .presentationDegradedAccessibility
        case (.switching, "accessibility"): .presentationSwitchingAccessibility
        case (.nativeParked, "accessibility"): .presentationNativeParkedAccessibility
        case (.recoveryRequired, "accessibility"): .presentationRecoveryAccessibility
        case (.localOpenCodexUnavailable, "accessibility"): .presentationLocalUnavailableAccessibility
        case (.relayUnavailable, "accessibility"): .presentationRelayUnavailableAccessibility
        default:
            preconditionFailure("unsupported routing presentation localization key")
        }
    }
}
