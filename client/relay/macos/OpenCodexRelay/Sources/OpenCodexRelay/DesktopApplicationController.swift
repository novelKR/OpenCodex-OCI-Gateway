import AppKit
import Foundation
import OpenCodexRelayCore

@MainActor
protocol DesktopApplicationLaunchObserving: AnyObject {
    var events: AsyncStream<Void> { get }
    var didObserveLaunch: Bool { get }
    var observedProcessIdentifiers: Set<Int32> { get }
    func stop()
}

struct DesktopApplicationRelaunchWitness: Equatable, Sendable {
    let processIdentifier: Int32
}

private enum DesktopApplicationRelaunchError: Error {
    case invalidApplication
}

// The MenuBar model depends on this narrow, exact-app controller boundary so
// its quit/timeout/relaunch behavior can be exercised without controlling a
// real Desktop application in XCTest.  Implementations must never broaden the
// selected target into a bundle-ID or PATH lookup.
@MainActor
protocol DesktopApplicationControlling: AnyObject {
    func isRunning(at target: URL) -> Bool
    func requestGracefulQuit(at target: URL) -> Bool
    func waitForExit(at target: URL, timeout: TimeInterval) async -> Bool
    func beginLaunchObservation(at target: URL) -> any DesktopApplicationLaunchObserving
    func relaunch(at target: URL) async throws
    func launchNewInstance(at target: URL) async throws -> DesktopApplicationRelaunchWitness
    func validateRelaunch(_ witness: DesktopApplicationRelaunchWitness, at target: URL) -> Bool
}

@MainActor
final class DesktopApplicationController: DesktopApplicationControlling {
    private func normalized(_ url: URL) -> URL {
        url.resolvingSymlinksInPath().standardizedFileURL
    }

    private func matchingApplications(for target: URL) -> [NSRunningApplication] {
        let expected = normalized(target)
        return NSWorkspace.shared.runningApplications.filter { application in
            guard let bundleURL = application.bundleURL else { return false }
            return normalized(bundleURL) == expected
        }
    }

    func isRunning(at target: URL) -> Bool {
        !matchingApplications(for: target).isEmpty
    }

    /// Sends the normal application termination request only to the app bundle
    /// selected by the user. A refusal is not escalated to force termination.
    func requestGracefulQuit(at target: URL) -> Bool {
        let applications = matchingApplications(for: target)
        return applications.allSatisfy { $0.terminate() }
    }

    func waitForExit(at target: URL, timeout: TimeInterval = 30) async -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while isRunning(at: target) {
            guard Date() < deadline else { return false }
            try? await Task.sleep(for: .milliseconds(250))
        }
        return true
    }

    func beginLaunchObservation(at target: URL) -> any DesktopApplicationLaunchObserving {
        WorkspaceDesktopLaunchObservation(target: normalized(target))
    }

    func relaunch(at target: URL) async throws {
        let configuration = NSWorkspace.OpenConfiguration()
        configuration.activates = false
        try await NSWorkspace.shared.openApplication(at: target, configuration: configuration)
    }

    /// The runtime lifecycle path has already established that the selected
    /// Desktop is absent. It alone needs a non-substituted PID witness so an
    /// unrelated launch cannot be mistaken for the post-mutation relaunch.
    func launchNewInstance(at target: URL) async throws -> DesktopApplicationRelaunchWitness {
        let configuration = NSWorkspace.OpenConfiguration()
        configuration.activates = false
        configuration.createsNewApplicationInstance = true
        configuration.allowsRunningApplicationSubstitution = false
        let application = try await NSWorkspace.shared.openApplication(
            at: target,
            configuration: configuration
        )
        guard let bundleURL = application.bundleURL,
              normalized(bundleURL) == normalized(target),
              application.processIdentifier > 0,
              !application.isTerminated else {
            throw DesktopApplicationRelaunchError.invalidApplication
        }
        return DesktopApplicationRelaunchWitness(
            processIdentifier: Int32(application.processIdentifier)
        )
    }

    func validateRelaunch(
        _ witness: DesktopApplicationRelaunchWitness,
        at target: URL
    ) -> Bool {
        guard witness.processIdentifier > 0 else { return false }
        let matches = matchingApplications(for: target)
        return matches.count == 1 &&
            Int32(matches[0].processIdentifier) == witness.processIdentifier &&
            !matches[0].isTerminated
    }
}

@MainActor
private final class WorkspaceDesktopLaunchObservation: DesktopApplicationLaunchObserving {
    let events: AsyncStream<Void>
    private(set) var observedProcessIdentifiers: Set<Int32> = []
    var didObserveLaunch: Bool { !observedProcessIdentifiers.isEmpty }

    private let notificationCenter: NotificationCenter
    private var continuation: AsyncStream<Void>.Continuation?
    private var observer: NSObjectProtocol?

    init(target: URL) {
        let targetPath = target.path
        notificationCenter = NSWorkspace.shared.notificationCenter
        var streamContinuation: AsyncStream<Void>.Continuation?
        events = AsyncStream(bufferingPolicy: .bufferingNewest(1)) {
            streamContinuation = $0
        }
        continuation = streamContinuation
        observer = notificationCenter.addObserver(
            forName: NSWorkspace.didLaunchApplicationNotification,
            object: nil,
            queue: .main
        ) { [weak self] notification in
            guard let application = notification.userInfo?[NSWorkspace.applicationUserInfoKey]
                    as? NSRunningApplication,
                  let bundleURL = application.bundleURL,
                  bundleURL.resolvingSymlinksInPath().standardizedFileURL.path == targetPath else {
                return
            }
            MainActor.assumeIsolated {
                self?.recordLaunch(processIdentifier: Int32(application.processIdentifier))
            }
        }
    }

    func stop() {
        if let observer {
            notificationCenter.removeObserver(observer)
            self.observer = nil
        }
        continuation?.finish()
        continuation = nil
    }

    private func recordLaunch(processIdentifier: Int32) {
        guard processIdentifier > 0 else { return }
        observedProcessIdentifiers.insert(processIdentifier)
        continuation?.yield(())
    }
}
