import Darwin
import Foundation

/// Runtime capability is deliberately independent from distribution
/// provenance. A preview bundle is complete enough for UI review, but it must
/// never consume an installed binding or control a resident Relay.
public enum RelayRuntimeMode: String, Equatable, Sendable {
    case managed
    case preview

    public static var current: Self {
        from(
            declaredMode: Bundle.main.object(
                forInfoDictionaryKey: "OpenCodexRuntimeMode"
            ) as? String
        )
    }

    /// Older managed bundles do not declare the key. Unknown values fail
    /// closed as preview rather than enabling system integration.
    public static func from(declaredMode: String?) -> Self {
        guard let declaredMode else { return .managed }
        return Self(rawValue: declaredMode) ?? .preview
    }
}

public enum RelayIntegrationAvailability: String, Equatable, Sendable {
    case preview
    case ready
    case missing
    case unsafe
    case invalid
    case helperUnavailable = "helper_unavailable"

    public var safeCode: String {
        switch self {
        case .preview: "preview_mode"
        case .ready: "ready"
        case .missing: RoutingBindingError.missing.safeCode
        case .unsafe: RoutingBindingError.unsafeFile.safeCode
        case .invalid: RoutingBindingError.malformed.safeCode
        case .helperUnavailable: "relayctl_unavailable"
        }
    }

    public var permitsManagedOperations: Bool { self == .ready }
}

public enum RelayIntegrationInspector {
    public static func inspect(
        runtimeMode: RelayRuntimeMode,
        bindingURL: URL,
        helperURL: URL
    ) -> RelayIntegrationAvailability {
        guard runtimeMode == .managed else { return .preview }
        do {
            _ = try RoutingBindingReader.load(at: bindingURL)
        } catch let error as RoutingBindingError {
            switch error {
            case .missing: return .missing
            case .unsafeFile: return .unsafe
            case .malformed: return .invalid
            }
        } catch {
            return .invalid
        }

        guard isExecutableRegularFile(helperURL) else {
            return .helperUnavailable
        }
        return .ready
    }

    private static func isExecutableRegularFile(_ url: URL) -> Bool {
        guard url.isFileURL else { return false }
        var info = stat()
        return Darwin.lstat(url.path, &info) == 0 &&
            (info.st_mode & S_IFMT) == S_IFREG &&
            (info.st_mode & mode_t(0o111)) != 0
    }
}
