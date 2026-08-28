import Foundation

public enum BundledRelayComponentKind: String, CaseIterable, Identifiable, Sendable {
    case relay
    case relayctl

    public var id: String { rawValue }

    public var executableName: String {
        switch self {
        case .relay: "opencodex-relay"
        case .relayctl: "opencodex-relayctl"
        }
    }

    fileprivate var versionArguments: [String] {
        switch self {
        case .relay: ["--version"]
        case .relayctl: ["version"]
        }
    }
}

public enum BundledRelayComponentAvailability: String, Sendable, Equatable {
    case loading
    case available
    case missing
    case unverified
}

public struct BundledRelayComponentInformation: Identifiable, Sendable, Equatable {
    public let kind: BundledRelayComponentKind
    public let availability: BundledRelayComponentAvailability
    public let version: String?
    public let architecture: String

    public var id: BundledRelayComponentKind { kind }

    public init(
        kind: BundledRelayComponentKind,
        availability: BundledRelayComponentAvailability,
        version: String?,
        architecture: String
    ) {
        self.kind = kind
        self.availability = availability
        self.version = version
        self.architecture = architecture
    }
}

public struct AppInformationSnapshot: Sendable, Equatable {
    public let displayName: String
    public let version: String?
    public let build: String?
    public let bundleIdentifier: String?
    public let distributionFlavor: DistributionFlavor
    public let minimumSystemVersion: String?
    public let architecture: String
    public let components: [BundledRelayComponentInformation]

    public init(
        displayName: String,
        version: String?,
        build: String?,
        bundleIdentifier: String?,
        distributionFlavor: DistributionFlavor,
        minimumSystemVersion: String?,
        architecture: String,
        components: [BundledRelayComponentInformation]
    ) {
        self.displayName = displayName
        self.version = version
        self.build = build
        self.bundleIdentifier = bundleIdentifier
        self.distributionFlavor = distributionFlavor
        self.minimumSystemVersion = minimumSystemVersion
        self.architecture = architecture
        self.components = components
    }
}

public struct AppInformationReader: Sendable {
    private let displayName: String
    private let version: String?
    private let build: String?
    private let bundleIdentifier: String?
    private let distributionFlavor: DistributionFlavor
    private let minimumSystemVersion: String?
    private let bundleURL: URL
    private let executionPolicy: RelayctlExecutionPolicy

    public init(bundle: Bundle = .main) {
        let bundleIdentifier = bundle.bundleIdentifier
        let declaredFlavor = bundle.object(forInfoDictionaryKey: "OpenCodexDistributionFlavor") as? String
        self.init(
            displayName: (bundle.object(forInfoDictionaryKey: "CFBundleDisplayName") as? String)
                ?? (bundle.object(forInfoDictionaryKey: "CFBundleName") as? String)
                ?? "PW OpenCodex Relay",
            version: bundle.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String,
            build: bundle.object(forInfoDictionaryKey: "CFBundleVersion") as? String,
            bundleIdentifier: bundleIdentifier,
            distributionFlavor: DistributionFlavor.from(
                bundleIdentifier: bundleIdentifier,
                declaredFlavor: declaredFlavor
            ),
            minimumSystemVersion: bundle.object(forInfoDictionaryKey: "LSMinimumSystemVersion") as? String,
            bundleURL: bundle.bundleURL
        )
    }

    public init(
        displayName: String,
        version: String?,
        build: String?,
        bundleIdentifier: String?,
        distributionFlavor: DistributionFlavor,
        minimumSystemVersion: String?,
        bundleURL: URL,
        executionPolicy: RelayctlExecutionPolicy = RelayctlExecutionPolicy(
            timeout: 2,
            terminationGracePeriod: 0.25,
            maximumOutputBytes: 1_024
        )
    ) {
        self.displayName = displayName
        self.version = Self.validMetadataValue(version)
        self.build = Self.validMetadataValue(build)
        self.bundleIdentifier = Self.validMetadataValue(bundleIdentifier)
        self.distributionFlavor = distributionFlavor
        self.minimumSystemVersion = Self.validMetadataValue(minimumSystemVersion)
        self.bundleURL = bundleURL.standardizedFileURL.resolvingSymlinksInPath()
        self.executionPolicy = executionPolicy
    }

    public var loadingSnapshot: AppInformationSnapshot {
        snapshot(
            components: BundledRelayComponentKind.allCases.map {
                BundledRelayComponentInformation(
                    kind: $0,
                    availability: .loading,
                    version: nil,
                    architecture: Self.currentArchitecture
                )
            }
        )
    }

    public func load() async -> AppInformationSnapshot {
        async let relay = loadComponent(.relay)
        async let relayctl = loadComponent(.relayctl)
        return await snapshot(components: [relay, relayctl])
    }

    private func snapshot(
        components: [BundledRelayComponentInformation]
    ) -> AppInformationSnapshot {
        AppInformationSnapshot(
            displayName: displayName,
            version: version,
            build: build,
            bundleIdentifier: bundleIdentifier,
            distributionFlavor: distributionFlavor,
            minimumSystemVersion: minimumSystemVersion,
            architecture: Self.currentArchitecture,
            components: components
        )
    }

    private func loadComponent(
        _ kind: BundledRelayComponentKind
    ) async -> BundledRelayComponentInformation {
        let executableURL = bundleURL
            .appendingPathComponent("Contents/Library/Helpers", isDirectory: true)
            .appendingPathComponent(kind.executableName, isDirectory: false)

        guard Self.isBundledExecutable(executableURL) else {
            return component(kind, availability: .missing, version: nil)
        }

        let operation = RelayctlProcessOperation(
            executableURL: executableURL,
            arguments: kind.versionArguments,
            policy: executionPolicy
        )
        do {
            let result = try await withTaskCancellationHandler {
                try await Task.detached(priority: .utility) {
                    try operation.run()
                }.value
            } onCancel: {
                operation.cancel()
            }
            guard !Task.isCancelled,
                  result.exitCode == 0,
                  let reportedVersion = Self.validVersionOutput(result.stdout) else {
                return component(kind, availability: .unverified, version: nil)
            }
            return component(kind, availability: .available, version: reportedVersion)
        } catch {
            return component(kind, availability: .unverified, version: nil)
        }
    }

    private func component(
        _ kind: BundledRelayComponentKind,
        availability: BundledRelayComponentAvailability,
        version: String?
    ) -> BundledRelayComponentInformation {
        BundledRelayComponentInformation(
            kind: kind,
            availability: availability,
            version: version,
            architecture: Self.currentArchitecture
        )
    }

    private static func isBundledExecutable(_ url: URL) -> Bool {
        guard url.isFileURL,
              let values = try? url.resourceValues(forKeys: [.isRegularFileKey, .isSymbolicLinkKey]),
              values.isRegularFile == true,
              values.isSymbolicLink != true else {
            return false
        }
        return FileManager.default.isExecutableFile(atPath: url.path)
    }

    private static func validVersionOutput(_ data: Data) -> String? {
        guard data.count <= 1_024,
              let output = String(data: data, encoding: .utf8) else {
            return nil
        }
        let value = output.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !value.isEmpty,
              value.utf8.count <= 128,
              !value.contains("\n"),
              !value.contains("\r") else {
            return nil
        }
        let pattern = #"^(?:dev|[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?)$"#
        return value.range(of: pattern, options: .regularExpression) == nil ? nil : value
    }

    private static func validMetadataValue(_ value: String?) -> String? {
        guard let value else { return nil }
        let trimmed = value.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty,
              trimmed.utf8.count <= 256,
              !trimmed.contains("\n"),
              !trimmed.contains("\r") else {
            return nil
        }
        return trimmed
    }

    private static var currentArchitecture: String {
        #if arch(arm64)
        return "arm64"
        #elseif arch(x86_64)
        return "x86_64"
        #else
        return "unknown"
        #endif
    }
}
