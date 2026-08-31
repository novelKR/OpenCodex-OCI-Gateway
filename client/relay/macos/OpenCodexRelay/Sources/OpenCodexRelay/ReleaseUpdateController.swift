import Foundation
import OpenCodexRelayCore

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
    private let distributionFlavor: DistributionFlavor
    private let currentVersion: String
    private let publicKeyURL: URL
    private let defaults: UserDefaults
    private let now: @Sendable () -> Date
    private let jitter: @Sendable () -> TimeInterval
    private var dismissedCandidateVersion: String?
    private var scheduledCheck: Task<Void, Never>?
    private var started = false

    init(
        checker: any ReleaseUpdateChecking = ProcessReleaseUpdateChecker(),
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
        status = result.status
        lastCheckedAt = ISO8601DateFormatter().date(from: result.checkedAt) ?? now()
        candidateVersion = result.candidateVersion
        releaseURL = result.canonicalReleaseURL
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
        defaults.removeObject(forKey: DefaultsKey.candidateVersion)
        defaults.removeObject(forKey: DefaultsKey.releaseURL)
        defaults.removeObject(forKey: DefaultsKey.dismissedCandidateVersion)
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
