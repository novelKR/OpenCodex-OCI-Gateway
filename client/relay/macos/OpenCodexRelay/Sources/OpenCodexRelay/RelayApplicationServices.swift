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
    let containerRuntime: ContainerRuntimeController

    private let launchRequest: ApplicationRelocationLaunchRequest?
    private var statusItemCoordinator: RelayStatusItemCoordinator?
    private var relocationStateObservation: AnyCancellable?
    private var presentedRelocationIntervention = false
    private var started = false

    init(arguments: [String] = ProcessInfo.processInfo.arguments) {
        let launchRequest = ApplicationRelocationLaunchRequest.parse(arguments)
        let localization = LocalizationStore()
        let distributionFlavor = DistributionFlavor.current
        let runtimeMode = RelayRuntimeMode.current
        let bindingURL = RoutingBindingReader.defaultURL(
            distributionFlavor: distributionFlavor
        )
        let helperURL = RelayctlHelperLocation.resolve()
        let model = MenuBarModel(
            bindingURL: bindingURL,
            helperURL: helperURL,
            distributionFlavor: distributionFlavor,
            runtimeMode: runtimeMode,
            localization: localization
        )
        let updates = ReleaseUpdateController()
        let runtimeUpgrade = RuntimeUpgradeController()
        let containerRuntime = ContainerRuntimeController(
            client: ProcessContainerRuntimeClient(
                executableURL: helperURL,
                bindingURL: bindingURL,
                runtimeMode: runtimeMode,
                distributionFlavor: distributionFlavor
            )
        )
        self.localization = localization
        self.model = model
        self.updates = updates
        self.runtimeUpgrade = runtimeUpgrade
        self.containerRuntime = containerRuntime
        self.windowCoordinator = ControlCenterWindowCoordinator(activityLog: model.activityLog)
        self.relocation = ApplicationRelocationController(
            runtimeMode: runtimeMode,
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
        containerRuntime.start()
        statusItemCoordinator = RelayStatusItemCoordinator(
            model: model,
            localization: localization,
            windowCoordinator: windowCoordinator,
            relocation: relocation,
            updates: updates,
            runtimeUpgrade: runtimeUpgrade,
            containerRuntime: containerRuntime
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
            containerRuntime: containerRuntime,
            section: section
        )
    }
}
