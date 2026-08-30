import Foundation
import OSLog

enum RelayActivityLogLevel: String, CaseIterable, Codable, Sendable {
    case debug
    case info
    case warning
    case error

    var osLogType: OSLogType {
        switch self {
        case .debug: .debug
        case .info: .info
        case .warning: .default
        case .error: .error
        }
    }
}

enum RelayActivityLogCategory: String, Codable, Sendable {
    case lifecycle
    case window
    case status
    case refresh
    case operation
    case discovery
    case removal
    case handoff
    case repair
}

struct RelayActivityLogEvent: Identifiable, Codable, Equatable, Sendable {
    let sequence: UInt64
    let timestamp: Date
    let level: RelayActivityLogLevel
    let category: RelayActivityLogCategory
    let code: String
    let fields: [String: String]

    var id: UInt64 { sequence }
}

/// A bounded, current-session diagnostic projection for the Control Center.
///
/// Callers may supply only finite status codes and token-like metadata. Paths,
/// command output, prompts, identifiers, error descriptions, and credentials
/// are rejected rather than heuristically redacted.
@MainActor
final class RelayActivityLogStore: ObservableObject {
    private static let allowedFieldKeys: Set<String> = [
        "action", "active_requests", "active_space", "adapter_id", "applied_backend",
        "automatic", "catalog", "catalog_refresh", "changed", "count", "desired_backend",
        "desktop_restart_required", "desktop_target", "destination", "distribution", "drain",
        "backend", "backup_created", "command_kind", "failure_code", "generation", "handoff_phase", "height", "key",
        "local_opencodex", "local_relay", "message_code", "mode", "model_catalog_json",
        "nonrouting_cleanup_incomplete", "openai_base_url", "owner", "owner_restore_attempts", "owner_restore_result", "phase", "repair_phase",
        "configuration", "integration", "retry_exhausted",
        "relay_admission", "relay_running", "remote_gateway", "result_code", "runtime_mode",
        "routing_sync", "schema", "section", "tier", "visible", "width", "version", "manager", "reason",
        "teardown_capability", "data_preserved",
    ]

    @Published private(set) var events: [RelayActivityLogEvent] = []

    let subsystem: String
    let capacity: Int

    private let logger: Logger
    private var nextSequence: UInt64 = 1

    init(
        subsystem: String = Bundle.main.bundleIdentifier ?? "io.github.novelkr.opencodex-relay",
        capacity: Int = 500
    ) {
        self.subsystem = subsystem
        self.capacity = max(1, capacity)
        self.logger = Logger(subsystem: subsystem, category: "Activity")
    }

    func record(
        _ level: RelayActivityLogLevel = .info,
        category: RelayActivityLogCategory,
        code: String,
        fields: [String: String] = [:],
        timestamp: Date = Date()
    ) {
        guard Self.isSafeToken(code) else { return }

        let safeFields = fields.reduce(into: [String: String]()) { result, pair in
            guard Self.allowedFieldKeys.contains(pair.key),
                  Self.isSafeValue(pair.value) else { return }
            result[pair.key] = pair.value
        }
        let event = RelayActivityLogEvent(
            sequence: nextSequence,
            timestamp: timestamp,
            level: level,
            category: category,
            code: code,
            fields: safeFields
        )
        nextSequence &+= 1
        events.append(event)
        if events.count > capacity {
            events.removeFirst(events.count - capacity)
        }

        let metadata = event.fields
            .sorted { $0.key < $1.key }
            .map { "\($0.key)=\($0.value)" }
            .joined(separator: " ")
        logger.log(
            level: level.osLogType,
            "\(event.category.rawValue, privacy: .public) \(event.code, privacy: .public) \(metadata, privacy: .public)"
        )
    }

    func clear() {
        events.removeAll(keepingCapacity: true)
        record(category: .operation, code: "activity_log_cleared")
    }

    func jsonLines(for source: [RelayActivityLogEvent]? = nil) -> String {
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        encoder.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        return (source ?? events).compactMap { event in
            guard let data = try? encoder.encode(event) else { return nil }
            return String(data: data, encoding: .utf8)
        }
        .joined(separator: "\n")
    }

    var unifiedLogCommand: String {
        "/usr/bin/log show --last 1h --style json --info --debug --predicate 'subsystem == \"\(subsystem)\" && category == \"Activity\"'"
    }

    private static func isSafeToken(_ value: String) -> Bool {
        guard !value.isEmpty, value.utf8.count <= 64 else { return false }
        return value.unicodeScalars.allSatisfy {
            CharacterSet.lowercaseLetters.contains($0) ||
            CharacterSet.decimalDigits.contains($0) ||
            $0 == "_"
        }
    }

    private static func isSafeValue(_ value: String) -> Bool {
        guard !value.isEmpty, value.utf8.count <= 128 else { return false }
        return value.unicodeScalars.allSatisfy {
            CharacterSet.alphanumerics.contains($0) ||
            $0 == "_" || $0 == "." || $0 == ":" || $0 == "-"
        }
    }
}
