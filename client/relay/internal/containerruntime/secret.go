package containerruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maximumBootstrapFrameBytes = 4096

type UnixSecretServer struct {
	directory string
	timeout   time.Duration
}

// DefaultBootstrapSocketDirectory is fixed and deliberately short enough for
// Darwin's sockaddr_un limit. The durable Application Support path is too long
// once a random socket name is appended, so sockets live in this owner-only,
// per-UID runtime directory and are removed after the one-shot ACK.
func DefaultBootstrapSocketDirectory() string {
	return filepath.Join("/private/tmp", "opencodex-relay-runtime-"+strconv.Itoa(os.Geteuid()))
}

func NewUnixSecretServer(timeout time.Duration) (*UnixSecretServer, error) {
	return newUnixSecretServer(DefaultBootstrapSocketDirectory(), timeout)
}

func newUnixSecretServer(directory string, timeout time.Duration) (*UnixSecretServer, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || timeout <= 0 || timeout > time.Minute {
		return nil, ErrInvalidRequest
	}
	maximumPath := filepath.Join(directory, "b-"+strings.Repeat("0", 32))
	if len(maximumPath) > 103 {
		return nil, ErrInvalidRequest
	}
	return &UnixSecretServer{directory: directory, timeout: timeout}, nil
}

type unixSecretLease struct {
	path     string
	listener *net.UnixListener
	done     chan error
	once     sync.Once
}

func (s *UnixSecretServer) Open(ctx context.Context, secrets Secrets) (SecretLease, error) {
	if s == nil || !validSecret(secrets.APIToken) || !validSecret(secrets.AdminToken) ||
		bytes.Equal(secrets.APIToken, secrets.AdminToken) {
		return nil, ErrCredential
	}
	store, err := newStateStore(s.directory)
	if err != nil {
		return nil, err
	}
	if err := store.prepareRoot(); err != nil {
		return nil, err
	}
	random, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(s.directory, "b-"+random)
	address := &net.UnixAddr{Name: path, Net: "unix"}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		return nil, ErrUnsafeState
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, ErrUnsafeState
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 || !ownedByCurrentUser(info) {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, ErrUnsafeState
	}
	lease := &unixSecretLease{path: path, listener: listener, done: make(chan error, 1)}
	api := append([]byte(nil), secrets.APIToken...)
	admin := append([]byte(nil), secrets.AdminToken...)
	go lease.serve(ctx, s.timeout, api, admin)
	return lease, nil
}

func (l *unixSecretLease) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

func (l *unixSecretLease) Wait(ctx context.Context) error {
	if l == nil {
		return ErrCredential
	}
	select {
	case err := <-l.done:
		return err
	case <-ctx.Done():
		_ = l.Close()
		return ErrCredential
	}
}

func (l *unixSecretLease) Close() error {
	if l == nil {
		return nil
	}
	var closeErr error
	l.once.Do(func() {
		if l.listener != nil {
			closeErr = l.listener.Close()
		}
		if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) && closeErr == nil {
			closeErr = err
		}
	})
	return closeErr
}

func (l *unixSecretLease) serve(ctx context.Context, timeout time.Duration, api, admin []byte) {
	result := error(ErrCredential)
	defer zeroBytes(api)
	defer zeroBytes(admin)
	defer func() {
		_ = l.Close()
		l.done <- result
	}()
	deadline := time.Now().Add(timeout)
	_ = l.listener.SetDeadline(deadline)
	connection, err := l.listener.AcceptUnix()
	if err != nil {
		return
	}
	// One client only: closing the listener immediately after the accepted peer
	// prevents a second connection from racing for the single envelope.
	_ = l.listener.Close()
	defer connection.Close()
	_ = connection.SetDeadline(deadline)
	select {
	case <-ctx.Done():
		return
	default:
	}
	payload := make([]byte, 0, 128)
	payload = append(payload, `{"schema":1,"api_auth_token":"`...)
	payload = append(payload, api...)
	payload = append(payload, `","admin_auth_token":"`...)
	payload = append(payload, admin...)
	payload = append(payload, `"}`...)
	if len(payload) > maximumBootstrapFrameBytes {
		zeroBytes(payload)
		return
	}
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)
	zeroBytes(payload)
	if err := writeAll(connection, frame); err != nil {
		zeroBytes(frame)
		return
	}
	zeroBytes(frame)
	ack, err := readBootstrapFrame(bufio.NewReader(io.LimitReader(connection, maximumBootstrapFrameBytes+5)))
	if err != nil {
		zeroBytes(ack)
		return
	}
	var response struct {
		Schema   int  `json:"schema"`
		Accepted bool `json:"accepted"`
	}
	if err := decodeStrict(ack, &response); err != nil || response.Schema != 1 || !response.Accepted {
		zeroBytes(ack)
		return
	}
	zeroBytes(ack)
	result = nil
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) != 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(value) {
			return io.ErrUnexpectedEOF
		}
		value = value[written:]
	}
	return nil
}

func readBootstrapFrame(reader *bufio.Reader) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header)
	if length == 0 || length > maximumBootstrapFrameBytes {
		return nil, ErrCredential
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		zeroBytes(payload)
		return nil, err
	}
	if _, err := reader.Peek(1); err == nil {
		zeroBytes(payload)
		return nil, ErrCredential
	} else if !errors.Is(err, io.EOF) {
		// A timeout means the peer kept the connection open after one complete
		// frame. That is normal; only immediately buffered trailing data is bad.
		if networkError, ok := err.(net.Error); !ok || !networkError.Timeout() {
			zeroBytes(payload)
			return nil, ErrCredential
		}
	}
	return payload, nil
}

func validSecret(value []byte) bool {
	if len(value) != 43 {
		return false
	}
	decoded := make([]byte, base64.RawURLEncoding.DecodedLen(len(value)))
	decodedLength, err := base64.RawURLEncoding.Decode(decoded, value)
	canonical := make([]byte, base64.RawURLEncoding.EncodedLen(decodedLength))
	if err == nil {
		base64.RawURLEncoding.Encode(canonical, decoded[:decodedLength])
	}
	valid := err == nil && decodedLength == 32 && bytes.Equal(canonical, value)
	zeroBytes(decoded)
	zeroBytes(canonical)
	return valid
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
