import AppKit
import CoreServices
import Foundation
import OpenCodexRelayCore

enum ReleaseUpdateStageState: Equatable, Sendable {
    case idle
    case staging
    case ready
    case preparingFinderHandoff
    case awaitingQuit
    case failed
}

protocol ReleaseUpdateQuarantining: Sendable {
    func applyAndVerify(
        stagedApplicationURL: URL,
        selection: ReleaseUpdateCheckResult
    ) throws
}

struct SystemReleaseUpdateQuarantine: ReleaseUpdateQuarantining, @unchecked Sendable {
    func applyAndVerify(
        stagedApplicationURL: URL,
        selection: ReleaseUpdateCheckResult
    ) throws {
        guard let tag = selection.tag,
              let releaseURL = selection.canonicalReleaseURL,
              let dataURL = URL(
                string: "https://github.com/novelKR/OpenCodex-OCI-Gateway/releases/download/\(tag)/OpenCodexRelay.app.zip"
              ) else {
            throw ReleaseUpdateContractError.invalidStage
        }
        let typeKey = kLSQuarantineTypeKey as String
        let agentKey = kLSQuarantineAgentNameKey as String
        let agentBundleKey = kLSQuarantineAgentBundleIdentifierKey as String
        let originKey = kLSQuarantineOriginURLKey as String
        let dataKey = kLSQuarantineDataURLKey as String
        let timestampKey = kLSQuarantineTimeStampKey as String
        let expected: [String: Any] = [
            typeKey: kLSQuarantineTypeWebDownload as String,
            agentKey: "OpenCodexRelay",
            agentBundleKey: "io.github.novelkr.opencodex-relay",
            originKey: releaseURL,
            dataKey: dataURL,
            timestampKey: Date(),
        ]
        var values = URLResourceValues()
        values.quarantineProperties = expected
        var mutableURL = stagedApplicationURL
        try mutableURL.setResourceValues(values)
        let readBack = try stagedApplicationURL.resourceValues(
            forKeys: [.quarantinePropertiesKey]
        ).quarantineProperties
        guard let readBack,
              readBack[typeKey] as? String == kLSQuarantineTypeWebDownload as String,
              readBack[agentKey] as? String == "OpenCodexRelay",
              readBack[agentBundleKey] as? String == "io.github.novelkr.opencodex-relay",
              readBack[timestampKey] is Date,
              let eventIdentifier = readBack["LSQuarantineEventIdentifier"] as? String,
              !eventIdentifier.isEmpty,
              eventIdentifier.count <= 128,
              readBack["LSQuarantineIsOwnedByCurrentUser"] as? Bool == true else {
            throw ReleaseUpdateContractError.invalidStage
        }
    }
}

@MainActor
protocol ReleaseUpdateFinderHandingOff: AnyObject {
    func reveal(_ applicationURL: URL)
    func terminateCurrentApplication()
}

@MainActor
final class SystemReleaseUpdateFinderHandoff: ReleaseUpdateFinderHandingOff {
    func reveal(_ applicationURL: URL) {
        NSWorkspace.shared.activateFileViewerSelecting([applicationURL])
    }

    func terminateCurrentApplication() {
        NSApplication.shared.terminate(nil)
    }
}

@MainActor
final class ReleaseUpdateController: ObservableObject {
    static let firstCheckMinimumDelay: TimeInterval = 5 * 60
    static let firstCheckMaximumDelay: TimeInterval = 15 * 60
    static let recurringCheckInterval: TimeInterval = 24 * 60 * 60

    @Published private(set) var channel: ReleaseUpdateChannel
    @Published private(set) var automaticChecksEnabled: Bool
    @Published private(set) var status: ReleaseUpdateStatus?
    @Published private(set) var lastCheckedAt: Date?
    @Published private(set) var candidateVersion: String?
    @Published private(set) var releaseURL: URL?
    @Published private(set) var isChecking = false
    @Published private(set) var lastCheckFailed = false
    @Published private(set) var stageState: ReleaseUpdateStageState = .idle
    @Published private(set) var stagedApplicationURL: URL?

    private enum DefaultsKey {
        static let channel = "release_update.channel"
        static let automaticChecksEnabled = "release_update.automatic_checks_enabled"
        static let lastCheckedAt = "release_update.last_checked_at"
        static let lastStatus = "release_update.last_status"
        static let candidateVersion = "release_update.candidate_version"
        static let releaseURL = "release_update.release_url"
        static let dismissedCandidateVersion = "release_update.dismissed_candidate_version"
    }

    private let checker: any ReleaseUpdateChecking
    private let stager: any ReleaseUpdateStaging
    private let quarantine: any ReleaseUpdateQuarantining
    private let finderHandoff: any ReleaseUpdateFinderHandingOff
    private let distributionFlavor: DistributionFlavor
    private let currentVersion: String
    private let publicKeyURL: URL
    private let defaults: UserDefaults
    private let now: @Sendable () -> Date
    private let jitter: @Sendable () -> TimeInterval
    private var dismissedCandidateVersion: String?
    private var candidateSelection: ReleaseUpdateCheckResult?
    private var stagedReceipt: ReleaseUpdateStageReceipt?
    private var scheduledCheck: Task<Void, Never>?
    private var started = false

    init(
        checker: any ReleaseUpdateChecking = ProcessReleaseUpdateChecker(),
        stager: any ReleaseUpdateStaging = ProcessReleaseUpdateStager(),
        quarantine: any ReleaseUpdateQuarantining = SystemReleaseUpdateQuarantine(),
        finderHandoff: any ReleaseUpdateFinderHandingOff = SystemReleaseUpdateFinderHandoff(),
        distributionFlavor: DistributionFlavor = .current,
        currentVersion: String = Bundle.main.object(
            forInfoDictionaryKey: "CFBundleShortVersionString"
        ) as? String ?? "dev",
        publicKeyURL: URL = ReleaseUpdateTrustKeyLocation.resolve(),
        defaults: UserDefaults = .standard,
        now: @escaping @Sendable () -> Date = Date.init,
        jitter: @escaping @Sendable () -> TimeInterval = {
            Double.random(in: 300...900)
        }
    ) {
        self.checker = checker
        self.stager = stager
        self.quarantine = quarantine
        self.finderHandoff = finderHandoff
        self.distributionFlavor = distributionFlavor
        self.currentVersion = currentVersion
        self.publicKeyURL = publicKeyURL
        self.defaults = defaults
        self.now = now
        self.jitter = jitter
        self.channel = ReleaseUpdateChannel(
            rawValue: defaults.string(forKey: DefaultsKey.channel) ?? ""
        ) ?? .stable
        self.automaticChecksEnabled = defaults.object(
            forKey: DefaultsKey.automaticChecksEnabled
        ) == nil ? true : defaults.bool(forKey: DefaultsKey.automaticChecksEnabled)
        self.lastCheckedAt = defaults.object(forKey: DefaultsKey.lastCheckedAt) as? Date
        self.status = defaults.string(forKey: DefaultsKey.lastStatus)
            .flatMap(ReleaseUpdateStatus.init(rawValue:))
        self.dismissedCandidateVersion = defaults.string(
            forKey: DefaultsKey.dismissedCandidateVersion
        )

        let storedCandidate = defaults.string(forKey: DefaultsKey.candidateVersion)
        let storedURL = defaults.string(forKey: DefaultsKey.releaseURL).flatMap(URL.init(string:))
        if let storedCandidate,
           let canonicalURL = ReleaseUpdateCheckResult.canonicalReleaseURL(
                version: storedCandidate,
                channel: channel
           ),
           storedURL == canonicalURL,
           status != .offline, status != .rateLimited, status != .invalidRelease {
            self.candidateVersion = storedCandidate
            self.releaseURL = canonicalURL
        } else {
            self.candidateVersion = nil
            self.releaseURL = nil
        }
    }

    deinit {
        scheduledCheck?.cancel()
    }

    var isUpdateBadgeVisible: Bool {
        status == .updateAvailable && candidateVersion != nil &&
            candidateVersion != dismissedCandidateVersion
    }

    var permitsAutomaticNetworkChecks: Bool {
        distributionFlavor == .production && automaticChecksEnabled
    }

    var canDownloadUpdate: Bool {
        distributionFlavor == .production && status == .updateAvailable &&
            candidateSelection != nil && (stageState == .idle || stageState == .failed)
    }

    func start() {
        guard !started else { return }
        started = true
        rescheduleAutomaticCheck()
    }

    func setChannel(_ value: ReleaseUpdateChannel) {
        guard channel != value else { return }
        channel = value
        defaults.set(value.rawValue, forKey: DefaultsKey.channel)
        clearCandidate()
        status = nil
        lastCheckedAt = nil
        defaults.removeObject(forKey: DefaultsKey.lastStatus)
        defaults.removeObject(forKey: DefaultsKey.lastCheckedAt)
        if started { rescheduleAutomaticCheck() }
    }

    func setAutomaticChecksEnabled(_ value: Bool) {
        guard automaticChecksEnabled != value else { return }
        automaticChecksEnabled = value
        defaults.set(value, forKey: DefaultsKey.automaticChecksEnabled)
        if started { rescheduleAutomaticCheck() }
    }

    func checkNow() {
        Task { await performCheck(manual: true) }
    }

    func downloadUpdate() {
        Task { await performStage() }
    }

    func performStage() async {
        guard canDownloadUpdate, let selection = candidateSelection else { return }
        stageState = .staging
        stagedApplicationURL = nil
        stagedReceipt = nil
        do {
            let receipt = try await stager.stage(
                selection: selection,
                currentVersion: currentVersion,
                publicKeyURL: publicKeyURL
            )
            guard candidateSelection == selection, stageState == .staging else { return }
            stagedReceipt = receipt
            stagedApplicationURL = receipt.stagedApplicationURL
            stageState = .ready
        } catch {
            if candidateSelection == selection, stageState == .staging {
                stageState = .failed
            }
        }
    }

    func prepareFinderHandoff() {
        Task { await performFinderHandoff() }
    }

    func performFinderHandoff() async {
        guard stageState == .ready,
              let selection = candidateSelection,
              stagedReceipt != nil else { return }
        stageState = .preparingFinderHandoff
        do {
            let reverified = try await stager.stage(
                selection: selection,
                currentVersion: currentVersion,
                publicKeyURL: publicKeyURL
            )
            guard candidateSelection == selection,
                  stageState == .preparingFinderHandoff else { return }
            try quarantine.applyAndVerify(
                stagedApplicationURL: reverified.stagedApplicationURL,
                selection: selection
            )
            stagedReceipt = reverified
            stagedApplicationURL = reverified.stagedApplicationURL
            finderHandoff.reveal(reverified.stagedApplicationURL)
            stageState = .awaitingQuit
        } catch {
            if candidateSelection == selection, stageState == .preparingFinderHandoff {
                stageState = .failed
            }
        }
    }

    func confirmQuitForFinderInstall() {
        guard stageState == .awaitingQuit else { return }
        finderHandoff.terminateCurrentApplication()
    }

    func performCheck(manual: Bool) async {
        guard !isChecking else { return }
        if manual {
            dismissedCandidateVersion = nil
            defaults.removeObject(forKey: DefaultsKey.dismissedCandidateVersion)
        } else if !permitsAutomaticNetworkChecks {
            return
        }
        isChecking = true
        lastCheckFailed = false
        defer { isChecking = false }
        do {
            let result = try await checker.check(
                channel: channel,
                currentVersion: currentVersion,
                publicKeyURL: publicKeyURL
            )
            guard result.channel == channel, result.currentVersion == currentVersion else {
                throw ReleaseUpdateContractError.invalidSchema
            }
            apply(result)
        } catch {
            lastCheckFailed = true
            lastCheckedAt = now()
            defaults.set(lastCheckedAt, forKey: DefaultsKey.lastCheckedAt)
        }
        if manual, started {
            rescheduleAutomaticCheck()
        }
    }

    func dismissBadge() {
        guard let candidateVersion else { return }
        dismissedCandidateVersion = candidateVersion
        defaults.set(candidateVersion, forKey: DefaultsKey.dismissedCandidateVersion)
        objectWillChange.send()
    }

    static func nextAutomaticDelay(
        distributionFlavor: DistributionFlavor,
        enabled: Bool,
        lastCheckedAt: Date?,
        now: Date,
        firstCheckDelay: TimeInterval
    ) -> TimeInterval? {
        guard distributionFlavor == .production, enabled else { return nil }
        let boundedFirstDelay = min(
            max(firstCheckDelay, firstCheckMinimumDelay),
            firstCheckMaximumDelay
        )
        guard let lastCheckedAt else { return boundedFirstDelay }
        let remaining = lastCheckedAt.addingTimeInterval(recurringCheckInterval)
            .timeIntervalSince(now)
        return remaining > 0 ? remaining : boundedFirstDelay
    }

    private func apply(_ result: ReleaseUpdateCheckResult) {
        let previousCandidate = candidateVersion
        let previousManifest = candidateSelection?.manifestSHA256
        status = result.status
        lastCheckedAt = ISO8601DateFormatter().date(from: result.checkedAt) ?? now()
        candidateVersion = result.candidateVersion
        releaseURL = result.canonicalReleaseURL
        candidateSelection = result.status == .updateAvailable && result.trustKeyID != nil
            ? result
            : nil
        if candidateVersion != previousCandidate || result.manifestSHA256 != previousManifest {
            clearStagedUpdate()
        }
        if candidateVersion != previousCandidate {
            dismissedCandidateVersion = nil
            defaults.removeObject(forKey: DefaultsKey.dismissedCandidateVersion)
        }
        defaults.set(lastCheckedAt, forKey: DefaultsKey.lastCheckedAt)
        defaults.set(result.status.rawValue, forKey: DefaultsKey.lastStatus)
        if let candidateVersion, let releaseURL {
            defaults.set(candidateVersion, forKey: DefaultsKey.candidateVersion)
            defaults.set(releaseURL.absoluteString, forKey: DefaultsKey.releaseURL)
        } else {
            defaults.removeObject(forKey: DefaultsKey.candidateVersion)
            defaults.removeObject(forKey: DefaultsKey.releaseURL)
        }
    }

    private func clearCandidate() {
        candidateVersion = nil
        releaseURL = nil
        dismissedCandidateVersion = nil
        candidateSelection = nil
        clearStagedUpdate()
        defaults.removeObject(forKey: DefaultsKey.candidateVersion)
        defaults.removeObject(forKey: DefaultsKey.releaseURL)
        defaults.removeObject(forKey: DefaultsKey.dismissedCandidateVersion)
    }

    private func clearStagedUpdate() {
        stageState = .idle
        stagedApplicationURL = nil
        stagedReceipt = nil
    }

    private func rescheduleAutomaticCheck() {
        scheduledCheck?.cancel()
        guard let delay = Self.nextAutomaticDelay(
            distributionFlavor: distributionFlavor,
            enabled: automaticChecksEnabled,
            lastCheckedAt: lastCheckedAt,
            now: now(),
            firstCheckDelay: jitter()
        ) else {
            scheduledCheck = nil
            return
        }
        scheduledCheck = Task { [weak self] in
            do {
                try await Task.sleep(for: .seconds(delay))
            } catch {
                return
            }
            guard let self, !Task.isCancelled else { return }
            await self.performCheck(manual: false)
            guard !Task.isCancelled else { return }
            self.scheduleRecurringCheck()
        }
    }

    private func scheduleRecurringCheck() {
        scheduledCheck?.cancel()
        guard permitsAutomaticNetworkChecks else { return }
        scheduledCheck = Task { [weak self] in
            do {
                try await Task.sleep(for: .seconds(Self.recurringCheckInterval))
            } catch {
                return
            }
            guard let self, !Task.isCancelled else { return }
            await self.performCheck(manual: false)
            guard !Task.isCancelled else { return }
            self.scheduleRecurringCheck()
        }
    }
}
