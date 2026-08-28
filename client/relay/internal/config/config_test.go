package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestValidateRejectsNonLoopbackAndAmbiguousUpstream(t *testing.T) {
	cfg, err := NewDefault("https://gateway.example.test/v1", "file")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ListenAddress = "0.0.0.0:18180"
	if err := cfg.Validate(); err == nil {
		t.Fatal("public listener was accepted")
	}
	cfg.ListenAddress = "127.0.0.1:18180"
	cfg.UpstreamBaseURL = "https://gateway.example.test/v1?unexpected=value"
	if err := cfg.Validate(); err == nil {
		t.Fatal("upstream query was accepted")
	}
}

func TestAutomaticAppServerRestartIsOptInAndHomeIsValidated(t *testing.T) {
	cfg, err := NewDefault("https://gateway.example.test/v1", "file")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Catalog.ManageAppServer {
		t.Fatal("automatic AppServer restart is enabled by default")
	}
	cfg.Catalog.AppServerHome = "relative-home"
	if err := cfg.Validate(); err == nil {
		t.Fatal("relative AppServer home was accepted")
	}
	cfg.Catalog.AppServerHome = "/home/test/.codex/../other"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unclean AppServer home was accepted")
	}
	cfg.Catalog.AppServerHome = "/home/test/.codex"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("clean absolute AppServer home rejected: %v", err)
	}
}

func TestLegacyAutomaticRestartConfigRemainsLoadableButHasNoHomeIdentity(t *testing.T) {
	cfg, err := NewDefault("https://gateway.example.test/v1", "file")
	if err != nil {
		t.Fatal(err)
	}
	// Older JSON contains only this boolean. It must remain loadable so an
	// upgrade fails closed at activation rather than preventing relay startup.
	cfg.Catalog.ManageAppServer = true
	cfg.Catalog.AppServerHome = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("legacy config stopped loading: %v", err)
	}
}

func TestNewDefaultSelectsBackwardCompatibleModes(t *testing.T) {
	cfg, err := NewDefault("https://gateway.example.test/v1", CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpstreamMode != UpstreamModeExternalGateway {
		t.Fatalf("upstream mode = %q, want %q", cfg.UpstreamMode, UpstreamModeExternalGateway)
	}
	if cfg.Responses.WebSocketMode != ResponsesWebSocketModePassthrough {
		t.Fatalf("Responses WebSocket mode = %q, want %q", cfg.Responses.WebSocketMode, ResponsesWebSocketModePassthrough)
	}
	if cfg.Catalog.Owner != CatalogOwnerRelay {
		t.Fatalf("catalog owner = %q, want %q", cfg.Catalog.Owner, CatalogOwnerRelay)
	}
	requireSchedulerDefaults(t, cfg.Responses.Scheduler, "127.0.0.1:18182")
}

func TestLoadLegacyJSONAppliesBackwardCompatibleDefaults(t *testing.T) {
	cfg, err := NewDefault("https://gateway.example.test/v1", CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.UpstreamMode = ""
	cfg.Responses = ResponsesConfig{}
	cfg.Catalog.Owner = ""

	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(encoded, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "upstream_mode")
	delete(legacy, "responses")
	catalog := legacy["catalog"].(map[string]any)
	delete(catalog, "owner")
	encoded, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "relay.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("legacy JSON stopped loading: %v", err)
	}
	if loaded.UpstreamMode != UpstreamModeExternalGateway {
		t.Fatalf("upstream mode = %q, want %q", loaded.UpstreamMode, UpstreamModeExternalGateway)
	}
	if loaded.Responses.WebSocketMode != ResponsesWebSocketModePassthrough {
		t.Fatalf("Responses WebSocket mode = %q, want %q", loaded.Responses.WebSocketMode, ResponsesWebSocketModePassthrough)
	}
	if loaded.Catalog.Owner != CatalogOwnerRelay {
		t.Fatalf("catalog owner = %q, want %q", loaded.Catalog.Owner, CatalogOwnerRelay)
	}
	requireSchedulerDefaults(t, loaded.Responses.Scheduler, "127.0.0.1:18182")
}

func TestLoadDefaultsZeroSchedulerFieldsAndPreservesNonZeroValues(t *testing.T) {
	cfg, err := NewDefault("https://gateway.example.test/v1", CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Responses.Scheduler = ResponsesSchedulerConfig{MaxClassifications: 12}

	path := filepath.Join(t.TempDir(), "relay.json")
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("zero-valued scheduler fields did not default: %v", err)
	}
	want := defaultScheduler("127.0.0.1:18182")
	want.MaxClassifications = 12
	if !reflect.DeepEqual(loaded.Responses.Scheduler, want) {
		t.Fatalf("scheduler = %#v, want %#v", loaded.Responses.Scheduler, want)
	}
}

func TestWriteMaterializesNestedSchedulerSchema(t *testing.T) {
	cfg := mustDefaultConfig(t)
	cfg.Responses.Scheduler = ResponsesSchedulerConfig{}
	path := filepath.Join(t.TempDir(), "relay.json")
	if err := Write(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	responses, ok := raw["responses"].(map[string]any)
	if !ok {
		t.Fatalf("responses object missing from %#v", raw)
	}
	scheduler, ok := responses["scheduler"].(map[string]any)
	if !ok {
		t.Fatalf("responses.scheduler object missing from %#v", responses)
	}
	want := map[string]any{
		"interactive_listen_address":    "127.0.0.1:18182",
		"max_classifications":           float64(8),
		"max_pending_requests":          float64(24),
		"max_pending_encoded_bytes":     float64(536_870_912),
		"queue_timeout_ms":              float64(60_000),
		"max_general_upstream":          float64(4),
		"interactive_reserved_upstream": float64(1),
		"max_concurrent_transforms":     float64(2),
		"max_open_deliveries":           float64(16),
	}
	if !reflect.DeepEqual(scheduler, want) {
		t.Fatalf("serialized scheduler = %#v, want %#v", scheduler, want)
	}
}

func TestLoadDerivesInteractiveListenerFromIPv6Primary(t *testing.T) {
	cfg, err := NewDefault("https://gateway.example.test/v1", CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ListenAddress = "[::1]:18180"
	cfg.Responses.Scheduler = ResponsesSchedulerConfig{}

	path := filepath.Join(t.TempDir(), "relay.json")
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("IPv6 scheduler defaults failed: %v", err)
	}
	requireSchedulerDefaults(t, loaded.Responses.Scheduler, "[::1]:18182")
}

func TestValidateSchedulerListenersAreNumericLoopbackAndDistinct(t *testing.T) {
	for _, address := range []string{
		"localhost:18180",
		"0.0.0.0:18180",
		"127.0.0.2:18180",
		"127.0.0.1:0",
		"127.0.0.1:65536",
		"127.0.0.1:http",
		"127.0.0.1:+18180",
		"127.0.0.1:-1",
	} {
		cfg := mustDefaultConfig(t)
		cfg.ListenAddress = address
		if err := cfg.Validate(); err == nil {
			t.Errorf("invalid primary listener %q was accepted", address)
		}
	}

	for _, address := range []string{
		"localhost:18182",
		"0.0.0.0:18182",
		"127.0.0.2:18182",
		"127.0.0.1:0",
		"127.0.0.1:65536",
		"[::1]:http",
		"[::1]:+18182",
		"[::1]:-1",
	} {
		cfg := mustDefaultConfig(t)
		cfg.Responses.Scheduler.InteractiveListenAddress = address
		if err := cfg.Validate(); err == nil {
			t.Errorf("invalid interactive listener %q was accepted", address)
		}
	}

	cfg := mustDefaultConfig(t)
	cfg.Responses.Scheduler.InteractiveListenAddress = cfg.ListenAddress
	if err := cfg.Validate(); err == nil {
		t.Fatal("scheduler accepted the primary listener as its interactive listener")
	}

	cfg = mustDefaultConfig(t)
	cfg.ListenAddress = "127.0.0.1:18182"
	cfg.Responses.Scheduler = ResponsesSchedulerConfig{}
	if err := cfg.Validate(); err == nil {
		t.Fatal("automatically derived interactive listener collided with the primary listener")
	}

	cfg = mustDefaultConfig(t)
	cfg.ListenAddress = "[::1]:18180"
	cfg.Responses.Scheduler.InteractiveListenAddress = "[::1]:18182"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid IPv6 listeners rejected: %v", err)
	}
	cfg.Responses.Scheduler.InteractiveListenAddress = "127.0.0.1:18180"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("distinct cross-family listeners rejected: %v", err)
	}
}

func TestValidateSchedulerNumericRanges(t *testing.T) {
	tests := []struct {
		name    string
		minimum int
		maximum int
		set     func(*ResponsesSchedulerConfig, int)
	}{
		{"max_classifications", 1, 64, func(c *ResponsesSchedulerConfig, value int) { c.MaxClassifications = value }},
		{"max_pending_requests", 1, 256, func(c *ResponsesSchedulerConfig, value int) { c.MaxPendingRequests = value }},
		{"queue_timeout_ms", 1_000, 300_000, func(c *ResponsesSchedulerConfig, value int) { c.QueueTimeoutMS = value }},
		{"max_general_upstream", 1, 64, func(c *ResponsesSchedulerConfig, value int) { c.MaxGeneralUpstream = value }},
		{"interactive_reserved_upstream", 1, 16, func(c *ResponsesSchedulerConfig, value int) { c.InteractiveReservedUpstream = value }},
		{"max_concurrent_transforms", 1, 16, func(c *ResponsesSchedulerConfig, value int) { c.MaxConcurrentTransforms = value }},
		{"max_open_deliveries", 1, 256, func(c *ResponsesSchedulerConfig, value int) { c.MaxOpenDeliveries = value }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, value := range []int{test.minimum, test.maximum} {
				cfg := mustDefaultConfig(t)
				test.set(&cfg.Responses.Scheduler, value)
				if err := cfg.Validate(); err != nil {
					t.Errorf("boundary value %d rejected: %v", value, err)
				}
			}
			belowMinimum := test.minimum - 1
			if belowMinimum == 0 {
				// Zero is the documented legacy/default sentinel. A negative
				// value exercises the lower-bound validation for 1-based fields.
				belowMinimum = -1
			}
			for _, value := range []int{belowMinimum, test.maximum + 1} {
				cfg := mustDefaultConfig(t)
				test.set(&cfg.Responses.Scheduler, value)
				if err := cfg.Validate(); err == nil {
					t.Errorf("out-of-range value %d was accepted", value)
				}
			}
		})
	}

	for _, value := range []int64{minResponsesPendingBytes, maxResponsesPendingBytes} {
		cfg := mustDefaultConfig(t)
		cfg.Responses.Scheduler.MaxPendingEncodedBytes = value
		if err := cfg.Validate(); err != nil {
			t.Errorf("pending-byte boundary %d rejected: %v", value, err)
		}
	}
	for _, value := range []int64{minResponsesPendingBytes - 1, maxResponsesPendingBytes + 1} {
		cfg := mustDefaultConfig(t)
		cfg.Responses.Scheduler.MaxPendingEncodedBytes = value
		if err := cfg.Validate(); err == nil {
			t.Errorf("out-of-range pending bytes %d were accepted", value)
		}
	}
}

func TestValidateLocalOpenCodexMode(t *testing.T) {
	cfg, err := NewDefault("https://gateway.example.test/v1", CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.UpstreamMode = UpstreamModeLocalOpenCodex
	cfg.Credentials.Source = CredentialsSourceNone
	cfg.Catalog.Owner = CatalogOwnerRemoteManager

	for _, upstream := range []string{localOpenCodexIPv4URL, localOpenCodexIPv6URL} {
		cfg.UpstreamBaseURL = upstream
		if err := cfg.Validate(); err != nil {
			t.Errorf("valid local upstream %q rejected: %v", upstream, err)
		}
	}

	for _, upstream := range []string{
		"http://localhost:10100/v1",
		"http://127.0.0.1:10101/v1",
		"http://127.0.0.1:10100/v1/",
		"https://127.0.0.1:10100/v1",
		"http://127.0.0.2:10100/v1",
	} {
		cfg.UpstreamBaseURL = upstream
		if err := cfg.Validate(); err == nil {
			t.Errorf("non-canonical local upstream %q was accepted", upstream)
		}
	}

	cfg.UpstreamBaseURL = localOpenCodexIPv4URL
	cfg.Credentials.Source = CredentialsSourceFile
	if err := cfg.Validate(); err == nil {
		t.Fatal("local upstream accepted file credentials")
	}
}

func TestValidateExternalGatewayAuthenticationProfiles(t *testing.T) {
	cfg, err := NewDefault("https://gateway.example.test/v1", CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid external gateway rejected: %v", err)
	}
	cfg.Credentials.Source = CredentialsSourceNone
	if err := cfg.Validate(); err != nil {
		t.Fatalf("external gateway rejected authentication_profile=none: %v", err)
	}
	cfg.Credentials.AuthenticationProfile = RemoteAuthenticationGatewayAPIKey
	if err := cfg.Validate(); err == nil {
		t.Fatal("gateway API key profile accepted credentials.source=none")
	}
	cfg.Credentials.Source = CredentialsSourceFile
	cfg.UpstreamBaseURL = "http://192.168.1.50/v1"
	if err := cfg.Validate(); err == nil {
		t.Fatal("HTTP gateway API key was accepted without acknowledgement")
	}
	cfg.Credentials.AllowInsecurePrivateIP = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("acknowledged private HTTP gateway rejected: %v", err)
	}
	cfg.Credentials.AuthenticationProfile = RemoteAuthenticationCloudflareAccessAndGatewayKey
	if err := cfg.Validate(); err == nil {
		t.Fatal("private HTTP gateway accepted Cloudflare credentials")
	}
	cfg.Credentials.AuthenticationProfile = RemoteAuthenticationNone
	cfg.Credentials.Source = CredentialsSourceNone
	cfg.Credentials.AllowInsecurePrivateIP = false
	if err := cfg.Validate(); err == nil {
		t.Fatal("private HTTP gateway without relay credentials was accepted without acknowledgement")
	}
	cfg.Credentials.AllowInsecurePrivateIP = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("acknowledged private HTTP gateway without relay credentials rejected: %v", err)
	}
	cfg.UpstreamBaseURL = "https://gateway.example.test/v1"
	cfg.Credentials.AllowInsecurePrivateIP = false
	cfg.UpstreamMode = "automatic"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown upstream mode was accepted")
	}
}

func TestNormalizeExternalGatewayURL(t *testing.T) {
	valid := map[string]string{
		"https://gateway.example.test":       "https://gateway.example.test/v1",
		"https://gateway.example.test/v1":    "https://gateway.example.test/v1",
		"https://gateway.example.test:8443/": "https://gateway.example.test:8443/v1",
		"http://10.0.0.8":                    "http://10.0.0.8/v1",
		"http://172.16.2.4/v1":               "http://172.16.2.4/v1",
		"http://192.168.2.4:8080/v1":         "http://192.168.2.4:8080/v1",
		"http://[fd12:3456::8]":              "http://[fd12:3456::8]/v1",
	}
	for input, want := range valid {
		got, err := NormalizeExternalGatewayURL(input)
		if err != nil || got != want {
			t.Errorf("NormalizeExternalGatewayURL(%q) = %q, %v; want %q", input, got, err, want)
		}
	}

	invalid := []string{
		"http://gateway.example.test",
		"http://127.0.0.1",
		"http://169.254.1.2",
		"http://203.0.113.1",
		"http://[fe80::1]",
		"https://gateway.example.test/api/v1",
		"https://gateway.example.test/v1?token=value",
		"https://user@gateway.example.test/v1",
		"https://gateway.example.test/v1#fragment",
	}
	for _, input := range invalid {
		if _, err := NormalizeExternalGatewayURL(input); err == nil {
			t.Errorf("NormalizeExternalGatewayURL(%q) unexpectedly succeeded", input)
		}
	}
}

func TestValidateResponsesModes(t *testing.T) {
	cfg, err := NewDefault("https://gateway.example.test/v1", CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Responses.WebSocketMode = ResponsesWebSocketModeHTTPFallback
	cfg.Responses.ModelModes = map[string]string{
		"opencode-go-responses/gpt-5.6-luna": ResponsesModelModeBoundedJSON,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid Responses modes rejected: %v", err)
	}
	cfg.Responses.WebSocketMode = ResponsesWebSocketModePassthrough
	if err := cfg.Validate(); err == nil {
		t.Fatal("configured model normalizer accepted Responses WebSocket passthrough")
	}

	cfg.Responses.WebSocketMode = "automatic"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown Responses WebSocket mode was accepted")
	}
	cfg.Responses.WebSocketMode = ResponsesWebSocketModeHTTPFallback
	cfg.Responses.ModelModes = map[string]string{"gpt-5.6-luna": "streaming"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown Responses model mode was accepted")
	}
}

func TestValidateResponsesModelKeysAreCanonical(t *testing.T) {
	cfg, err := NewDefault("https://gateway.example.test/v1", CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"", " gpt-5.6-luna", "gpt-5.6-luna ", "\tgpt-5.6-luna"} {
		cfg.Responses.ModelModes = map[string]string{model: ResponsesModelModeBoundedJSON}
		if err := cfg.Validate(); err == nil {
			t.Errorf("non-canonical model key %q was accepted", model)
		}
	}
	cfg.Responses.ModelModes = map[string]string{
		"GPT-5.6-LUNA": ResponsesModelModeBoundedJSON,
		"gpt-5.6-luna": ResponsesModelModeBoundedJSON,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("case-insensitive duplicate model keys were accepted")
	}
}

func TestResponsesModeForModelUsesCaseInsensitiveExactMatch(t *testing.T) {
	responses := ResponsesConfig{ModelModes: map[string]string{
		"opencode-go-responses/gpt-5.6-luna": ResponsesModelModeBoundedJSON,
	}}
	if mode, ok := responses.ModeForModel("OPENCODE-GO-RESPONSES/GPT-5.6-LUNA"); !ok || mode != ResponsesModelModeBoundedJSON {
		t.Fatalf("case-insensitive exact lookup = (%q, %t)", mode, ok)
	}
	for _, model := range []string{
		"opencode-go-responses/gpt-5.6-luna:reasoning",
		"gpt-5.6-luna",
		" opencode-go-responses/gpt-5.6-luna",
	} {
		if mode, ok := responses.ModeForModel(model); ok {
			t.Errorf("non-exact model %q inherited mode %q", model, mode)
		}
	}
}

func TestValidateCatalogOwner(t *testing.T) {
	cfg, err := NewDefault("https://gateway.example.test/v1", CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Catalog.Owner = CatalogOwnerRelay
	if err := cfg.Validate(); err != nil {
		t.Fatalf("relay-owned external catalog rejected: %v", err)
	}
	cfg.Catalog.Owner = CatalogOwnerRemoteManager
	if err := cfg.Validate(); err == nil {
		t.Fatal("external gateway accepted remote_manager catalog ownership")
	}
	cfg.UpstreamMode = UpstreamModeLocalOpenCodex
	cfg.UpstreamBaseURL = localOpenCodexIPv4URL
	cfg.Credentials.Source = CredentialsSourceNone
	if err := cfg.Validate(); err != nil {
		t.Fatalf("remote-manager-owned local catalog rejected: %v", err)
	}
	cfg.Catalog.Owner = CatalogOwnerRelay
	if err := cfg.Validate(); err == nil {
		t.Fatal("local OpenCodex accepted relay catalog ownership")
	}
	cfg.Catalog.Owner = "both"
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown catalog owner was accepted")
	}
}

func TestOptionalLocalOpenCodexProfileKeepsExternalConfigCanonical(t *testing.T) {
	cfg := mustDefaultConfig(t)
	profile := &LocalOpenCodexProfile{
		UpstreamBaseURL: localOpenCodexIPv4URL,
		CatalogPath:     filepath.Join(t.TempDir(), "local-catalog.json"),
	}
	cfg.LocalOpenCodex = profile
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid optional local profile rejected: %v", err)
	}
	if cfg.UpstreamMode != UpstreamModeExternalGateway || cfg.Catalog.Owner != CatalogOwnerRelay {
		t.Fatalf("optional profile changed canonical external config: %#v", cfg)
	}
	runtimeCfg, err := cfg.LocalOpenCodexRuntimeConfig()
	if err != nil {
		t.Fatalf("derive local runtime config: %v", err)
	}
	if runtimeCfg.UpstreamMode != UpstreamModeLocalOpenCodex || runtimeCfg.UpstreamBaseURL != localOpenCodexIPv4URL {
		t.Fatalf("local runtime topology = %#v", runtimeCfg)
	}
	if runtimeCfg.Credentials.Source != CredentialsSourceNone || runtimeCfg.Catalog.Owner != CatalogOwnerRelay || runtimeCfg.Catalog.Path != profile.CatalogPath {
		t.Fatalf("local runtime ownership/safety = %#v", runtimeCfg)
	}
	if runtimeCfg.ConnectionProbe.Enabled {
		t.Fatal("local runtime inherited the external connection probe")
	}
}

func TestOptionalLocalOpenCodexProfileRejectsAmbiguousCatalogOrTopology(t *testing.T) {
	cfg := mustDefaultConfig(t)
	cfg.LocalOpenCodex = &LocalOpenCodexProfile{
		UpstreamBaseURL: "http://localhost:10100/v1",
		CatalogPath:     filepath.Join(t.TempDir(), "local-catalog.json"),
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("local profile accepted localhost")
	}
	cfg.LocalOpenCodex.UpstreamBaseURL = localOpenCodexIPv4URL
	cfg.LocalOpenCodex.CatalogPath = cfg.Catalog.Path
	if err := cfg.Validate(); err == nil {
		t.Fatal("local profile accepted the external catalog path")
	}
	cfg.LocalOpenCodex.CatalogPath = "relative-catalog.json"
	if err := cfg.Validate(); err == nil {
		t.Fatal("local profile accepted a relative catalog path")
	}
	cfg.LocalOpenCodex.CatalogPath = filepath.Join(t.TempDir(), "local-catalog.json")
	cfg.UpstreamMode = UpstreamModeLocalOpenCodex
	cfg.UpstreamBaseURL = localOpenCodexIPv4URL
	cfg.Credentials.Source = CredentialsSourceNone
	cfg.Catalog.Owner = CatalogOwnerRemoteManager
	if err := cfg.Validate(); err == nil {
		t.Fatal("legacy local topology accepted an optional relay-owned profile")
	}
}

func TestOptionalLocalOpenCodexProfileRequiresCleanExternalCatalogPath(t *testing.T) {
	cfg := mustDefaultConfig(t)
	cfg.Catalog.Path = "relative-catalog.json"
	cfg.LocalOpenCodex = &LocalOpenCodexProfile{
		UpstreamBaseURL: localOpenCodexIPv4URL,
		CatalogPath:     filepath.Join(t.TempDir(), "local-catalog.json"),
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("dual-profile config accepted a relative external catalog")
	}
}

func TestOptionalLocalOpenCodexProfileRejectsCatalogArtifactAliases(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasDirectory := filepath.Join(root, "alias")
	if err := os.Symlink(realDirectory, aliasDirectory); err != nil {
		t.Fatal(err)
	}
	brokenDirectory := filepath.Join(root, "broken")
	if err := os.Symlink(filepath.Join(root, "missing-target"), brokenDirectory); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(realDirectory, "external.json")

	tests := []struct {
		name  string
		local string
	}{
		{name: "same path", local: external},
		{name: "symlinked parent", local: filepath.Join(aliasDirectory, "external.json")},
		{name: "case-only spelling", local: filepath.Join(realDirectory, "EXTERNAL.JSON")},
		{name: "external pending marker", local: external + CatalogRestartPendingSuffix},
		{name: "external previous backup", local: external + CatalogPreviousSuffix},
		{name: "broken symlink parent", local: filepath.Join(brokenDirectory, "local.json")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := mustDefaultConfig(t)
			cfg.Catalog.Path = external
			cfg.LocalOpenCodex = &LocalOpenCodexProfile{
				UpstreamBaseURL: localOpenCodexIPv4URL,
				CatalogPath:     test.local,
			}
			if err := cfg.Validate(); err == nil {
				t.Fatal("catalog artifact alias was accepted")
			}
			if _, err := cfg.LocalOpenCodexRuntimeConfig(); err == nil {
				t.Fatal("local runtime config accepted a catalog artifact alias")
			}
		})
	}
}

func TestOptionalLocalOpenCodexProfileRejectsHardLinkedCatalog(t *testing.T) {
	root := t.TempDir()
	external := filepath.Join(root, "external.json")
	local := filepath.Join(root, "local.json")
	if err := os.WriteFile(external, []byte("external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(external, local); err != nil {
		t.Fatal(err)
	}
	cfg := mustDefaultConfig(t)
	cfg.Catalog.Path = external
	cfg.LocalOpenCodex = &LocalOpenCodexProfile{
		UpstreamBaseURL: localOpenCodexIPv4URL,
		CatalogPath:     local,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("hard-linked local catalog was accepted")
	}
}

func TestOptionalLocalOpenCodexProfileAllowsDistinctCatalogNamespaces(t *testing.T) {
	root := t.TempDir()
	cfg := mustDefaultConfig(t)
	cfg.Catalog.Path = filepath.Join(root, "external.json")
	cfg.LocalOpenCodex = &LocalOpenCodexProfile{
		UpstreamBaseURL: localOpenCodexIPv4URL,
		CatalogPath:     filepath.Join(root, "local.json"),
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("distinct catalog namespaces rejected: %v", err)
	}
	if _, err := cfg.LocalOpenCodexRuntimeConfig(); err != nil {
		t.Fatalf("distinct local runtime config rejected: %v", err)
	}
}

func TestLegacyLocalOpenCodexRemoteManagerContractRemainsValid(t *testing.T) {
	cfg := mustDefaultConfig(t)
	cfg.UpstreamMode = UpstreamModeLocalOpenCodex
	cfg.UpstreamBaseURL = localOpenCodexIPv6URL
	cfg.Credentials.Source = CredentialsSourceNone
	cfg.Catalog.Owner = CatalogOwnerRemoteManager
	if err := cfg.Validate(); err != nil {
		t.Fatalf("legacy Linux local_opencodex contract changed: %v", err)
	}
	if cfg.HasLocalOpenCodexProfile() {
		t.Fatal("legacy local topology unexpectedly has an optional profile")
	}
}

func TestConnectionProbeIsOptInAndExternalGatewayOnly(t *testing.T) {
	cfg := mustDefaultConfig(t)
	if cfg.ConnectionProbe.Enabled {
		t.Fatal("connection probe is enabled by default")
	}
	cfg.ConnectionProbe.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("external connection probe rejected: %v", err)
	}
	cfg.UpstreamMode = UpstreamModeLocalOpenCodex
	cfg.UpstreamBaseURL = localOpenCodexIPv4URL
	cfg.Credentials.Source = CredentialsSourceNone
	cfg.Catalog.Owner = CatalogOwnerRemoteManager
	if err := cfg.Validate(); err == nil {
		t.Fatal("local OpenCodex accepted external connection probe")
	}
}

func TestNewLocalOpenCodexProfileForCodexConfigUsesSelectedHome(t *testing.T) {
	codexConfig := filepath.Join(t.TempDir(), "custom-home", "config.toml")
	profile, err := NewLocalOpenCodexProfileForCodexConfig(codexConfig)
	if err != nil {
		t.Fatal(err)
	}
	if profile.UpstreamBaseURL != "http://127.0.0.1:10100/v1" || profile.CatalogPath != filepath.Join(filepath.Dir(codexConfig), "opencodex-relay-local-catalog.json") {
		t.Fatalf("custom local profile = %#v", profile)
	}
	if _, err := NewLocalOpenCodexProfileForCodexConfig("relative/config.toml"); err == nil {
		t.Fatal("relative custom Codex config was accepted")
	}
}

func mustDefaultConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := NewDefault("https://gateway.example.test/v1", CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func defaultScheduler(interactiveListenAddress string) ResponsesSchedulerConfig {
	return ResponsesSchedulerConfig{
		InteractiveListenAddress:    interactiveListenAddress,
		MaxClassifications:          DefaultResponsesMaxClassifications,
		MaxPendingRequests:          DefaultResponsesMaxPendingRequests,
		MaxPendingEncodedBytes:      DefaultResponsesMaxPendingEncodedBytes,
		QueueTimeoutMS:              DefaultResponsesQueueTimeoutMS,
		MaxGeneralUpstream:          DefaultResponsesMaxGeneralUpstream,
		InteractiveReservedUpstream: DefaultResponsesInteractiveReservedUpstream,
		MaxConcurrentTransforms:     DefaultResponsesMaxConcurrentTransforms,
		MaxOpenDeliveries:           DefaultResponsesMaxOpenDeliveries,
	}
}

func requireSchedulerDefaults(t *testing.T, got ResponsesSchedulerConfig, interactiveListenAddress string) {
	t.Helper()
	want := defaultScheduler(interactiveListenAddress)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scheduler = %#v, want %#v", got, want)
	}
}
