import Foundation

public struct DesktopTarget: Codable, Equatable, Sendable {
    public let path: String
    public let bookmark: Data?

    public init(url: URL, bookmark: Data? = nil) {
        self.path = url.resolvingSymlinksInPath().path
        self.bookmark = bookmark
    }

    public var displayName: String {
        URL(fileURLWithPath: path).deletingPathExtension().lastPathComponent
    }
}

public protocol DesktopTargetStoring: AnyObject {
    var desktopTarget: DesktopTarget? { get set }
}

public final class UserDefaultsDesktopTargetStore: DesktopTargetStoring {
    private let defaults: UserDefaults
    private let key = "OpenCodexRelay.desktop-target.v1"

    public init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
    }

    public var desktopTarget: DesktopTarget? {
        get {
            guard let data = defaults.data(forKey: key) else { return nil }
            return try? JSONDecoder().decode(DesktopTarget.self, from: data)
        }
        set {
            if let value = newValue, let data = try? JSONEncoder().encode(value) {
                defaults.set(data, forKey: key)
            } else {
                defaults.removeObject(forKey: key)
            }
        }
    }
}

public enum DesktopTargetError: LocalizedError, Equatable {
    case notApplicationBundle
    case unavailable(String)

    public var errorDescription: String? {
        switch self {
        case .notApplicationBundle:
            return "Choose an existing .app bundle."
        case let .unavailable(path):
            return "The registered Codex Desktop app is unavailable: \(path)"
        }
    }
}

public enum DesktopTargetResolver {
    public static func validate(_ url: URL) throws -> URL {
        let resolved = url.resolvingSymlinksInPath()
        var isDirectory: ObjCBool = false
        guard resolved.pathExtension.lowercased() == "app",
              FileManager.default.fileExists(atPath: resolved.path, isDirectory: &isDirectory),
              isDirectory.boolValue else {
            throw DesktopTargetError.notApplicationBundle
        }
        return resolved
    }

    public static func resolve(_ target: DesktopTarget) throws -> URL {
        if let bookmark = target.bookmark {
            var stale = false
            let bookmarkedURL = try URL(
                resolvingBookmarkData: bookmark,
                options: [.withoutUI, .withoutMounting],
                relativeTo: nil,
                bookmarkDataIsStale: &stale
            )
            let resolved = try validate(bookmarkedURL)
            return resolved
        }
        do {
            return try validate(URL(fileURLWithPath: target.path))
        } catch {
            throw DesktopTargetError.unavailable(target.path)
        }
    }
}
