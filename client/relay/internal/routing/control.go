package routing

import (
	"bufio"
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
	controlSchemaVersion = 1
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
	// path for inspectability, but use an owner-verified short /tmp name when
	// a long custom CODEX_HOME/config directory would make ListenUnix fail.
	if len(path) <= 96 {
		return path
	}
	hash := sha256.Sum256([]byte(filepath.Clean(configPath)))
	return filepath.Join("/tmp", fmt.Sprintf("pw-ocx-routing-%x.sock", hash[:12]))
}

type ControlRequest struct {
	Schema     int     `json:"schema"`
	Generation uint64  `json:"generation"`
	Backend    Backend `json:"backend"`
}

func (r ControlRequest) validate() error {
	if r.Schema != controlSchemaVersion || r.Generation == 0 || !validBackend(r.Backend) {
		return ErrControlRequest
	}
	return nil
}

type ControlResponse struct {
	OK         bool    `json:"ok"`
	Generation uint64  `json:"generation,omitempty"`
	Backend    Backend `json:"backend,omitempty"`
	Code       string  `json:"code,omitempty"`
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
	request := ControlRequest{Schema: controlSchemaVersion, Generation: generation, Backend: backend}
	if err := request.validate(); err != nil {
		return err
	}
	if err := validateControlSocket(c.path); err != nil {
		return ErrControlUnavailable
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "unix", c.path)
	if err != nil {
		return ErrControlUnavailable
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	} else {
		_ = connection.SetDeadline(time.Now().Add(35 * time.Second))
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return ErrControlUnavailable
	}
	decoder := json.NewDecoder(io.LimitReader(connection, maxControlBytes))
	decoder.DisallowUnknownFields()
	var response ControlResponse
	if err := decoder.Decode(&response); err != nil {
		return ErrControlUnavailable
	}
	if !response.OK || response.Generation != generation || response.Backend != backend {
		return ErrControlUnavailable
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
	decoder := json.NewDecoder(io.LimitReader(bufio.NewReader(connection), maxControlBytes))
	decoder.DisallowUnknownFields()
	var request ControlRequest
	if err := decoder.Decode(&request); err != nil || request.validate() != nil {
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
		response = ControlResponse{Code: "apply_failed"}
	}
	if response.OK && (response.Generation != request.Generation || response.Backend != request.Backend) {
		response = ControlResponse{Code: "invalid_response"}
	}
	_ = json.NewEncoder(connection).Encode(response)
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
