import OpenCodexRelayCore

enum NativeRepairFlowStep: String, CaseIterable, Equatable {
    case preflight
    case desktopExit = "desktop_exit"
    case ownerRepair = "owner_repair"
    case nativeVerification = "native_verification"
    case stateCommit = "state_commit"
    case desktopRelaunch = "desktop_relaunch"
    case statusRefresh = "status_refresh"
}

enum NativeRepairStepState: Equatable {
    case pending
    case running
    case completed
    case failed
}

struct NativeRepairProgress: Equatable {
    let inspection: NativeRepairInspection
    var currentStep: NativeRepairFlowStep
    var failedStep: NativeRepairFlowStep?
    var result: SafeStatusMessage?
    var receipt: NativeRoutingRepairReceipt?

    func state(for step: NativeRepairFlowStep) -> NativeRepairStepState {
        let steps = NativeRepairFlowStep.allCases
        guard let stepIndex = steps.firstIndex(of: step),
              let currentIndex = steps.firstIndex(of: currentStep) else {
            return .pending
        }
        if let failedStep, let failedIndex = steps.firstIndex(of: failedStep) {
            if stepIndex < failedIndex { return .completed }
            if stepIndex == failedIndex { return .failed }
            return .pending
        }
        if receipt != nil, result != nil { return .completed }
        if stepIndex < currentIndex { return .completed }
        if stepIndex == currentIndex { return .running }
        return .pending
    }
}
