import Foundation

/// A bounded bundle flavor. Distribution describes provenance and namespace;
/// whether the running bundle may use an installed Relay is modeled separately
/// by `RelayRuntimeMode`.
public enum DistributionFlavor: String, Sendable, Equatable {
    case production
    case localDevelopment = "local_development"

    public static let developmentBundleIdentifier = "io.github.novelkr.opencodex-relay.dev"

    public static var current: DistributionFlavor {
        from(
            bundleIdentifier: Bundle.main.bundleIdentifier,
            declaredFlavor: Bundle.main.object(forInfoDictionaryKey: "OpenCodexDistributionFlavor") as? String
        )
    }

    public static func from(bundleIdentifier: String?, declaredFlavor: String?) -> DistributionFlavor {
        if bundleIdentifier == developmentBundleIdentifier || declaredFlavor == localDevelopment.rawValue {
            return .localDevelopment
        }
        return .production
    }

    public var routingBindingRelativePath: String {
        switch self {
        case .production:
            return "Library/Application Support/OpenCodexRelay/routing-binding.json"
        case .localDevelopment:
            return "Library/Application Support/OpenCodexRelayDev/routing-binding.json"
        }
    }

    public var isLocalDevelopment: Bool { self == .localDevelopment }

    @available(*, deprecated, renamed: "isLocalDevelopment")
    public var isUnsignedLocalDevelopment: Bool { isLocalDevelopment }
}
