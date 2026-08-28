import Foundation

/// Small testable boundary around the ServiceManagement APIs used by the
/// signed MenuBar bundle. The core never knows which login-item mechanism is
/// used and never carries system error text into the UI or installer output.
public enum LoginRegistrationState: Equatable, Sendable {
    case enabled
    case pending
    case disabled
}

public enum LoginRegistrationResult: String, Equatable, Sendable {
    case enabled
    case pending
    case disabled
    case failed
}

public protocol LoginRegistrationManaging {
    var registrationState: LoginRegistrationState { get }
    func register() throws
    func unregister() throws
}

public enum LoginRegistrationCoordinator {
    /// Query-only mapping used by installer preflight. It never calls
    /// ServiceManagement registration APIs and therefore cannot alter Login
    /// Items state.
    public static func status(
        _ registration: some LoginRegistrationManaging
    ) -> LoginRegistrationResult {
        switch registration.registrationState {
        case .enabled:
            return .enabled
        case .pending:
            return .pending
        case .disabled:
            return .disabled
        }
    }

    public static func ensureRegistered(
        _ registration: some LoginRegistrationManaging
    ) -> LoginRegistrationResult {
        if registration.registrationState == .enabled {
            return .enabled
        }
        do {
            try registration.register()
        } catch {
            return .failed
        }
        return registration.registrationState == .enabled ? .enabled : .pending
    }

    /// Unregister is idempotent for an already-disabled item. A pending result
    /// means the OS did not confirm removal, so installers must treat it as a
    /// rollback failure instead of claiming registration was restored.
    public static func unregister(
        _ registration: some LoginRegistrationManaging
    ) -> LoginRegistrationResult {
        if registration.registrationState == .disabled {
            return .disabled
        }
        do {
            try registration.unregister()
        } catch {
            return .failed
        }
        return registration.registrationState == .disabled ? .disabled : .pending
    }
}
