import AppKit
import SwiftUI
import OpenCodexRelayLocalization

private enum RelayActivityLogFilter: String, CaseIterable, Identifiable {
    case all
    case debug
    case info
    case warning
    case error

    var id: String { rawValue }
}

struct RelayActivityLogView: View {
    @ObservedObject var store: RelayActivityLogStore
    let localizer: AppLocalizer

    @State private var filter: RelayActivityLogFilter = .all
    @State private var showsClearConfirmation = false
    @State private var searchText = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            VStack(alignment: .leading, spacing: 8) {
                Label(localizer.text(.activityLogCurrentSession), systemImage: "clock")
                Label(localizer.text(.activityLogPrivacy), systemImage: "hand.raised")

                DisclosureGroup(localizer.text(.activityLogUnifiedQuery)) {
                    Text(store.unifiedLogCommand)
                        .font(.system(.caption, design: .monospaced))
                        .textSelection(.enabled)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            .font(.callout)
            .foregroundStyle(.secondary)
            .frame(maxWidth: .infinity, alignment: .leading)

            ControlCenterSectionCard(
                localizer.text(.activityLogEvents),
                systemImage: "list.bullet.rectangle"
            ) {
                VStack(alignment: .leading, spacing: 12) {
                    TextField(localizer.text(.activityLogSearch), text: $searchText)
                        .textFieldStyle(.roundedBorder)

                    ViewThatFits(in: .horizontal) {
                        HStack(spacing: 8) {
                            filters
                            Spacer(minLength: 0)
                            exportMenu
                            Button(role: .destructive) {
                                showsClearConfirmation = true
                            } label: {
                                Label(localizer.text(.activityLogClear), systemImage: "trash")
                            }
                        }
                        VStack(alignment: .leading, spacing: 8) {
                            filters
                            exportMenu
                            Button(localizer.text(.activityLogClear), role: .destructive) {
                                showsClearConfirmation = true
                            }
                        }
                    }

                    Text(localizer.text(
                        .activityLogEventCount,
                        localizer.formattedNumber(filteredEvents.count)
                    ))
                        .font(.caption)
                        .foregroundStyle(.secondary)

                    Divider()

                    if filteredEvents.isEmpty {
                        ContentUnavailableView(
                            localizer.text(.activityLogEmpty),
                            systemImage: "list.bullet.rectangle"
                        )
                        .frame(maxWidth: .infinity)
                        .padding(.vertical, 20)
                    } else {
                        LazyVStack(alignment: .leading, spacing: 0) {
                            ForEach(Array(filteredEvents.reversed())) { event in
                                RelayActivityLogRow(event: event, localizer: localizer)
                                if event.id != filteredEvents.first?.id {
                                    Divider()
                                }
                            }
                        }
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.vertical, 4)
        .alert(
            localizer.text(.activityLogClearConfirmationTitle),
            isPresented: $showsClearConfirmation
        ) {
            Button(localizer.text(.activityLogClear), role: .destructive) {
                store.clear()
            }
            Button(localizer.text(.controlCenterNativeRepairCancel), role: .cancel) {}
        } message: {
            Text(localizer.text(.activityLogClearConfirmationDetail))
        }
            }
        }
    }

    private var filters: some View {
        Picker(localizer.text(.activityLogFilter), selection: $filter) {
            ForEach(RelayActivityLogFilter.allCases) { option in
                Text(filterTitle(option)).tag(option)
            }
        }
        .pickerStyle(.menu)
        .frame(maxWidth: 180, alignment: .leading)
    }

    private var exportMenu: some View {
        Menu {
            Button {
                copy(store.jsonLines(for: filteredEvents))
                store.record(category: .operation, code: "activity_log_json_copied")
            } label: {
                Label(localizer.text(.activityLogCopyJSONL), systemImage: "doc.on.doc")
            }
            .disabled(filteredEvents.isEmpty)

            Button {
                copy(store.unifiedLogCommand)
                store.record(category: .operation, code: "activity_log_query_copied")
            } label: {
                Label(localizer.text(.activityLogCopyQuery), systemImage: "terminal")
            }
        } label: {
            Label(localizer.text(.activityLogExport), systemImage: "square.and.arrow.up")
        }
    }

    private var filteredEvents: [RelayActivityLogEvent] {
        store.events.filter { event in
            let matchesLevel = filter == .all || event.level.rawValue == filter.rawValue
            let query = searchText.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
            guard !query.isEmpty else { return matchesLevel }
            let metadata = event.fields
                .sorted { $0.key < $1.key }
                .map { "\($0.key)=\($0.value)" }
                .joined(separator: " ")
            return matchesLevel && [
                event.level.rawValue,
                event.category.rawValue,
                event.code,
                metadata,
            ].contains { $0.lowercased().contains(query) }
        }
    }

    private func filterTitle(_ filter: RelayActivityLogFilter) -> String {
        switch filter {
        case .all: localizer.text(.activityLogFilterAll)
        case .debug: localizer.text(.activityLogLevelDebug)
        case .info: localizer.text(.activityLogLevelInfo)
        case .warning: localizer.text(.activityLogLevelWarning)
        case .error: localizer.text(.activityLogLevelError)
        }
    }

    private func copy(_ value: String) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(value, forType: .string)
    }
}

private struct RelayActivityLogRow: View {
    let event: RelayActivityLogEvent
    let localizer: AppLocalizer

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: iconName)
                .foregroundStyle(levelColor)
                .frame(width: 18)
                .accessibilityLabel(levelTitle)

            VStack(alignment: .leading, spacing: 4) {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Text(localizer.formattedDate(event.timestamp))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Text(event.category.rawValue)
                        .font(.system(.caption, design: .monospaced))
                        .foregroundStyle(.secondary)
                }

                Text(event.code)
                    .font(.system(.body, design: .monospaced).weight(.semibold))
                    .textSelection(.enabled)

                if !event.fields.isEmpty {
                    Text(event.fields
                        .sorted { $0.key < $1.key }
                        .map { "\($0.key)=\($0.value)" }
                        .joined(separator: "  "))
                        .font(.system(.caption, design: .monospaced))
                        .foregroundStyle(.secondary)
                        .textSelection(.enabled)
                        .fixedSize(horizontal: false, vertical: true)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .padding(.vertical, 8)
    }

    private var iconName: String {
        switch event.level {
        case .debug: "ladybug"
        case .info: "info.circle"
        case .warning: "exclamationmark.triangle"
        case .error: "xmark.octagon"
        }
    }

    private var levelColor: Color {
        switch event.level {
        case .debug: .secondary
        case .info: .blue
        case .warning: .orange
        case .error: .red
        }
    }

    private var levelTitle: String {
        switch event.level {
        case .debug: localizer.text(.activityLogLevelDebug)
        case .info: localizer.text(.activityLogLevelInfo)
        case .warning: localizer.text(.activityLogLevelWarning)
        case .error: localizer.text(.activityLogLevelError)
        }
    }
}
