import Darwin
import Foundation

/// Implementations must not block the caller's actor. The bundled process
/// client performs its process wait and pipe draining on a utility task so the
/// SwiftUI MainActor remains responsive while status polling is active.
public protocol RelayctlExecuting: Sendable {
    func execute(_ command: RelayctlCommand) async throws -> RoutingStatus
}

public struct RelayctlExecutionPolicy: Equatable, Sendable {
    public let timeout: TimeInterval
    public let terminationGracePeriod: TimeInterval
    public let maximumOutputBytes: Int

    public init(
        timeout: TimeInterval = 5,
        terminationGracePeriod: TimeInterval = 0.25,
        maximumOutputBytes: Int = 64 * 1024
    ) {
        self.timeout = max(timeout, 0.05)
        self.terminationGracePeriod = max(terminationGracePeriod, 0)
        self.maximumOutputBytes = max(maximumOutputBytes, 1024)
    }
}

public struct ProcessRelayctlClient: RelayctlExecuting, Sendable {
    public let executableURL: URL
    public let additionalArguments: [String]
    public let executionPolicy: RelayctlExecutionPolicy

    public init(
        executableURL: URL,
        additionalArguments: [String] = [],
        executionPolicy: RelayctlExecutionPolicy = RelayctlExecutionPolicy()
    ) {
        self.executableURL = executableURL
        self.additionalArguments = additionalArguments
        self.executionPolicy = executionPolicy
    }

    public func execute(_ command: RelayctlCommand) async throws -> RoutingStatus {
        guard executableURL.isFileURL,
              FileManager.default.isExecutableFile(atPath: executableURL.path) else {
            throw RelayctlError.helperUnavailable
        }

        let operation = RelayctlProcessOperation(
            executableURL: executableURL,
            arguments: command.arguments + additionalArguments,
			policy: policy(for: command)
        )
        let result = try await withTaskCancellationHandler {
            try await Task.detached(priority: .utility) {
                try operation.run()
            }.value
        } onCancel: {
            // This affects only the short-lived relayctl helper. It never
            // targets the user-selected Codex Desktop application.
            operation.cancel()
        }
        if Task.isCancelled {
            throw RelayctlError.cancelled
        }
        if result.exitCode != 0 {
            if let envelope = try? JSONDecoder().decode(RelayctlOperationErrorEnvelope.self, from: result.stdout),
               let code = envelope.reportedCode() {
                throw RelayctlError.reported(code)
            }
            throw RelayctlError.invocationFailed(exitCode: result.exitCode)
        }
        do {
            return try JSONDecoder().decode(RoutingStatus.self, from: result.stdout).validated()
        } catch let error as RelayctlError {
            throw error
        } catch {
            throw RelayctlError.invalidJSON
        }
    }

	private func policy(for command: RelayctlCommand) -> RelayctlExecutionPolicy {
		// `mode status` can safely spend up to ~14 seconds observing two
		// loopback listeners plus an enrolled Local identity/catalog. Apply and
		// recovery deliberately drain for up to 30 seconds. Never let the UI's
		// helper timeout manufacture an avoidable applying/journal recovery.
		let grace: TimeInterval
		if case .handoff = command {
			// relayctl handles SIGTERM by canceling its exact OCX child. Give
			// that bounded cleanup a moment before the helper itself is reaped.
			grace = max(executionPolicy.terminationGracePeriod, 2)
		} else {
			grace = executionPolicy.terminationGracePeriod
		}
		return RelayctlExecutionPolicy(
			timeout: max(executionPolicy.timeout, command.minimumHelperTimeout),
			terminationGracePeriod: grace,
			maximumOutputBytes: executionPolicy.maximumOutputBytes
		)
	}
}

public struct ProcessNativeRepairClient: NativeRepairExecuting, Sendable {
    public let executableURL: URL
    public let additionalArguments: [String]
    public let executionPolicy: RelayctlExecutionPolicy

    public init(
        executableURL: URL,
        additionalArguments: [String] = [],
        executionPolicy: RelayctlExecutionPolicy = RelayctlExecutionPolicy(
            timeout: 90,
            terminationGracePeriod: 2,
            maximumOutputBytes: 64 * 1024
        )
    ) {
        self.executableURL = executableURL
        self.additionalArguments = additionalArguments
        self.executionPolicy = executionPolicy
    }

    public func inspect(expectedGeneration: UInt64) async throws -> NativeRepairInspection {
        guard expectedGeneration > 0 else { throw RelayctlError.invalidStatus }
        let output = try await execute(arguments: [
            "mode", "inspect-native-repair",
            "--expected-routing-generation", String(expectedGeneration),
            "--json",
        ], timeout: 20)
        do {
            let result = try JSONDecoder().decode(NativeRepairInspection.self, from: output).validated()
            guard result.generation == expectedGeneration else { throw RelayctlError.invalidStatus }
            return result
        } catch let error as RelayctlError {
            throw error
        } catch {
            throw RelayctlError.invalidJSON
        }
    }

    public func inspectOwner(
        expectedGeneration: UInt64,
        owner: NativeRepairKind,
        openCodexSelection: OpenCodexNativeRepairSelection
    ) async throws -> NativeRepairOwnerInspection {
        let arguments = try Self.ownerInspectionArguments(
            expectedGeneration: expectedGeneration,
            owner: owner,
            selection: openCodexSelection
        )
        let output = try await execute(arguments: arguments, timeout: 20)
        do {
            let result = try JSONDecoder().decode(NativeRepairOwnerInspection.self, from: output).validated()
            guard result.generation == expectedGeneration, result.owner == owner else {
                throw RelayctlError.invalidStatus
            }
            return result
        } catch let error as RelayctlError {
            throw error
        } catch {
            throw RelayctlError.invalidJSON
        }
    }

    static func ownerInspectionArguments(
        expectedGeneration: UInt64,
        owner: NativeRepairKind,
        selection: OpenCodexNativeRepairSelection
    ) throws -> [String] {
        guard expectedGeneration > 0, owner == .openCodex else { throw RelayctlError.invalidStatus }
        return [
            "mode", "inspect-native-repair-owner",
            "--expected-routing-generation", String(expectedGeneration),
            "--expected-owner", owner.rawValue,
            "--installation-id", selection.installationID,
            "--installation-fingerprint", selection.installationFingerprint,
            "--native-restore-fingerprint", selection.nativeRestoreFingerprint,
            "--ocx-executable", selection.executable.path,
            "--ocx-sha256", selection.executable.sha256,
            "--json",
        ]
    }

    public func repair(
        expectedGeneration: UInt64,
        owner: NativeRepairKind,
        openCodexSelection: OpenCodexNativeRepairSelection?
    ) async throws -> NativeRoutingRepairReceipt {
        let arguments = try Self.repairArguments(
            expectedGeneration: expectedGeneration,
            owner: owner,
            selection: openCodexSelection
        )
        let output = try await execute(arguments: arguments, timeout: 90)
        do {
            let receipt = try JSONDecoder().decode(NativeRoutingRepairReceipt.self, from: output).validated()
            guard receipt.status.generation > expectedGeneration else { throw RelayctlError.invalidStatus }
            return receipt
        } catch let error as RelayctlError {
            throw error
        } catch {
            throw RelayctlError.invalidJSON
        }
    }

    static func repairArguments(
        expectedGeneration: UInt64,
        owner: NativeRepairKind,
        selection: OpenCodexNativeRepairSelection?
    ) throws -> [String] {
        guard expectedGeneration > 0,
              owner == .localRelay || owner == .openCodex,
              (owner == .openCodex) == (selection != nil) else {
            throw RelayctlError.invalidStatus
        }
        var arguments = [
            "mode", "repair-native-routing",
            "--expected-routing-generation", String(expectedGeneration),
            "--expected-owner", owner.rawValue,
            "--confirm-desktop-exited",
            "--confirm-local-development-native-routing-repair",
        ]
        if let selection {
            arguments.append(contentsOf: [
                "--installation-id", selection.installationID,
                "--installation-fingerprint", selection.installationFingerprint,
                "--native-restore-fingerprint", selection.nativeRestoreFingerprint,
                "--ocx-executable", selection.executable.path,
                "--ocx-sha256", selection.executable.sha256,
            ])
        }
        arguments.append("--json")
        return arguments
    }

    private func execute(arguments: [String], timeout: TimeInterval) async throws -> Data {
        guard executableURL.isFileURL,
              FileManager.default.isExecutableFile(atPath: executableURL.path) else {
            throw RelayctlError.helperUnavailable
        }
        let policy = RelayctlExecutionPolicy(
            timeout: max(executionPolicy.timeout, timeout),
            terminationGracePeriod: max(executionPolicy.terminationGracePeriod, 2),
            maximumOutputBytes: executionPolicy.maximumOutputBytes
        )
        let operation = RelayctlProcessOperation(
            executableURL: executableURL,
            arguments: arguments + additionalArguments,
            policy: policy
        )
        let result = try await withTaskCancellationHandler {
            try await Task.detached(priority: .utility) {
                try operation.run()
            }.value
        } onCancel: {
            operation.cancel()
        }
        if Task.isCancelled { throw RelayctlError.cancelled }
        if result.exitCode != 0 {
            if let envelope = try? JSONDecoder().decode(RelayctlOperationErrorEnvelope.self, from: result.stdout),
               let code = envelope.reportedCode() {
                throw RelayctlError.reported(code)
            }
            throw RelayctlError.invocationFailed(exitCode: result.exitCode)
        }
        return result.stdout
    }
}

public struct ProcessOpenCodexDiscoveryClient: OpenCodexDiscovering, Sendable {
    public let executableURL: URL
    public let additionalArguments: [String]
    public let executionPolicy: RelayctlExecutionPolicy

    public init(
        executableURL: URL,
        additionalArguments: [String] = [],
        executionPolicy: RelayctlExecutionPolicy = RelayctlExecutionPolicy(timeout: 25, maximumOutputBytes: 256 * 1024)
    ) {
        self.executableURL = executableURL
        self.additionalArguments = additionalArguments
        self.executionPolicy = executionPolicy
    }

    public func discover(tier: OpenCodexDiscoveryTier, broadScanApproved: Bool) async throws -> OpenCodexDiscoveryResult {
        guard executableURL.isFileURL,
              FileManager.default.isExecutableFile(atPath: executableURL.path) else {
            throw RelayctlError.helperUnavailable
        }
        var arguments = ["mode", "discover-open-codex", "--tier", tier.rawValue, "--json"]
        if tier == .c, broadScanApproved {
            arguments.append("--confirm-broad-scan")
        }
        arguments.append(contentsOf: additionalArguments)
        let policy = RelayctlExecutionPolicy(
            timeout: max(executionPolicy.timeout, tier == .c ? 25 : 10),
            terminationGracePeriod: executionPolicy.terminationGracePeriod,
            maximumOutputBytes: max(executionPolicy.maximumOutputBytes, 256 * 1024)
        )
        let operation = RelayctlProcessOperation(
            executableURL: executableURL,
            arguments: arguments,
            policy: policy
        )
        let result = try await withTaskCancellationHandler {
            try await Task.detached(priority: .utility) {
                try operation.run()
            }.value
        } onCancel: {
            operation.cancel()
        }
        if Task.isCancelled {
            throw RelayctlError.cancelled
        }
        if result.exitCode != 0 {
            if let envelope = try? JSONDecoder().decode(RelayctlOperationErrorEnvelope.self, from: result.stdout),
               let code = envelope.reportedCode() {
                throw RelayctlError.reported(code)
            }
            throw RelayctlError.invocationFailed(exitCode: result.exitCode)
        }
        do {
            return try JSONDecoder().decode(OpenCodexDiscoveryResult.self, from: result.stdout).validated()
        } catch let error as RelayctlError {
            throw error
        } catch {
            throw RelayctlError.invalidJSON
        }
    }
}

public struct ProcessOpenCodexRemovalClient: OpenCodexRemovalExecuting, Sendable {
    public let executableURL: URL
    public let relayConfig: String
    public let codexConfig: String
    public let inventoryExecutionPolicy: RelayctlExecutionPolicy
    public let removalExecutionPolicy: RelayctlExecutionPolicy

    public init(
        executableURL: URL,
        relayConfig: String,
        codexConfig: String,
        inventoryExecutionPolicy: RelayctlExecutionPolicy = RelayctlExecutionPolicy(
            timeout: 40,
            terminationGracePeriod: 2,
            maximumOutputBytes: 512 * 1024
        ),
        removalExecutionPolicy: RelayctlExecutionPolicy = RelayctlExecutionPolicy(
            timeout: 210,
            terminationGracePeriod: 2,
            maximumOutputBytes: 256 * 1024
        )
    ) {
        self.executableURL = executableURL
        self.relayConfig = relayConfig
        self.codexConfig = codexConfig
        self.inventoryExecutionPolicy = inventoryExecutionPolicy
        self.removalExecutionPolicy = removalExecutionPolicy
    }

    public func inspect(selection: OpenCodexRemovalSelection) async throws -> OpenCodexDataInventoryReceipt {
        try validateConfiguration()
        let output = try await execute(
            arguments: selection.inventoryArguments(relayConfig: relayConfig),
            policy: inventoryExecutionPolicy
        )
        do {
            return try JSONDecoder()
                .decode(OpenCodexDataInventoryReceipt.self, from: output)
                .validated(for: selection)
        } catch let error as OpenCodexRemovalContractError {
            throw error
        } catch {
            throw OpenCodexRemovalContractError.invalidInventoryReceipt
        }
    }

    public func remove(_ request: OpenCodexRemovalRequest) async throws -> OpenCodexRemovalReceipt {
        try validateConfiguration()
        let output = try await execute(
            arguments: request.removalArguments(relayConfig: relayConfig, codexConfig: codexConfig),
            policy: removalExecutionPolicy
        )
        do {
            return try JSONDecoder()
                .decode(OpenCodexRemovalReceipt.self, from: output)
                .validated(for: request)
        } catch let error as OpenCodexRemovalContractError {
            throw error
        } catch {
            throw OpenCodexRemovalContractError.invalidRemovalReceipt
        }
    }

    private func validateConfiguration() throws {
        guard executableURL.isFileURL,
              FileManager.default.isExecutableFile(atPath: executableURL.path),
              Self.isCanonicalAbsolutePath(relayConfig),
              Self.isCanonicalAbsolutePath(codexConfig) else {
            throw RelayctlError.helperUnavailable
        }
    }

    private func execute(arguments: [String], policy: RelayctlExecutionPolicy) async throws -> Data {
        let operation = RelayctlProcessOperation(
            executableURL: executableURL,
            arguments: arguments,
            policy: policy
        )
        let result = try await withTaskCancellationHandler {
            try await Task.detached(priority: .utility) {
                try operation.run()
            }.value
        } onCancel: {
            operation.cancel()
        }
        if Task.isCancelled {
            throw RelayctlError.cancelled
        }
        if result.exitCode != 0 {
            if let envelope = try? JSONDecoder().decode(RelayctlOperationErrorEnvelope.self, from: result.stdout),
               let code = envelope.reportedCode() {
                throw RelayctlError.reported(code)
            }
            throw RelayctlError.invocationFailed(exitCode: result.exitCode)
        }
        return result.stdout
    }

    private static func isCanonicalAbsolutePath(_ value: String) -> Bool {
        guard !value.isEmpty,
              value.utf8.count <= 4_096,
              value.hasPrefix("/"),
              !value.contains("\0") else {
            return false
        }
        return URL(fileURLWithPath: value).standardizedFileURL.path == value
    }
}

struct RelayctlProcessResult {
    let stdout: Data
    let exitCode: Int32
}

final class RelayctlProcessOperation: @unchecked Sendable {
    private enum StopReason {
        case cancelled
        case timedOut
        case outputTooLarge
    }

    private let executableURL: URL
    private let arguments: [String]
    private let standardInput: Data?
    private let policy: RelayctlExecutionPolicy
    private let lock = NSLock()
    private var process: Process?
    private var cancellationRequested = false

    init(
        executableURL: URL,
        arguments: [String],
        standardInput: Data? = nil,
        policy: RelayctlExecutionPolicy
    ) {
        self.executableURL = executableURL
        self.arguments = arguments
        self.standardInput = standardInput
        self.policy = policy
    }

    func cancel() {
        let running: Process?
        lock.lock()
        cancellationRequested = true
        running = process
        lock.unlock()
        // `terminate()` is SIGTERM. The runner grants the helper a short
        // grace period before safely reaping only that helper PID.
        if running?.isRunning == true {
            running?.terminate()
        }
    }

    func run() throws -> RelayctlProcessResult {
        if isCancellationRequested {
            throw RelayctlError.cancelled
        }

        let process = Process()
        let output = Pipe()
        let input = standardInput.map { _ in Pipe() }
        process.executableURL = executableURL
        process.arguments = arguments
        process.standardOutput = output
        process.standardInput = input ?? FileHandle.nullDevice
        // The MenuBar must never expose raw relayctl stderr. Sending it to the
        // null device also prevents an unbounded diagnostic stream from being
        // retained in this long-lived process.
        process.standardError = FileHandle.nullDevice

        install(process)
        defer { clear(process) }
        do {
            try process.run()
        } catch {
            throw RelayctlError.launchFailed
        }
        if let standardInput, let input {
            // The payload is bounded by the gateway client before launch.
            // Ignore EPIPE if relayctl rejects its flags before reading stdin;
            // the bounded JSON error envelope remains authoritative.
            try? input.fileHandleForWriting.write(contentsOf: standardInput)
            try? input.fileHandleForWriting.close()
        }

        let collector = RelayctlOutputCollector(
            handle: output.fileHandleForReading,
            maximumBytes: policy.maximumOutputBytes
        )
        let readers = DispatchGroup()
        readers.enter()
        DispatchQueue.global(qos: .utility).async {
            collector.drain()
            readers.leave()
        }

        let deadline = Date().addingTimeInterval(policy.timeout)
        var reason: StopReason?
        while process.isRunning {
            if isCancellationRequested {
                reason = .cancelled
                break
            }
            if collector.didOverflow {
                reason = .outputTooLarge
                break
            }
            if Date() >= deadline {
                reason = .timedOut
                break
            }
            Thread.sleep(forTimeInterval: 0.02)
        }

        if let reason {
            terminateAndReap(process)
            // Preserve the reason even if a concurrent cancellation arrives
            // after a timeout; cancellation takes precedence for the caller.
            if isCancellationRequested {
                self.closeReader(output.fileHandleForReading)
                _ = readers.wait(timeout: .now() + .milliseconds(100))
                throw RelayctlError.cancelled
            }
            self.closeReaderAfterProcessExit(output.fileHandleForReading, readers: readers)
            switch reason {
            case .cancelled:
                throw RelayctlError.cancelled
            case .timedOut:
                throw RelayctlError.timedOut
            case .outputTooLarge:
                throw RelayctlError.outputTooLarge
            }
        }

        // `isRunning == false` has been observed, but waitUntilExit performs
        // the framework's final reaping before reading the termination status.
        process.waitUntilExit()
        closeReaderAfterProcessExit(output.fileHandleForReading, readers: readers)
        if isCancellationRequested {
            throw RelayctlError.cancelled
        }
        if collector.didOverflow {
            throw RelayctlError.outputTooLarge
        }
        return RelayctlProcessResult(
            stdout: collector.data,
            exitCode: process.terminationStatus
        )
    }

    private func install(_ process: Process) {
        lock.lock()
        self.process = process
        lock.unlock()
    }

    private func clear(_ process: Process) {
        lock.lock()
        if self.process === process {
            self.process = nil
        }
        lock.unlock()
    }

    private var isCancellationRequested: Bool {
        lock.lock()
        defer { lock.unlock() }
        return cancellationRequested
    }

    private func terminateAndReap(_ process: Process) {
        if process.isRunning {
            process.terminate()
        }
        let deadline = Date().addingTimeInterval(policy.terminationGracePeriod)
        while process.isRunning, Date() < deadline {
            Thread.sleep(forTimeInterval: 0.01)
        }
        if process.isRunning, process.processIdentifier > 0 {
            // The timeout runner owns this exact Process PID. SIGKILL is only
            // a final reaping mechanism for relayctl, never for Codex Desktop.
            _ = Darwin.kill(process.processIdentifier, SIGKILL)
        }
        process.waitUntilExit()
    }

    private func closeReaderAfterProcessExit(_ handle: FileHandle, readers: DispatchGroup) {
        if readers.wait(timeout: .now() + .seconds(1)) == .timedOut {
            closeReader(handle)
            _ = readers.wait(timeout: .now() + .milliseconds(100))
        }
    }

    private func closeReader(_ handle: FileHandle) {
        handle.closeFile()
    }
}

private final class RelayctlOutputCollector: @unchecked Sendable {
    private let handle: FileHandle
    private let maximumBytes: Int
    private let lock = NSLock()
    private var stored = Data()
    private var overflow = false

    init(handle: FileHandle, maximumBytes: Int) {
        self.handle = handle
        self.maximumBytes = maximumBytes
    }

    func drain() {
        while true {
            let chunk = handle.availableData
            if chunk.isEmpty {
                return
            }
            lock.lock()
            if stored.count + chunk.count > maximumBytes {
                overflow = true
                let remaining = maximumBytes - stored.count
                if remaining > 0 {
                    stored.append(chunk.prefix(remaining))
                }
            } else {
                stored.append(chunk)
            }
            lock.unlock()
        }
    }

    var didOverflow: Bool {
        lock.lock()
        defer { lock.unlock() }
        return overflow
    }

    var data: Data {
        lock.lock()
        defer { lock.unlock() }
        return stored
    }
}

public enum RelayctlHelperLocation {
    /// Release bundles always use the signed helper at this fixed app-relative
    /// path. XCTest can inject an explicit development environment in debug
    /// builds, but a process environment never redirects a shipped app.
    public static func resolve(bundle: Bundle = .main, environment: [String: String] = [:]) -> URL {
#if DEBUG
        if let override = environment["OPENCODEX_RELAYCTL_PATH"],
           override.hasPrefix("/") {
            return URL(fileURLWithPath: override, isDirectory: false)
        }
#endif
        return bundle.bundleURL
            .resolvingSymlinksInPath()
            .appendingPathComponent("Contents/Library/Helpers/opencodex-relayctl", isDirectory: false)
    }
}
