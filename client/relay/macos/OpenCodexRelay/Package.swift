// swift-tools-version: 6.2
import PackageDescription

let package = Package(
    name: "OpenCodexRelay",
    defaultLocalization: "ko",
    platforms: [.macOS(.v26)],
    products: [
        .library(name: "OpenCodexRelayCore", targets: ["OpenCodexRelayCore"]),
        .library(name: "OpenCodexRelayHomebrewGuard", targets: ["OpenCodexRelayHomebrewGuard"]),
        .library(name: "OpenCodexRelayHelperInstallerCore", targets: ["OpenCodexRelayHelperInstallerCore"]),
        .executable(name: "OpenCodexRelay", targets: ["OpenCodexRelay"]),
        .executable(name: "OpenCodexRelayPrivilegedHelper", targets: ["OpenCodexRelayPrivilegedHelper"]),
        .executable(name: "OpenCodexRelayHelperInstaller", targets: ["OpenCodexRelayHelperInstaller"]),
    ],
    targets: [
        .target(name: "OpenCodexRelayCore"),
        .target(name: "OpenCodexRelayHomebrewGuard"),
        .target(
            name: "OpenCodexRelayHelperInstallerCore"
        ),
        .executableTarget(
            name: "OpenCodexRelayHelperInstaller",
            dependencies: [
                "OpenCodexRelayHelperInstallerCore",
                "OpenCodexRelayHomebrewGuard",
            ],
            linkerSettings: [
                .linkedFramework("Security"),
                .linkedFramework("SystemConfiguration"),
            ]
        ),
        .executableTarget(
            name: "OpenCodexRelayPrivilegedHelper",
            dependencies: ["OpenCodexRelayHomebrewGuard"],
            linkerSettings: [
                .linkedFramework("Security"),
            ]
        ),
        .target(
            name: "OpenCodexRelayLocalization",
            dependencies: ["OpenCodexRelayCore"],
            resources: [.process("Resources")]
        ),
        .target(
            name: "OpenCodexRelayLegacyKeychainACL",
            cSettings: [
                .unsafeFlags([
                    "-Werror",
                    "-Wno-deprecated-declarations",
                ]),
            ],
            linkerSettings: [
                .linkedFramework("Security"),
            ]
        ),
        .executableTarget(
            name: "OpenCodexRelay",
            dependencies: [
                "OpenCodexRelayCore",
                "OpenCodexRelayHelperInstallerCore",
                "OpenCodexRelayLegacyKeychainACL",
                "OpenCodexRelayLocalization",
                "OpenCodexRelayHomebrewGuard",
            ],
            linkerSettings: [
                .linkedFramework("Security"),
                .linkedFramework("ServiceManagement"),
            ]
        ),
        .testTarget(
            name: "OpenCodexRelayHomebrewGuardTests",
            dependencies: ["OpenCodexRelayHomebrewGuard"]
        ),
        .testTarget(
            name: "OpenCodexRelayHelperInstallerCoreTests",
            dependencies: ["OpenCodexRelayHelperInstallerCore"]
        ),
        .testTarget(
            name: "OpenCodexRelayCoreTests",
            dependencies: ["OpenCodexRelayCore"]
        ),
        .testTarget(
            name: "OpenCodexRelayLocalizationTests",
            dependencies: ["OpenCodexRelayLocalization", "OpenCodexRelayCore"]
        ),
        .testTarget(
            name: "OpenCodexRelayAppTests",
            dependencies: ["OpenCodexRelay", "OpenCodexRelayCore", "OpenCodexRelayLocalization", "OpenCodexRelayHomebrewGuard"]
        ),
    ]
)
