import CryptoKit
import Darwin
import Foundation

/// The two bounded, user-confirmed OpenCodex lifecycle actions understood by
/// the bundled relayctl helper. The MenuBar never invokes `ocx` itself.
public enum OpenCodexHandoffAction: String, Codable, CaseIterable, Sendable {
    /// Keep the local OpenCodex proxy, but remove its Codex integration and
    /// Codex Shim ownership before the relay takes over routing.
    case retainProxyRemoveShim = "retain_proxy_remove_shim"
    /// Keep the local OpenCodex proxy and the existing Codex Shim.
    case retainProxyKeepShim = "retain_proxy_keep_shim"

    public var displayName: String {
        switch self {
        case .retainProxyRemoveShim:
            return "Keep proxy; remove Codex integration and Shim"
        case .retainProxyKeepShim:
            return "Keep proxy and Codex Shim"
        }
    }

    public var detail: String {
        switch self {
        case .retainProxyRemoveShim:
            return "The local proxy remains available for the relay, while OpenCodex releases Codex routing and Shim ownership."
        case .retainProxyKeepShim:
            return "The local proxy and existing Codex Shim remain in place. Only the requested OpenCodex integration handoff is performed."
        }
    }
}

/// A user-selected, canonical OpenCodex executable plus the bounded
/// fingerprint observed at selection time. It is intentionally ephemeral: the
/// app does not persist an executable path or attempt PATH discovery.
public struct OpenCodexExecutable: Codable, Equatable, Sendable {
    public let path: String
    public let sha256: String

    public init(path: String, sha256: String) throws {
        guard OpenCodexExecutableResolver.isCanonicalAbsolutePath(path),
              OpenCodexExecutableResolver.isSHA256(sha256) else {
            throw OpenCodexExecutableError.invalidSelection
        }
        self.path = path
        self.sha256 = sha256
    }

    enum CodingKeys: String, CodingKey {
        case path
        case sha256
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        try self.init(
            path: container.decode(String.self, forKey: .path),
            sha256: container.decode(String.self, forKey: .sha256)
        )
    }
}

/// These errors are intentionally safe to display. In particular, they never
/// include a selected filesystem path or an OS error string.
public enum OpenCodexExecutableError: LocalizedError, Equatable, Sendable {
    case invalidSelection
    case unavailable
    case tooLarge
    case changed

    public var safeCode: String {
        switch self {
        case .invalidSelection:
            return "ocx_selection_invalid"
        case .unavailable:
            return "ocx_selection_unavailable"
        case .tooLarge:
            return "ocx_selection_too_large"
        case .changed:
            return "ocx_selection_changed"
        }
    }

    public var safeMessage: String {
        switch self {
        case .invalidSelection:
            return "Choose an existing OpenCodex executable."
        case .unavailable:
            return "The selected OpenCodex executable is no longer available. Choose it again."
        case .tooLarge:
            return "The selected OpenCodex executable is too large to verify safely."
        case .changed:
            return "The selected OpenCodex executable changed after selection. Choose it again before handoff."
        }
    }

    public var errorDescription: String? { safeMessage }
}

/// A small, testable selection boundary for the handoff UI. It never runs the
/// executable. relayctl independently validates the executable immediately
/// before carrying out a confirmed handoff.
public enum OpenCodexExecutableResolver {
    public static let maximumBytes = 128 * 1024 * 1024

    public static func select(_ url: URL) throws -> OpenCodexExecutable {
        let canonicalURL = try canonicalExecutableURL(url)
        let sha256 = try fingerprint(at: canonicalURL)
        return try OpenCodexExecutable(path: canonicalURL.path, sha256: sha256)
    }

    /// Re-read the canonical regular executable after the user has confirmed
    /// the sheet. A changed binary fails closed rather than being handed to
    /// relayctl under an earlier selection's identity.
    public static func revalidate(_ executable: OpenCodexExecutable) throws -> OpenCodexExecutable {
        let canonicalURL = try canonicalExecutableURL(URL(fileURLWithPath: executable.path))
        guard canonicalURL.path == executable.path else {
            throw OpenCodexExecutableError.changed
        }
        let sha256 = try fingerprint(at: canonicalURL)
        guard sha256 == executable.sha256 else {
            throw OpenCodexExecutableError.changed
        }
        return executable
    }

    fileprivate static func isCanonicalAbsolutePath(_ path: String) -> Bool {
        guard path.hasPrefix("/"), path != "/", !path.contains("\0") else {
            return false
        }
        let url = URL(fileURLWithPath: path)
        return url.standardizedFileURL.path == path && url.resolvingSymlinksInPath().path == path
    }

    fileprivate static func isSHA256(_ value: String) -> Bool {
        guard value.count == 64 else { return false }
        return value.unicodeScalars.allSatisfy {
            (48...57).contains($0.value) || (97...102).contains($0.value)
        }
    }

    private static func canonicalExecutableURL(_ url: URL) throws -> URL {
        guard url.isFileURL else {
            throw OpenCodexExecutableError.invalidSelection
        }
        let canonical = url.standardizedFileURL.resolvingSymlinksInPath()
        guard isCanonicalAbsolutePath(canonical.path) else {
            throw OpenCodexExecutableError.invalidSelection
        }
        var metadata = stat()
        guard Darwin.lstat(canonical.path, &metadata) == 0 else {
            throw OpenCodexExecutableError.unavailable
        }
        guard (metadata.st_mode & S_IFMT) == S_IFREG,
              (metadata.st_mode & 0o111) != 0 else {
            throw OpenCodexExecutableError.invalidSelection
        }
        return canonical
    }

    private static func fingerprint(at url: URL) throws -> String {
        let handle: FileHandle
        do {
            handle = try FileHandle(forReadingFrom: url)
        } catch {
            throw OpenCodexExecutableError.unavailable
        }
        defer { try? handle.close() }

        var hasher = SHA256()
        var bytesRead = 0
        do {
            while let chunk = try handle.read(upToCount: 64 * 1024), !chunk.isEmpty {
                bytesRead += chunk.count
                guard bytesRead <= maximumBytes else {
                    throw OpenCodexExecutableError.tooLarge
                }
                hasher.update(data: chunk)
            }
        } catch let error as OpenCodexExecutableError {
            throw error
        } catch {
            throw OpenCodexExecutableError.unavailable
        }
        return hasher.finalize().map { String(format: "%02x", $0) }.joined()
    }
}
