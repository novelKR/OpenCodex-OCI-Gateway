import AppKit
import Foundation
import Security
import OpenCodexRelayCore
import OpenCodexRelayHelperInstallerCore
import OpenCodexRelayHomebrewGuard

enum HomebrewGuardBackend: String, Equatable, Sendable {
    case smAppService = "sm_app_service"
    case manualAdmin = "manual_admin"
}

enum HomebrewGuardSetupAction: String, Sendable {
    case install
    case update
    case uninstall
    case recover
}

enum HomebrewGuardSetupCommandAvailability: Equatable, Sendable {
    case available(String)
    case unavailable(String)
}

enum HomebrewGuardRegistrationState: String, Equatable, Sendable {
    case preview
    case notRequired = "not_required"
    case notRegistered = "not_registered"
    case approvalRequired = "approval_required"
    case manualInstallRequired = "manual_install_required"
    case manualUpdateRequired = "manual_update_required"
    case manualInstallerRecoveryRequired = "manual_installer_recovery_required"
    case daemonLaunchFailed = "daemon_launch_failed"
    case ready
    case busy
    case recoveryRequired = "recovery_required"
    case unavailable
}

struct HomebrewGuardAvailability: Equatable, Sendable {
    let registration: HomebrewGuardRegistrationState
    let helperVersion: String?
    let protocolVersion: Int
    let errorCode: HomebrewGuardErrorCode?
    let operationID: String?

    init(
        registration: HomebrewGuardRegistrationState,
        helperVersion: String?,
        protocolVersion: Int,
        errorCode: HomebrewGuardErrorCode?,
        operationID: String?
    ) {
        self.registration = registration
        self.helperVersion = helperVersion
        self.protocolVersion = protocolVersion
        self.errorCode = errorCode
        self.operationID = operationID
    }

    static let notRequired = HomebrewGuardAvailability(
        registration: .notRequired,
        helperVersion: nil,
        protocolVersion: homebrewGuardProtocolVersion,
        errorCode: nil,
        operationID: nil
    )

    static let preview = HomebrewGuardAvailability(
        registration: .preview,
        helperVersion: nil,
        protocolVersion: homebrewGuardProtocolVersion,
        errorCode: nil,
        operationID: nil
    )

    var canPrepare: Bool {
        registration == .ready && errorCode == nil
    }
}

@MainActor
protocol HomebrewGuardManaging: AnyObject {
    var backend: HomebrewGuardBackend { get }
    func availability(candidate: HomebrewGuardCandidate?) async -> HomebrewGuardAvailability
    func register() throws
    func openSystemSettingsLoginItems()
    func setupCommand(for action: HomebrewGuardSetupAction) -> HomebrewGuardSetupCommandAvailability
    func prepare(candidate: HomebrewGuardCandidate, operationID: String) async throws
    func commit(operationID: String) async throws
    func release(operationID: String) async throws
    func recover(operationID: String) async throws
}

extension HomebrewGuardManaging {
    var backend: HomebrewGuardBackend { .smAppService }
    func setupCommand(for _: HomebrewGuardSetupAction) -> HomebrewGuardSetupCommandAvailability {
        .unavailable("artifact_invalid")
    }
}

struct HomebrewGuardServiceConfiguration: Equatable, Sendable {
    static let machServiceKey = "OpenCodexHomebrewGuardMachService"
    static let helperRequirementKey = "OpenCodexHomebrewGuardHelperRequirement"
    static let helperVersionKey = "OpenCodexHomebrewGuardHelperVersion"
    static let backendKey = "OpenCodexHomebrewGuardBackend"
    static let installerExecutableKey = "OpenCodexHomebrewGuardInstallerExecutable"

    let distribution: HomebrewGuardDistribution
    let backend: HomebrewGuardBackend
    let machServiceName: String
    let helperRequirement: String
    let helperVersion: String
    let installerExecutableName: String?
    let installerProfile: HelperInstallerProfile

    init?(bundle: Bundle = .main, flavor: DistributionFlavor = .current) {
        guard let machServiceName = Self.metadata(
                  bundle.object(forInfoDictionaryKey: Self.machServiceKey) as? String,
                  maximumBytes: 255
              ),
              let helperRequirement = Self.metadata(
                  bundle.object(forInfoDictionaryKey: Self.helperRequirementKey) as? String,
                  maximumBytes: 2_048
              ),
              let helperVersion = Self.metadata(
                  bundle.object(forInfoDictionaryKey: Self.helperVersionKey) as? String,
                  maximumBytes: 64
              ) else {
            return nil
        }
        let distribution: HomebrewGuardDistribution = flavor == .production
            ? .production
            : .localDevelopment
        let installerDistribution: HelperInstallerDistribution = distribution == .production
            ? .production
            : .localDevelopment
        let installerProfile = HelperInstallerProfile.forDistribution(installerDistribution)
        let expectedBackend = HomebrewGuardBackend.manualAdmin
        let configuredBackend = Self.metadata(
            bundle.object(forInfoDictionaryKey: Self.backendKey) as? String,
            maximumBytes: 64
        ).flatMap(HomebrewGuardBackend.init(rawValue:)) ?? .manualAdmin
        let expectedService = installerProfile.serviceName
        let installerExecutableName = Self.metadata(
            bundle.object(forInfoDictionaryKey: Self.installerExecutableKey) as? String,
            maximumBytes: 255
        )
        guard configuredBackend == expectedBackend,
              machServiceName == expectedService,
              installerExecutableName == HelperInstallerConstants.installerExecutableName,
              helperVersion == "dev" || helperVersion.range(
                  of: #"^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$"#,
                  options: .regularExpression
              ) != nil else {
            return nil
        }
        self.distribution = distribution
        self.backend = configuredBackend
        self.machServiceName = machServiceName
        self.helperRequirement = helperRequirement
        self.helperVersion = helperVersion
        self.installerExecutableName = installerExecutableName
        self.installerProfile = installerProfile
    }

    private static func metadata(_ value: String?, maximumBytes: Int) -> String? {
        guard let value,
              value == value.trimmingCharacters(in: .whitespacesAndNewlines),
              !value.isEmpty,
              value.utf8.count <= maximumBytes,
              !value.contains("\0"),
              !value.contains("\n"),
              !value.contains("\r") else {
            return nil
        }
        return value
    }
}

@MainActor
final class SystemHomebrewGuardManager: HomebrewGuardManaging {
    private let configuration: HomebrewGuardServiceConfiguration?
    private let manualSystemRootURL: URL
    private var activeOperationSession: (operationID: String, session: HomebrewGuardXPCSession)?

    var backend: HomebrewGuardBackend {
        configuration?.backend ?? .manualAdmin
    }

    init(
        configuration: HomebrewGuardServiceConfiguration? = HomebrewGuardServiceConfiguration(),
        manualSystemRootURL: URL = URL(fileURLWithPath: "/")
    ) {
        self.configuration = configuration
        self.manualSystemRootURL = manualSystemRootURL.standardizedFileURL
    }

    func availability(candidate: HomebrewGuardCandidate?) async -> HomebrewGuardAvailability {
        guard let configuration else {
            return unavailable(.homebrewGuardNotRegistered)
        }
        guard configuration.backend == .manualAdmin else {
            return unavailable(.homebrewGuardNotRegistered)
        }
        return await manualAvailability(candidate: candidate)
    }

    func register() throws {
        throw HomebrewGuardErrorCode.homebrewGuardNotRegistered
    }

    func openSystemSettingsLoginItems() {}

    func setupCommand(
        for action: HomebrewGuardSetupAction
    ) -> HomebrewGuardSetupCommandAvailability {
        guard configuration?.backend == .manualAdmin,
              setupActionAllowed(action),
              let executableName = configuration?.installerExecutableName else {
            return .unavailable("artifact_invalid")
        }
        let executable = Bundle.main.bundleURL
            .appending(path: "Contents/Library/Helpers")
            .appending(path: executableName)
        var info = stat()
        guard lstat(executable.path, &info) == 0,
              (info.st_mode & S_IFMT) == S_IFREG,
              info.st_mode & mode_t(0o111) != 0 else {
            return .unavailable("artifact_invalid")
        }
        let escaped = executable.path.replacingOccurrences(of: "'", with: "'\\''")
        return .available(
            "/usr/bin/sudo -- '\(escaped)' \(action.rawValue)"
        )
    }

    func unregisterIfSafe() async -> HomebrewGuardRegistrationState {
        configuration?.backend == .manualAdmin ? manualInstallationState() : .unavailable
    }

    func prepare(candidate: HomebrewGuardCandidate, operationID: String) async throws {
        guard activeOperationSession == nil else {
            throw HomebrewGuardErrorCode.busy
        }
        guard let configuration else {
            throw HomebrewGuardErrorCode.homebrewGuardNotRegistered
        }
        let session = HomebrewGuardXPCSession(
            machServiceName: configuration.machServiceName,
            helperRequirement: configuration.helperRequirement
        )
        do {
            let response = try await invoke(
                .prepare,
                operationID: operationID,
                candidate: candidate,
                session: session
            )
            guard response.resultCode == .prepared,
                  response.state == .prepared,
                  response.operationID == operationID else {
                throw response.errorCode ?? HomebrewGuardErrorCode.protectionFailed
            }
            activeOperationSession = (operationID, session)
        } catch {
            session.invalidate()
            throw error
        }
    }

    func commit(operationID: String) async throws {
        guard let active = activeOperationSession,
              active.operationID == operationID else {
            throw HomebrewGuardErrorCode.busy
        }
        do {
            let response = try await invoke(
                .commit,
                operationID: operationID,
                session: active.session
            )
            guard response.resultCode == .committed,
                  response.state == .committed,
                  response.operationID == operationID else {
                throw response.errorCode ?? HomebrewGuardErrorCode.protectionFailed
            }
        } catch {
            activeOperationSession = nil
            active.session.invalidate()
            throw error
        }
    }

    func release(operationID: String) async throws {
        guard let active = activeOperationSession,
              active.operationID == operationID else {
            throw HomebrewGuardErrorCode.busy
        }
        defer {
            activeOperationSession = nil
            active.session.invalidate()
        }
        let response = try await invoke(
            .release,
            operationID: operationID,
            session: active.session
        )
        guard response.resultCode == .released,
              response.state == .ready,
              response.operationID == operationID else {
            throw response.errorCode ?? HomebrewGuardErrorCode.restoreFailed
        }
    }

    func recover(operationID: String) async throws {
        guard activeOperationSession == nil else {
            throw HomebrewGuardErrorCode.busy
        }
        let response = try await invoke(.recover, operationID: operationID)
        guard response.resultCode == .recovered,
              response.state == .ready,
              response.operationID == operationID else {
            throw response.errorCode ?? HomebrewGuardErrorCode.restoreFailed
        }
    }

    private func invoke(
        _ operation: HomebrewGuardOperation,
        operationID: String? = nil,
        candidate: HomebrewGuardCandidate? = nil,
        session: HomebrewGuardXPCSession? = nil,
        acceptsFailure: Bool = false
    ) async throws -> HomebrewGuardResponse {
        guard let configuration else {
            throw HomebrewGuardErrorCode.homebrewGuardNotRegistered
        }
        let request = HomebrewGuardRequest(
            distribution: configuration.distribution,
            operationID: operationID,
            candidate: candidate
        )
        let requestData = try HomebrewGuardCodec.encode(request)
        let ownsSession = session == nil
        let invocationSession = session ?? HomebrewGuardXPCSession(
            machServiceName: configuration.machServiceName,
            helperRequirement: configuration.helperRequirement
        )
        defer {
            if ownsSession {
                invocationSession.invalidate()
            }
        }
        let responseData = try await invocationSession.invoke(
            operation,
            requestData: requestData
        )
        let response = try HomebrewGuardCodec.decodeResponse(responseData)
        if let error = response.errorCode, !acceptsFailure {
            throw error
        }
        return response
    }

    private func registrationState(
        _ response: HomebrewGuardResponse
    ) -> HomebrewGuardRegistrationState {
        switch response.state {
        case .ready: .ready
        case .prepared, .committed: .busy
        case .recoveryRequired: .recoveryRequired
        case .notRegistered: .notRegistered
        case .approvalRequired: .approvalRequired
        case .unavailable: .unavailable
        }
    }

    private func unavailable(
        _ code: HomebrewGuardErrorCode,
        registration: HomebrewGuardRegistrationState = .unavailable
    ) -> HomebrewGuardAvailability {
        HomebrewGuardAvailability(
            registration: registration,
            helperVersion: configuration?.helperVersion,
            protocolVersion: homebrewGuardProtocolVersion,
            errorCode: code,
            operationID: nil
        )
    }

    private func manualAvailability(
        candidate: HomebrewGuardCandidate?
    ) async -> HomebrewGuardAvailability {
        let installedState = manualInstallationState()
        guard installedState == .ready else {
            let errorCode: HomebrewGuardErrorCode = (
                installedState == .recoveryRequired ||
                    installedState == .manualInstallerRecoveryRequired
            )
                ? .recoveryRequired
                : .homebrewGuardNotRegistered
            return HomebrewGuardAvailability(
                registration: installedState,
                helperVersion: configuration?.helperVersion,
                protocolVersion: homebrewGuardProtocolVersion,
                errorCode: errorCode,
                operationID: nil
            )
        }
        do {
            let response = try await invoke(.status, candidate: candidate, acceptsFailure: true)
            let state = registrationState(response)
            return HomebrewGuardAvailability(
                registration: state,
                helperVersion: response.helperVersion,
                protocolVersion: response.protocolVersion,
                errorCode: response.errorCode,
                operationID: response.operationID
            )
        } catch is HomebrewGuardConnectionError {
            return HomebrewGuardAvailability(
                registration: .daemonLaunchFailed,
                helperVersion: configuration?.helperVersion,
                protocolVersion: homebrewGuardProtocolVersion,
                errorCode: .homebrewGuardNotRegistered,
                operationID: nil
            )
        } catch let code as HomebrewGuardErrorCode {
            return unavailable(code)
        } catch {
            return unavailable(.homebrewGuardNotRegistered)
        }
    }

    private func manualInstallationState() -> HomebrewGuardRegistrationState {
        guard let configuration,
              configuration.backend == .manualAdmin else {
            return .unavailable
        }
        let root = manualSystemRootURL
        let profile = configuration.installerProfile
        let installerTransaction = root
            .appending(path: HelperInstallerConstants.stateDirectoryRelativePath)
            .appending(path: profile.transactionDirectoryName)
        if pathExists(installerTransaction) {
            return .manualInstallerRecoveryRequired
        }
        let helper = root
            .appending(path: profile.helperRelativePath)
        let plist = root
            .appending(path: profile.plistRelativePath)
        let helperExists = pathExists(helper)
        let plistExists = pathExists(plist)
        if !helperExists && !plistExists { return .manualInstallRequired }
        guard helperExists, plistExists,
              rootOwnedRegularFile(helper, mode: 0o755),
              rootOwnedRegularFile(plist, mode: 0o644) else {
            return .recoveryRequired
        }
        guard code(at: helper, satisfies: configuration.helperRequirement),
              installedManualPlistMatches(plist, configuration: configuration) else {
            return .manualUpdateRequired
        }
        return .ready
    }

    private func setupActionAllowed(_ action: HomebrewGuardSetupAction) -> Bool {
        let state = manualInstallationState()
        switch action {
        case .install:
            return state == .manualInstallRequired
        case .update:
            return state == .manualUpdateRequired || state == .ready
        case .uninstall:
            return state != .manualInstallRequired &&
                state != .manualInstallerRecoveryRequired &&
                state != .recoveryRequired
        case .recover:
            return state == .manualInstallerRecoveryRequired
        }
    }

    private func installedManualPlistMatches(
        _ url: URL,
        configuration: HomebrewGuardServiceConfiguration
    ) -> Bool {
        guard let data = try? Data(contentsOf: url),
              data.count <= 64 * 1024,
              let value = try? PropertyListSerialization.propertyList(
                  from: data,
                  options: [],
                  format: nil
              ),
              let dictionary = value as? [String: Any],
              dictionary["Label"] as? String == configuration.installerProfile.serviceName,
              dictionary["Program"] as? String == manualSystemRootURL
                  .appending(path: configuration.installerProfile.helperRelativePath).path,
              let arguments = dictionary["ProgramArguments"] as? [String],
              let clientRequirement = expectedManualClientRequirement(),
              arguments == [
                  manualSystemRootURL
                      .appending(path: configuration.installerProfile.helperRelativePath).path,
                  "--daemon",
                  "--distribution", configuration.installerProfile.distribution.rawValue,
                  "--mach-service", configuration.installerProfile.serviceName,
                  "--client-requirement", clientRequirement,
                  "--helper-version", configuration.helperVersion,
              ],
              let services = dictionary["MachServices"] as? [String: Bool],
              services == [configuration.installerProfile.serviceName: true] else {
            return false
        }
        return true
    }

    private func expectedManualClientRequirement() -> String? {
        guard let installerName = configuration?.installerExecutableName,
              let app = codeRequirement(at: Bundle.main.bundleURL),
              let installer = codeRequirement(
                  at: Bundle.main.bundleURL
                      .appending(path: "Contents/Library/Helpers")
                      .appending(path: installerName)
              ) else {
            return nil
        }
        return "(\(app)) or (\(installer))"
    }

    private func codeRequirement(at url: URL) -> String? {
        var staticCode: SecStaticCode?
        guard SecStaticCodeCreateWithPath(url as CFURL, SecCSFlags(), &staticCode) == errSecSuccess,
              let staticCode,
              SecStaticCodeCheckValidity(
                  staticCode,
                  SecCSFlags(rawValue: kSecCSStrictValidate | kSecCSCheckAllArchitectures),
                  nil
              ) == errSecSuccess else {
            return nil
        }
        var information: CFDictionary?
        guard SecCodeCopySigningInformation(
                  staticCode,
                  SecCSFlags(rawValue: kSecCSSigningInformation),
                  &information
              ) == errSecSuccess,
              let values = information as? [String: Any],
              let unique = values[kSecCodeInfoUnique as String] as? Data,
              unique.count >= 20,
              unique.count <= 64 else {
            return nil
        }
        return "cdhash H\"" + unique.map { String(format: "%02x", $0) }.joined() + "\""
    }

    private func code(at url: URL, satisfies requirementString: String) -> Bool {
        var requirement: SecRequirement?
        var staticCode: SecStaticCode?
        guard SecRequirementCreateWithString(
                  requirementString as CFString,
                  SecCSFlags(),
                  &requirement
              ) == errSecSuccess,
              let requirement,
              SecStaticCodeCreateWithPath(url as CFURL, SecCSFlags(), &staticCode) == errSecSuccess,
              let staticCode else {
            return false
        }
        return SecStaticCodeCheckValidity(
            staticCode,
            SecCSFlags(rawValue: kSecCSStrictValidate | kSecCSCheckAllArchitectures),
            requirement
        ) == errSecSuccess
    }

    private func pathExists(_ url: URL) -> Bool {
        var info = stat()
        return lstat(url.path, &info) == 0
    }

    private func rootOwnedRegularFile(_ url: URL, mode: mode_t) -> Bool {
        var info = stat()
        guard lstat(url.path, &info) == 0,
              (info.st_mode & S_IFMT) == S_IFREG,
              info.st_uid == 0,
              info.st_mode & mode_t(0o7777) == mode else {
            return false
        }
        return true
    }
}

enum HomebrewGuardConnectionError: Error {
    case daemonLaunchFailed
}

private final class HomebrewGuardXPCSession: @unchecked Sendable {
    private struct PendingInvocation {
        let id: UUID
        let continuation: CheckedContinuation<Data, any Error>
    }

    private let lock = NSLock()
    private let connection: NSXPCConnection
    private var pendingInvocation: PendingInvocation?
    private var invalidated = false

    init(machServiceName: String, helperRequirement: String) {
        connection = NSXPCConnection(
            machServiceName: machServiceName,
            options: .privileged
        )
        connection.remoteObjectInterface = NSXPCInterface(
            with: HomebrewGuardXPCProtocol.self
        )
        connection.setCodeSigningRequirement(helperRequirement)
        connection.interruptionHandler = { [weak self] in
            self?.connectionEnded(with: HomebrewGuardConnectionError.daemonLaunchFailed)
        }
        connection.invalidationHandler = { [weak self] in
            self?.connectionEnded(with: HomebrewGuardErrorCode.homebrewGuardNotRegistered)
        }
        connection.activate()
    }

    deinit {
        invalidate()
    }

    func invalidate() {
        let pending: PendingInvocation?
        lock.lock()
        if invalidated {
            pending = nil
        } else {
            invalidated = true
            pending = pendingInvocation
            pendingInvocation = nil
        }
        lock.unlock()
        connection.invalidate()
        pending?.continuation.resume(
            throwing: HomebrewGuardErrorCode.homebrewGuardNotRegistered
        )
    }

    func invoke(
        _ operation: HomebrewGuardOperation,
        requestData: Data
    ) async throws -> Data {
        try await withCheckedThrowingContinuation { continuation in
            let invocationID = UUID()
            lock.lock()
            guard !invalidated, pendingInvocation == nil else {
                lock.unlock()
                continuation.resume(throwing: HomebrewGuardErrorCode.busy)
                return
            }
            pendingInvocation = PendingInvocation(
                id: invocationID,
                continuation: continuation
            )
            lock.unlock()

            let proxy = connection.remoteObjectProxyWithErrorHandler { [weak self] _ in
                self?.finish(
                    invocationID,
                    result: .failure(HomebrewGuardErrorCode.homebrewGuardNotRegistered),
                    invalidating: true
                )
            }
            guard let remote = proxy as? HomebrewGuardXPCProtocol else {
                finish(
                    invocationID,
                    result: .failure(HomebrewGuardErrorCode.homebrewGuardNotRegistered),
                    invalidating: true
                )
                return
            }
            DispatchQueue.global(qos: .utility).asyncAfter(deadline: .now() + 5) { [weak self] in
                self?.finish(
                    invocationID,
                    result: .failure(HomebrewGuardConnectionError.daemonLaunchFailed),
                    invalidating: true
                )
            }
            let reply: (Data) -> Void = { [weak self] data in
                self?.finish(invocationID, result: .success(data), invalidating: false)
            }
            switch operation {
            case .status:
                remote.status(requestData, withReply: reply)
            case .prepare:
                remote.prepare(requestData, withReply: reply)
            case .commit:
                remote.commit(requestData, withReply: reply)
            case .release:
                remote.release(requestData, withReply: reply)
            case .recover:
                remote.recover(requestData, withReply: reply)
            }
        }
    }

    private func finish(
        _ invocationID: UUID,
        result: Result<Data, any Error>,
        invalidating: Bool
    ) {
        let continuation: CheckedContinuation<Data, any Error>?
        lock.lock()
        guard pendingInvocation?.id == invocationID else {
            lock.unlock()
            return
        }
        continuation = pendingInvocation?.continuation
        pendingInvocation = nil
        if invalidating {
            invalidated = true
        }
        lock.unlock()
        if invalidating {
            connection.invalidate()
        }
        continuation?.resume(with: result)
    }

    private func connectionEnded(with error: any Error) {
        let continuation: CheckedContinuation<Data, any Error>?
        lock.lock()
        invalidated = true
        continuation = pendingInvocation?.continuation
        pendingInvocation = nil
        lock.unlock()
        continuation?.resume(throwing: error)
    }
}

extension OpenCodexInstallationCandidate {
    func homebrewGuardCandidate() throws -> HomebrewGuardCandidate {
        guard requiresHomebrewGuard,
              removalCapability == .homebrewGuardedNPM,
              manager == .homebrew,
              let cliEntry,
              let bunExecutable,
              let nodeExecutable,
              let npmCLI else {
            throw HomebrewGuardErrorCode.candidateChanged
        }
        return try HomebrewGuardCandidate(
            installationID: id,
            installationFingerprint: fingerprint,
            prefix: prefix,
            packageRoot: packageRoot,
            executable: executable,
            cliEntry: cliEntry,
            bunExecutable: bunExecutable,
            nodeExecutable: nodeExecutable,
            npmCLI: npmCLI,
            launchers: launchers
        ).validated(allowedRoot: "/opt/homebrew")
    }
}

extension OpenCodexNativeRemovalCandidate {
    func homebrewGuardCandidate() throws -> HomebrewGuardCandidate {
        guard homebrewGuardRequired,
              removalCapability == .homebrewGuardedNPM,
              manager == .homebrew,
              let snapshot = homebrewGuard else {
            throw HomebrewGuardErrorCode.candidateChanged
        }
        return try HomebrewGuardCandidate(
            installationID: installationID,
            installationFingerprint: installationFingerprint,
            prefix: snapshot.prefix,
            packageRoot: snapshot.packageRoot,
            executable: snapshot.executable,
            cliEntry: snapshot.cliEntry,
            bunExecutable: snapshot.bunExecutable,
            nodeExecutable: snapshot.nodeExecutable,
            npmCLI: snapshot.npmCLI,
            launchers: snapshot.launchers
        ).validated(allowedRoot: "/opt/homebrew")
    }
}
