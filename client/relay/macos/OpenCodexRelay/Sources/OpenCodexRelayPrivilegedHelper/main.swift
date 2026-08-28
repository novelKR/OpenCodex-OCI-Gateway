import Darwin
import Foundation
import Security
import OpenCodexRelayHomebrewGuard

private struct HelperLaunchConfiguration {
    let distribution: HomebrewGuardDistribution
    let machServiceName: String
    let clientRequirement: String
    let helperVersion: String

    static func parse(_ arguments: [String]) -> HelperLaunchConfiguration? {
        if arguments.count == 1, arguments[0] == "--version" {
            return nil
        }
        guard arguments.first == "--daemon" else { return nil }
        var values: [String: String] = [:]
        var index = 1
        while index < arguments.count {
            guard index + 1 < arguments.count,
                  arguments[index].hasPrefix("--"),
                  values[arguments[index]] == nil else {
                return nil
            }
            values[arguments[index]] = arguments[index + 1]
            index += 2
        }
        guard values.count == 4,
              let distributionValue = values["--distribution"],
              let distribution = HomebrewGuardDistribution(rawValue: distributionValue),
              let machServiceName = values["--mach-service"],
              let clientRequirement = values["--client-requirement"],
              let helperVersion = values["--helper-version"],
              validServiceName(machServiceName, distribution: distribution),
              validRequirement(clientRequirement),
              validVersion(helperVersion) else {
            return nil
        }
        return HelperLaunchConfiguration(
            distribution: distribution,
            machServiceName: machServiceName,
            clientRequirement: clientRequirement,
            helperVersion: helperVersion
        )
    }

    private static func validServiceName(
        _ value: String,
        distribution: HomebrewGuardDistribution
    ) -> Bool {
        switch distribution {
        case .production:
            return value == "io.github.novelkr.opencodex-relay.homebrew-guard"
        case .localDevelopment:
            return value == "io.github.novelkr.opencodex-relay.homebrew-guard.dev" ||
                value == "io.github.novelkr.opencodex-relay.homebrew-guard.manual.dev"
        }
    }

    private static func validRequirement(_ value: String) -> Bool {
        !value.isEmpty &&
            value.utf8.count <= 2_048 &&
            !value.contains("\0") &&
            !value.contains("\n") &&
            !value.contains("\r")
    }

    private static func validVersion(_ value: String) -> Bool {
        value == "dev" || value.range(
            of: #"^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$"#,
            options: .regularExpression
        ) != nil
    }
}

private final class HomebrewGuardXPCService: NSObject, HomebrewGuardXPCProtocol {
    private let engine: HomebrewGuardEngine
    private let peerUID: uid_t
    private let leaseID: UUID

    init(engine: HomebrewGuardEngine, peerUID: uid_t, leaseID: UUID) {
        self.engine = engine
        self.peerUID = peerUID
        self.leaseID = leaseID
    }

    func status(_ request: Data, withReply reply: @escaping (Data) -> Void) {
        reply(engine.perform(operation: .status, requestData: request, peerUID: peerUID, leaseID: leaseID))
    }

    func prepare(_ request: Data, withReply reply: @escaping (Data) -> Void) {
        reply(engine.perform(operation: .prepare, requestData: request, peerUID: peerUID, leaseID: leaseID))
    }

    func commit(_ request: Data, withReply reply: @escaping (Data) -> Void) {
        reply(engine.perform(operation: .commit, requestData: request, peerUID: peerUID, leaseID: leaseID))
    }

    func release(_ request: Data, withReply reply: @escaping (Data) -> Void) {
        reply(engine.perform(operation: .release, requestData: request, peerUID: peerUID, leaseID: leaseID))
    }

    func recover(_ request: Data, withReply reply: @escaping (Data) -> Void) {
        reply(engine.perform(operation: .recover, requestData: request, peerUID: peerUID, leaseID: leaseID))
    }
}

private final class HomebrewGuardListenerDelegate: NSObject, NSXPCListenerDelegate {
    private let engine: HomebrewGuardEngine
    private let clientRequirement: String

    init(engine: HomebrewGuardEngine, clientRequirement: String) {
        self.engine = engine
        self.clientRequirement = clientRequirement
    }

    func listener(
        _ listener: NSXPCListener,
        shouldAcceptNewConnection connection: NSXPCConnection
    ) -> Bool {
        guard connection.effectiveUserIdentifier != 0 else { return false }
        connection.setCodeSigningRequirement(clientRequirement)
        let peerUID = connection.effectiveUserIdentifier
        let leaseID = UUID()
        connection.exportedInterface = NSXPCInterface(with: HomebrewGuardXPCProtocol.self)
        connection.exportedObject = HomebrewGuardXPCService(
            engine: engine,
            peerUID: peerUID,
            leaseID: leaseID
        )
        let endLease = { [engine] in
            engine.connectionInvalidated(leaseID: leaseID, peerUID: peerUID)
        }
        connection.interruptionHandler = endLease
        connection.invalidationHandler = endLease
        connection.activate()
        return true
    }
}

@main
struct OpenCodexRelayPrivilegedHelperMain {
    static func main() {
        let arguments = Array(CommandLine.arguments.dropFirst())
        if arguments == ["--version"] {
            print(embeddedVersion())
            return
        }
        guard geteuid() == 0,
              let launch = HelperLaunchConfiguration.parse(arguments),
              let clientRequirement = resolvedClientRequirement(for: launch) else {
            exit(EX_USAGE)
        }
        let engine = HomebrewGuardEngine(
            configuration: HomebrewGuardEngineConfiguration(
                distribution: launch.distribution,
                helperVersion: launch.helperVersion
            )
        )
        let delegate = HomebrewGuardListenerDelegate(
            engine: engine,
            clientRequirement: clientRequirement
        )
        let listener = NSXPCListener(machServiceName: launch.machServiceName)
        listener.delegate = delegate
        listener.resume()
        dispatchMain()
    }
    private static func resolvedClientRequirement(
        for launch: HelperLaunchConfiguration
    ) -> String? {
        guard launch.clientRequirement == "embedded_app_cdhash" else {
            return validRequirementString(launch.clientRequirement)
                ? launch.clientRequirement
                : nil
        }
        guard launch.distribution == .localDevelopment,
              let executableURL = currentExecutableURL(),
              executableURL.lastPathComponent == "OpenCodexRelayPrivilegedHelper",
              executableURL.deletingLastPathComponent().lastPathComponent == "HelperTools",
              executableURL.deletingLastPathComponent()
                .deletingLastPathComponent().lastPathComponent == "Library" else {
            return nil
        }
        let contentsURL = executableURL.deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
        guard contentsURL.lastPathComponent == "Contents" else { return nil }
        let appURL = contentsURL.deletingLastPathComponent()
        var staticCode: SecStaticCode?
        guard SecStaticCodeCreateWithPath(appURL as CFURL, SecCSFlags(), &staticCode) == errSecSuccess,
              let staticCode,
              SecStaticCodeCheckValidity(
                  staticCode,
                  SecCSFlags(rawValue: kSecCSStrictValidate | kSecCSCheckAllArchitectures),
                  nil
              ) == errSecSuccess else {
            return nil
        }
        var signingInformation: CFDictionary?
        guard SecCodeCopySigningInformation(
                  staticCode,
                  SecCSFlags(rawValue: kSecCSSigningInformation),
                  &signingInformation
              ) == errSecSuccess,
              let values = signingInformation as? [String: Any],
              values[kSecCodeInfoIdentifier as String] as? String ==
                "io.github.novelkr.opencodex-relay.dev",
              let cdHash = values[kSecCodeInfoUnique as String] as? Data,
              cdHash.count >= 20,
              cdHash.count <= 64 else {
            return nil
        }
        let requirement = "cdhash H\"" +
            cdHash.map { String(format: "%02x", $0) }.joined() + "\""
        return validRequirementString(requirement) ? requirement : nil
    }

    private static func validRequirementString(_ value: String) -> Bool {
        var requirement: SecRequirement?
        return SecRequirementCreateWithString(
            value as CFString,
            SecCSFlags(),
            &requirement
        ) == errSecSuccess
    }

    private static func currentExecutableURL() -> URL? {
        var size: UInt32 = 0
        _ = _NSGetExecutablePath(nil, &size)
        guard size > 0, size <= 16_384 else { return nil }
        var buffer = [CChar](repeating: 0, count: Int(size))
        guard _NSGetExecutablePath(&buffer, &size) == 0 else { return nil }
        let path = String(
            decoding: buffer.prefix { $0 != 0 }.map { UInt8(bitPattern: $0) },
            as: UTF8.self
        )
        return path.isEmpty ? nil : URL(fileURLWithPath: path).standardizedFileURL
    }

    private static func embeddedVersion() -> String {
        let executable = URL(fileURLWithPath: CommandLine.arguments[0]).standardizedFileURL
        let infoURL = executable
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("Info.plist", isDirectory: false)
        guard let data = try? Data(contentsOf: infoURL),
              let plist = try? PropertyListSerialization.propertyList(from: data, format: nil),
              let dictionary = plist as? [String: Any],
              let version = dictionary["CFBundleShortVersionString"] as? String,
              !version.isEmpty,
              version.utf8.count <= 64 else {
            return "dev"
        }
        return version
    }
}
