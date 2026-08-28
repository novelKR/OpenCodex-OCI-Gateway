import SwiftUI
import OpenCodexRelayCore
import OpenCodexRelayLocalization

struct OpenCodexHandoffProgressView: View {
    let progress: OpenCodexHandoffProgress
    let status: RoutingStatus?
    let automaticRemovalEligible: Bool
    let localizer: AppLocalizer

    private enum StepState {
        case pending
        case active
        case completed
        case blocked
        case failed
    }

    private struct Step: Identifiable {
        let phase: OpenCodexHandoffProgressPhase
        let titleKey: AppStringKey

        var id: Int { phase.rawValue }
    }

    var body: some View {
        GroupBox(localizer.text(.removalHandoffProgressTitle)) {
            VStack(alignment: .leading, spacing: 12) {
                Text(localizer.text(.removalHandoffProgressDetail))
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)

                VStack(alignment: .leading, spacing: 10) {
                    ForEach(steps) { step in
                        stepRow(step)
                    }
                }

                if let result = progress.result {
                    Divider()
                    Label {
                        VStack(alignment: .leading, spacing: 5) {
                            Text(result.text(using: localizer))
                                .foregroundStyle(.primary)
                            Text(resultGuidance)
                                .foregroundStyle(.secondary)
                        }
                    } icon: {
                        Image(systemName: resultSymbol)
                            .foregroundStyle(resultColor)
                    }
                    .fixedSize(horizontal: false, vertical: true)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.vertical, 4)
        }
    }

    private var steps: [Step] {
        [
            Step(phase: .preflight, titleKey: .removalHandoffStepPreflight),
            Step(phase: .desktopExit, titleKey: .removalHandoffStepDesktopExit),
            Step(
                phase: .openCodexOperation,
                titleKey: progress.action == .retainProxyRemoveShim
                    ? .removalHandoffStepRemoveShim
                    : .removalHandoffStepKeepShim
            ),
            Step(phase: .desktopRelaunch, titleKey: .removalHandoffStepDesktopRelaunch),
            Step(phase: .statusRefresh, titleKey: .removalHandoffStepStatusRefresh),
        ]
    }

    @ViewBuilder
    private func stepRow(_ step: Step) -> some View {
        let state = state(for: step.phase)
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            statusIcon(state)
                .frame(width: 18)
            Text(localizer.text(step.titleKey))
                .frame(maxWidth: .infinity, alignment: .leading)
            Text(statusText(state))
                .font(.caption)
                .foregroundStyle(state == .failed || state == .blocked ? .orange : .secondary)
        }
        .accessibilityElement(children: .combine)
    }

    @ViewBuilder
    private func statusIcon(_ state: StepState) -> some View {
        switch state {
        case .pending:
            Image(systemName: "circle")
                .foregroundStyle(.tertiary)
        case .active:
            ProgressView()
                .controlSize(.small)
        case .completed:
            Image(systemName: "checkmark.circle.fill")
                .foregroundStyle(.green)
        case .blocked:
            Image(systemName: "lock.trianglebadge.exclamationmark.fill")
                .foregroundStyle(.orange)
        case .failed:
            Image(systemName: "exclamationmark.triangle.fill")
                .foregroundStyle(.orange)
        }
    }

    private func state(for phase: OpenCodexHandoffProgressPhase) -> StepState {
        if progress.phase == .completed {
            return .completed
        }
        if progress.phase == .failed {
            let failed = progress.failedPhase ?? .openCodexOperation
            if phase.rawValue < failed.rawValue { return .completed }
            if phase == failed { return failed == .preflight ? .blocked : .failed }
            return .pending
        }
        if phase.rawValue < progress.phase.rawValue { return .completed }
        if phase == progress.phase { return .active }
        return .pending
    }

    private func statusText(_ state: StepState) -> String {
        switch state {
        case .pending: localizer.text(.removalHandoffStatusPending)
        case .active: localizer.text(.removalHandoffStatusRunning)
        case .completed: localizer.text(.removalHandoffStatusCompleted)
        case .blocked: localizer.text(.removalHandoffStatusBlocked)
        case .failed: localizer.text(.removalHandoffStatusFailed)
        }
    }

    private var resultGuidance: String {
        if progress.phase == .failed {
            return localizer.text(.removalHandoffResultUnverified)
        }
        if status?.phase == .recoveryRequired {
            return localizer.text(.removalHandoffResultRecoveryRequired)
        }
        if automaticRemovalEligible, status?.canUninstallOpenCodex == true {
            return localizer.text(.removalHandoffResultRemovalAvailable)
        }
        return localizer.text(.removalHandoffResultRemovalBlocked)
    }

    private var resultSymbol: String {
        if progress.phase == .failed { return "exclamationmark.triangle.fill" }
        if status?.phase == .recoveryRequired { return "arrow.trianglehead.2.clockwise.rotate.90" }
        if automaticRemovalEligible, status?.canUninstallOpenCodex == true {
            return "checkmark.shield.fill"
        }
        return "lock.shield"
    }

    private var resultColor: Color {
        if progress.phase == .failed || status?.phase == .recoveryRequired { return .orange }
        if automaticRemovalEligible, status?.canUninstallOpenCodex == true { return .green }
        return .secondary
    }
}
