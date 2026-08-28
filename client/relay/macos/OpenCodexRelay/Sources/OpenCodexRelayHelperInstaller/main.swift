import Darwin
import Foundation
import OSLog
import Security
import SystemConfiguration
import OpenCodexRelayHelperInstallerCore
import OpenCodexRelayHomebrewGuard

private let installerLogger = Logger(
    subsystem: "io.github.novelkr.opencodex-relay.homebrew-guard.installer",
    category: "installer"
)

private struct SigningArtifact {
    let cdHash: String
    let requirement: String
}

private struct InspectedInstallation {
    let profile: HelperInstallerProfile
    let artifacts: HelperInstallerArtifacts
}

private enum InstallerArtifactInspector {
    static func load(
        onVersionRead: (String) -> Void = { _ in }
    ) throws -> InspectedInstallation {
        guard let executableURL = currentExecutableURL(),
              executableURL.lastPathComponent == HelperInstallerConstants.installerExecutableName,
              executableURL.deletingLastPathComponent().lastPathComponent == "Helpers" else {
            throw preflightFailure(reason: .sourceUnreadable)
        }
        let contentsURL = executableURL.deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
        guard contentsURL.lastPathComponent == "Contents" else {
            throw preflightFailure(reason: .sourceUnreadable)
        }
        let appURL = contentsURL.deletingLastPathComponent()
        guard let bundle = Bundle(url: appURL),
              let distributionValue = bundle.object(
                  forInfoDictionaryKey: "OpenCodexDistributionFlavor"
              ) as? String,
              let distribution = HelperInstallerDistribution(rawValue: distributionValue) else {
            throw preflightFailure(reason: .sourceUnreadable)
        }
        let profile = HelperInstallerProfile.forDistribution(distribution)
        guard bundle.bundleIdentifier == profile.appIdentifier,
              let version = boundedMetadata(
                  bundle.object(forInfoDictionaryKey: "OpenCodexHomebrewGuardHelperVersion") as? String,
                  maximumBytes: 64
              ),
              let expectedHelperRequirement = boundedMetadata(
                  bundle.object(forInfoDictionaryKey: "OpenCodexHomebrewGuardHelperRequirement") as? String,
                  maximumBytes: 2_048
              ) else {
            throw preflightFailure(reason: .sourceUnreadable)
        }
        onVersionRead(version)

        let helperURL = contentsURL.appending(path: "Library/HelperTools/" +
            HelperInstallerConstants.helperExecutableName)
        let app = try signingArtifact(
            at: appURL,
            expectedIdentifier: profile.appIdentifier,
            requireHardenedRuntime: distribution == .production
        )
        let installer = try signingArtifact(
            at: executableURL,
            expectedIdentifier: profile.installerIdentifier,
            requireHardenedRuntime: distribution == .production
        )
        let helper = try signingArtifact(
            at: helperURL,
            expectedIdentifier: profile.helperIdentifier,
            requireHardenedRuntime: distribution == .production
        )
        guard helper.requirement == expectedHelperRequirement else {
            throw preflightFailure(reason: .signatureInvalid)
        }
        let daemonRequirement = "(\(app.requirement)) or (\(installer.requirement))"
        var parsed: SecRequirement?
        guard SecRequirementCreateWithString(
                  daemonRequirement as CFString,
                  SecCSFlags(),
                  &parsed
              ) == errSecSuccess else {
            throw preflightFailure(reason: .signatureInvalid)
        }
        return InspectedInstallation(
            profile: profile,
            artifacts: HelperInstallerArtifacts(
                sourceHelperURL: helperURL,
                helperVersion: version,
                daemonClientRequirement: daemonRequirement
            )
        )
    }

    private static func signingArtifact(
        at url: URL,
        expectedIdentifier: String,
        requireHardenedRuntime: Bool
    ) throws -> SigningArtifact {
        var staticCode: SecStaticCode?
        guard SecStaticCodeCreateWithPath(url as CFURL, SecCSFlags(), &staticCode) == errSecSuccess,
              let staticCode,
              SecStaticCodeCheckValidity(
                  staticCode,
                  SecCSFlags(rawValue: kSecCSStrictValidate | kSecCSCheckAllArchitectures),
                  nil
              ) == errSecSuccess else {
            throw preflightFailure(reason: .signatureInvalid)
        }
        var information: CFDictionary?
        guard SecCodeCopySigningInformation(
                  staticCode,
                  SecCSFlags(rawValue: kSecCSSigningInformation),
                  &information
              ) == errSecSuccess,
              let values = information as? [String: Any],
              values[kSecCodeInfoIdentifier as String] as? String == expectedIdentifier,
              values[kSecCodeInfoTeamIdentifier as String] == nil,
              let unique = values[kSecCodeInfoUnique as String] as? Data,
              unique.count >= 20,
              unique.count <= 64 else {
            throw preflightFailure(reason: .signatureInvalid)
        }
        guard let flags = values[kSecCodeInfoFlags as String] as? NSNumber,
              // CS_ADHOC and CS_RUNTIME are not exposed by Security.framework.
              flags.uint32Value & 0x00000002 != 0,
              !requireHardenedRuntime || flags.uint32Value & 0x00010000 != 0 else {
            throw preflightFailure(reason: .signatureInvalid)
        }
        let hash = unique.map { String(format: "%02x", $0) }.joined()
        return SigningArtifact(cdHash: hash, requirement: "cdhash H\"\(hash)\"")
    }

    private static func boundedMetadata(_ value: String?, maximumBytes: Int) -> String? {
        guard let value,
              !value.isEmpty,
              value == value.trimmingCharacters(in: .whitespacesAndNewlines),
              value.utf8.count <= maximumBytes,
              !value.contains("\0"),
              !value.contains("\n"),
              !value.contains("\r") else {
            return nil
        }
        return value
    }

    private static func preflightFailure(
        reason: HelperInstallerFailureReason
    ) -> HelperInstallerFailure {
        HelperInstallerFailure(
            errorCode: .artifactInvalid,
            phase: .preflight,
            reason: reason
        )
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
}

private enum ProbeOutcome: Sendable {
    case ready
    case failed(HelperInstallerFailureReason)
}

private final class ProbeResultBox: @unchecked Sendable {
    private let lock = NSLock()
    private var value: ProbeOutcome?

    func storeIfEmpty(_ newValue: ProbeOutcome) {
        lock.lock()
        if value == nil { value = newValue }
        lock.unlock()
    }

    func load() -> ProbeOutcome? {
        lock.lock()
        defer { lock.unlock() }
        return value
    }
}

private final class SystemHelperInstallerRuntime: @unchecked Sendable, HelperInstallerRuntime {
    private let helperRequirement: String
    private let consoleUID: uid_t?
    private let installerURL: URL

    init(helperRequirement: String) throws {
        self.helperRequirement = helperRequirement
        var uid: uid_t = 0
        var gid: gid_t = 0
        let user = SCDynamicStoreCopyConsoleUser(nil, &uid, &gid) as String?
        // A read-only status check must remain available over SSH and before a
        // GUI login. Mutating commands still fail closed in probeReadiness()
        // when no non-root console user can authenticate the installed XPC
        // service from the same release bundle.
        self.consoleUID = user != nil && user != "loginwindow" && uid != 0 ? uid : nil
        let executable = URL(fileURLWithPath: CommandLine.arguments[0]).standardizedFileURL
        guard executable.lastPathComponent == HelperInstallerConstants.installerExecutableName else {
            throw HelperInstallerFailure(
                errorCode: .artifactInvalid,
                phase: .preflight,
                reason: .sourceUnreadable
            )
        }
        self.installerURL = executable
    }

    func bootout(serviceName: String) throws {
        let wasLoaded = serviceIsLoaded(serviceName: serviceName)
        let status = runLaunchctl(["bootout", "system/\(serviceName)"])
        if status != 0 && wasLoaded && serviceIsLoaded(serviceName: serviceName) {
            throw HelperInstallerFailureReason.daemonStopFailed
        }
    }

    func bootstrap(plistURL: URL) throws {
        guard runLaunchctl(["bootstrap", "system", plistURL.path]) == 0 else {
            throw HelperInstallerFailureReason.daemonStartRejected
        }
    }

    func serviceIsLoaded(serviceName: String) -> Bool {
        runLaunchctl(["print", "system/\(serviceName)"]) == 0
    }

    func validateInstalledHelper(at helperURL: URL) throws {
        var requirement: SecRequirement?
        var code: SecStaticCode?
        guard SecRequirementCreateWithString(
                  helperRequirement as CFString,
                  SecCSFlags(),
                  &requirement
              ) == errSecSuccess,
              let requirement,
              SecStaticCodeCreateWithPath(helperURL as CFURL, SecCSFlags(), &code) == errSecSuccess,
              let code else {
            throw HelperInstallerFailureReason.signatureInvalid
        }
        guard SecStaticCodeCheckValidity(
            code,
            SecCSFlags(rawValue: kSecCSStrictValidate | kSecCSCheckAllArchitectures),
            requirement
        ) == errSecSuccess else {
            throw HelperInstallerFailureReason.signatureInvalid
        }
    }

    func probeReadiness() throws {
        guard let consoleUID else {
            throw HelperInstallerFailureReason.probeSpawnFailed
        }
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/sudo")
        process.arguments = [
            "-n", "-u", "#\(consoleUID)", "--",
            installerURL.path, "--probe-readiness",
        ]
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
        let completion = DispatchSemaphore(value: 0)
        process.terminationHandler = { _ in completion.signal() }
        do {
            try process.run()
        } catch {
            throw HelperInstallerFailureReason.probeSpawnFailed
        }
        guard completion.wait(timeout: .now() + 7) == .success else {
            HelperInstallerProbeTimeoutCleanup.stop(
                isRunning: { process.isRunning },
                terminate: { process.terminate() },
                waitForExit: { seconds in
                    completion.wait(timeout: .now() + seconds) == .success
                },
                forceKill: { _ = Darwin.kill(process.processIdentifier, SIGKILL) }
            )
            throw HelperInstallerFailureReason.probeTimeout
        }
        switch process.terminationStatus {
        case 0:
            return
        case EX_TEMPFAIL:
            throw HelperInstallerFailureReason.probeTimeout
        case EX_NOPERM:
            throw HelperInstallerFailureReason.xpcRejected
        case EX_PROTOCOL:
            throw HelperInstallerFailureReason.invalidResponse
        default:
            throw HelperInstallerFailureReason.xpcRejected
        }
    }

    static func performProbe(
        helperRequirement: String,
        profile: HelperInstallerProfile
    ) throws {
        let connection = NSXPCConnection(
            machServiceName: profile.serviceName,
            options: .privileged
        )
        connection.remoteObjectInterface = NSXPCInterface(with: HomebrewGuardXPCProtocol.self)
        connection.setCodeSigningRequirement(helperRequirement)
        let semaphore = DispatchSemaphore(value: 0)
        let result = ProbeResultBox()
        connection.interruptionHandler = {
            result.storeIfEmpty(.failed(.xpcRejected))
            semaphore.signal()
        }
        connection.invalidationHandler = {
            result.storeIfEmpty(.failed(.xpcRejected))
            semaphore.signal()
        }
        connection.activate()
        let proxy = connection.remoteObjectProxyWithErrorHandler { _ in
            result.storeIfEmpty(.failed(.xpcRejected))
            semaphore.signal()
        }
        let guardDistribution: HomebrewGuardDistribution = profile.distribution == .production
            ? .production
            : .localDevelopment
        guard let remote = proxy as? HomebrewGuardXPCProtocol,
              let request = try? HomebrewGuardCodec.encode(
                  HomebrewGuardRequest(distribution: guardDistribution)
              ) else {
            connection.invalidate()
            throw HelperInstallerFailureReason.invalidResponse
        }
        remote.status(request) { data in
            if let response = try? HomebrewGuardCodec.decodeResponse(data),
               response.state == .ready,
               response.errorCode == nil {
                result.storeIfEmpty(.ready)
            } else {
                result.storeIfEmpty(.failed(.invalidResponse))
            }
            semaphore.signal()
        }
        guard semaphore.wait(timeout: .now() + 5) == .success else {
            connection.invalidate()
            throw HelperInstallerFailureReason.probeTimeout
        }
        connection.invalidate()
        switch result.load() {
        case .ready: return
        case let .failed(reason): throw reason
        case nil: throw HelperInstallerFailureReason.invalidResponse
        }
    }

    private func runLaunchctl(_ arguments: [String]) -> Int32 {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/bin/launchctl")
        process.arguments = arguments
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
        do {
            try process.run()
            process.waitUntilExit()
            return process.terminationStatus
        } catch {
            return EX_UNAVAILABLE
        }
    }
}

@main
struct OpenCodexRelayHelperInstallerMain {
    static func main() {
        let arguments = Array(CommandLine.arguments.dropFirst())
        if arguments == ["--probe-readiness"] {
            guard geteuid() != 0 else { exit(EX_NOPERM) }
            do {
                let inspected = try InstallerArtifactInspector.load()
                let requirement = try helperRequirement(for: inspected.artifacts.sourceHelperURL)
                try SystemHelperInstallerRuntime.performProbe(
                    helperRequirement: requirement,
                    profile: inspected.profile
                )
                exit(0)
            } catch let reason as HelperInstallerFailureReason {
                switch reason {
                case .probeTimeout: exit(EX_TEMPFAIL)
                case .xpcRejected: exit(EX_NOPERM)
                case .invalidResponse: exit(EX_PROTOCOL)
                default: exit(EX_UNAVAILABLE)
                }
            } catch {
                exit(EX_PROTOCOL)
            }
        }
        guard let command = parsedCommand(arguments) else {
            emitFallback(command: .status, error: .invalidInvocation)
        }
        var helperVersion: String?
        do {
            let inspected = try diagnosedPreflight(
                fallbackCode: .artifactInvalid
            ) {
                try InstallerArtifactInspector.load { helperVersion = $0 }
            }
            let artifacts = inspected.artifacts
            helperVersion = artifacts.helperVersion
            let requirement = try diagnosedPreflight(
                fallbackCode: .artifactInvalid
            ) {
                try helperRequirement(for: artifacts.sourceHelperURL)
            }
            let runtime = try diagnosedPreflight(
                fallbackCode: .installationFailed
            ) {
                try SystemHelperInstallerRuntime(helperRequirement: requirement)
            }
            let controller = HelperInstallerController(
                configuration: HelperInstallerConfiguration(
                    distribution: inspected.profile.distribution
                ),
                artifacts: artifacts,
                runtime: runtime
            )
            let receipt: HelperInstallerReceipt
            do {
                receipt = try controller.perform(command)
            } catch {
                receipt = controller.failureReceipt(command: command, error: error)
                emit(
                    receipt,
                    exitCode: exitCode(for: receipt.errorCode ?? .installationFailed)
                )
            }
            emit(receipt, exitCode: 0)
        } catch {
            emitFailure(command: command, error: error, helperVersion: helperVersion)
        }
    }

    private static func diagnosedPreflight<T>(
        fallbackCode: HelperInstallerErrorCode,
        _ operation: () throws -> T
    ) throws -> T {
        do {
            return try operation()
        } catch {
            throw HelperInstallerDiagnostics.failure(
                error,
                phase: .preflight,
                fallbackReason: .unknown,
                fallbackCode: fallbackCode
            )
        }
    }

    private static func parsedCommand(_ arguments: [String]) -> HelperInstallerCommand? {
        if arguments == ["status", "--json"] { return .status }
        guard arguments.count == 1 else { return nil }
        return HelperInstallerCommand(rawValue: arguments[0]).flatMap {
            $0 == .status ? nil : $0
        }
    }

    private static func helperRequirement(for helperURL: URL) throws -> String {
        var code: SecStaticCode?
        guard SecStaticCodeCreateWithPath(helperURL as CFURL, SecCSFlags(), &code) == errSecSuccess,
              let code else {
            throw HelperInstallerFailure(
                errorCode: .artifactInvalid,
                phase: .preflight,
                reason: .signatureInvalid
            )
        }
        var information: CFDictionary?
        guard SecCodeCopySigningInformation(
                  code,
                  SecCSFlags(rawValue: kSecCSSigningInformation),
                  &information
              ) == errSecSuccess,
              let values = information as? [String: Any],
              let unique = values[kSecCodeInfoUnique as String] as? Data else {
            throw HelperInstallerFailure(
                errorCode: .artifactInvalid,
                phase: .preflight,
                reason: .signatureInvalid
            )
        }
        let hash = unique.map { String(format: "%02x", $0) }.joined()
        return "cdhash H\"\(hash)\""
    }

    private static func emitFallback(
        command: HelperInstallerCommand,
        error: HelperInstallerErrorCode
    ) -> Never {
        emitFailure(command: command, error: error, helperVersion: nil)
    }

    private static func emitFailure(
        command: HelperInstallerCommand,
        error: any Error,
        helperVersion: String?
    ) -> Never {
        let receipt = HelperInstallerDiagnostics.receipt(
            command: command,
            fallbackState: .installRequired,
            helperVersion: helperVersion,
            error: error
        )
        emit(receipt, exitCode: exitCode(for: receipt.errorCode ?? .installationFailed))
    }

    private static func emit(_ receipt: HelperInstallerReceipt, exitCode: Int32) -> Never {
        let phase = receipt.failurePhase?.rawValue ?? "none"
        let reason = receipt.failureReason?.rawValue ?? "none"
        let rollback = receipt.rollbackResult?.rawValue ?? "not_needed"
        installerLogger.log(
            level: exitCode == 0 ? .info : .error,
            "command=\(receipt.command, privacy: .public) result=\(receipt.resultCode, privacy: .public) phase=\(phase, privacy: .public) reason=\(reason, privacy: .public) rollback=\(rollback, privacy: .public)"
        )
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        if let data = try? encoder.encode(receipt) {
            FileHandle.standardOutput.write(data)
            FileHandle.standardOutput.write(Data([0x0A]))
        }
        exit(exitCode)
    }

    private static func exitCode(for error: HelperInstallerErrorCode) -> Int32 {
        switch error {
        case .invalidInvocation: EX_USAGE
        case .rootRequired: EX_NOPERM
        case .busy, .protectionActive: EX_TEMPFAIL
        case .artifactInvalid, .updateRequired, .installRequired: EX_CONFIG
        case .recoveryRequired, .rollbackFailed, .recoveryUnavailable,
             .recoveryVerificationFailed:
            EX_SOFTWARE
        case .installationFailed, .daemonLaunchFailed: EX_UNAVAILABLE
        }
    }
}
