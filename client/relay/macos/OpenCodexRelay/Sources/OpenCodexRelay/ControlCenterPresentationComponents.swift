import SwiftUI
import OpenCodexRelayCore
import OpenCodexRelayLocalization

enum ControlCenterPresentationMetrics {
    static let pageMaximumWidth: CGFloat = 800
    static let statusLabelWidth: CGFloat = 180
    static let pageSpacing: CGFloat = 24
    static let actionSpacing: CGFloat = 8
    static let horizontalPageInset: CGFloat = 28
    static let verticalPageInset: CGFloat = 24
    static let cardContentInset: CGFloat = 16
    static let statusRowMinimumHeight: CGFloat = 42
}

enum ControlCenterStatusTone: Equatable {
    case neutral
    case info
    case success
    case warning
    case error

    static func messageTone(
        for message: SafeStatusMessage,
        statusError: SafeStatusMessage?
    ) -> Self {
        statusError == message ? .error : .neutral
    }

    static func messageTone(
        for message: SafeStatusMessage,
        statusError: SafeStatusMessage?,
        integrationMessage: SafeStatusMessage?,
        integrationAvailability: RelayIntegrationAvailability
    ) -> Self {
        if message == integrationMessage {
            return integrationTone(for: integrationAvailability)
        }
        return statusError == message ? .error : .neutral
    }

    static func integrationTone(
        for availability: RelayIntegrationAvailability
    ) -> Self {
        switch availability {
        case .ready: .neutral
        case .preview: .info
        case .missing: .warning
        case .unsafe, .invalid, .helperUnavailable: .error
        }
    }

    var color: Color {
        switch self {
        case .neutral: .secondary
        case .info: .blue
        case .success: .green
        case .warning: .orange
        case .error: .red
        }
    }

    var systemImage: String {
        switch self {
        case .neutral: "info.circle"
        case .info: "info.circle.fill"
        case .success: "checkmark.circle.fill"
        case .warning: "exclamationmark.triangle.fill"
        case .error: "xmark.octagon.fill"
        }
    }
}

enum ControlCenterNoticePresentation {
    static func presents(
        _ message: SafeStatusMessage,
        integrationMessage: SafeStatusMessage?,
        integrationAvailability: RelayIntegrationAvailability,
        handlesMissingIntegrationInline: Bool
    ) -> Bool {
        guard handlesMissingIntegrationInline,
              integrationAvailability == .missing,
              message == integrationMessage else {
            return true
        }
        return false
    }
}

struct ControlCenterPage<Content: View>: View {
    let title: String
    let systemImage: String
    @ObservedObject var model: MenuBarModel
    let localizer: AppLocalizer
    var handlesMissingIntegrationInline = false
    @ViewBuilder let content: Content

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: ControlCenterPresentationMetrics.pageSpacing) {
                Label(title, systemImage: systemImage)
                    .font(.title2.weight(.bold))
                    .foregroundStyle(.primary)
                    .accessibilityAddTraits(.isHeader)

                if let integrationMessage = model.integrationStatusMessage,
                   integrationMessage != model.message,
                   integrationMessage != model.statusError,
                   ControlCenterNoticePresentation.presents(
                       integrationMessage,
                       integrationMessage: model.integrationStatusMessage,
                       integrationAvailability: model.integrationAvailability,
                       handlesMissingIntegrationInline: handlesMissingIntegrationInline
                   ) {
                    ControlCenterNotice(
                        tone: .integrationTone(for: model.integrationAvailability)
                    ) {
                        SafeStatusMessageView(message: integrationMessage, localizer: localizer)
                    }
                }

                if let message = model.message,
                   ControlCenterNoticePresentation.presents(
                       message,
                       integrationMessage: model.integrationStatusMessage,
                       integrationAvailability: model.integrationAvailability,
                       handlesMissingIntegrationInline: handlesMissingIntegrationInline
                   ) {
                    ControlCenterNotice(
                        tone: .messageTone(
                            for: message,
                            statusError: model.statusError,
                            integrationMessage: model.integrationStatusMessage,
                            integrationAvailability: model.integrationAvailability
                        )
                    ) {
                        SafeStatusMessageView(message: message, localizer: localizer)
                    }
                }

                if let statusError = model.statusError,
                   statusError != model.message,
                   ControlCenterNoticePresentation.presents(
                       statusError,
                       integrationMessage: model.integrationStatusMessage,
                       integrationAvailability: model.integrationAvailability,
                       handlesMissingIntegrationInline: handlesMissingIntegrationInline
                   ) {
                    ControlCenterNotice(
                        tone: .messageTone(
                            for: statusError,
                            statusError: statusError,
                            integrationMessage: model.integrationStatusMessage,
                            integrationAvailability: model.integrationAvailability
                        )
                    ) {
                        SafeStatusMessageView(message: statusError, localizer: localizer)
                    }
                }

                content
            }
            .frame(
                maxWidth: ControlCenterPresentationMetrics.pageMaximumWidth,
                alignment: .leading
            )
            .padding(.horizontal, ControlCenterPresentationMetrics.horizontalPageInset)
            .padding(.vertical, ControlCenterPresentationMetrics.verticalPageInset)
            .frame(maxWidth: .infinity, alignment: .topLeading)
        }
        .controlSize(.regular)
    }
}

struct ControlCenterSectionCard<Content: View, Accessory: View>: View {
    let title: String
    let systemImage: String?
    private let content: () -> Content
    private let accessory: () -> Accessory

    init(
        _ title: String,
        systemImage: String? = nil,
        @ViewBuilder content: @escaping () -> Content,
        @ViewBuilder accessory: @escaping () -> Accessory
    ) {
        self.title = title
        self.systemImage = systemImage
        self.content = content
        self.accessory = accessory
    }

    var body: some View {
        GroupBox {
            VStack(alignment: .leading, spacing: 0) {
                content()
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, ControlCenterPresentationMetrics.cardContentInset)
            .padding(.vertical, 2)
        } label: {
            HStack(spacing: 8) {
                if let systemImage {
                    Label(title, systemImage: systemImage)
                } else {
                    Text(title)
                }
                Spacer(minLength: 12)
                accessory()
            }
            .font(.headline)
        }
        .groupBoxStyle(.automatic)
        .accessibilityElement(children: .contain)
    }
}

extension ControlCenterSectionCard where Accessory == EmptyView {
    init(
        _ title: String,
        systemImage: String? = nil,
        @ViewBuilder content: @escaping () -> Content
    ) {
        self.init(title, systemImage: systemImage, content: content) {
            EmptyView()
        }
    }
}

struct ControlCenterStatusBadge: View {
    let text: String
    var tone: ControlCenterStatusTone = .neutral
    var monospaced = false

    var body: some View {
        Text(text)
            .font(monospaced ? .caption.monospaced() : .caption.weight(.semibold))
            .foregroundStyle(tone.color)
            .padding(.horizontal, 8)
            .padding(.vertical, 3)
            .background(tone.color.opacity(0.12), in: Capsule())
            .fixedSize(horizontal: true, vertical: true)
    }
}

struct ControlCenterNotice<Content: View>: View {
    let tone: ControlCenterStatusTone
    private let content: () -> Content

    init(tone: ControlCenterStatusTone, @ViewBuilder content: @escaping () -> Content) {
        self.tone = tone
        self.content = content
    }

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: tone.systemImage)
                .foregroundStyle(tone.color)
                .padding(.top, 1)
            content()
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .padding(12)
        .background(.quaternary, in: RoundedRectangle(cornerRadius: 10, style: .continuous))
        .fixedSize(horizontal: false, vertical: true)
    }
}

struct ControlCenterActionFooter<Secondary: View, Primary: View>: View {
    private let secondary: () -> Secondary
    private let primary: () -> Primary

    init(
        @ViewBuilder secondary: @escaping () -> Secondary,
        @ViewBuilder primary: @escaping () -> Primary
    ) {
        self.secondary = secondary
        self.primary = primary
    }

    var body: some View {
        ViewThatFits(in: .horizontal) {
            HStack(spacing: ControlCenterPresentationMetrics.actionSpacing) {
                secondary()
                Spacer(minLength: 12)
                primary()
            }

            VStack(alignment: .leading, spacing: ControlCenterPresentationMetrics.actionSpacing) {
                primary()
                secondary()
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .buttonBorderShape(.roundedRectangle(radius: 8))
        .fixedSize(horizontal: false, vertical: true)
    }
}

struct AdaptiveActionRow<Content: View>: View {
    private let content: () -> Content

    init(@ViewBuilder content: @escaping () -> Content) {
        self.content = content
    }

    var body: some View {
        ViewThatFits(in: .horizontal) {
            HStack(spacing: ControlCenterPresentationMetrics.actionSpacing) {
                content()
                Spacer(minLength: 0)
            }

            VStack(alignment: .leading, spacing: ControlCenterPresentationMetrics.actionSpacing) {
                content()
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .buttonBorderShape(.roundedRectangle(radius: 8))
        .fixedSize(horizontal: false, vertical: true)
    }
}

struct ControlCenterSupportingText: View {
    let text: String
    var systemImage: String?

    init(_ text: String, systemImage: String? = nil) {
        self.text = text
        self.systemImage = systemImage
    }

    var body: some View {
        Group {
            if let systemImage {
                Label(text, systemImage: systemImage)
            } else {
                Text(text)
            }
        }
        .font(.body)
        .foregroundStyle(.secondary)
        .fixedSize(horizontal: false, vertical: true)
    }
}

struct LocalDevelopmentWarningView: View {
    let localizer: AppLocalizer
    var compact = false

    var body: some View {
        Label {
            Text(localizer.text(.viewUnsignedWarning))
        } icon: {
            Image(systemName: "exclamationmark.triangle.fill")
        }
        .font(compact ? .caption : .body.weight(.medium))
        .foregroundStyle(.orange)
        .lineLimit(compact ? 2 : nil)
        .fixedSize(horizontal: false, vertical: true)
        .accessibilityLabel(localizer.text(.viewUnsignedWarningAccessibility))
    }
}

struct StatusRow: View {
    let label: String
    let value: String
    let systemImage: String?
    let badgeTone: ControlCenterStatusTone?
    let showsDivider: Bool
    @EnvironmentObject private var localization: LocalizationStore
    let minimumHeight: CGFloat

    private var localizer: AppLocalizer { localization.localizer }

    init(
        _ label: String,
        value: String,
        systemImage: String? = nil,
        badgeTone: ControlCenterStatusTone? = nil,
        showsDivider: Bool = false,
        minimumHeight: CGFloat = ControlCenterPresentationMetrics.statusRowMinimumHeight
    ) {
        self.label = label
        self.value = value
        self.systemImage = systemImage
        self.badgeTone = badgeTone
        self.showsDivider = showsDivider
        self.minimumHeight = minimumHeight
    }

    var body: some View {
        ViewThatFits(in: .horizontal) {
            HStack(alignment: .firstTextBaseline, spacing: 18) {
                HStack(spacing: 8) {
                    if let systemImage {
                        Image(systemName: systemImage)
                            .foregroundStyle(.secondary)
                            .frame(width: 18)
                    }
                    Text(label)
                        .foregroundStyle(.primary)
                }
                    .frame(
                        width: ControlCenterPresentationMetrics.statusLabelWidth,
                        alignment: .leading
                    )

                Spacer(minLength: 0)
                valueView
            }

            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 8) {
                    if let systemImage {
                        Image(systemName: systemImage)
                    }
                    Text(label)
                }
                    .font(.subheadline.weight(.medium))
                    .foregroundStyle(.secondary)
                valueView
            }
        }
        .font(.body)
        .frame(
            maxWidth: .infinity,
            minHeight: minimumHeight,
            alignment: .leading
        )
        .fixedSize(horizontal: false, vertical: true)
        .overlay(alignment: .bottom) {
            if showsDivider {
                Divider()
                    .padding(.leading, systemImage == nil ? 0 : 26)
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel(localizer.text(.viewStatusRowAccessibility, label, value))
    }

    @ViewBuilder
    private var valueView: some View {
        if let badgeTone {
            ControlCenterStatusBadge(text: value, tone: badgeTone)
        } else {
            Text(value)
                .fontWeight(.medium)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.trailing)
                .textSelection(.enabled)
        }
    }
}

struct SafeStatusMessageView: View {
    let message: SafeStatusMessage
    let localizer: AppLocalizer

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(message.text(using: localizer))
                .font(.body)
                .foregroundStyle(.primary)
            Text(localizer.text(.viewStatusCode, message.code))
                .font(.caption)
                .foregroundStyle(.secondary)
        }
        .fixedSize(horizontal: false, vertical: true)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(
            localizer.text(
                .viewStatusMessageAccessibility,
                message.text(using: localizer),
                message.code
            )
        )
    }
}
