package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/compat"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/credentials"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/responses"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/scheduler"
)

func TestRelayInjectsOuterCredentialsAndStreams(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listeners are unavailable in this environment: %v", err)
	}
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/responses" {
			t.Fatalf("upstream path = %q", got)
		}
		if got := r.Header.Get("X-OpenCodex-API-Key"); got != "gateway" {
			t.Fatalf("gateway credential = %q", got)
		}
		if got := r.Header.Get("CF-Access-Client-Id"); got != "id" {
			t.Fatalf("Cloudflare client id = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer native" {
			t.Fatalf("native Authorization was not preserved: %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	upstream.Listener = listener
	upstream.Start()
	defer upstream.Close()
	cfg, err := config.NewDefault(upstream.URL+"/v1", "file")
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(cfg, func() (credentials.Values, error) {
		return credentials.Values{CFClientID: "id", CFClientSecret: "secret", GatewayKey: "gateway"}, nil
	}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	relayListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listeners are unavailable in this environment: %v", err)
	}
	relay := httptest.NewUnstartedServer(server.Handler())
	relay.Listener = relayListener
	relay.Start()
	defer relay.Close()
	req, _ := http.NewRequest(http.MethodPost, relay.URL+"/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer native")
	req.Header.Set("X-OpenCodex-API-Key", "untrusted")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type = %q", response.Header.Get("Content-Type"))
	}
}

func TestAppleRuntimeRewriteStripsCallerAdmissionHeadersAndInjectsOnlyAPIKey(t *testing.T) {
	directory := t.TempDir()
	codexPath := filepath.Join(directory, "config.toml")
	canonical, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceNone)
	if err != nil {
		t.Fatal(err)
	}
	canonical.Catalog.Path = filepath.Join(directory, "external-catalog.json")
	canonical.LocalAppleContainer, err = config.NewLocalAppleContainerProfileForCodexConfig(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := canonical.LocalAppleContainerRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(cfg, func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	incoming := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	incoming.Header.Set("Authorization", "Bearer native")
	incoming.Header.Set("CF-Access-Client-Id", "caller-id")
	incoming.Header.Set("CF-Access-Client-Secret", "caller-secret")
	incoming.Header.Set("Cf-Access-Jwt-Assertion", "caller-jwt")
	incoming.Header.Set("X-OpenCodex-API-Key", "caller-key")
	incoming.Header.Set("X-OpenCodex-Relay", "caller-marker")
	incoming = incoming.WithContext(context.WithValue(incoming.Context(), credentialContextKey{}, credentials.Values{
		CFClientID:           "must-not-be-used",
		CFClientSecret:       "must-not-be-used",
		GatewayKey:           "must-not-be-used",
		LocalOpenCodexAPIKey: "apple-api-key",
	}))
	outgoing := incoming.Clone(incoming.Context())
	server.rewrite(&httputil.ProxyRequest{In: incoming, Out: outgoing})
	if outgoing.URL.String() != "http://127.0.0.1:10210/v1/models" {
		t.Fatalf("Apple upstream URL = %q", outgoing.URL.String())
	}
	if outgoing.Header.Get("X-OpenCodex-API-Key") != "apple-api-key" || outgoing.Header.Get("X-OpenCodex-Relay") != "pw-local-v1" {
		t.Fatalf("Apple injected headers = %#v", outgoing.Header)
	}
	if outgoing.Header.Get("CF-Access-Client-Id") != "" || outgoing.Header.Get("CF-Access-Client-Secret") != "" || outgoing.Header.Get("Cf-Access-Jwt-Assertion") != "" {
		t.Fatalf("Apple runtime retained Cloudflare headers: %#v", outgoing.Header)
	}
	if outgoing.Header.Get("Authorization") != "Bearer native" {
		t.Fatalf("Apple runtime changed caller Authorization = %q", outgoing.Header.Get("Authorization"))
	}
}

func TestJoinPathPreservesBasePrefixWithoutDuplicatingV1(t *testing.T) {
	for _, test := range []struct {
		base string
		path string
		want string
	}{
		{base: "/v1", path: "/v1/models", want: "/v1/models"},
		{base: "/gateway/v1", path: "/v1/responses", want: "/gateway/v1/responses"},
		{base: "/gateway/v1/", path: "/v1/alpha/search", want: "/gateway/v1/alpha/search"},
	} {
		if got := joinPath(test.base, test.path); got != test.want {
			t.Fatalf("joinPath(%q, %q) = %q, want %q", test.base, test.path, got, test.want)
		}
	}
}

func TestRelayFailsClosedForDisabledVoice(t *testing.T) {
	cfg, err := config.NewDefault("https://example.test/v1", "file")
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(cfg, func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/live", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestRelayRejectsAnIncompleteCredentialLoaderResult(t *testing.T) {
	cfg, err := config.NewDefault("https://example.test/v1", "file")
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(cfg, func() (credentials.Values, error) {
		return credentials.Values{CFClientID: "only-one-value"}, nil
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestHealthReportsSharedSchedulerAndListenerLane(t *testing.T) {
	cfg, err := config.NewDefault("https://example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(cfg, func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		lane scheduler.Lane
	}{
		{lane: scheduler.LaneGeneral},
		{lane: scheduler.LaneInteractive},
	} {
		recorder := httptest.NewRecorder()
		server.HandlerForLane(test.lane).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/__relay/healthz", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s health status = %d", test.lane, recorder.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["listener_lane"] != string(test.lane) || body["general_listener"] != cfg.ListenAddress || body["interactive_listener"] != cfg.Responses.Scheduler.InteractiveListenAddress {
			t.Fatalf("%s health listeners = %#v", test.lane, body)
		}
		limits, ok := body["scheduler_limits"].(map[string]any)
		if !ok || limits["max_general_upstream"] != float64(4) || limits["interactive_reserved_upstream"] != float64(1) {
			t.Fatalf("%s scheduler limits = %#v", test.lane, limits)
		}
		for _, field := range []string{"active_classifications", "pending_requests", "pending_encoded_bytes", "active_general_upstream", "active_interactive_upstream", "active_transforms", "active_deliveries", "capacity_rejections"} {
			if value, ok := body[field].(float64); !ok || value < 0 {
				t.Fatalf("%s %s = %#v", test.lane, field, body[field])
			}
		}
	}
}

func TestRoutingParkedStatesDenyBeforeCredentialsOrUpstream(t *testing.T) {
	for _, test := range []struct {
		name        string
		desiredMode routing.Mode
		appliedMode routing.Mode
		phase       routing.Phase
	}{
		{
			name:        "native active",
			desiredMode: routing.ModeNative,
			appliedMode: routing.ModeNative,
			phase:       routing.PhaseNativeActive,
		},
		{
			name:        "applying native",
			desiredMode: routing.ModeNative,
			appliedMode: routing.ModeRelay,
			phase:       routing.PhaseApplying,
		},
		{
			name:        "recovery required",
			desiredMode: routing.ModeUnknown,
			appliedMode: routing.ModeUnknown,
			phase:       routing.PhaseRecoveryRequired,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(t.TempDir(), "relay.json")
			store, err := routing.Open(configPath)
			if err != nil {
				t.Fatal(err)
			}
			state, err := routing.NewRelayState(configPath)
			if err != nil {
				t.Fatal(err)
			}
			state, err = routing.BindCodexConfig(state, filepath.Join(filepath.Dir(configPath), "codex-config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			state.DesiredMode = test.desiredMode
			state.AppliedMode = test.appliedMode
			state.DesiredBackend = testBackendForMode(test.desiredMode)
			state.AppliedBackend = testBackendForMode(test.appliedMode)
			state.Phase = test.phase
			lock, err := store.Lock(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := lock.Save(state); err != nil {
				_ = lock.Close()
				t.Fatal(err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
			watcher := routing.NewWatcher(store, 0)

			var credentialLoads atomic.Int64
			var upstreamCalls atomic.Int64
			server, err := New(
				cfg,
				func() (credentials.Values, error) {
					credentialLoads.Add(1)
					return credentials.Values{CFClientID: "id", CFClientSecret: "secret", GatewayKey: "gateway"}, nil
				},
				nil,
				slog.New(slog.NewTextHandler(io.Discard, nil)),
				WithRouting(watcher),
			)
			if err != nil {
				t.Fatal(err)
			}
			server.proxy.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
				upstreamCalls.Add(1)
				return nil, fmt.Errorf("unexpected upstream call")
			})

			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
			var body struct {
				Error struct {
					Type string `json:"type"`
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Type != "service_unavailable" || body.Error.Code != "relay_native_mode" {
				t.Fatalf("typed error = %#v", body.Error)
			}
			if credentialLoads.Load() != 0 || upstreamCalls.Load() != 0 {
				t.Fatalf("parked request performed credential/upstream work: credentials=%d upstream=%d", credentialLoads.Load(), upstreamCalls.Load())
			}
			if server.Tracker().Active() != 0 {
				t.Fatalf("parked request leaked active tracking: %d", server.Tracker().Active())
			}

			health := httptest.NewRecorder()
			server.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/__relay/healthz", nil))
			var healthBody map[string]any
			if err := json.Unmarshal(health.Body.Bytes(), &healthBody); err != nil {
				t.Fatal(err)
			}
			if healthBody["relay_admission"] != "deny" || healthBody["catalog_refresh"] != "pause" {
				t.Fatalf("parked health gate = %#v", healthBody)
			}
		})
	}
}

func testBackendForMode(mode routing.Mode) routing.Backend {
	switch mode {
	case routing.ModeRelay:
		return routing.BackendExternal
	case routing.ModeNative:
		return routing.BackendNone
	default:
		return routing.BackendUnknown
	}
}

func TestLocalResponsesNormalizerUsesOneRequestAndPreservesNativeAuthorization(t *testing.T) {
	var calls atomic.Int64
	upstream := newLoopbackTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("CF-Access-Client-Id") != "" || r.Header.Get("CF-Access-Client-Secret") != "" || r.Header.Get("X-OpenCodex-API-Key") != "" {
			t.Error("local upstream received outer admission credentials")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer native" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		if got := r.Header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("Accept-Encoding = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		}
		if stream, ok := body["stream"].(bool); !ok || stream {
			t.Errorf("upstream stream = %#v", body["stream"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_redacted","object":"response","status":"completed","output":[{"id":"fc_redacted","type":"function_call","call_id":"call_redacted","name":"fixture_tool","arguments":"{\"value\":1}"}],"usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer upstream.Close()

	cfg := normalizerTestConfig(t, upstream.URL+"/v1")
	var credentialLoads atomic.Int64
	server, err := New(cfg, func() (credentials.Values, error) {
		credentialLoads.Add(1)
		return credentials.Values{}, nil
	}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	transport := server.proxy.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("local transport retained an environment proxy")
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"opencode-go-responses/gpt-5.6-luna","stream":true,"store":false,"input":[]}`))
	request.Header.Set("Authorization", "Bearer native")
	request.Header.Set("CF-Access-Client-Id", "untrusted")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
	if credentialLoads.Load() != 0 {
		t.Fatalf("local credential loads = %d", credentialLoads.Load())
	}
	body := response.Body.String()
	for _, event := range []string{"response.created", "response.output_item.done", "response.completed"} {
		if !strings.Contains(body, `"type":"`+event+`"`) {
			t.Errorf("missing event %s in %q", event, body)
		}
	}
	if strings.Count(body, "data: [DONE]\n\n") != 1 {
		t.Fatalf("DONE count = %d body=%q", strings.Count(body, "data: [DONE]\n\n"), body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("response has no terminal EOF marker: %q", body)
	}
	if server.Tracker().Active() != 0 {
		t.Fatalf("active requests = %d", server.Tracker().Active())
	}
}

func TestResponsesSchedulerReservesInteractiveCapacity(t *testing.T) {
	arrivals := make(chan struct{}, 8)
	release := make(chan struct{})
	upstream := newLoopbackTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrivals <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_capacity","object":"response","status":"completed","output":[]}`)
	}))
	defer upstream.Close()
	cfg := normalizerTestConfig(t, upstream.URL+"/v1")
	cfg.Responses.Scheduler.MaxGeneralUpstream = 4
	cfg.Responses.Scheduler.InteractiveReservedUpstream = 1
	server, err := New(cfg, func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		lane scheduler.Lane
		code int
	}
	results := make(chan outcome, 6)
	run := func(lane scheduler.Lane) {
		go func() {
			recorder := httptest.NewRecorder()
			server.HandlerForLane(lane).ServeHTTP(recorder, normalizedRequest())
			results <- outcome{lane: lane, code: recorder.Code}
		}()
	}
	for range 4 {
		run(scheduler.LaneGeneral)
	}
	for range 4 {
		select {
		case <-arrivals:
		case <-time.After(2 * time.Second):
			t.Fatal("four general requests did not reach upstream")
		}
	}
	run(scheduler.LaneGeneral)
	select {
	case <-arrivals:
		t.Fatal("fifth general request bypassed the general capacity limit")
	case <-time.After(40 * time.Millisecond):
	}
	run(scheduler.LaneInteractive)
	select {
	case <-arrivals:
	case <-time.After(2 * time.Second):
		t.Fatal("interactive request did not use its reserved permit")
	}
	snapshot := server.responsesScheduler.Snapshot()
	if snapshot.ActiveGeneralUpstream != 4 || snapshot.ActiveInteractiveUpstream != 1 || snapshot.WaitingGeneralUpstream != 1 {
		t.Fatalf("scheduler snapshot = %#v", snapshot)
	}
	close(release)
	for range 6 {
		select {
		case result := <-results:
			if result.code != http.StatusOK {
				t.Errorf("%s response status = %d", result.lane, result.code)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("scheduled request did not finish")
		}
	}
}

func TestResponsesSchedulerTimesOutBeforeUpstreamWithoutReplay(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int64
	upstream := newLoopbackTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		arrived <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_timeout","object":"response","status":"completed","output":[]}`)
	}))
	defer upstream.Close()
	cfg := normalizerTestConfig(t, upstream.URL+"/v1")
	cfg.Responses.Scheduler.MaxGeneralUpstream = 1
	cfg.Responses.Scheduler.QueueTimeoutMS = 50
	server, err := New(cfg, func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		server.HandlerForLane(scheduler.LaneGeneral).ServeHTTP(recorder, normalizedRequest())
		firstDone <- recorder.Code
	}()
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not reach upstream")
	}

	recorder := httptest.NewRecorder()
	server.HandlerForLane(scheduler.LaneGeneral).ServeHTTP(recorder, normalizedRequest())
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q", got)
	}
	if !strings.Contains(recorder.Body.String(), `"type":"rate_limit_error"`) || !strings.Contains(recorder.Body.String(), `"code":"responses_normalizer_capacity_exhausted"`) {
		t.Fatalf("capacity body = %s", recorder.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	close(release)
	if code := <-firstDone; code != http.StatusOK {
		t.Fatalf("first status = %d", code)
	}
}

type closeUnblocksRequestBody struct {
	started   chan struct{}
	closed    chan struct{}
	readOnce  sync.Once
	closeOnce sync.Once
}

func newCloseUnblocksRequestBody() *closeUnblocksRequestBody {
	return &closeUnblocksRequestBody{started: make(chan struct{}), closed: make(chan struct{})}
}

func (r *closeUnblocksRequestBody) Read([]byte) (int, error) {
	r.readOnce.Do(func() { close(r.started) })
	<-r.closed
	return 0, io.ErrClosedPipe
}

func (r *closeUnblocksRequestBody) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

func TestResponsesSchedulerDeadlineClosesStalledRequestBody(t *testing.T) {
	var calls atomic.Int64
	upstream := newLoopbackTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	cfg := normalizerTestConfig(t, upstream.URL+"/v1")
	cfg.Responses.Scheduler.QueueTimeoutMS = 40
	server, err := New(cfg, func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	body := newCloseUnblocksRequestBody()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	request.ContentLength = -1
	recorder := httptest.NewRecorder()
	started := time.Now()
	server.HandlerForLane(scheduler.LaneGeneral).ServeHTTP(recorder, request)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stalled request took %s", elapsed)
	}
	if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != "5" {
		t.Fatalf("status=%d Retry-After=%q body=%s", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("stalled body reached upstream %d times", calls.Load())
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("stalled request body was not closed")
	}
	waitForProxySnapshot(t, server, func(snapshot scheduler.Snapshot) bool {
		return snapshot.ActiveClassifications == 0 && snapshot.PendingRequests == 0 && snapshot.PendingBytes == 0
	})
}

type inspectFirstReadCloser struct {
	reader  io.Reader
	onFirst func()
	once    sync.Once
}

func (r *inspectFirstReadCloser) Read(destination []byte) (int, error) {
	r.once.Do(r.onFirst)
	return r.reader.Read(destination)
}

func (r *inspectFirstReadCloser) Close() error { return nil }

func TestResponsesSchedulerReservesKnownContentLengthBeforeReading(t *testing.T) {
	body := []byte(`{"model":"other/responses-model","stream":true,"input":[]}`)
	upstream := newLoopbackTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	server, err := New(normalizerTestConfig(t, upstream.URL+"/v1"), func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var reservedAtFirstRead atomic.Int64
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", &inspectFirstReadCloser{
		reader: bytes.NewReader(body),
		onFirst: func() {
			reservedAtFirstRead.Store(server.responsesScheduler.Snapshot().PendingBytes)
		},
	})
	request.ContentLength = int64(len(body))
	response := httptest.NewRecorder()
	server.HandlerForLane(scheduler.LaneGeneral).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if got := reservedAtFirstRead.Load(); got != int64(len(body)) {
		t.Fatalf("reserved bytes at first read = %d, want %d", got, len(body))
	}
	waitForProxySnapshot(t, server, func(snapshot scheduler.Snapshot) bool {
		return snapshot.PendingRequests == 0 && snapshot.PendingBytes == 0
	})
}

func TestResponsesSchedulerRejectsMismatchedKnownContentLengthWithoutDispatch(t *testing.T) {
	body := []byte(`{"model":"other/responses-model","stream":true,"input":[]}`)
	for _, delta := range []int64{-1, 1} {
		t.Run(fmt.Sprintf("delta_%d", delta), func(t *testing.T) {
			var calls atomic.Int64
			upstream := newLoopbackTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				calls.Add(1)
			}))
			defer upstream.Close()
			server, err := New(normalizerTestConfig(t, upstream.URL+"/v1"), func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			request.ContentLength = int64(len(body)) + delta
			response := httptest.NewRecorder()
			server.HandlerForLane(scheduler.LaneGeneral).ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
			if calls.Load() != 0 {
				t.Fatalf("mismatched request reached upstream %d times", calls.Load())
			}
			waitForProxySnapshot(t, server, func(snapshot scheduler.Snapshot) bool {
				return snapshot.PendingRequests == 0 && snapshot.PendingBytes == 0
			})
		})
	}
}

type blockingResponseWriter struct {
	header  http.Header
	started chan struct{}
	release chan struct{}
	once    sync.Once
	code    int
}

func (w *blockingResponseWriter) Header() http.Header { return w.header }

func (w *blockingResponseWriter) WriteHeader(code int) { w.code = code }

func (w *blockingResponseWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(data), nil
}

func TestSlowDeliveryDoesNotHoldUpstreamOrTransformCapacity(t *testing.T) {
	upstream := newLoopbackTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_delivery","object":"response","status":"completed","output":[]}`)
	}))
	defer upstream.Close()
	cfg := normalizerTestConfig(t, upstream.URL+"/v1")
	cfg.Responses.Scheduler.MaxOpenDeliveries = 1
	cfg.Responses.Scheduler.MaxConcurrentTransforms = 1
	cfg.Responses.Scheduler.MaxGeneralUpstream = 2
	server, err := New(cfg, func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	blocked := &blockingResponseWriter{header: make(http.Header), started: make(chan struct{}), release: make(chan struct{})}
	firstDone := make(chan struct{})
	go func() {
		server.HandlerForLane(scheduler.LaneGeneral).ServeHTTP(blocked, normalizedRequest())
		close(firstDone)
	}()
	select {
	case <-blocked.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first response did not reach client delivery")
	}
	waitForProxySnapshot(t, server, func(snapshot scheduler.Snapshot) bool {
		return snapshot.ActiveDeliveries == 1 && snapshot.ActiveGeneralUpstream == 0 && snapshot.ActiveTransforms == 0
	})

	secondDone := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		server.HandlerForLane(scheduler.LaneGeneral).ServeHTTP(recorder, normalizedRequest())
		secondDone <- recorder.Code
	}()
	waitForProxySnapshot(t, server, func(snapshot scheduler.Snapshot) bool {
		return snapshot.WaitingDeliveries == 1 && snapshot.ActiveGeneralUpstream == 0 && snapshot.ActiveTransforms == 0
	})
	close(blocked.release)
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first slow delivery did not finish")
	}
	select {
	case code := <-secondDone:
		if code != http.StatusOK {
			t.Fatalf("second status = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second delivery did not resume")
	}
	waitForProxySnapshot(t, server, func(snapshot scheduler.Snapshot) bool {
		return snapshot.ActiveDeliveries == 0 && snapshot.WaitingDeliveries == 0
	})
}

func TestDeliveryWaitersRemainInsideEndToEndJobQuota(t *testing.T) {
	var calls atomic.Int64
	upstream := newLoopbackTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_delivery_bound","object":"response","status":"completed","output":[]}`)
	}))
	defer upstream.Close()
	cfg := normalizerTestConfig(t, upstream.URL+"/v1")
	cfg.Responses.Scheduler.MaxPendingRequests = 2
	cfg.Responses.Scheduler.MaxOpenDeliveries = 1
	// Keep this config-valid timeout comfortably above the delivery pause. The
	// third request supplies its own short deadline below, so it is the only
	// request that must time out while the two quota slots are occupied.
	cfg.Responses.Scheduler.QueueTimeoutMS = 1_000
	server, err := New(cfg, func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	blocked := &blockingResponseWriter{header: make(http.Header), started: make(chan struct{}), release: make(chan struct{})}
	firstDone := make(chan struct{})
	go func() {
		server.HandlerForLane(scheduler.LaneGeneral).ServeHTTP(blocked, normalizedRequest())
		close(firstDone)
	}()
	select {
	case <-blocked.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first response did not reach client delivery")
	}

	secondDone := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		server.HandlerForLane(scheduler.LaneGeneral).ServeHTTP(recorder, normalizedRequest())
		secondDone <- recorder.Code
	}()
	waitForProxySnapshot(t, server, func(snapshot scheduler.Snapshot) bool {
		return snapshot.PendingRequests == 2 && snapshot.ActiveDeliveries == 1 && snapshot.WaitingDeliveries == 1
	})

	thirdCtx, thirdCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer thirdCancel()
	third := httptest.NewRecorder()
	server.HandlerForLane(scheduler.LaneGeneral).ServeHTTP(third, normalizedRequest().WithContext(thirdCtx))
	if third.Code != http.StatusTooManyRequests {
		t.Fatalf("third status = %d body=%s", third.Code, third.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}

	close(blocked.release)
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first slow delivery did not finish")
	}
	select {
	case code := <-secondDone:
		if code != http.StatusOK {
			t.Fatalf("second status = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second delivery did not resume")
	}
	waitForProxySnapshot(t, server, func(snapshot scheduler.Snapshot) bool {
		return snapshot.PendingRequests == 0 && snapshot.PendingBytes == 0 && snapshot.ActiveDeliveries == 0 && snapshot.WaitingDeliveries == 0
	})
}

func TestNormalizedNon2xxIsCapturedBeforeSlowDelivery(t *testing.T) {
	upstream := newLoopbackTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Error", "preserved")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","code":"fixture_limit"}}`)
	}))
	defer upstream.Close()
	cfg := normalizerTestConfig(t, upstream.URL+"/v1")
	cfg.Responses.Scheduler.MaxGeneralUpstream = 1
	server, err := New(cfg, func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	blocked := &blockingResponseWriter{header: make(http.Header), started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		server.HandlerForLane(scheduler.LaneGeneral).ServeHTTP(blocked, normalizedRequest())
		close(done)
	}()
	select {
	case <-blocked.started:
	case <-time.After(2 * time.Second):
		t.Fatal("non-2xx response did not reach client delivery")
	}
	waitForProxySnapshot(t, server, func(snapshot scheduler.Snapshot) bool {
		return snapshot.PendingRequests == 1 && snapshot.ActiveGeneralUpstream == 0 && snapshot.ActiveDeliveries == 1
	})
	if blocked.code != http.StatusTooManyRequests || blocked.header.Get("X-Upstream-Error") != "preserved" {
		t.Fatalf("non-2xx status=%d headers=%v", blocked.code, blocked.header)
	}
	close(blocked.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("non-2xx delivery did not finish")
	}
	waitForProxySnapshot(t, server, func(snapshot scheduler.Snapshot) bool {
		return snapshot.PendingRequests == 0 && snapshot.PendingBytes == 0 && snapshot.ActiveDeliveries == 0
	})
}

func waitForProxySnapshot(t *testing.T, server *Server, condition func(scheduler.Snapshot) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition(server.responsesScheduler.Snapshot()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("scheduler condition not met: %#v", server.responsesScheduler.Snapshot())
}

func TestResponsesNormalizerRejectsHostedComputerBeforeUpstream(t *testing.T) {
	var calls atomic.Int64
	upstream := newLoopbackTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	server, err := New(normalizerTestConfig(t, upstream.URL+"/v1"), func() (credentials.Values, error) {
		t.Fatal("local credential loader was called")
		return credentials.Values{}, nil
	}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"opencode-go-responses/gpt-5.6-luna","stream":true,"tools":[{"type":"computer"}]}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("hosted computer reached upstream %d times", calls.Load())
	}
	if !strings.Contains(response.Body.String(), `"type":"invalid_request_error"`) || !strings.Contains(response.Body.String(), `"code":"hosted_computer_unsupported_by_native_codex"`) {
		t.Fatalf("error body = %s", response.Body.String())
	}
}

func TestResponsesNormalizerBypassesHostedImageWithoutChangingBytes(t *testing.T) {
	original := []byte(`{"model":"opencode-go-responses/gpt-5.6-luna","stream":true,"tools":[{"type":"image_generation"}]}`)
	var got []byte
	upstream := newLoopbackTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer upstream.Close()
	server, err := New(normalizerTestConfig(t, upstream.URL+"/v1"), func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(original)))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("hosted image request changed: %s", got)
	}
	if response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
	}
}

func TestResponsesNormalizerLeavesNonTargetAndStreamFalseRequestsTransparent(t *testing.T) {
	requests := [][]byte{
		[]byte(`{"model":"gpt-5.6-luna","stream":true,"input":[]}`),
		[]byte(`{"model":"opencode-go-responses/gpt-5.6-luna","stream":false,"input":[]}`),
	}
	var index atomic.Int64
	upstream := newLoopbackTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := int(index.Add(1) - 1)
		body, _ := io.ReadAll(r.Body)
		if current >= len(requests) || !bytes.Equal(body, requests[current]) {
			t.Errorf("request %d body = %q", current, body)
		}
		if current == 0 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_passthrough\"}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_nonstream","object":"response","status":"completed","output":[]}`)
	}))
	defer upstream.Close()
	server, err := New(normalizerTestConfig(t, upstream.URL+"/v1"), func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for requestIndex, body := range requests {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		server.HandlerForLane(scheduler.LaneGeneral).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d status = %d body=%s", requestIndex, recorder.Code, recorder.Body.String())
		}
	}
	if got := index.Load(); got != int64(len(requests)) {
		t.Fatalf("upstream calls = %d", got)
	}
	snapshot := server.responsesScheduler.Snapshot()
	if snapshot.PendingRequests != 0 || snapshot.PendingBytes != 0 || snapshot.ActiveGeneralUpstream != 0 || snapshot.ActiveTransforms != 0 || snapshot.ActiveDeliveries != 0 {
		t.Fatalf("passthrough leaked scheduler capacity: %#v", snapshot)
	}
}

func TestPassthroughBodyRetainsQuotaUntilOutboundClose(t *testing.T) {
	body := []byte(`{"model":"other/responses-model","stream":true,"input":[]}`)
	server, err := New(normalizerTestConfig(t, "http://127.0.0.1:1/v1"), func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	arrived := make(chan struct{})
	release := make(chan struct{})
	server.proxy.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		close(arrived)
		<-release
		got, readErr := io.ReadAll(request.Body)
		closeErr := request.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if !bytes.Equal(got, body) {
			return nil, fmt.Errorf("passthrough body = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			Request:    request,
		}, nil
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		server.HandlerForLane(scheduler.LaneGeneral).ServeHTTP(response, request)
		done <- response
	}()
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("passthrough request did not reach transport")
	}
	snapshot := server.responsesScheduler.Snapshot()
	if snapshot.PendingRequests != 1 || snapshot.PendingBytes != int64(len(body)) {
		t.Fatalf("retained passthrough snapshot = %#v", snapshot)
	}
	close(release)
	select {
	case response := <-done:
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("passthrough request did not finish")
	}
	waitForProxySnapshot(t, server, func(snapshot scheduler.Snapshot) bool {
		return snapshot.PendingRequests == 0 && snapshot.PendingBytes == 0
	})
}

type releaseOrderBody struct {
	retained bool
	readEOF  bool
}

func (b *releaseOrderBody) Read([]byte) (int, error) {
	b.retained = false
	b.readEOF = true
	return 0, io.EOF
}

func (b *releaseOrderBody) Close() error {
	b.retained = false
	return nil
}

func TestReleaseReadCloserDropsSourceOwnershipBeforeJobRelease(t *testing.T) {
	for _, operation := range []string{"EOF", "Close"} {
		t.Run(operation, func(t *testing.T) {
			source := &releaseOrderBody{retained: true}
			released := false
			body := &releaseReadCloser{
				source: source,
				release: func() {
					if source.retained {
						t.Fatal("job released before source backing was dropped")
					}
					released = true
				},
			}
			if operation == "EOF" {
				if _, err := body.Read(make([]byte, 1)); err != io.EOF {
					t.Fatalf("Read() error = %v, want EOF", err)
				}
			} else if err := body.Close(); err != nil {
				t.Fatal(err)
			}
			if !released {
				t.Fatal("job release callback was not invoked")
			}
		})
	}
}

func TestResponsesNormalizerUnexpectedSSEFailsOnceWithoutReplay(t *testing.T) {
	var calls atomic.Int64
	upstream := newLoopbackTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer upstream.Close()
	server, err := New(normalizerTestConfig(t, upstream.URL+"/v1"), func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"opencode-go-responses/gpt-5.6-luna","stream":true,"input":[]}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
}

func TestExternalResponsesNormalizerKeepsAdmissionAndNativeAuthorization(t *testing.T) {
	var calls atomic.Int64
	upstream := newLoopbackTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("CF-Access-Client-Id") != "edge-id" || r.Header.Get("CF-Access-Client-Secret") != "edge-secret" || r.Header.Get("X-OpenCodex-API-Key") != "gateway" {
			t.Error("external admission credentials were not injected")
		}
		if r.Header.Get("Authorization") != "Bearer native" {
			t.Error("native Authorization was not preserved")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"stream":false`)) {
			t.Errorf("upstream body = %s", body)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, `{"id":"resp_fixture","status":"completed","output":[{"type":"message","role":"assistant","content":[]}]}`)
	}))
	defer upstream.Close()
	cfg, err := config.NewDefault(upstream.URL+"/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Responses.WebSocketMode = config.ResponsesWebSocketModeHTTPFallback
	cfg.Responses.ModelModes = map[string]string{"opencode-go-responses/gpt-5.6-luna": config.ResponsesModelModeBoundedJSON}
	server, err := New(cfg, func() (credentials.Values, error) {
		return credentials.Values{CFClientID: "edge-id", CFClientSecret: "edge-secret", GatewayKey: "gateway"}, nil
	}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"opencode-go-responses/gpt-5.6-luna","stream":true,"input":[]}`))
	request.Header.Set("Authorization", "Bearer native")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"type":"response.completed"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
}

func TestResponsesNormalizerRejectsBodyIntegrityHeaderBeforeDispatch(t *testing.T) {
	var calls atomic.Int64
	upstream := newLoopbackTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	server, err := New(normalizerTestConfig(t, upstream.URL+"/v1"), func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"opencode-go-responses/gpt-5.6-luna","stream":true,"input":[]}`))
	request.Header.Set("Content-Digest", "sha-256=:fixture:")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"body_integrity_header_unsupported"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("digest-bearing request reached upstream %d times", calls.Load())
	}
}

func TestResponsesNormalizerRejectsCompressedJSONResponse(t *testing.T) {
	var calls atomic.Int64
	upstream := newLoopbackTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("Accept-Encoding = %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = io.WriteString(w, `{"id":"r","status":"completed","output":[]}`)
	}))
	defer upstream.Close()
	server, err := New(normalizerTestConfig(t, upstream.URL+"/v1"), func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, normalizedRequest())
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "unexpected_responses_content_encoding") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
}

func TestResponsesNormalizerDropsStaleRepresentationHeadersAndTrailers(t *testing.T) {
	upstream := newLoopbackTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=upstream")
		w.Header().Set("ETag", `"json-representation"`)
		w.Header().Set("Content-Digest", "sha-256=:fixture:")
		w.Header().Set("Trailer", "X-Upstream-Trailer")
		_, _ = io.WriteString(w, `{"id":"r","status":"completed","output":[]}`)
		w.Header().Set("X-Upstream-Trailer", "stale")
	}))
	defer upstream.Close()
	server, err := New(normalizerTestConfig(t, upstream.URL+"/v1"), func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, normalizedRequest())
	response := recorder.Result()
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := response.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
	for _, header := range []string{"Set-Cookie", "ETag", "Content-Digest", "Trailer", "X-Upstream-Trailer"} {
		if got := response.Header.Get(header); got != "" {
			t.Errorf("stale header %s = %q", header, got)
		}
	}
	if len(response.Trailer) != 0 {
		t.Fatalf("stale trailers = %#v", response.Trailer)
	}
}

func TestResponsesNormalizerFailuresDoNotWaitForOriginalBodyClose(t *testing.T) {
	tests := []struct {
		name            string
		contentType     string
		contentEncoding string
		body            string
		blockRead       bool
		wantStatus      int
		wantCode        string
	}{
		{
			name:        "content type",
			contentType: "text/event-stream",
			body:        "data: {}\n\n",
			wantStatus:  http.StatusBadGateway,
			wantCode:    "unexpected_responses_content_type",
		},
		{
			name:            "content encoding",
			contentType:     "application/json",
			contentEncoding: "gzip",
			body:            `{"id":"r","status":"completed","output":[]}`,
			wantStatus:      http.StatusBadGateway,
			wantCode:        "unexpected_responses_content_encoding",
		},
		{
			name:        "malformed terminal JSON",
			contentType: "application/json",
			body:        "{",
			wantStatus:  http.StatusBadGateway,
			wantCode:    "responses_protocol_error",
		},
		{
			name:        "first byte timeout",
			contentType: "application/json",
			blockRead:   true,
			wantStatus:  http.StatusGatewayTimeout,
			wantCode:    "responses_upstream_timeout",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, err := New(normalizerTestConfig(t, "http://127.0.0.1:1/v1"), func() (credentials.Values, error) { return credentials.Values{}, nil }, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			server.responsesReadTimeout = responses.ReadTimeouts{
				FirstByte:  25 * time.Millisecond,
				InterChunk: time.Second,
				Total:      time.Second,
			}
			releaseRead := make(chan struct{})
			releaseClose := make(chan struct{})
			server.proxy.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				header := http.Header{"Content-Type": []string{test.contentType}}
				if test.contentEncoding != "" {
					header.Set("Content-Encoding", test.contentEncoding)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     header,
					Body: &blockingCloseBody{
						Reader:       strings.NewReader(test.body),
						blockRead:    test.blockRead,
						releaseRead:  releaseRead,
						releaseClose: releaseClose,
					},
					Request: request,
				}, nil
			})
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, normalizedRequest())
				done <- response
			}()
			select {
			case response := <-done:
				if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), `"code":"`+test.wantCode+`"`) {
					t.Errorf("status = %d body=%s", response.Code, response.Body.String())
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("handler blocked on the original response Body.Close")
			}
			if got := server.responsesScheduler.Snapshot().ActiveGeneralUpstream; got != 1 {
				t.Fatalf("active upstream permits = %d, want 1 until source cleanup completes", got)
			}
			close(releaseRead)
			close(releaseClose)
			waitForProxySnapshot(t, server, func(snapshot scheduler.Snapshot) bool {
				return snapshot.ActiveGeneralUpstream == 0
			})
		})
	}
}

func TestResponsesWebSocketFallsBackWithoutAffectingRealtime(t *testing.T) {
	cfg, err := config.NewDefault("https://example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.VoiceEnabled = true
	cfg.Responses.WebSocketMode = config.ResponsesWebSocketModeHTTPFallback
	cfg.Responses.ModelModes = map[string]string{"opencode-go-responses/gpt-5.6-luna": config.ResponsesModelModeBoundedJSON}
	server, err := New(cfg, func() (credentials.Values, error) {
		return credentials.Values{CFClientID: "id", CFClientSecret: "secret", GatewayKey: "key"}, nil
	}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	request.Header.Set("Upgrade", "websocket")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUpgradeRequired {
		t.Fatalf("Responses WS status = %d", response.Code)
	}
	if _, err := compatRouteRealtimeForTest(); err != nil {
		t.Fatalf("Realtime WS route was affected: %v", err)
	}
}

func normalizerTestConfig(t *testing.T, upstream string) config.Config {
	t.Helper()
	cfg, err := config.NewDefault(upstream, config.CredentialsSourceNone)
	if err != nil {
		t.Fatal(err)
	}
	cfg.UpstreamMode = config.UpstreamModeLocalOpenCodex
	cfg.Credentials = config.CredentialsConfig{Source: config.CredentialsSourceNone}
	cfg.Catalog.Owner = config.CatalogOwnerRemoteManager
	cfg.Responses.WebSocketMode = config.ResponsesWebSocketModeHTTPFallback
	cfg.Responses.ModelModes = map[string]string{"opencode-go-responses/gpt-5.6-luna": config.ResponsesModelModeBoundedJSON}
	return cfg
}

func normalizedRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"opencode-go-responses/gpt-5.6-luna","stream":true,"input":[]}`))
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type blockingCloseBody struct {
	*strings.Reader
	blockRead    bool
	releaseRead  <-chan struct{}
	releaseClose <-chan struct{}
}

func (b *blockingCloseBody) Read(destination []byte) (int, error) {
	if b.blockRead {
		<-b.releaseRead
		return 0, io.ErrClosedPipe
	}
	return b.Reader.Read(destination)
}

func (b *blockingCloseBody) Close() error {
	<-b.releaseClose
	return nil
}

func newLoopbackTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listeners are unavailable in this environment: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}

func compatRouteRealtimeForTest() (string, error) {
	request := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	request.Header.Set("Upgrade", "websocket")
	websocket := strings.EqualFold(request.Header.Get("Upgrade"), "websocket")
	kind, err := compat.Classify(request.Method, request.URL.Path, websocket, true)
	return string(kind), err
}

func TestTrackerQuiescenceExcludesActiveAndNewRequests(t *testing.T) {
	tracker := NewTracker()
	finish := tracker.Begin()
	if tracker.TryQuiesce(func() { t.Fatal("quiesced with an active request") }) {
		t.Fatal("quiescence succeeded while a request held admission")
	}
	finish()

	requestAttempted := make(chan struct{})
	requestAdmitted := make(chan struct{})
	requestDone := make(chan struct{})
	if !tracker.TryQuiesce(func() {
		go func() {
			close(requestAttempted)
			finishRequest := tracker.Begin()
			close(requestAdmitted)
			<-requestDone
			finishRequest()
		}()
		<-requestAttempted
		select {
		case <-requestAdmitted:
			t.Error("new request entered during quiescence")
		case <-time.After(50 * time.Millisecond):
		}
	}) {
		t.Fatal("idle tracker did not enter quiescence")
	}
	select {
	case <-requestAdmitted:
	case <-time.After(time.Second):
		t.Fatal("request remained blocked after quiescence ended")
	}
	close(requestDone)
}
