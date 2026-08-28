package handoff

const (
	moduleDesired222   = "6f9f526d81f9f93013ed0c43eca0514b54e5eab77a72054d3d8ca84d0f04ce08"
	moduleDesired223   = "648ef53c33216ec01742c81045ed50e035906a6795b77802bcb020a4bb8fcca1"
	moduleInject222    = "fd020e956a6b7b50d256c9bdd7d585172cc7662993e4eb21fe2c55b0e2387e37"
	moduleInject223    = "21536a2f4d1065428217a55e5b97a21b76aae6e97d774c3e63cdc17d795399c5"
	moduleInject226    = "af007ed3639d31c4ad5654564f84982fe3d4f5cd1113bf47bdb9db0b02b6dea6"
	moduleInject229    = "d46a4316609d32c6a861b2c236ec70f581c9ac7b1d1d6db871501163337c2117"
	moduleInject232    = "cede7361d9518815e3f3c2f2e02ffdb37c802a7df7fc1ff43e72f11b6c68a8e5"
	moduleShimLegacy   = "f779c71050a1b2c3d6c08c22dbb6597d393a106f20a27566cc949203dbdad837"
	moduleShim233      = "0ffddafd8f9c10344a213e67363318598894a113ae8e5d383440d935a9c3132f"
	moduleGrokLegacy   = "223ca19a42ea4ec35207b83faf140ecf523ebf630520bc630ebacb99571f1a26"
	moduleGrokModern   = "06f1683768e0231efe6d964c7fae0d4199e03266bcedd438083bf62e382eea30"
	moduleRegistry222  = "9b489102e85d1b65cf8bd18b73e118649e37f247de85469b920f3a0144d62cb0"
	moduleRegistry226  = "7aedca8f0d0be0a5846c823b1e3b91b10bf8f63406e729d73a292e5af04d2e80"
	moduleRegistry229  = "193787b2fa41017a556fd3ac4183753b072ad6444eaae7868bb3c973d6ed3975"
	moduleStateLegacy  = "2b31611ea745ae22d4292719c2deb453efe6d5ea0cecba62d6d4352a30744e77"
	moduleState232     = "d98c0e384fa3226ab798b7d9f2da257640deabb4d35da11dd6baae34d8ecc5de"
	moduleWriterLegacy = "788709e9e0505cc717f5c015110d8fa17246dc5b33185fafc99d8136236f1606"
	moduleWriter229    = "9864b3e4153d881ae71d78001b72522d0da87b2cfff279e2b9dc803eb7f063eb"
	moduleWriter232    = "25f819c8c576bb2598ecae4084cd2f7c2d1f72f616c4638d5061ac79ef2280f6"
	moduleSystem222    = "4ba855172048295bd4933bcdade706a901fbf96b345c98e226c548bd32720a6e"
	moduleSystem223    = "4a501266f1b914fa83f0646f3679d38b0fdda8399bb01cc0451054d03b93dfe1"
	moduleSystem224    = "b068365c21dbc6c92de920f9ebb3c771b9f82ecff0dfa315618923d748a93025"
	moduleSystem228    = "e83b17223b42325c562be8925bfe573d4c342c68b0c9195b39657cfb64d3f0ea"
)

func teardownModuleHashes(
	packageJSON, desiredState, inject, shim, config, grok, registry, state, writer, systemEnv, service string,
) map[string]string {
	return map[string]string{
		"package.json":                 packageJSON,
		"src/codex/desired-state.ts":   desiredState,
		"src/codex/inject.ts":          inject,
		"src/codex/shim.ts":            shim,
		"src/config.ts":                config,
		"src/grok/inject.ts":           grok,
		"src/integrations/registry.ts": registry,
		"src/integrations/state.ts":    state,
		"src/integrations/writer.ts":   writer,
		"src/server/system-env.ts":     systemEnv,
		"src/service.ts":               service,
	}
}

func reviewedTeardownProfile(
	version, variant, integrity, closure string,
	modules map[string]string,
) teardownAdapterProfile {
	return teardownAdapterProfile{
		packageName:           OpenCodexPackageName,
		version:               version,
		artifactVariant:       variant,
		goos:                  "darwin",
		goarch:                "arm64",
		registryIntegrity:     integrity,
		reviewedClosureSHA256: closure,
		adapterID:             teardownAdapterIDForVersion(version),
		requiredModules:       modules,
	}
}

// teardownAdapterProfiles is an exact allowlist. Each entry binds one reviewed
// darwin/arm64 npm installation closure. Nearby versions, previews, and changed
// dependency closures remain manual-only.
var teardownAdapterProfiles = []teardownAdapterProfile{
	reviewedTeardownProfile(
		"2.22.0", "npm_2_22_0_darwin_arm64_v1",
		"sha512-YFWwTrHEgCEeBOsRBuFltLUdmmhf48xEQM4oaIDO9mEiWBor0wAqE0uIvj6eJpyZY0xd/Mhz8O77q9Nflp/y7A==",
		"564ca977dec168bcbf2b1228e96218d587232b1096fa099502528891133773da",
		teardownModuleHashes(
			"23d58fd93c12138baca76f044899d82aea45768aa38f5f78c86e957e080be06b", moduleDesired222, moduleInject222, moduleShimLegacy,
			"299b6ebd1a6d89bc34352ac5e2fcf446e4fb7fd08da7bbd178123c85a6a8162d", moduleGrokLegacy, moduleRegistry222, moduleStateLegacy,
			moduleWriterLegacy, moduleSystem222, "da173595e82233c5b20b78026255027004f0eea388fc66a7bf073229dd69e45e",
		),
	),
	reviewedTeardownProfile(
		"2.22.0", "npm_2_22_0_darwin_arm64_v2",
		"sha512-YFWwTrHEgCEeBOsRBuFltLUdmmhf48xEQM4oaIDO9mEiWBor0wAqE0uIvj6eJpyZY0xd/Mhz8O77q9Nflp/y7A==",
		"5d86734bef065edbb72149de08bf2bbb8fa00c114bea69282efe91d7f817bc16",
		teardownModuleHashes(
			"23d58fd93c12138baca76f044899d82aea45768aa38f5f78c86e957e080be06b", moduleDesired222, moduleInject222, moduleShimLegacy,
			"299b6ebd1a6d89bc34352ac5e2fcf446e4fb7fd08da7bbd178123c85a6a8162d", moduleGrokLegacy, moduleRegistry222, moduleStateLegacy,
			moduleWriterLegacy, moduleSystem222, "da173595e82233c5b20b78026255027004f0eea388fc66a7bf073229dd69e45e",
		),
	),
	reviewedTeardownProfile(
		"2.23.0", "npm_2_23_0_darwin_arm64_v1",
		"sha512-oX90wP95Inh9CjxL6CL6+qPGCDWBm0WbcD4gyG+BMi+NvzmuhamVGdmsWIhJwlKd01mEMnUW6BJqmmUT/epF3A==",
		"1114ab69f65069569a18b64ba971d211cf62a075e9997fc274e45a35f865f648",
		teardownModuleHashes(
			"57bd46dc1a69c71fc5d775e860bfa1dee2852d2f8151ff3a22b3c269fe47cf70", moduleDesired223, moduleInject223, moduleShimLegacy,
			"3552e7f943e5bcd6af57acfbe0bd2316d9ab4a060e32e6ca01adac97d27960d4", moduleGrokLegacy, moduleRegistry222, moduleStateLegacy,
			moduleWriterLegacy, moduleSystem223, "de767b040554e966d3858bfbdf8b0a4b047ed816bd24b91d7380b7349fe66ba1",
		),
	),
	reviewedTeardownProfile(
		"2.24.0", "npm_2_24_0_darwin_arm64_v1",
		"sha512-ZEIr5EiKlbZFor1XEwuIje/67BZ89ah4G/0zh7IANxYrrbvkFuNiJre1Nu5WKey2mnTkd7MWXAMSzM0A5nwhoQ==",
		"527abca275d56a62890729f8d955780fea9050f4a661331a2e5a0b89ff1baf23",
		teardownModuleHashes(
			"82a2b976e7b6e80afbf2b72f446758665ef0c78c83adf116f1cad43f2cfbc34b", moduleDesired223, moduleInject223, moduleShimLegacy,
			"3552e7f943e5bcd6af57acfbe0bd2316d9ab4a060e32e6ca01adac97d27960d4", moduleGrokLegacy, moduleRegistry222, moduleStateLegacy,
			moduleWriterLegacy, moduleSystem223, "2091110dc15b31cf2481875cf894e4b16f1dfc10b5f2c81f55d37ae929049fa1",
		),
	),
	reviewedTeardownProfile(
		"2.24.1", "npm_2_24_1_darwin_arm64_v1",
		"sha512-IFVqZ8qv5jArHaW0yA+DtMqUbYsoZvDBym/5xmtbHHtwh2mQYplSEgPsgfkAlQIHmqWaYxkrYrjW8eSoAHAs7w==",
		"59d49cf4a12c367f292ba04d63218635e89f1e669a9404e9c83998977923b0fc",
		teardownModuleHashes(
			"550826e575297c9f0bd8f16cea06355c50bb78bacf4b9382c1d077520683177d", moduleDesired223, moduleInject223, moduleShimLegacy,
			"3552e7f943e5bcd6af57acfbe0bd2316d9ab4a060e32e6ca01adac97d27960d4", moduleGrokLegacy, moduleRegistry222, moduleStateLegacy,
			moduleWriterLegacy, moduleSystem223, "2091110dc15b31cf2481875cf894e4b16f1dfc10b5f2c81f55d37ae929049fa1",
		),
	),
	reviewedTeardownProfile(
		"2.24.2", "npm_2_24_2_darwin_arm64_v1",
		"sha512-LgKPmDhjzmYGNISmxRmMr2w8JHjTLPfR30EIVy//k5c3eWd8+k7/k/B7fuVyPMlI4SFXEgZBqyo7EHKDxJ5pvw==",
		"b1e2d21847299632ea462bef940e7e629fb08abb572df76bef683bfd75bf068a",
		teardownModuleHashes(
			"6eddddb2ce07468a1c26f394225a047ae265a21e7457f38ad6500a62c2f63b66", moduleDesired223, moduleInject223, moduleShimLegacy,
			"3552e7f943e5bcd6af57acfbe0bd2316d9ab4a060e32e6ca01adac97d27960d4", moduleGrokLegacy, moduleRegistry222, moduleStateLegacy,
			moduleWriterLegacy, moduleSystem224, "2091110dc15b31cf2481875cf894e4b16f1dfc10b5f2c81f55d37ae929049fa1",
		),
	),
	reviewedTeardownProfile(
		"2.25.0", "npm_2_25_0_darwin_arm64_v1",
		"sha512-7Hkss1GAEOCk+5+gYvONQZxFjs2NxNXEmW8mlk0i/NXXR9MLN7+tXbtli7K+cefovfHs4vlg6+P8sQceGDS38Q==",
		"3046875d3cb21530919743153ff0f196b13e2f5437c9d9cc4dcdbaf8185cda8e",
		teardownModuleHashes(
			"94bdcc17454ccace4e7e201babb428a1c187fe748194ff6a837df22dfe36cd50", moduleDesired223, moduleInject223, moduleShimLegacy,
			"2297577079aed2e89e4603050402629fea2df094978ff24c0db5102ca8da91ae", moduleGrokLegacy, moduleRegistry222, moduleStateLegacy,
			moduleWriterLegacy, moduleSystem224, "2091110dc15b31cf2481875cf894e4b16f1dfc10b5f2c81f55d37ae929049fa1",
		),
	),
	reviewedTeardownProfile(
		"2.26.0", "npm_2_26_0_darwin_arm64_v1",
		"sha512-5pHAxGEa8l6dzVptonNMD6vfj5Vmiau0vvmR/QHV+412R97wx2xWaoKd1WiBDOLmZWvS5o1XfFGrY+rfHxscZw==",
		"7f2e8a3245198a1d9b3655fed53d8e21ca92ae7c617ac38a69bed4e6dcb718cb",
		teardownModuleHashes(
			"6f5cf9089f862aa882d31d789900771e9be0157832471cf91cc5fc2069a4b3c6", moduleDesired223, moduleInject226, moduleShimLegacy,
			"47a45addede39075329436bc21c271908a05d961c34a131241f86db1a9183620", moduleGrokModern, moduleRegistry226, moduleStateLegacy,
			moduleWriterLegacy, moduleSystem224, "5f30baf3c2b3ef7ecc709de3810eec3bbbf26ba8dea236119ecc15bf9530c857",
		),
	),
	reviewedTeardownProfile(
		"2.27.0", "npm_2_27_0_darwin_arm64_v1",
		"sha512-ge9PxJXnkbUYB7q9mMnt/fZhKltuw+TS1DHAqbJftF3ll82RKp7Im2U0YNi/F1okjyhg4H79DuLkVUV9ZATz/Q==",
		"bad3b9d96dcada5aa69f623924d5bdd436e4246fa3deeb5b0b6fdd4de9d76d7d",
		teardownModuleHashes(
			"f51abeea3f3868735b479d9b97b58e7cbdc452e9e5cc4937dfa794a40d9e10c7", moduleDesired223, moduleInject226, moduleShimLegacy,
			"503fa58079502d808a568a1f11ac757208efae01307f7ac55783493cbc4ab7c8", moduleGrokModern, moduleRegistry226, moduleStateLegacy,
			moduleWriterLegacy, moduleSystem224, "a9babc47b0d3fd5e3f1fcd31f9be8438071ebad6d86fa716f52cecfc4d4a14e3",
		),
	),
	reviewedTeardownProfile(
		"2.28.0", "npm_2_28_0_darwin_arm64_v1",
		"sha512-V/lMcehRY+UI/ypDLoCiZYTia2OWA7tZoAibnCj1ay1vzrwhNzDcmf1ZTZ97RhrH07dhq4J9It5RT6v1/DvBZg==",
		"3b0e4b3f2f8881623ad661fdd2363c3f87b0d38752910c477834e3acb55c3922",
		teardownModuleHashes(
			"7a24163416c4a91b5c086551f354ad721b4dd57b8e92216aab025231446d69ac", moduleDesired223, moduleInject226, moduleShimLegacy,
			"503fa58079502d808a568a1f11ac757208efae01307f7ac55783493cbc4ab7c8", moduleGrokModern, moduleRegistry226, moduleStateLegacy,
			moduleWriterLegacy, moduleSystem228, "a9babc47b0d3fd5e3f1fcd31f9be8438071ebad6d86fa716f52cecfc4d4a14e3",
		),
	),
	reviewedTeardownProfile(
		"2.29.0", "npm_2_29_0_darwin_arm64_v1",
		"sha512-0N5TgmfMdSysrGM4aZbvbbYvl4A6/jKIxI94him5uMta6ddFFsXQ0Y910wpwJ505FIMH9gxFIjNwp+uJhZZ1aw==",
		"15db8efebd3b515db34f9147d98369670479c263813fd6dad9a53c364448a2ba",
		teardownModuleHashes(
			"fd4c288d8c09df6271ed0e94759eba0c897c31a3a66431ba5c658225046c3745", moduleDesired223, moduleInject229, moduleShimLegacy,
			"c0feb3be3e2170236b5cbd2b3b035fdef0458cc4665a6b62fa951bfc2d8ae701", moduleGrokModern, moduleRegistry229, moduleStateLegacy,
			moduleWriter229, moduleSystem228, "a9babc47b0d3fd5e3f1fcd31f9be8438071ebad6d86fa716f52cecfc4d4a14e3",
		),
	),
	reviewedTeardownProfile(
		"2.31.0", "npm_2_31_0_darwin_arm64_v1",
		"sha512-GBycwxCqIy4sMXOEh+vq4ZSkD4fcmkkAWGOEU6WcYHlJV/fAUZeRX4usF/L4wQ1SsgwddGWIkHmQ60oe2vquWQ==",
		"cc3b7f287253308a481b44f0ae069dbd8ccbb76a1fec9b353d7f15b04767468c",
		teardownModuleHashes(
			"3570a5627769ae9dd2796ee178f958ef4e8456479c934274c3b87f6de79f463e", moduleDesired223, moduleInject229, moduleShimLegacy,
			"c0feb3be3e2170236b5cbd2b3b035fdef0458cc4665a6b62fa951bfc2d8ae701", moduleGrokModern, moduleRegistry229, moduleStateLegacy,
			moduleWriter229, moduleSystem228, "3c1198b34fbef2f69f0a8dff1ca3f604bb16e7bdbf96b0d59e9eac0bc29f848b",
		),
	),
	reviewedTeardownProfile(
		"2.32.0", "npm_2_32_0_darwin_arm64_v1",
		"sha512-njHOtoAyHC+w+Dk9ZPJ3Ea50lvXqf2BO4Jq92KUUgFAMD3iK2OMf8puN5tWX9225iepNJrVtQa3IvrOx6+LShw==",
		"1b4630d50e11d9deb05533fcec66f8e75f4bdf029a3431970d7d6e2844769679",
		teardownModuleHashes(
			"189cacf9c5018d308a4d0f87a67259a8f178d69f754077605c2a2c79cde65150", moduleDesired223, moduleInject232, moduleShimLegacy,
			"13dea38a22b2a511adf830f9ddfcb66ff7e384dc52028891585d2e8572cf75c7", moduleGrokModern, moduleRegistry229, moduleState232,
			moduleWriter232, moduleSystem228, "9be297cab5c0b059030e2b60c0954420b0b43b4586039ea6792e66d7a5a9ea7f",
		),
	),
	reviewedTeardownProfile(
		"2.32.1", "npm_2_32_1_darwin_arm64_v1",
		"sha512-Iq1Cqva+ORbDiBmlvm42QYPRVqgTDt2bi9b6nH4Y5pwZN45+qeauB3atRyHGFoMtxfOmFGlTTl8wsMKpD8yRUw==",
		"5d6c88dd40732a9e16219a598b1122f820d178a40858a902550a0b2eecdd6015",
		teardownModuleHashes(
			"04296a57ecf1045f16342e12ccba46e204ba2991cf33660531ea0924b4dbbcc1", moduleDesired223, moduleInject232, moduleShimLegacy,
			"13dea38a22b2a511adf830f9ddfcb66ff7e384dc52028891585d2e8572cf75c7", moduleGrokModern, moduleRegistry229, moduleState232,
			moduleWriter232, moduleSystem228, "9be297cab5c0b059030e2b60c0954420b0b43b4586039ea6792e66d7a5a9ea7f",
		),
	),
	reviewedTeardownProfile(
		"2.33.0", "npm_2_33_0_darwin_arm64_v1",
		"sha512-lZISJQa+oTiIeyydQ1llUFOYH15FkfTsQdbGku/KPPAmrzYJmOTetDDq0/rZt6SERuP5BY9nXnBUKTlvCoK60A==",
		"6b1d2c1cbcc5925f65c8cf8aa54f2ee1038e66159a23801b46515c0fd666f2f7",
		teardownModuleHashes(
			"b2e14672cf7df2f0eadb7e4327c50c6ff2768e6fdc023927fe22da8fc336e249", moduleDesired223, moduleInject232, moduleShim233,
			"875dc438cd7bbbebe3645abac09b85efcaf5829548da154c09f116bc72329ee8", moduleGrokModern, moduleRegistry229, moduleState232,
			moduleWriter232, moduleSystem228, "9be297cab5c0b059030e2b60c0954420b0b43b4586039ea6792e66d7a5a9ea7f",
		),
	),
}
