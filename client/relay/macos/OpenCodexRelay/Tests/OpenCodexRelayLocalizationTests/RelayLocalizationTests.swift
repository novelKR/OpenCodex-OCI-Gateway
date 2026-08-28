import Foundation
import XCTest
@testable import OpenCodexRelayLocalization
import OpenCodexRelayCore

@MainActor
final class RelayLocalizationTests: XCTestCase {
    func testSystemKoreanUsesKoreanCatalog() {
        let localizer = AppLocalizer(selection: .system, preferredLanguageIdentifiers: ["ko-KR", "en-US"])

        XCTAssertEqual(localizer.resolvedLanguage, .korean)
        XCTAssertEqual(localizer.displayName(RoutingBackend.external), "외부 게이트웨이")
        XCTAssertEqual(localizer.title(.nativeParked), "기본 Codex 활성")
    }

    func testSystemLanguageUsesOnlyTheFirstPreferredLocale() {
        let englishFirst = AppLocalizer(
            selection: .system,
            preferredLanguageIdentifiers: ["en-US", "ko-KR"]
        )
        XCTAssertEqual(englishFirst.resolvedLanguage, .english)

        let koreanFirst = AppLocalizer(
            selection: .system,
            preferredLanguageIdentifiers: ["ko-KR", "en-US"]
        )
        XCTAssertEqual(koreanFirst.resolvedLanguage, .korean)

        let unsupportedFirst = AppLocalizer(
            selection: .system,
            preferredLanguageIdentifiers: ["fr-FR", "ko-KR"]
        )
        XCTAssertEqual(unsupportedFirst.resolvedLanguage, .korean)

        let empty = AppLocalizer(selection: .system, preferredLanguageIdentifiers: [])
        XCTAssertEqual(empty.resolvedLanguage, .korean)
    }

    func testUnsupportedSystemLanguageFallsBackToKorean() {
        let localizer = AppLocalizer(selection: .system, preferredLanguageIdentifiers: ["fr-FR"])

        XCTAssertEqual(localizer.resolvedLanguage, .korean)
        XCTAssertEqual(localizer.displayName(RoutingBackend.localOpenCodex), "로컬 OpenCodex (10100)")
        XCTAssertEqual(localizer.text(.messageRequesting, "External gateway"), "External gateway 요청 중…")
    }

    func testManualSelectionOverridesSystemLanguageAndFormatsValues() {
        let localizer = AppLocalizer(selection: .english, preferredLanguageIdentifiers: ["ko-KR"])

        XCTAssertEqual(localizer.resolvedLanguage, .english)
        XCTAssertEqual(localizer.text(.messageDesktopSelected, "Codex"), "Verified and selected Codex.")
        XCTAssertFalse(localizer.formattedDate(Date(timeIntervalSince1970: 0)).isEmpty)
        XCTAssertFalse(localizer.formattedNumber(12_345).isEmpty)
    }

    func testLanguagePreferencePersistsInProvidedDefaults() {
        let suite = "OpenCodexRelayLocalizationTests.\(UUID().uuidString)"
        guard let defaults = UserDefaults(suiteName: suite) else {
            return XCTFail("create isolated defaults")
        }
        defer { defaults.removePersistentDomain(forName: suite) }

        let first = LocalizationStore(defaults: defaults, preferenceKey: "language")
        XCTAssertEqual(first.selection, .system)
        first.selection = .korean

        let second = LocalizationStore(defaults: defaults, preferenceKey: "language")
        XCTAssertEqual(second.selection, .korean)
    }

    func testLanguageSelectionCodableRetainsItsLegacySingleStringShape() throws {
        let encoded = try JSONEncoder().encode(AppLanguageSelection.korean)
        XCTAssertEqual(String(decoding: encoded, as: UTF8.self), "\"korean\"")
        XCTAssertEqual(
            try JSONDecoder().decode(AppLanguageSelection.self, from: Data("\"future-language\"".utf8)),
            AppLanguageSelection(rawValue: "future-language")
        )
    }

    func testProductionAndDevelopmentPreferenceDomainsRemainSeparate() {
        let productionSuite = "OpenCodexRelayLocalizationTests.production.\(UUID().uuidString)"
        let developmentSuite = "OpenCodexRelayLocalizationTests.development.\(UUID().uuidString)"
        let production = UserDefaults(suiteName: productionSuite)!
        let development = UserDefaults(suiteName: developmentSuite)!
        defer {
            production.removePersistentDomain(forName: productionSuite)
            development.removePersistentDomain(forName: developmentSuite)
        }

        LocalizationStore(defaults: production, preferenceKey: LocalizationStore.preferenceKey).selection = .korean
        LocalizationStore(defaults: development, preferenceKey: LocalizationStore.preferenceKey).selection = .english

        XCTAssertEqual(
            LocalizationStore(defaults: production, preferenceKey: LocalizationStore.preferenceKey).selection,
            .korean
        )
        XCTAssertEqual(
            LocalizationStore(defaults: development, preferenceKey: LocalizationStore.preferenceKey).selection,
            .english
        )
    }

    func testEveryStableKeyHasEnglishAndKoreanFallbackText() {
        let english = AppLocalizer(selection: .english)
        let korean = AppLocalizer(selection: .korean)

        for key in AppStringKey.allCases {
            XCTAssertNotEqual(english.text(key), key.rawValue, "missing English text for \(key.rawValue)")
            XCTAssertNotEqual(korean.text(key), key.rawValue, "missing Korean text for \(key.rawValue)")
        }
    }

    func testRelocationSourceFailuresHaveDistinctEnglishAndKoreanGuidance() {
        for selection in [AppLanguageSelection.english, .korean] {
            let localizer = AppLocalizer(selection: selection)
            let generic = localizer.text(.relocationDetail)
            let details = [
                localizer.text(.relocationSourceBundleInvalid),
                localizer.text(.relocationSourceProcessInvalid),
                localizer.text(.relocationSourceLocationInvalid),
            ]

            XCTAssertEqual(Set(details).count, details.count)
            XCTAssertFalse(details.contains(generic))
            XCTAssertNotEqual(
                localizer.text(.relocationRetryValidation),
                AppStringKey.relocationRetryValidation.rawValue
            )
        }
    }

    func testPrivateHTTPWarningCoversCodexTrafficAndCredentials() {
        for localizer in [
            AppLocalizer(selection: .english),
            AppLocalizer(selection: .korean),
        ] {
            let warning = localizer.text(.gatewayInsecurePrivateIPConfirmation)
            XCTAssertTrue(warning.contains("Codex"))
            XCTAssertTrue(warning.contains("Authorization"))
            XCTAssertTrue(warning.contains("TLS"))
        }
    }

    func testIntegrationGuideExplainsInputSourcesWithoutExternalDocumentation() {
        let english = AppLocalizer(selection: .english)
        let korean = AppLocalizer(selection: .korean)

        for localizer in [english, korean] {
            XCTAssertTrue(localizer.text(.integrationGuideSourceDetail).contains("OpenCodex Relay"))
            XCTAssertTrue(localizer.text(.integrationGuideGatewayHelp).contains("https://"))
            XCTAssertTrue(localizer.text(.integrationGuideGatewayHelp).contains("Relay"))
            XCTAssertTrue(localizer.text(.integrationGuideSigningNotCredential).contains("Gateway API"))
            XCTAssertTrue(localizer.text(.integrationGuideSigningNotCredential).contains("Cloudflare"))
            XCTAssertTrue(localizer.text(.integrationGuideReviewDetail).contains("Terminal"))
        }
    }

    func testLanguageRegistryDrivesThePickerAndUnknownPersistedValuesNormalizeToSystem() {
        XCTAssertEqual(
            AppLanguageRegistry.standard.descriptors.map(\.selection),
            [.system, .korean, .english]
        )
        XCTAssertEqual(
            AppLanguageRegistry.standard.descriptors.compactMap(\.catalogLanguage?.rawValue),
            ["ko", "en"]
        )

        let suite = "OpenCodexRelayLocalizationTests.unknown.\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        defer { defaults.removePersistentDomain(forName: suite) }
        defaults.set("removed-language", forKey: "language")
        XCTAssertEqual(
            LocalizationStore(defaults: defaults, preferenceKey: "language").selection,
            .system
        )
    }

    func testCustomDescriptorResolvesWithoutSelectionOrResolverSwitches() {
        let testDescriptor = AppLanguageDescriptor(
            selection: AppLanguageSelection(rawValue: "test_language"),
            catalogLanguage: ResolvedAppLanguage(rawValue: "zz"),
            systemLanguageCodes: ["zz"],
            pickerFallbackName: "Test language"
        )
        let registry = AppLanguageRegistry(
            descriptors: AppLanguageRegistry.standard.descriptors + [testDescriptor]
        )
        let explicit = AppLocalizer(
            selection: testDescriptor.selection,
            preferredLanguageIdentifiers: ["en-US"],
            registry: registry
        )
        XCTAssertEqual(explicit.resolvedLanguage, ResolvedAppLanguage(rawValue: "zz"))
        XCTAssertEqual(explicit.text(.messageRequesting, "External gateway"), "External gateway 요청 중…")

        let system = AppLocalizer(
            selection: .system,
            preferredLanguageIdentifiers: ["zz-ZZ", "ko-KR"],
            registry: registry
        )
        XCTAssertEqual(system.resolvedLanguage, ResolvedAppLanguage(rawValue: "zz"))
        XCTAssertEqual(system.languageName(testDescriptor), "Test language")
    }

    func testRuntimeResourceResolverPrefersContentsResourcesBeforeSwiftPMFallback() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let appURL = root.appendingPathComponent("Test.app", isDirectory: true)
        let resourceBundleURL = appURL
            .appendingPathComponent("Contents/Resources", isDirectory: true)
            .appendingPathComponent(AppLocalizationResourceBundle.name, isDirectory: true)
        let fallbackBundleURL = root.appendingPathComponent("Fallback.bundle", isDirectory: true)
        let appBundle = try makeBundle(at: appURL, packageType: "APPL", resourceDirectory: true)
        let stagedBundle = try makeBundle(at: resourceBundleURL, packageType: "BNDL")
        let fallbackBundle = try makeBundle(at: fallbackBundleURL, packageType: "BNDL")
        try writeCatalog(
            "\"generic.unknown\" = \"staged app resource\";\n",
            language: "en",
            to: stagedBundle.bundleURL
        )

        var fallbackUsed = false
        let resolved = AppLocalizationResourceBundle.resolve(mainBundle: appBundle) {
            fallbackUsed = true
            return fallbackBundle
        }
        XCTAssertFalse(fallbackUsed)
        XCTAssertEqual(resolved?.bundleURL.standardizedFileURL, stagedBundle.bundleURL.standardizedFileURL)

        let localizer = AppLocalizer(
            selection: .english,
            preferredLanguageIdentifiers: ["en-US"],
            registry: .standard,
            resourceBundle: resolved
        )
        XCTAssertEqual(localizer.text(.genericUnknown), "staged app resource")

        let emptyAppURL = root.appendingPathComponent("Empty.app", isDirectory: true)
        let emptyApp = try makeBundle(at: emptyAppURL, packageType: "APPL", resourceDirectory: true)
        var fallbackUsedForMissingResource = false
        let fallbackResolution = AppLocalizationResourceBundle.resolve(mainBundle: emptyApp) {
            fallbackUsedForMissingResource = true
            return fallbackBundle
        }
        XCTAssertTrue(fallbackUsedForMissingResource)
        XCTAssertEqual(fallbackResolution?.bundleURL.standardizedFileURL, fallbackBundle.bundleURL.standardizedFileURL)
    }

    private func makeBundle(
        at url: URL,
        packageType: String,
        resourceDirectory: Bool = false
    ) throws -> Bundle {
        let infoURL: URL
        if resourceDirectory {
            infoURL = url.appendingPathComponent("Contents/Info.plist", isDirectory: false)
        } else {
            infoURL = url.appendingPathComponent("Info.plist", isDirectory: false)
        }
        try FileManager.default.createDirectory(at: infoURL.deletingLastPathComponent(), withIntermediateDirectories: true)
        let info: [String: Any] = [
            "CFBundleIdentifier": "test.\(UUID().uuidString)",
            "CFBundlePackageType": packageType,
            "CFBundleVersion": "1",
        ]
        let data = try PropertyListSerialization.data(fromPropertyList: info, format: .xml, options: 0)
        try data.write(to: infoURL)
        guard let bundle = Bundle(url: url) else {
            throw NSError(domain: "RelayLocalizationTests", code: 1)
        }
        return bundle
    }

    private func writeCatalog(_ contents: String, language: String, to bundleURL: URL) throws {
        let url = bundleURL
            .appendingPathComponent("\(language).lproj", isDirectory: true)
            .appendingPathComponent("Localizable.strings", isDirectory: false)
        try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        try contents.data(using: .utf8)!.write(to: url)
    }
}
