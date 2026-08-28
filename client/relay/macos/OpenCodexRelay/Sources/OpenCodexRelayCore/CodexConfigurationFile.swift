import Darwin
import Foundation

public struct CodexConfigurationFileIdentity: Equatable, Sendable {
    public let device: UInt64
    public let inode: UInt64
    public let byteCount: UInt64
    public let modifiedSeconds: Int64
    public let modifiedNanoseconds: Int64

    fileprivate init(_ info: stat) {
        device = UInt64(info.st_dev)
        inode = UInt64(info.st_ino)
        byteCount = UInt64(info.st_size)
        modifiedSeconds = Int64(info.st_mtimespec.tv_sec)
        modifiedNanoseconds = Int64(info.st_mtimespec.tv_nsec)
    }
}

public struct CodexConfigurationMetadata: Equatable, Sendable {
    public let location: String
    public let byteCount: UInt64
    public let modifiedAt: Date
    public let identity: CodexConfigurationFileIdentity

    public var fileURL: URL {
        URL(fileURLWithPath: location, isDirectory: false)
    }

    fileprivate init(location: String, identity: CodexConfigurationFileIdentity) {
        self.location = location
        self.byteCount = identity.byteCount
        self.modifiedAt = Date(
            timeIntervalSince1970: TimeInterval(identity.modifiedSeconds) +
                (TimeInterval(identity.modifiedNanoseconds) / 1_000_000_000)
        )
        self.identity = identity
    }
}

public struct CodexConfigurationDocument: Equatable, Sendable {
    public let metadata: CodexConfigurationMetadata
    public let contents: String
}

public enum CodexConfigurationFileError: LocalizedError, Equatable, Sendable {
    case previewMode
    case bindingMissing
    case bindingUnsafe
    case bindingInvalid
    case missing
    case unsafeFile
    case readFailed
    case changedDuringRead
    case previewTooLarge
    case previewNotUTF8

    public var safeCode: String {
        switch self {
        case .previewMode: "config_preview_mode"
        case .bindingMissing: "config_binding_missing"
        case .bindingUnsafe: "config_binding_unsafe"
        case .bindingInvalid: "config_binding_invalid"
        case .missing: "config_file_missing"
        case .unsafeFile: "config_file_unsafe"
        case .readFailed: "config_read_failed"
        case .changedDuringRead: "config_file_changed"
        case .previewTooLarge: "config_preview_too_large"
        case .previewNotUTF8: "config_preview_not_utf8"
        }
    }

    public var errorDescription: String? { safeCode }
}

public protocol CodexConfigurationReading: Sendable {
    func inspect(bindingURL: URL) throws -> CodexConfigurationMetadata
    func readDocument(bindingURL: URL) throws -> CodexConfigurationDocument
}

/// Reads only the Codex configuration selected by the installer-owned routing
/// binding. The binding is reloaded for every operation; the target is opened
/// without following a symbolic link and its identity is compared before and
/// after inspection so atomic replacement and deletion are observed safely.
public struct SecureCodexConfigurationReader: CodexConfigurationReading, Sendable {
    public static let maximumPreviewBytes: UInt64 = 1024 * 1024

    private let postOpenHook: (@Sendable () -> Void)?
    private let runtimeMode: RelayRuntimeMode

    public init(runtimeMode: RelayRuntimeMode = .current) {
        postOpenHook = nil
        self.runtimeMode = runtimeMode
    }

    init(
        runtimeMode: RelayRuntimeMode = .current,
        postOpenHook: @escaping @Sendable () -> Void
    ) {
        self.postOpenHook = postOpenHook
        self.runtimeMode = runtimeMode
    }

    public func inspect(bindingURL: URL) throws -> CodexConfigurationMetadata {
        try withValidatedFile(bindingURL: bindingURL) { _, location, identity in
            CodexConfigurationMetadata(location: location, identity: identity)
        }
    }

    public func readDocument(bindingURL: URL) throws -> CodexConfigurationDocument {
        try withValidatedFile(bindingURL: bindingURL) { descriptor, location, identity in
            guard identity.byteCount <= Self.maximumPreviewBytes else {
                throw CodexConfigurationFileError.previewTooLarge
            }

            var payload = Data()
            payload.reserveCapacity(Int(identity.byteCount))
            var buffer = [UInt8](repeating: 0, count: 16 * 1024)
            while true {
                let count = Darwin.read(descriptor, &buffer, buffer.count)
                if count == 0 { break }
                if count < 0 {
                    if errno == EINTR { continue }
                    throw CodexConfigurationFileError.readFailed
                }
                let byteCount = Int(count)
                guard payload.count + byteCount <= Self.maximumPreviewBytes else {
                    throw CodexConfigurationFileError.previewTooLarge
                }
                payload.append(contentsOf: buffer.prefix(byteCount))
            }

            guard UInt64(payload.count) == identity.byteCount else {
                throw CodexConfigurationFileError.changedDuringRead
            }
            guard let contents = String(data: payload, encoding: .utf8) else {
                throw CodexConfigurationFileError.previewNotUTF8
            }
            return CodexConfigurationDocument(
                metadata: CodexConfigurationMetadata(location: location, identity: identity),
                contents: contents
            )
        }
    }

    private func withValidatedFile<T>(
        bindingURL: URL,
        body: (Int32, String, CodexConfigurationFileIdentity) throws -> T
    ) throws -> T {
        guard runtimeMode == .managed else {
            throw CodexConfigurationFileError.previewMode
        }
        let binding: RoutingBinding
        do {
            binding = try RoutingBindingReader.load(at: bindingURL)
        } catch let error as RoutingBindingError {
            switch error {
            case .missing:
                throw CodexConfigurationFileError.bindingMissing
            case .unsafeFile:
                throw CodexConfigurationFileError.bindingUnsafe
            case .malformed:
                throw CodexConfigurationFileError.bindingInvalid
            }
        } catch {
            throw CodexConfigurationFileError.bindingInvalid
        }

        let location = binding.codexConfig
        var pathInfo = stat()
        guard Darwin.lstat(location, &pathInfo) == 0 else {
            if errno == ENOENT {
                throw CodexConfigurationFileError.missing
            }
            throw CodexConfigurationFileError.unsafeFile
        }
        try validateRegularFile(pathInfo)

        let descriptor = Darwin.open(location, O_RDONLY | O_NOFOLLOW)
        guard descriptor >= 0 else {
            if errno == ENOENT {
                throw CodexConfigurationFileError.missing
            }
            throw CodexConfigurationFileError.unsafeFile
        }
        defer { _ = Darwin.close(descriptor) }

        var openedInfo = stat()
        guard Darwin.fstat(descriptor, &openedInfo) == 0 else {
            throw CodexConfigurationFileError.readFailed
        }
        try validateRegularFile(openedInfo)
        guard sameFile(pathInfo, openedInfo) else {
            throw CodexConfigurationFileError.changedDuringRead
        }
        let openedIdentity = CodexConfigurationFileIdentity(openedInfo)

        postOpenHook?()
        let result = try body(descriptor, location, openedIdentity)

        var finalOpenedInfo = stat()
        guard Darwin.fstat(descriptor, &finalOpenedInfo) == 0 else {
            throw CodexConfigurationFileError.readFailed
        }
        try validateRegularFile(finalOpenedInfo)

        var finalPathInfo = stat()
        guard Darwin.lstat(location, &finalPathInfo) == 0 else {
            throw CodexConfigurationFileError.changedDuringRead
        }
        try validateRegularFile(finalPathInfo)
        guard CodexConfigurationFileIdentity(finalOpenedInfo) == openedIdentity,
              CodexConfigurationFileIdentity(finalPathInfo) == openedIdentity else {
            throw CodexConfigurationFileError.changedDuringRead
        }
        return result
    }

    private func validateRegularFile(_ info: stat) throws {
        guard (info.st_mode & S_IFMT) == S_IFREG, info.st_size >= 0 else {
            throw CodexConfigurationFileError.unsafeFile
        }
    }

    private func sameFile(_ lhs: stat, _ rhs: stat) -> Bool {
        lhs.st_dev == rhs.st_dev && lhs.st_ino == rhs.st_ino
    }
}
