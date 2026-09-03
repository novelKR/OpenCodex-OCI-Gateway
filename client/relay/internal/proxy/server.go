// Package proxy implements the local, credential-injecting compatibility relay.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/compat"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/credentials"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/loopbackauth"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/responses"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/scheduler"
)

type CredentialLoader func() (credentials.Values, error)

type Tracker struct {
	gate   sync.RWMutex
	active atomic.Int64
}

func NewTracker() *Tracker {
	return &Tracker{}
}

func (t *Tracker) Begin() func() {
	t.gate.RLock()
	t.active.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			t.active.Add(-1)
			t.gate.RUnlock()
		})
	}
}

func (t *Tracker) Active() int64 { return t.active.Load() }

// TryQuiesce runs fn only when no protocol request holds admission. A pending
// writer excludes new request admission for the duration of fn. It never waits
// for an existing SSE or WebSocket, so activation is retried on the next tick
// instead of blocking the relay behind a long-lived request.
func (t *Tracker) TryQuiesce(fn func()) bool {
	if !t.gate.TryLock() {
		return false
	}
	defer t.gate.Unlock()
	fn()
	return true
}

type Server struct {
	cfg                  config.Config
	upstream             *url.URL
	credentials          CredentialLoader
	tracker              *Tracker
	proxy                *httputil.ReverseProxy
	logger               *slog.Logger
	responsesPolicy      responses.Policy
	responsesLimits      responses.Limits
	responsesReadTimeout responses.ReadTimeouts
	responsesScheduler   *scheduler.Scheduler
	responsesModels      []string
	routing              *routing.Watcher
	observation          *ConnectionObservation
	appleAuthorize       loopbackauth.Authorizer
}

type Option func(*Server)

// WithRouting supplies the resident state watcher. A nil watcher preserves the
// legacy relay-active behavior for callers that have not installed routing
// control yet.
func WithRouting(watcher *routing.Watcher) Option {
	return func(server *Server) { server.routing = watcher }
}

// WithConnectionObservation installs the in-memory, non-secret connection
// projection shared with the resident catalog lifecycle. A nil value retains
// the server-created observer.
func WithConnectionObservation(observation *ConnectionObservation) Option {
	return func(server *Server) {
		if observation != nil {
			server.observation = observation
		}
	}
}

// WithAppleRuntimeConnectionBinding keeps the lifecycle reader through a fresh
// connection's post-dial identity proof and credential header write. The
// RuntimeManager gate separately covers the complete loopback response.
func WithAppleRuntimeConnectionBinding(authorize loopbackauth.Authorizer) Option {
	return func(server *Server) {
		server.appleAuthorize = authorize
	}
}

type credentialContextKey struct{}
type normalizationContextKey struct{}
type listenerLaneContextKey struct{}

type normalizationDecision struct {
	dispatchStarted time.Time
	lane            scheduler.Lane
	codec           string
	encodedBytes    int64
	decodedBytes    int64
	requestSpilled  bool
	queueWait       time.Duration
	job             *scheduler.JobLease
	upstream        *scheduler.Lease
	delivery        *scheduler.Lease
}

func (d *normalizationDecision) releaseUpstream() {
	if d != nil && d.upstream != nil {
		d.upstream.Release()
	}
}

// releaseUpstreamAfter transfers permit ownership to transport cleanup. A
// broken source Read/Close can remain stuck, but it remains bounded by the
// configured upstream permit count instead of allowing unbounded accumulation.
func (d *normalizationDecision) releaseUpstreamAfter(done <-chan struct{}) {
	if d == nil || d.upstream == nil {
		return
	}
	lease := d.upstream
	d.upstream = nil
	select {
	case <-done:
		lease.Release()
	default:
		go func() {
			<-done
			lease.Release()
		}()
	}
}

func (d *normalizationDecision) releaseDelivery() {
	if d != nil && d.delivery != nil {
		d.delivery.Release()
	}
}

func (d *normalizationDecision) close() {
	d.releaseDelivery()
	d.releaseUpstream()
	if d != nil && d.job != nil {
		d.job.Release()
	}
}

type upstreamResponseError struct {
	status int
	code   string
	phase  string
	cause  error
}

func (e *upstreamResponseError) Error() string {
	if e.cause == nil {
		return e.code
	}
	return fmt.Sprintf("%s: %v", e.code, e.cause)
}

func (e *upstreamResponseError) Unwrap() error { return e.cause }

func New(cfg config.Config, loader CredentialLoader, tracker *Tracker, logger *slog.Logger, options ...Option) (*Server, error) {
	upstream, err := url.Parse(cfg.UpstreamBaseURL)
	if err != nil {
		return nil, err
	}
	if tracker == nil {
		tracker = NewTracker()
	}
	if logger == nil {
		logger = slog.Default()
	}
	policy, err := responses.NewPolicy(cfg.Responses.ModelModes)
	if err != nil {
		return nil, err
	}
	limits := responses.DefaultLimits()
	if cfg.UpstreamMode == config.UpstreamModeLocalOpenCodex || cfg.UpstreamMode == config.UpstreamModeLocalAppleContainer {
		limits.MaxEncodedBytes = 256 * responses.MiB
	}
	schedulerConfig := cfg.Responses.Scheduler
	responsesScheduler, err := scheduler.New(scheduler.Limits{
		MaxClassifications:          schedulerConfig.MaxClassifications,
		MaxPendingRequests:          schedulerConfig.MaxPendingRequests,
		MaxPendingBytes:             schedulerConfig.MaxPendingEncodedBytes,
		QueueTimeout:                time.Duration(schedulerConfig.QueueTimeoutMS) * time.Millisecond,
		MaxGeneralUpstream:          schedulerConfig.MaxGeneralUpstream,
		InteractiveReservedUpstream: schedulerConfig.InteractiveReservedUpstream,
		MaxConcurrentTransforms:     schedulerConfig.MaxConcurrentTransforms,
		MaxOpenDeliveries:           schedulerConfig.MaxOpenDeliveries,
	})
	if err != nil {
		return nil, err
	}
	responseModels := make([]string, 0, len(cfg.Responses.ModelModes))
	for model := range cfg.Responses.ModelModes {
		responseModels = append(responseModels, model)
	}
	sort.Slice(responseModels, func(left, right int) bool {
		return strings.ToLower(responseModels[left]) < strings.ToLower(responseModels[right])
	})
	s := &Server{
		cfg:                  cfg,
		upstream:             upstream,
		credentials:          loader,
		tracker:              tracker,
		logger:               logger,
		responsesPolicy:      policy,
		responsesLimits:      limits,
		responsesReadTimeout: responses.DefaultReadTimeouts(),
		responsesScheduler:   responsesScheduler,
		responsesModels:      responseModels,
		observation:          NewConnectionObservation(cfg.UpstreamMode),
	}
	for _, option := range options {
		if option != nil {
			option(s)
		}
	}
	transport := &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if cfg.UpstreamMode == config.UpstreamModeExternalGateway {
		transport.Proxy = http.ProxyFromEnvironment
	}
	var roundTripper http.RoundTripper = transport
	if cfg.UpstreamMode == config.UpstreamModeLocalAppleContainer {
		bound, bindErr := loopbackauth.NewTransport(transport, requireRuntimeConnectionLease, s.appleAuthorize)
		if bindErr != nil {
			return nil, bindErr
		}
		roundTripper = bound
	}
	s.proxy = &httputil.ReverseProxy{
		Rewrite:        s.rewrite,
		ModifyResponse: s.modifyResponse,
		FlushInterval:  -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var responseErr *upstreamResponseError
			if errors.As(err, &responseErr) {
				s.logger.Warn("normalized Responses upstream failure", "method", r.Method, "path", r.URL.Path, "code", responseErr.code, "phase", responseErr.phase)
				writeError(w, responseErr.status, responseErr.code)
				return
			}
			// A canceled local request is not evidence that the external
			// gateway is unreachable. Every other transport failure is safe to
			// reduce to the non-secret `unreachable` observation.
			if r.Context().Err() == nil {
				s.observation.RecordGatewayUnreachable()
			}
			if _, normalized := r.Context().Value(normalizationContextKey{}).(*normalizationDecision); normalized && errors.Is(err, context.DeadlineExceeded) {
				s.logger.Warn("normalized Responses upstream timeout", "method", r.Method, "path", r.URL.Path, "phase", responses.TimeoutTotal)
				writeError(w, http.StatusGatewayTimeout, "responses_upstream_timeout")
				return
			}
			s.logger.Warn("upstream relay failure", "method", r.Method, "path", r.URL.Path, "error", err.Error())
			writeError(w, http.StatusBadGateway, "upstream_unavailable")
		},
		Transport: roundTripper,
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	return s.HandlerForLane(scheduler.LaneGeneral)
}

// HandlerForLane binds a trusted listener lane into request context. Lane
// selection is owned by the local listener and is never accepted from a client
// header or request body.
func (s *Server) HandlerForLane(lane scheduler.Lane) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lane != scheduler.LaneGeneral && lane != scheduler.LaneInteractive {
			writeError(w, http.StatusInternalServerError, "invalid_listener_lane")
			return
		}
		ctx := context.WithValue(r.Context(), listenerLaneContextKey{}, lane)
		s.serveHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) Tracker() *Tracker { return s.tracker }

// ConnectionObservation is shared with the resident catalog lifecycle. It is
// intentionally in-memory; callers must not persist its values alongside
// routing state.
func (s *Server) ConnectionObservation() *ConnectionObservation { return s.observation }

func (s *Server) routingSnapshot() routing.Snapshot {
	if s.routing != nil {
		return s.routing.Snapshot()
	}
	return routing.Snapshot{State: routing.State{
		Schema:      routing.SchemaVersion,
		Generation:  1,
		DesiredMode: routing.ModeRelay,
		AppliedMode: routing.ModeRelay,
		Phase:       routing.PhaseRelayActive,
	}, Legacy: true}
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	lane, _ := r.Context().Value(listenerLaneContextKey{}).(scheduler.Lane)
	if lane == "" {
		lane = scheduler.LaneGeneral
	}
	if r.URL.Path == "/__relay/healthz" {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		snapshot := s.responsesScheduler.Snapshot()
		route := s.routingSnapshot()
		relayAdmission := "deny"
		catalogRefresh := "pause"
		if route.AllowsDataPlane() {
			relayAdmission = "allow"
		}
		if route.AllowsCatalog() {
			catalogRefresh = "run"
		}
		connection := s.observation.Snapshot()
		// Parked/applying routing must not advertise a usable remote connection
		// merely because a previous relay request succeeded before the switch.
		if !route.AllowsCatalog() {
			connection.RemoteGateway = ConnectionNotApplicable
			// A remote-manager-owned catalog has no resident worker to cancel.
			// Its relay-side egress is therefore already paused once routing
			// parks, while a relay-owned worker must report its own confirmed
			// cancellation through the shared observation below.
			if s.cfg.Catalog.Owner != config.CatalogOwnerRelay {
				connection.CatalogLifecycle = CatalogLifecyclePaused
			}
		}
		limits := s.responsesScheduler.Limits()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":                          true,
			"active_requests":             s.tracker.Active(),
			"listener_lane":               lane,
			"general_listener":            s.cfg.ListenAddress,
			"interactive_listener":        s.cfg.Responses.Scheduler.InteractiveListenAddress,
			"active_classifications":      snapshot.ActiveClassifications,
			"pending_requests":            snapshot.PendingRequests,
			"pending_encoded_bytes":       snapshot.PendingBytes,
			"active_general_upstream":     snapshot.ActiveGeneralUpstream,
			"active_interactive_upstream": snapshot.ActiveInteractiveUpstream,
			"active_transforms":           snapshot.ActiveTransforms,
			"active_deliveries":           snapshot.ActiveDeliveries,
			"capacity_rejections":         snapshot.CapacityRejections,
			"scheduler_limits": map[string]any{
				"max_classifications":           limits.MaxClassifications,
				"max_pending_requests":          limits.MaxPendingRequests,
				"max_pending_encoded_bytes":     limits.MaxPendingBytes,
				"queue_timeout_ms":              limits.QueueTimeout.Milliseconds(),
				"max_general_upstream":          limits.MaxGeneralUpstream,
				"interactive_reserved_upstream": limits.InteractiveReservedUpstream,
				"max_concurrent_transforms":     limits.MaxConcurrentTransforms,
				"max_open_deliveries":           limits.MaxOpenDeliveries,
			},
			"catalog_owner":            s.cfg.Catalog.Owner,
			"upstream_base_url":        s.cfg.UpstreamBaseURL,
			"upstream_mode":            s.cfg.UpstreamMode,
			"responses_models":         s.responsesModels,
			"responses_websocket_mode": s.cfg.Responses.WebSocketMode,
			"responses_normalizer":     !s.responsesPolicy.Empty(),
			"voice_enabled":            s.cfg.VoiceEnabled,
			"routing_generation":       route.State.Generation,
			"routing_desired_mode":     route.State.DesiredMode,
			"routing_applied_mode":     route.State.AppliedMode,
			"routing_phase":            route.State.Phase,
			"relay_admission":          relayAdmission,
			"catalog_refresh":          catalogRefresh,
			"routing_state_invalid":    route.Invalid,
			"catalog_lifecycle":        connection.CatalogLifecycle,
			"remote_gateway":           connection.RemoteGateway,
			"local_opencodex":          connection.LocalOpenCodex,
			"connection_probe":         connection.Probe,
		})
		return
	}
	// Enter the tracker before observing admission. Once the controller has a
	// watcher acknowledgement for applying/native and sees Active()==0, any
	// later request will observe deny before credential lookup. Conversely an
	// old admitted request remains counted until it finishes, so it is never
	// handed off across the routing boundary.
	finish := s.tracker.Begin()
	defer finish()
	if !s.routingSnapshot().AllowsDataPlane() {
		// A native/recovery/applying routing state keeps the loopback listener
		// alive for control-plane health, but must never load credentials or
		// forward an old AppServer request to the remote gateway.
		writeTypedError(w, http.StatusServiceUnavailable, "service_unavailable", "relay_native_mode")
		return
	}
	websocket := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
	kind, err := compat.Classify(r.Method, r.URL.Path, websocket, s.cfg.VoiceEnabled)
	if err != nil {
		writeError(w, http.StatusNotFound, "endpoint_not_enabled")
		return
	}
	if kind == compat.RouteResponses && websocket && s.cfg.Responses.WebSocketMode == config.ResponsesWebSocketModeHTTPFallback {
		writeError(w, http.StatusUpgradeRequired, "responses_http_fallback_required")
		return
	}
	values := credentials.Values{}
	profile := s.cfg.Credentials.RemoteAuthenticationProfile()
	if profile != config.RemoteAuthenticationNone && s.cfg.UpstreamMode != config.UpstreamModeLocalAppleContainer {
		values, err = s.credentials()
		if err != nil {
			s.logger.Error("relay credential lookup failed", "method", r.Method, "path", r.URL.Path, "error", err.Error())
			writeError(w, http.StatusServiceUnavailable, "credential_unavailable")
			return
		}
		if err := values.ValidateForProfile(profile); err != nil {
			s.logger.Error("relay credential validation failed", "method", r.Method, "path", r.URL.Path, "error", err.Error())
			writeError(w, http.StatusServiceUnavailable, "credential_unavailable")
			return
		}
	}
	r = r.WithContext(context.WithValue(r.Context(), credentialContextKey{}, values))

	if kind == compat.RouteResponses && r.Method == http.MethodPost && !s.responsesPolicy.Empty() {
		ok, cleanup := s.prepareResponsesRequest(w, &r)
		if !ok {
			return
		}
		defer cleanup()
	}
	s.proxy.ServeHTTP(w, r)
}

func (s *Server) prepareResponsesRequest(w http.ResponseWriter, request **http.Request) (bool, func()) {
	r := *request
	originalContentLength := r.ContentLength
	if r.ContentLength > s.responsesLimits.MaxEncodedBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "responses_request_too_large")
		return false, func() {}
	}
	started := time.Now()
	lane, _ := r.Context().Value(listenerLaneContextKey{}).(scheduler.Lane)
	if lane == "" {
		lane = scheduler.LaneGeneral
	}
	queueContext, cancelQueue := context.WithTimeout(
		r.Context(),
		time.Duration(s.cfg.Responses.Scheduler.QueueTimeoutMS)*time.Millisecond,
	)
	defer cancelQueue()
	controller := http.NewResponseController(w)
	if err := controller.SetReadDeadline(time.Now().Add(time.Duration(s.cfg.Responses.Scheduler.QueueTimeoutMS) * time.Millisecond)); err == nil {
		defer func() { _ = controller.SetReadDeadline(time.Time{}) }()
	} else if !errors.Is(err, http.ErrNotSupported) {
		s.logger.Warn("unable to set Responses request read deadline", "lane", lane, "error", err.Error())
	}
	stopBodyClose := func() bool { return false }
	if r.Body != nil {
		originalBody := r.Body
		stopBodyClose = context.AfterFunc(queueContext, func() { _ = originalBody.Close() })
	}
	defer stopBodyClose()

	pending, err := s.responsesScheduler.AcquirePendingRequest(queueContext, lane)
	if err != nil {
		s.writeSchedulerAdmissionError(w, err)
		return false, func() {}
	}
	pendingOpen := true
	closePending := func() {
		if pendingOpen {
			_ = pending.Close()
			pendingOpen = false
		}
	}
	defer closePending()

	var quotaBody io.ReadCloser
	if originalContentLength >= 0 {
		if err := pending.ReserveBytes(queueContext, originalContentLength); err != nil {
			s.writeSchedulerAdmissionError(w, err)
			return false, func() {}
		}
		quotaBody = &exactLengthReadCloser{source: r.Body, remaining: originalContentLength}
	} else {
		quotaBody = &pendingQuotaReadCloser{source: r.Body, context: queueContext, pending: pending}
	}

	classification, err := s.responsesScheduler.AcquireClassification(queueContext)
	if err != nil {
		s.writeSchedulerAdmissionError(w, err)
		return false, func() {}
	}
	prepared, err := responses.PrepareRequest(queueContext, quotaBody, r.Header.Get("Content-Encoding"), s.responsesPolicy, s.responsesLimits)
	classification.Release()
	if err != nil {
		if errors.Is(err, scheduler.ErrQueueTimeout) || errors.Is(err, context.DeadlineExceeded) {
			s.writeSchedulerAdmissionError(w, err)
		} else {
			s.writeRequestPreparationError(w, err)
		}
		return false, func() {}
	}

	if prepared.Action == responses.ActionRejectHostedComputer {
		_ = prepared.Close()
		writeTypedError(w, http.StatusBadRequest, "invalid_request_error", "hosted_computer_unsupported_by_native_codex")
		return false, func() {}
	}

	r.GetBody = nil
	if prepared.Action != responses.ActionNormalize {
		job, err := pending.HandoffPassthrough()
		if err != nil {
			_ = prepared.Close()
			if queueErr := queueContext.Err(); queueErr != nil {
				err = queueErr
			}
			s.writeSchedulerAdmissionError(w, err)
			return false, func() {}
		}
		pendingOpen = false
		prepared.Body = &releaseReadCloser{source: prepared.Body, release: job.Release}
		r.Body = prepared.Body
		r.ContentLength = originalContentLength
		return true, func() { _ = prepared.Close() }
	}
	if header, present := bodyIntegrityHeader(r.Header); present {
		s.logger.Warn("Responses normalization rejected a body integrity header", "header", header)
		writeTypedError(w, http.StatusBadRequest, "invalid_request_error", "body_integrity_header_unsupported")
		_ = prepared.Close()
		return false, func() {}
	}

	upstream, job, err := pending.AcquireUpstreamJob(queueContext)
	pendingOpen = false
	if err != nil {
		_ = prepared.Close()
		if queueErr := queueContext.Err(); queueErr != nil {
			err = queueErr
		}
		s.writeSchedulerAdmissionError(w, err)
		return false, func() {}
	}

	r.Body = prepared.Body
	r.ContentLength = prepared.ContentLength
	r.TransferEncoding = nil
	r.Header.Set("Content-Length", strconv.FormatInt(prepared.ContentLength, 10))
	r.Header.Del("Transfer-Encoding")
	if prepared.ContentEncoding == "" {
		r.Header.Del("Content-Encoding")
	} else {
		r.Header.Set("Content-Encoding", prepared.ContentEncoding)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	// The bounded response parser accepts terminal JSON bytes, not an
	// additional content-coding layer. Prevent inherited client preferences
	// from turning a valid upstream response into opaque compressed bytes.
	r.Header.Set("Accept-Encoding", "identity")
	codec := prepared.ContentEncoding
	if codec == "" {
		codec = "identity"
	}
	decision := normalizationDecision{
		dispatchStarted: time.Now(),
		lane:            lane,
		codec:           codec,
		encodedBytes:    prepared.EncodedBytes,
		decodedBytes:    prepared.DecodedBytes,
		requestSpilled:  prepared.Spilled,
		queueWait:       time.Since(started),
		job:             job,
		upstream:        upstream,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	ctx = context.WithValue(ctx, normalizationContextKey{}, &decision)
	*request = r.WithContext(ctx)
	// Cleanup is a fallback for every transport/error path. Successful 2xx JSON
	// capture releases upstream earlier, and client EOF releases delivery.
	return true, func() {
		cancel()
		_ = prepared.Close()
		decision.close()
		s.logger.Info("Responses normalization request finished", "upstream_mode", s.cfg.UpstreamMode, "lane", lane, "codec", codec, "encoded_bytes", prepared.EncodedBytes, "decoded_bytes", prepared.DecodedBytes, "request_spilled", prepared.Spilled, "queue_wait_ms", decision.queueWait.Milliseconds(), "elapsed_ms", time.Since(started).Milliseconds())
	}
}

func (s *Server) writeSchedulerAdmissionError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	if errors.Is(err, scheduler.ErrQueueTimeout) || errors.Is(err, scheduler.ErrPendingBytesTooLarge) || errors.Is(err, context.DeadlineExceeded) {
		w.Header().Set("Retry-After", "5")
		writeTypedError(w, http.StatusTooManyRequests, "rate_limit_error", "responses_normalizer_capacity_exhausted")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "responses_scheduler_unavailable")
}

func (s *Server) writeRequestPreparationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, responses.ErrEncodedBodyTooLarge), errors.Is(err, responses.ErrDecodedBodyTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "responses_request_too_large")
	case errors.Is(err, responses.ErrUnsupportedContentEncoding):
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_content_encoding")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusRequestTimeout, "request_cancelled")
	default:
		writeTypedError(w, http.StatusBadRequest, "invalid_request_error", "invalid_responses_request")
	}
}

func (s *Server) rewrite(pr *httputil.ProxyRequest) {
	values, _ := pr.In.Context().Value(credentialContextKey{}).(credentials.Values)
	request := pr.Out
	request.URL.Scheme = s.upstream.Scheme
	request.URL.Host = s.upstream.Host
	request.URL.Path = joinPath(s.upstream.Path, pr.In.URL.Path)
	request.URL.RawPath = ""
	request.Host = s.upstream.Host
	// The three outer credentials belong only to this relay. Never trust a
	// same-user local process to choose them.
	request.Header.Del("CF-Access-Client-Id")
	request.Header.Del("CF-Access-Client-Secret")
	request.Header.Del("Cf-Access-Jwt-Assertion")
	request.Header.Del("X-OpenCodex-API-Key")
	request.Header.Del("X-OpenCodex-Relay")
	for key := range request.Trailer {
		if strings.EqualFold(key, loopbackauth.CredentialHeader) {
			delete(request.Trailer, key)
		}
	}
	profile := s.cfg.Credentials.RemoteAuthenticationProfile()
	if profile == config.RemoteAuthenticationCloudflareAccessAndGatewayKey {
		request.Header.Set("CF-Access-Client-Id", values.CFClientID)
		request.Header.Set("CF-Access-Client-Secret", values.CFClientSecret)
	}
	if profile == config.RemoteAuthenticationGatewayAPIKey || profile == config.RemoteAuthenticationCloudflareAccessAndGatewayKey {
		request.Header.Set("X-OpenCodex-API-Key", values.GatewayKey)
	}
	if profile == config.LocalAuthenticationOpenCodexAPIKey && s.cfg.UpstreamMode != config.UpstreamModeLocalAppleContainer {
		request.Header.Set("X-OpenCodex-API-Key", values.LocalOpenCodexAPIKey)
	}
	if profile != config.RemoteAuthenticationNone {
		request.Header.Set("X-OpenCodex-Relay", "pw-local-v1")
	}
}

func (s *Server) modifyResponse(response *http.Response) error {
	// Reaching any HTTP response proves only transport reachability. The
	// response may still be rejected by the protocol normalizer below, but that
	// local validation error must not turn a live gateway into `unreachable`.
	s.observation.RecordGatewayReachable()
	decision, normalized := response.Request.Context().Value(normalizationContextKey{}).(*normalizationDecision)
	if !normalized {
		return nil
	}
	timedBody, err := responses.NewTimedReadCloser(response.Request.Context(), response.Body, s.responsesReadTimeout)
	if err != nil {
		decision.releaseUpstream()
		return &upstreamResponseError{status: http.StatusBadGateway, code: "responses_protocol_error", phase: "response_body", cause: err}
	}
	defer func() {
		_ = timedBody.Close()
		decision.releaseUpstreamAfter(timedBody.CleanupDone())
	}()
	// ReverseProxy closes response.Body synchronously whenever ModifyResponse
	// returns an error. Replace it before every normalized-response validation
	// branch so a hostile original Close cannot delay fail-closed 502/504 paths.
	response.Body = timedBody
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return s.captureNon2xxResponse(response, decision, timedBody)
	}

	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return &upstreamResponseError{
			status: http.StatusBadGateway,
			code:   "unexpected_responses_content_type",
			phase:  "response_headers",
			cause:  err,
		}
	}
	contentEncoding := strings.TrimSpace(response.Header.Get("Content-Encoding"))
	if contentEncoding != "" && !strings.EqualFold(contentEncoding, "identity") {
		return &upstreamResponseError{
			status: http.StatusBadGateway,
			code:   "unexpected_responses_content_encoding",
			phase:  "response_headers",
		}
	}
	counter := &countingReadCloser{source: timedBody}
	defer counter.Close()
	headerLatency := time.Since(decision.dispatchStarted)
	captured, err := responses.CaptureResponseJSON(response.Request.Context(), counter, s.responsesLimits)
	_ = counter.Close()
	if err != nil {
		status, code, phase := normalizedResponseError(err)
		return &upstreamResponseError{status: status, code: code, phase: phase, cause: err}
	}
	decision.releaseUpstreamAfter(timedBody.CleanupDone())
	defer captured.Close()

	transform, err := s.responsesScheduler.AcquireTransform(response.Request.Context())
	if err != nil {
		return &upstreamResponseError{status: http.StatusServiceUnavailable, code: "responses_transform_capacity_exhausted", phase: "transform", cause: err}
	}
	transformWait := transform.WaitDuration()
	defer transform.Release()

	spool, err := anonymousSpool(".opencodex-relay-responses-sse-")
	if err != nil {
		return &upstreamResponseError{status: http.StatusBadGateway, code: "responses_spool_unavailable", phase: "response_body", cause: err}
	}
	spoolOwned := true
	defer func() {
		if spoolOwned {
			_ = spool.Close()
		}
	}()

	result, err := captured.SynthesizeTerminalSSE(response.Request.Context(), spool, s.responsesLimits)
	if err != nil {
		status, code, phase := normalizedResponseError(err)
		return &upstreamResponseError{status: status, code: code, phase: phase, cause: err}
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return &upstreamResponseError{status: http.StatusBadGateway, code: "responses_spool_unavailable", phase: "response_body", cause: err}
	}
	info, err := spool.Stat()
	if err != nil {
		return &upstreamResponseError{status: http.StatusBadGateway, code: "responses_spool_unavailable", phase: "response_body", cause: err}
	}
	transform.Release()

	delivery, err := s.responsesScheduler.AcquireDelivery(response.Request.Context())
	if err != nil {
		return &upstreamResponseError{status: http.StatusServiceUnavailable, code: "responses_delivery_capacity_exhausted", phase: "delivery", cause: err}
	}
	decision.delivery = delivery

	response.Body = &releaseReadCloser{source: spool, release: decision.releaseDelivery}
	spoolOwned = false
	response.ContentLength = info.Size()
	response.Header.Set("Content-Type", "text/event-stream; charset=utf-8")
	response.Header.Set("Cache-Control", "no-store")
	response.Header.Set("X-Content-Type-Options", "nosniff")
	response.Header.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	for _, header := range []string{
		"Content-Encoding", "Transfer-Encoding", "Connection", "Keep-Alive",
		"Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Upgrade",
		"Set-Cookie", "Set-Cookie2", "ETag", "Last-Modified", "Expires", "Vary",
		"Content-MD5", "Content-Digest", "Repr-Digest", "Digest", "Content-Range",
		"Accept-Ranges", "Content-Location",
	} {
		response.Header.Del(header)
	}
	response.TransferEncoding = nil
	response.Trailer = nil
	response.Uncompressed = false

	firstBodyLatency := int64(-1)
	if !counter.firstRead.IsZero() {
		firstBodyLatency = counter.firstRead.Sub(decision.dispatchStarted).Milliseconds()
	}
	s.logger.Info(
		"Responses normalization completed",
		"upstream_mode", s.cfg.UpstreamMode,
		"lane", decision.lane,
		"codec", decision.codec,
		"queue_wait_ms", decision.queueWait.Milliseconds(),
		"transform_wait_ms", transformWait.Milliseconds(),
		"delivery_wait_ms", delivery.WaitDuration().Milliseconds(),
		"request_encoded_bytes", decision.encodedBytes,
		"request_decoded_bytes", decision.decodedBytes,
		"request_spilled", decision.requestSpilled,
		"response_bytes", result.Bytes,
		"response_chunks", counter.chunks,
		"response_spilled", result.Spilled,
		"first_header_ms", headerLatency.Milliseconds(),
		"first_body_ms", firstBodyLatency,
		"terminal", result.Status,
		"terminal_frames", 1,
		"done_frames", 1,
		"output_items", result.OutputItems,
		"elapsed_ms", time.Since(decision.dispatchStarted).Milliseconds(),
	)
	return nil
}

func (s *Server) captureNon2xxResponse(response *http.Response, decision *normalizationDecision, timedBody responses.TimedReadCloser) error {
	counter := &countingReadCloser{source: timedBody}
	defer counter.Close()
	spool, err := anonymousSpool(".opencodex-relay-responses-error-")
	if err != nil {
		return &upstreamResponseError{status: http.StatusBadGateway, code: "responses_spool_unavailable", phase: "response_body", cause: err}
	}
	spoolOwned := true
	defer func() {
		if spoolOwned {
			_ = spool.Close()
		}
	}()
	if err := copyBoundedResponse(response.Request.Context(), spool, counter, s.responsesLimits.MaxResponseBytes); err != nil {
		status, code, phase := normalizedResponseError(err)
		return &upstreamResponseError{status: status, code: code, phase: phase, cause: err}
	}
	_ = counter.Close()
	decision.releaseUpstreamAfter(timedBody.CleanupDone())
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return &upstreamResponseError{status: http.StatusBadGateway, code: "responses_spool_unavailable", phase: "response_body", cause: err}
	}
	info, err := spool.Stat()
	if err != nil {
		return &upstreamResponseError{status: http.StatusBadGateway, code: "responses_spool_unavailable", phase: "response_body", cause: err}
	}
	delivery, err := s.responsesScheduler.AcquireDelivery(response.Request.Context())
	if err != nil {
		return &upstreamResponseError{status: http.StatusServiceUnavailable, code: "responses_delivery_capacity_exhausted", phase: "delivery", cause: err}
	}
	decision.delivery = delivery
	response.Body = &releaseReadCloser{source: spool, release: decision.releaseDelivery}
	spoolOwned = false
	response.ContentLength = info.Size()
	response.Header.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	for _, header := range []string{"Transfer-Encoding", "Trailer"} {
		response.Header.Del(header)
	}
	response.TransferEncoding = nil
	response.Trailer = nil
	s.logger.Info(
		"Responses non-success response captured",
		"upstream_mode", s.cfg.UpstreamMode,
		"lane", decision.lane,
		"status", response.StatusCode,
		"response_bytes", info.Size(),
		"response_chunks", counter.chunks,
		"delivery_wait_ms", delivery.WaitDuration().Milliseconds(),
	)
	return nil
}

func copyBoundedResponse(ctx context.Context, destination io.Writer, source io.Reader, limit int64) error {
	buffer := make([]byte, 32*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			if int64(count) > limit-total {
				return responses.ErrResponseBodyTooLarge
			}
			written, writeErr := destination.Write(buffer[:count])
			total += int64(written)
			if writeErr != nil {
				return writeErr
			}
			if written != count {
				return io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
		if count == 0 {
			return io.ErrNoProgress
		}
	}
}

func normalizedResponseError(err error) (int, string, string) {
	status := http.StatusBadGateway
	code := "responses_protocol_error"
	phase := "response_body"
	var timeoutErr *responses.ReadTimeoutError
	if errors.As(err, &timeoutErr) || errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
		code = "responses_upstream_timeout"
		if timeoutErr != nil {
			phase = string(timeoutErr.Phase)
		} else {
			phase = string(responses.TimeoutTotal)
		}
	} else if errors.Is(err, responses.ErrHostedComputerOutput) {
		code = "hosted_computer_unsupported_by_native_codex"
	} else if errors.Is(err, responses.ErrResponseBodyTooLarge) {
		code = "responses_response_too_large"
	}
	return status, code, phase
}

type countingReadCloser struct {
	source    io.ReadCloser
	bytes     int64
	chunks    int64
	firstRead time.Time
}

type releaseReadCloser struct {
	source  io.ReadCloser
	release func()
	once    sync.Once
}

func (r *releaseReadCloser) Read(destination []byte) (int, error) {
	count, err := r.source.Read(destination)
	if errors.Is(err, io.EOF) {
		r.releaseOnce()
	}
	return count, err
}

func (r *releaseReadCloser) Close() error {
	err := r.source.Close()
	r.releaseOnce()
	return err
}

func (r *releaseReadCloser) releaseOnce() {
	r.once.Do(func() {
		if r.release != nil {
			r.release()
		}
	})
}

// pendingQuotaReadCloser reserves encoded-body quota before exposing each
// chunk to the request spool. At most one caller buffer is temporarily outside
// the quota while a reservation waits, so queued storage remains bounded.
type pendingQuotaReadCloser struct {
	source  io.ReadCloser
	context context.Context
	pending *scheduler.PendingRequest
	failed  error
}

func (r *pendingQuotaReadCloser) Read(destination []byte) (int, error) {
	if r.failed != nil {
		return 0, r.failed
	}
	count, readErr := r.source.Read(destination)
	if err := r.context.Err(); err != nil {
		r.failed = err
		return 0, err
	}
	if count > 0 {
		if err := r.pending.ReserveBytes(r.context, int64(count)); err != nil {
			r.failed = err
			return 0, err
		}
	}
	return count, readErr
}

func (r *pendingQuotaReadCloser) Close() error { return r.source.Close() }

var errContentLengthMismatch = errors.New("Responses request body does not match Content-Length")

// exactLengthReadCloser permits at most the atomically reserved encoded
// Content-Length plus one probe byte. The probe detects synthetic or malformed
// requests whose body is larger than the declared length without allowing an
// unbounded body to escape the reservation.
type exactLengthReadCloser struct {
	source    io.ReadCloser
	remaining int64
	failed    error
}

func (r *exactLengthReadCloser) Read(destination []byte) (int, error) {
	if r.failed != nil {
		return 0, r.failed
	}
	limit := int64(len(destination))
	if limit > r.remaining+1 {
		limit = r.remaining + 1
	}
	count, readErr := r.source.Read(destination[:int(limit)])
	if int64(count) > r.remaining {
		r.failed = errContentLengthMismatch
		return 0, r.failed
	}
	r.remaining -= int64(count)
	if errors.Is(readErr, io.EOF) && r.remaining != 0 {
		r.failed = errContentLengthMismatch
		return count, r.failed
	}
	return count, readErr
}

func (r *exactLengthReadCloser) Close() error { return r.source.Close() }

func (r *countingReadCloser) Read(destination []byte) (int, error) {
	count, err := r.source.Read(destination)
	if count > 0 {
		if r.firstRead.IsZero() {
			r.firstRead = time.Now()
		}
		r.bytes += int64(count)
		r.chunks++
	}
	return count, err
}

func (r *countingReadCloser) Close() error { return r.source.Close() }

func anonymousSpool(pattern string) (*os.File, error) {
	file, err := os.CreateTemp("", pattern)
	if err != nil {
		return nil, err
	}
	name := file.Name()
	fail := func(cause error) (*os.File, error) {
		_ = file.Close()
		_ = os.Remove(name)
		return nil, cause
	}
	if err := file.Chmod(0o600); err != nil {
		return fail(err)
	}
	if err := os.Remove(name); err != nil {
		return fail(err)
	}
	return file, nil
}

func bodyIntegrityHeader(header http.Header) (string, bool) {
	for _, name := range []string{
		"Content-MD5", "Content-Digest", "Repr-Digest", "Digest",
		"Signature", "Signature-Input", "Content-Signature", "X-Amz-Content-Sha256",
	} {
		if header.Get(name) != "" {
			return name, true
		}
	}
	return "", false
}

func joinPath(base, requestPath string) string {
	base = strings.TrimRight(base, "/")
	requestPath = "/" + strings.TrimLeft(requestPath, "/")
	// The configured upstream is an OpenAI-compatible base URL ending in
	// /v1, and native Codex sends absolute /v1/... request paths. Preserve
	// any deployment prefix (for example /gateway/v1) without producing
	// /v1/v1/... at the central gateway.
	if requestPath == "/v1" || strings.HasPrefix(requestPath, "/v1/") {
		return base + strings.TrimPrefix(requestPath, "/v1")
	}
	return base + requestPath
}

func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code}})
}

func writeTypedError(w http.ResponseWriter, status int, errorType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"type":    errorType,
			"code":    code,
			"message": code,
		},
	})
}
