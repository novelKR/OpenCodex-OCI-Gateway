import Darwin
import Foundation

/// The installer-owned, non-secret binding that prevents the MenuBar app from
/// silently falling back to a different user's default relay or Codex config.
/// The file stores paths only; it must never contain credentials or URLs.
public struct RoutingBinding: Codable, Equatable, Sendable {
    public static let schemaVersion = 1

    public let schema: Int
    public let relayConfig: String
    public let codexConfig: String

    public init(schema: Int = RoutingBinding.schemaVersion, relayConfig: String, codexConfig: String) {
        self.schema = schema
        self.relayConfig = relayConfig
        self.codexConfig = codexConfig
    }

    enum CodingKeys: String, CodingKey {
        case schema
        case relayConfig = "relay_config"
        case codexConfig = "codex_config"
    }

    public var relayctlArguments: [String] {
        ["--config", relayConfig, "--codex-config", codexConfig]
    }
}

public enum RoutingBindingError: LocalizedError, Equatable, Sendable {
    case missing
    case unsafeFile
    case malformed

    public var safeCode: String {
        switch self {
        case .missing:
            return "routing_binding_missing"
        case .unsafeFile:
            return "routing_binding_unsafe"
        case .malformed:
            return "routing_binding_invalid"
        }
    }

    public var safeMessage: String {
        switch self {
        case .missing:
            return "Relay setup for this user is incomplete. Go to Settings > Connect a self-hosted server and choose Prepare Relay. Use Recover setup only when recovery is required."
        case .unsafeFile:
            return "Routing binding failed its local security checks. Routing is unavailable."
        case .malformed:
            return "Routing binding is invalid. Routing is unavailable."
        }
    }

    public var errorDescription: String? { safeMessage }
}

public enum RoutingBindingReader {
    public static let maximumBytes = 16 * 1024

    /// This exact location is intentionally not configurable at runtime. The
    /// installer owns its atomic creation, backup, and rollback transaction.
    public static func defaultURL(fileManager: FileManager = .default) -> URL {
        fileManager.homeDirectoryForCurrentUser
            .appendingPathComponent(DistributionFlavor.current.routingBindingRelativePath, isDirectory: false)
    }

    public static func load(at url: URL = defaultURL()) throws -> RoutingBinding {
        let payload = try readOwnerOnlyRegularFile(at: url)
        return try decode(payload)
    }

    private static func readOwnerOnlyRegularFile(at url: URL) throws -> Data {
        guard url.isFileURL else {
            throw RoutingBindingError.unsafeFile
        }
        let path = url.path
        var lstatInfo = stat()
        guard Darwin.lstat(path, &lstatInfo) == 0 else {
            if errno == ENOENT {
                throw RoutingBindingError.missing
            }
            throw RoutingBindingError.unsafeFile
        }
        try validateOwnerOnlyRegularFile(lstatInfo)

        let descriptor = Darwin.open(path, O_RDONLY | O_NOFOLLOW)
        guard descriptor >= 0 else {
            throw RoutingBindingError.unsafeFile
        }
        defer { _ = Darwin.close(descriptor) }

        var openedInfo = stat()
        guard Darwin.fstat(descriptor, &openedInfo) == 0 else {
            throw RoutingBindingError.unsafeFile
        }
        try validateOwnerOnlyRegularFile(openedInfo)
        guard openedInfo.st_dev == lstatInfo.st_dev, openedInfo.st_ino == lstatInfo.st_ino else {
            throw RoutingBindingError.unsafeFile
        }

        var payload = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while true {
            let count = Darwin.read(descriptor, &buffer, buffer.count)
            if count == 0 {
                return payload
            }
            if count < 0 {
                throw RoutingBindingError.unsafeFile
            }
            let byteCount = Int(count)
            guard payload.count + byteCount <= maximumBytes else {
                throw RoutingBindingError.malformed
            }
            payload.append(contentsOf: buffer.prefix(byteCount))
        }
    }

    private static func validateOwnerOnlyRegularFile(_ info: stat) throws {
        guard (info.st_mode & S_IFMT) == S_IFREG,
              info.st_uid == geteuid(),
              (info.st_mode & 0o777) == 0o600 else {
            throw RoutingBindingError.unsafeFile
        }
    }

    private static func decode(_ payload: Data) throws -> RoutingBinding {
        guard !payload.isEmpty,
              let object = try? JSONSerialization.jsonObject(with: payload),
              let dictionary = object as? [String: Any],
              Set(dictionary.keys) == Set(["schema", "relay_config", "codex_config"]),
              let binding = try? JSONDecoder().decode(RoutingBinding.self, from: payload),
              binding.schema == RoutingBinding.schemaVersion,
              isCanonicalAbsolutePath(binding.relayConfig),
              isCanonicalAbsolutePath(binding.codexConfig) else {
            throw RoutingBindingError.malformed
        }
        return binding
    }

    private static func isCanonicalAbsolutePath(_ path: String) -> Bool {
        guard !path.isEmpty,
              path.utf8.count <= 4096,
              path.hasPrefix("/"),
              !path.contains("\0") else {
            return false
        }
        return URL(fileURLWithPath: path).standardizedFileURL.path == path
    }
}
