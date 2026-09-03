package routing

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	controlSchemaVersion = 2
	maxControlBytes      = 16 << 10
)

var (
	ErrControlUnavailable  = errors.New("resident relay runtime control is unavailable")
	ErrControlUnauthorized = errors.New("resident relay runtime control peer is unauthorized")
	ErrControlRequest      = errors.New("resident relay runtime control request is invalid")
)

// ControlPath is a per-relay, owner-only Unix socket. It contains no URL,
// credential, configuration payload, or shell command.
func ControlPath(configPath string) string {
	path := filepath.Clean(configPath) + ".routing-control.sock"
	// sockaddr_un is deliberately small on macOS. Keep the normal adjacent
	// path for inspectability, but use the user's process temp directory for a
	// bounded hashed name when a custom CODEX_HOME would exceed that limit.
	if len(path) <= 96 {
		return path
	}
	hash := sha256.Sum256([]byte(filepath.Clean(configPath)))
	return filepath.Join(os.TempDir(), fmt.Sprintf("pw-ocx-routing-%x.sock", hash[:12]))
}

type ControlOperation string

const (
	ControlOperationApply               ControlOperation = "apply"
	ControlOperationMaintenanceStatus   ControlOperation = "runtime_maintenance_status"
	ControlOperationMaintenancePrepare  ControlOperation = "runtime_maintenance_prepare"
	ControlOperationMaintenanceCommit   ControlOperation = "runtime_maintenance_commit"
	ControlOperationMaintenanceRollback ControlOperation = "runtime_maintenance_rollback"
)

type ControlRequest struct {
	Schema     int                 `json:"schema"`
	Operation  ControlOperation    `json:"operation"`
	Generation uint64              `json:"generation,omitempty"`
	Backend    Backend             `json:"backend,omitempty"`
	Intent     *MaintenanceIntent  `json:"intent,omitempty"`
	Witness    *MaintenanceWitness `json:"witness,omitempty"`
}

func (r ControlRequest) validate() error {
	if r.Schema != controlSchemaVersion {
		return ErrControlRequest
	}
	switch r.Operation {
	case ControlOperationApply:
		if r.Generation == 0 || !validBackend(r.Backend) || r.Intent != nil || r.Witness != nil {
			return ErrControlRequest
		}
	case ControlOperationMaintenanceStatus:
		if r.Generation != 0 || r.Backend != "" || r.Intent != nil || r.Witness != nil {
			return ErrControlRequest
		}
	case ControlOperationMaintenancePrepare:
		if r.Generation == 0 || r.Backend != BackendLocalAppleContainer || r.Intent == nil || r.Intent.validate() != nil || r.Witness != nil {
			return ErrControlRequest
		}
	case ControlOperationMaintenanceCommit, ControlOperationMaintenanceRollback:
		if r.Generation != 0 || r.Backend != "" || r.Intent != nil || r.Witness == nil || r.Witness.validate() != nil {
			return ErrControlRequest
		}
	default:
		return ErrControlRequest
	}
	return nil
}

type ControlResponse struct {
	OK          bool                      `json:"ok"`
	Generation  uint64                    `json:"generation,omitempty"`
	Backend     Backend                   `json:"backend,omitempty"`
	Maintenance *MaintenanceRoutingStatus `json:"maintenance,omitempty"`
	Witness     *MaintenanceWitness       `json:"witness,omitempty"`
	Code        string                    `json:"code,omitempty"`
}

// RuntimeControl is deliberately narrower than a general RPC interface. It
// can only apply the durable desired backend for one known state generation.
// No caller can pass an upstream URL, catalog path, or header through it.
type RuntimeControl interface {
	Apply(context.Context, uint64, Backend) error
}

type SocketRuntimeControl struct {
	path string
}

func NewSocketRuntimeControl(configPath string) *SocketRuntimeControl {
	return &SocketRuntimeControl{path: ControlPath(configPath)}
}

func (c *SocketRuntimeControl) Apply(ctx context.Context, generation uint64, backend Backend) error {
	if c == nil || c.path == "" {
		return ErrControlUnavailable
	}
	response, err := roundTripControl(ctx, c.path, ControlRequest{
		Schema: controlSchemaVersion, Operation: ControlOperationApply, Generation: generation, Backend: backend,
	})
	if err != nil {
		return err
	}
	if !response.OK || response.Generation != generation || response.Backend != backend {
		return controlResponseError(response)
	}
	return nil
}

// SocketRuntimeMaintenance is the lifecycle manager's narrow same-user
// client. It cannot supply a URL, credential, path, or command to the relay.
type SocketRuntimeMaintenance struct{ path string }

func NewSocketRuntimeMaintenance(configPath string) *SocketRuntimeMaintenance {
	return &SocketRuntimeMaintenance{path: ControlPath(configPath)}
}

func (c *SocketRuntimeMaintenance) Status(ctx context.Context) (MaintenanceRoutingStatus, error) {
	if c == nil || c.path == "" {
		return MaintenanceRoutingStatus{}, ErrControlUnavailable
	}
	response, err := roundTripControl(ctx, c.path, ControlRequest{
		Schema: controlSchemaVersion, Operation: ControlOperationMaintenanceStatus,
	})
	if err != nil {
		return MaintenanceRoutingStatus{}, err
	}
	if !response.OK || response.Maintenance == nil || response.Maintenance.Validate() != nil ||
		response.Generation != response.Maintenance.RoutingGeneration || response.Backend != response.Maintenance.Backend {
		return MaintenanceRoutingStatus{}, controlResponseError(response)
	}
	return *response.Maintenance, nil
}

func (c *SocketRuntimeMaintenance) Prepare(ctx context.Context, expectedRoutingGeneration uint64, intent MaintenanceIntent) (MaintenanceWitness, error) {
	if c == nil || c.path == "" || expectedRoutingGeneration == 0 || intent.validate() != nil {
		return MaintenanceWitness{}, ErrMaintenanceWitness
	}
	response, err := roundTripControl(ctx, c.path, ControlRequest{
		Schema:     controlSchemaVersion,
		Operation:  ControlOperationMaintenancePrepare,
		Generation: expectedRoutingGeneration,
		Backend:    BackendLocalAppleContainer,
		Intent:     &intent,
	})
	if err != nil {
		return MaintenanceWitness{}, err
	}
	if !response.OK || response.Witness == nil || response.Witness.validate() != nil ||
		response.Witness.OriginRoutingGeneration != expectedRoutingGeneration || response.Witness.Intent != intent ||
		response.Generation != response.Witness.PreparedRoutingGeneration || response.Backend != BackendLocalAppleContainer {
		return MaintenanceWitness{}, controlResponseError(response)
	}
	return *response.Witness, nil
}

func (c *SocketRuntimeMaintenance) Commit(ctx context.Context, witness MaintenanceWitness) error {
	return c.finish(ctx, ControlOperationMaintenanceCommit, witness)
}

func (c *SocketRuntimeMaintenance) Rollback(ctx context.Context, witness MaintenanceWitness) error {
	return c.finish(ctx, ControlOperationMaintenanceRollback, witness)
}

func (c *SocketRuntimeMaintenance) finish(ctx context.Context, operation ControlOperation, witness MaintenanceWitness) error {
	if c == nil || c.path == "" || witness.validate() != nil ||
		(operation != ControlOperationMaintenanceCommit && operation != ControlOperationMaintenanceRollback) {
		return ErrMaintenanceWitness
	}
	response, err := roundTripControl(ctx, c.path, ControlRequest{
		Schema: controlSchemaVersion, Operation: operation, Witness: &witness,
	})
	if err != nil {
		return err
	}
	if !response.OK || response.Generation != witness.FinalRoutingGeneration || response.Backend != BackendLocalAppleContainer {
		return controlResponseError(response)
	}
	return nil
}

func roundTripControl(ctx context.Context, path string, request ControlRequest) (ControlResponse, error) {
	if err := request.validate(); err != nil {
		return ControlResponse{}, err
	}
	if err := validateControlSocket(path); err != nil {
		return ControlResponse{}, ErrControlUnavailable
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return ControlResponse{}, ErrControlUnavailable
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	} else {
		_ = connection.SetDeadline(time.Now().Add(35 * time.Second))
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return ControlResponse{}, ErrControlUnavailable
	}
	var response ControlResponse
	if err := readControlJSON(bufio.NewReaderSize(connection, maxControlBytes+1), &response); err != nil {
		return ControlResponse{}, ErrControlUnavailable
	}
	if !response.OK {
		return response, controlResponseError(response)
	}
	if !validControlResponse(request, response) {
		return ControlResponse{}, ErrControlUnavailable
	}
	return response, nil
}

func validControlResponse(request ControlRequest, response ControlResponse) bool {
	if !response.OK || response.Code != "" {
		return false
	}
	switch request.Operation {
	case ControlOperationApply:
		return response.Generation == request.Generation && response.Backend == request.Backend &&
			response.Maintenance == nil && response.Witness == nil
	case ControlOperationMaintenanceStatus:
		return response.Maintenance != nil && response.Witness == nil &&
			response.Maintenance.Validate() == nil &&
			response.Generation == response.Maintenance.RoutingGeneration &&
			response.Backend == response.Maintenance.Backend
	case ControlOperationMaintenancePrepare:
		return response.Maintenance == nil && response.Witness != nil &&
			response.Witness.validate() == nil && response.Witness.Intent == *request.Intent &&
			response.Generation == response.Witness.PreparedRoutingGeneration &&
			response.Backend == BackendLocalAppleContainer
	case ControlOperationMaintenanceCommit, ControlOperationMaintenanceRollback:
		return response.Maintenance == nil && response.Witness == nil &&
			response.Generation == request.Witness.FinalRoutingGeneration &&
			response.Backend == BackendLocalAppleContainer
	default:
		return false
	}
}

func controlResponseError(response ControlResponse) error {
	switch response.Code {
	case "conflict":
		return ErrMaintenanceConflict
	case "recovery_required":
		return ErrMaintenanceRecoveryRequired
	case "invalid_request", "invalid_witness":
		return ErrMaintenanceWitness
	default:
		return ErrControlUnavailable
	}
}

// readControlJSON consumes one newline-delimited, size-bounded JSON object.
// Both client and server use it so unknown fields, duplicate keys, trailing
// values, and oversized receipts fail closed at the same boundary.
func readControlJSON(reader *bufio.Reader, destination any) error {
	if reader == nil || destination == nil {
		return ErrControlRequest
	}
	line, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(line) > maxControlBytes {
		return ErrControlRequest
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return ErrControlRequest
	}
	payload := bytes.TrimSpace(line)
	if len(payload) == 0 || len(payload) > maxControlBytes || rejectDuplicateJSONKeys(payload) != nil {
		return ErrControlRequest
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return ErrControlRequest
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ErrControlRequest
	}
	return nil
}

func rejectDuplicateJSONKeys(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return ErrControlRequest
				}
				if _, duplicate := seen[key]; duplicate {
					return ErrControlRequest
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return ErrControlRequest
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return ErrControlRequest
			}
		default:
			return ErrControlRequest
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrControlRequest
	}
	return nil
}

// ControlHandler is implemented by the resident relay. It owns the runtime
// construction and never exposes it to relayctl.
type ControlHandler func(context.Context, ControlRequest) (ControlResponse, error)

// ControlServer accepts only local same-user peers, using a 0600 filesystem
// socket plus platform peer-credential verification. It is intentionally not
// an HTTP endpoint and has no remote listener.
type ControlServer struct {
	listener *net.UnixListener
	path     string
	handler  ControlHandler
	once     sync.Once
}

func StartControlServer(ctx context.Context, configPath string, handler ControlHandler) (*ControlServer, error) {
	if handler == nil {
		return nil, ErrControlRequest
	}
	path := ControlPath(configPath)
	if err := removeStaleControlSocket(path); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create routing control directory: %w", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen routing control: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("protect routing control socket: %w", err)
	}
	listener.SetUnlinkOnClose(true)
	server := &ControlServer{listener: listener, path: path, handler: handler}
	go server.run(ctx)
	return server, nil
}

func (s *ControlServer) run(ctx context.Context) {
	if s == nil || s.listener == nil {
		return
	}
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	for {
		connection, err := s.listener.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go s.serveConnection(ctx, connection)
	}
}

func (s *ControlServer) serveConnection(parent context.Context, connection *net.UnixConn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(35 * time.Second))
	if err := verifyControlPeer(connection); err != nil {
		_ = json.NewEncoder(connection).Encode(ControlResponse{Code: "unauthorized"})
		return
	}
	var request ControlRequest
	if err := readControlJSON(bufio.NewReaderSize(connection, maxControlBytes+1), &request); err != nil || request.validate() != nil {
		_ = json.NewEncoder(connection).Encode(ControlResponse{Code: "invalid_request"})
		return
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	response, err := s.handler(ctx, request)
	if err != nil {
		// Raw errors can include local config/transport details. The socket
		// response carries only a finite error code; relayctl maps it to a safe
		// status rather than surfacing the text in the MenuBar.
		response = responseForControlError(err)
	}
	if response.OK && !validControlResponse(request, response) {
		response = ControlResponse{Code: "invalid_response"}
	}
	_ = json.NewEncoder(connection).Encode(response)
}

func responseForControlError(err error) ControlResponse {
	switch {
	case errors.Is(err, ErrMaintenanceConflict):
		return ControlResponse{Code: "conflict"}
	case errors.Is(err, ErrMaintenanceRecoveryRequired):
		return ControlResponse{Code: "recovery_required"}
	case errors.Is(err, ErrMaintenanceWitness), errors.Is(err, ErrControlRequest):
		return ControlResponse{Code: "invalid_witness"}
	default:
		return ControlResponse{Code: "apply_failed"}
	}
}

func (s *ControlServer) Close() error {
	if s == nil {
		return nil
	}
	var result error
	s.once.Do(func() {
		if s.listener != nil {
			result = s.listener.Close()
		}
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) && result == nil {
			result = err
		}
	})
	return result
}

func validateControlSocket(path string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 || !ownedByCurrentUser(info) {
		return ErrControlUnavailable
	}
	return nil
}

func removeStaleControlSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSocket == 0 || !ownedByCurrentUser(info) {
		return ErrControlUnauthorized
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale routing control socket: %w", err)
	}
	return nil
}
