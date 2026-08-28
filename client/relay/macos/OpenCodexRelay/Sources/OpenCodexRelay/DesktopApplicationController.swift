import AppKit
import Foundation
import OpenCodexRelayCore

// The MenuBar model depends on this narrow, exact-app controller boundary so
// its quit/timeout/relaunch behavior can be exercised without controlling a
// real Desktop application in XCTest.  Implementations must never broaden the
// selected target into a bundle-ID or PATH lookup.
@MainActor
protocol DesktopApplicationControlling: AnyObject {
    func isRunning(at target: URL) -> Bool
    func requestGracefulQuit(at target: URL) -> Bool
    func waitForExit(at target: URL, timeout: TimeInterval) async -> Bool
    func relaunch(at target: URL) async throws
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

    func relaunch(at target: URL) async throws {
        let configuration = NSWorkspace.OpenConfiguration()
        configuration.activates = false
        try await NSWorkspace.shared.openApplication(at: target, configuration: configuration)
    }
}
