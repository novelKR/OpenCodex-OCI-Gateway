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

    func testMissingBindingGuidanceUsesConsumerSetupAndStateGatedRecovery() {
        let english = AppLocalizer(selection: .english)
        let korean = AppLocalizer(selection: .korean)
        let englishBindingGuidance = "Relay setup for this user is incomplete. Go to Settings > Connect a self-hosted server and choose Prepare Relay. Use Recover setup only when recovery is required."

        XCTAssertEqual(english.text(.bindingMissing), englishBindingGuidance)
        XCTAssertEqual(RoutingBindingError.missing.safeMessage, englishBindingGuidance)
        XCTAssertEqual(
            korean.text(.bindingMissing),
            "현재 사용자의 Relay 설정이 완료되지 않았습니다. 설정 > 셀프 호스팅 서버 연결에서 Relay 준비를 선택하세요. 복구가 필요한 경우에만 설정 복구를 사용하세요."
        )

        XCTAssertEqual(
            english.text(.gatewayDetailIntegrationRequired),
            "Enter the server address and required credentials, then choose Prepare Relay. Prepare Relay creates only user-owned files and never opens Terminal or requests administrator access."
        )
        XCTAssertEqual(
            korean.text(.gatewayDetailIntegrationRequired),
            "서버 주소와 필요한 자격 증명을 입력한 뒤 Relay 준비를 선택하세요. Relay 준비는 사용자 소유 파일만 만들며 Terminal을 열거나 관리자 권한을 요청하지 않습니다."
        )

        XCTAssertEqual(
            english.text(.codexConfigBindingMissing),
            "The protected routing binding is unavailable. Prepare Relay in Settings, or use Recover setup there when recovery is required, before reviewing the Codex configuration."
        )
        XCTAssertEqual(
            korean.text(.codexConfigBindingMissing),
            "보호된 라우팅 binding을 사용할 수 없습니다. Codex 설정을 확인하기 전에 설정에서 Relay 준비를 진행하거나 복구가 필요한 경우 설정 복구를 사용하세요."
        )
        XCTAssertEqual(
            english.text(.controlCenterLocalOpenCodexManagement),
            "Local OpenCodex management"
        )
        XCTAssertEqual(
            korean.text(.controlCenterLocalOpenCodexManagement),
            "Local OpenCodex 관리"
        )
        XCTAssertEqual(
            english.text(.controlCenterLocalOpenCodexInspectAction),
            "Inspect OpenCodex installation…"
        )
        XCTAssertEqual(
            korean.text(.controlCenterLocalOpenCodexInspectAction),
            "OpenCodex 설치 검사…"
        )
        XCTAssertEqual(
            english.text(.controlCenterLocalOpenCodexRelayBadge),
            "Relay setup required"
        )
        XCTAssertEqual(
            korean.text(.controlCenterLocalOpenCodexRelayBadge),
            "Relay 준비 필요"
        )
        XCTAssertEqual(
            english.text(.controlCenterLocalOpenCodexRelayHint),
            "Opens Settings > Connect a self-hosted server, where you can prepare Relay or recover setup when required."
        )
        XCTAssertEqual(
            korean.text(.controlCenterLocalOpenCodexRelayHint),
            "설정 > 셀프 호스팅 서버 연결을 열어 Relay를 준비하거나 필요한 경우 설정을 복구합니다."
        )
        XCTAssertEqual(
            english.text(.homebrewGuardDevelopmentSetupSuccessTitle),
            "Privileged helper is ready"
        )
        XCTAssertEqual(
            korean.text(.homebrewGuardDevelopmentSetupSuccessTitle),
            "권한 helper 준비 완료"
        )
        XCTAssertEqual(
            english.text(.homebrewGuardDevelopmentSetupReviewUpdatedState),
            "View updated status"
        )
        XCTAssertEqual(
            korean.text(.homebrewGuardDevelopmentSetupReviewUpdatedState),
            "갱신된 상태 보기"
        )

        XCTAssertFalse(english.text(.bindingMissing).localizedCaseInsensitiveContains("reinstall"))
        XCTAssertFalse(korean.text(.bindingMissing).contains("다시 설치"))
        XCTAssertFalse(english.text(.codexConfigBindingMissing).localizedCaseInsensitiveContains("reinstall"))
        XCTAssertFalse(korean.text(.codexConfigBindingMissing).contains("다시 설치"))
        XCTAssertFalse(english.text(.appInformationVersionMismatch).localizedCaseInsensitiveContains("reinstall"))
        XCTAssertFalse(korean.text(.appInformationVersionMismatch).contains("다시 설치"))
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
