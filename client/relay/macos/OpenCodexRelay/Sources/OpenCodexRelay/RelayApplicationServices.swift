import Combine
import Foundation
import OpenCodexRelayCore
import OpenCodexRelayLocalization

@MainActor
final class RelayApplicationServices: ObservableObject {
    static var current: RelayApplicationServices?

    let localization: LocalizationStore
    let model: MenuBarModel
    let windowCoordinator: ControlCenterWindowCoordinator
    let relocation: ApplicationRelocationController
    let updates: ReleaseUpdateController
    let runtimeUpgrade: RuntimeUpgradeController

    private let launchRequest: ApplicationRelocationLaunchRequest?
    private var statusItemCoordinator: RelayStatusItemCoordinator?
    private var relocationStateObservation: AnyCancellable?
    private var presentedRelocationIntervention = false
    private var started = false

    init(arguments: [String] = ProcessInfo.processInfo.arguments) {
        let launchRequest = ApplicationRelocationLaunchRequest.parse(arguments)
        let localization = LocalizationStore()
        let model = MenuBarModel(localization: localization)
        let updates = ReleaseUpdateController()
        let runtimeUpgrade = RuntimeUpgradeController()
        self.localization = localization
        self.model = model
        self.updates = updates
        self.runtimeUpgrade = runtimeUpgrade
        self.windowCoordinator = ControlCenterWindowCoordinator(activityLog: model.activityLog)
        self.relocation = ApplicationRelocationController(
            runtimeMode: RelayRuntimeMode.current,
            resumesDestinationLaunch: launchRequest != nil,
            activityLog: model.activityLog
        )
        self.launchRequest = launchRequest
    }

    func start() {
        guard !started else { return }
        started = true
        model.start()
        updates.start()
        runtimeUpgrade.start()
        statusItemCoordinator = RelayStatusItemCoordinator(
            model: model,
            localization: localization,
            windowCoordinator: windowCoordinator,
            relocation: relocation,
            updates: updates,
            runtimeUpgrade: runtimeUpgrade
        )
        relocationStateObservation = relocation.$state
            .removeDuplicates()
            .sink { [weak self] state in
                let requiresAttention = state == .sourceExitRequired ||
                    state == .backupCleanupFailed ||
                    state == .sourceCleanupRequired ||
                    state == .recoveryRequired
                guard requiresAttention,
                      let self,
                      !self.presentedRelocationIntervention else { return }
                self.presentedRelocationIntervention = true
                self.presentControlCenter(section: .settings)
            }
        if let launchRequest {
            Task {
                await relocation.completeLaunchRequest(launchRequest)
            }
        }
    }

    func presentControlCenter(section: RelayControlCenterSection? = nil) {
        windowCoordinator.present(
            model: model,
            localization: localization,
            relocation: relocation,
            updates: updates,
            runtimeUpgrade: runtimeUpgrade,
            section: section
        )
    }
}
