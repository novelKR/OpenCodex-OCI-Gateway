// Package loopbackauth binds an OpenCodex credential to one freshly opened
// loopback TCP connection. The request object never contains the credential:
// a caller-supplied lifecycle lease is acquired before dial, the authority
// check and secret load run only after dial, and a single-use connection
// injects the header into the first HTTP/1 request.
package loopbackauth

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	Authority                   = "127.0.0.1:10210"
	CredentialHeader            = "X-OpenCodex-API-Key"
	AuthorizationTimeout        = 5 * time.Second
	maximumHeaderBytes          = 64 << 10
	maximumTokenBytes           = 4 << 10
	headerTerminatorBytes       = 4
	maximumCandidateHeaderBytes = maximumHeaderBytes + headerTerminatorBytes
	maximumEncodedHeaderBytes   = maximumHeaderBytes + maximumTokenBytes + len(CredentialHeader) + 8
)

var ErrBinding = errors.New("OpenCodex loopback credential binding failed")

// Authorization transfers ownership of Token to Transport.
type Authorization struct {
	Token []byte
}

type LeaseAcquirer func(context.Context) (func() error, error)
type Authorizer func(context.Context) (Authorization, error)

// Transport deliberately creates a new hardened http.Transport per request.
// There is no idle connection, proxy lookup, HTTP/2 coalescing, or automatic
// reused-connection retry that could escape the post-dial authority proof.
type Transport struct {
	template  *http.Transport
	lease     LeaseAcquirer
	authorize Authorizer
}

func NewTransport(template *http.Transport, lease LeaseAcquirer, authorize Authorizer) (*Transport, error) {
	if lease == nil || authorize == nil {
		return nil, ErrBinding
	}
	if template == nil {
		template = http.DefaultTransport.(*http.Transport)
	}
	return &Transport{template: template.Clone(), lease: lease, authorize: authorize}, nil
}

func (t *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t == nil || t.template == nil || t.lease == nil || t.authorize == nil || !validRequest(request) {
		return nil, ErrBinding
	}
	if hasCredentialHeader(request.Header) || hasCredentialHeader(request.Trailer) || declaresCredentialTrailer(request.Header) {
		return nil, ErrBinding
	}
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	release, err := t.lease(clone.Context())
	if err != nil || release == nil {
		return nil, ErrBinding
	}
	lease := &requestLease{release: release}

	transport := t.template.Clone()
	baseDial := transport.DialContext
	if baseDial == nil {
		dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: -1}
		baseDial = dialer.DialContext
	}
	transport.Proxy = nil
	transport.DialTLSContext = nil
	transport.DisableKeepAlives = true
	transport.ForceAttemptHTTP2 = false
	transport.MaxIdleConns = 0
	transport.MaxIdleConnsPerHost = 0
	transport.MaxConnsPerHost = 1
	transport.IdleConnTimeout = 0
	requestContext := clone.Context()
	var dialed atomic.Bool
	var boundConnection atomic.Pointer[credentialConn]
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if !dialed.CompareAndSwap(false, true) {
			return nil, ErrBinding
		}
		if address != Authority || network != "tcp" && network != "tcp4" {
			return nil, ErrBinding
		}
		connection, err := baseDial(ctx, "tcp4", Authority)
		if err != nil {
			return nil, ErrBinding
		}
		authorization, err := t.authorize(requestContext)
		if err != nil || !validToken(authorization.Token) {
			zero(authorization.Token)
			_ = connection.Close()
			return nil, ErrBinding
		}
		credentialConnection := newCredentialConn(connection, authorization, lease.Close)
		boundConnection.Store(credentialConnection)
		return credentialConnection, nil
	}
	response, err := transport.RoundTrip(clone)
	transport.CloseIdleConnections()
	if err != nil {
		_ = lease.Close()
		return nil, ErrBinding
	}
	if response.StatusCode == http.StatusSwitchingProtocols {
		readWriteBody, ok := response.Body.(io.ReadWriteCloser)
		connection := boundConnection.Load()
		if !ok || connection == nil {
			_ = response.Body.Close()
			_ = lease.Close()
			return nil, ErrBinding
		}
		// Go 1.26 exposes CloseWrite on its 101 response body, while the Go
		// 1.24 toolchain used by CI only exposes io.ReadWriteCloser. Preserve
		// the TCP half-close contract explicitly and consistently.
		response.Body = &upgradeResponseBody{
			ReadWriteCloser: readWriteBody,
			connection:      connection,
		}
	}
	return response, nil
}

func (*Transport) CloseIdleConnections() {}

func validRequest(request *http.Request) bool {
	if request == nil || request.URL == nil || request.Context() == nil ||
		request.Method == http.MethodConnect || request.URL.Scheme != "http" ||
		request.URL.Host != Authority || request.URL.User != nil || request.URL.Fragment != "" ||
		request.Host != "" && request.Host != Authority {
		return false
	}
	return true
}

func validToken(token []byte) bool {
	if len(token) == 0 || len(token) > maximumTokenBytes {
		return false
	}
	return !bytes.ContainsAny(token, "\x00\r\n")
}

func hasCredentialHeader(header http.Header) bool {
	for key := range header {
		if strings.EqualFold(key, CredentialHeader) {
			return true
		}
	}
	return false
}

func declaresCredentialTrailer(header http.Header) bool {
	for _, declaration := range header.Values("Trailer") {
		for _, name := range strings.Split(declaration, ",") {
			if strings.EqualFold(strings.TrimSpace(name), CredentialHeader) {
				return true
			}
		}
	}
	return false
}

type credentialConn struct {
	net.Conn
	mu            sync.Mutex
	header        []byte
	token         []byte
	release       func() error
	writeTimeout  time.Duration
	authenticated bool
	failed        bool
}

type upgradeResponseBody struct {
	io.ReadWriteCloser
	connection *credentialConn
}

func (b *upgradeResponseBody) CloseWrite() error {
	if b == nil || b.connection == nil {
		return ErrBinding
	}
	return b.connection.CloseWrite()
}

func newCredentialConn(connection net.Conn, authorization Authorization, release func() error) *credentialConn {
	return newCredentialConnWithTimeout(connection, authorization, release, AuthorizationTimeout)
}

func newCredentialConnWithTimeout(connection net.Conn, authorization Authorization, release func() error, timeout time.Duration) *credentialConn {
	result := &credentialConn{
		Conn:         connection,
		token:        append([]byte(nil), authorization.Token...),
		release:      release,
		writeTimeout: timeout,
	}
	zero(authorization.Token)
	return result
}

func (c *credentialConn) Write(value []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed {
		return 0, ErrBinding
	}
	if c.authenticated {
		return c.Conn.Write(value)
	}

	previous := len(c.header)
	if previous > maximumHeaderBytes {
		return c.failLocked()
	}
	remaining := maximumCandidateHeaderBytes - previous
	if remaining <= 0 {
		return c.failLocked()
	}
	prefixLength := len(value)
	if prefixLength > remaining {
		prefixLength = remaining
	}
	// Both staging buffers use constant, reviewed capacities. Besides making
	// the protocol's memory bound explicit, this avoids deriving allocation
	// sizes from caller-controlled write lengths.
	candidate := make([]byte, 0, maximumCandidateHeaderBytes)
	candidate = append(candidate, c.header...)
	candidate = append(candidate, value[:prefixLength]...)
	separator := bytes.Index(candidate, headerTerminator)
	if separator < 0 {
		if len(value) > maximumHeaderBytes-previous {
			zero(candidate)
			return c.failLocked()
		}
		c.header = append(c.header, value...)
		zero(candidate)
		return len(value), nil
	}
	if separator > maximumHeaderBytes || !validGeneratedHeader(candidate[:separator]) {
		zero(candidate)
		return c.failLocked()
	}
	bodyOffset := separator + headerTerminatorBytes - previous
	if bodyOffset < 0 || bodyOffset > len(value) {
		zero(candidate)
		return c.failLocked()
	}
	encoded := make([]byte, 0, maximumEncodedHeaderBytes)
	encoded = append(encoded, candidate[:separator]...)
	encoded = append(encoded, '\r', '\n')
	encoded = append(encoded, CredentialHeader...)
	encoded = append(encoded, ':', ' ')
	encoded = append(encoded, c.token...)
	encoded = append(encoded, headerTerminator...)
	zero(candidate)
	writeTimeout := c.writeTimeout
	if writeTimeout <= 0 || writeTimeout > AuthorizationTimeout {
		writeTimeout = AuthorizationTimeout
	}
	if err := c.Conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		zero(encoded)
		return c.failLocked()
	}
	if err := writeAll(c.Conn, encoded); err != nil {
		zero(encoded)
		return c.failLocked()
	}
	zero(encoded)
	if err := c.Conn.SetWriteDeadline(time.Time{}); err != nil {
		return c.failLocked()
	}
	c.authenticated = true
	zero(c.token)
	c.token = nil
	c.finishAuthorizationLocked()
	zero(c.header)
	c.header = nil
	if bodyOffset < len(value) {
		if err := writeAll(c.Conn, value[bodyOffset:]); err != nil {
			c.failed = true
			_ = c.Conn.Close()
			return 0, ErrBinding
		}
	}
	return len(value), nil
}

type requestLease struct {
	once    sync.Once
	release func() error
}

func (l *requestLease) Close() error {
	if l == nil {
		return nil
	}
	var err error
	l.once.Do(func() {
		if l.release != nil {
			err = l.release()
			l.release = nil
		}
	})
	return err
}

func (c *credentialConn) Close() error {
	c.mu.Lock()
	c.failed = true
	zero(c.header)
	c.header = nil
	c.finishAuthorizationLocked()
	c.mu.Unlock()
	return c.Conn.Close()
}

func (c *credentialConn) failLocked() (int, error) {
	c.failed = true
	zero(c.header)
	c.header = nil
	c.finishAuthorizationLocked()
	_ = c.Conn.Close()
	return 0, ErrBinding
}

func (c *credentialConn) finishAuthorizationLocked() {
	zero(c.token)
	c.token = nil
	if c.release != nil {
		_ = c.release()
		c.release = nil
	}
}

func (c *credentialConn) CloseWrite() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed || !c.authenticated {
		return ErrBinding
	}
	halfCloser, ok := c.Conn.(interface{ CloseWrite() error })
	if !ok {
		return ErrBinding
	}
	return halfCloser.CloseWrite()
}

func validGeneratedHeader(header []byte) bool {
	lines := bytes.Split(header, []byte("\r\n"))
	if len(lines) < 1 || len(lines[0]) == 0 || bytes.ContainsAny(lines[0], "\x00\r\n") {
		return false
	}
	for _, line := range lines[1:] {
		separator := bytes.IndexByte(line, ':')
		if separator <= 0 || line[0] == ' ' || line[0] == '\t' {
			return false
		}
		name := strings.TrimSpace(string(line[:separator]))
		if strings.EqualFold(name, CredentialHeader) {
			return false
		}
	}
	return true
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		count, err := writer.Write(value)
		if count > 0 {
			value = value[count:]
		}
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

var headerTerminator = []byte("\r\n\r\n")

// Compile-time assertion keeps the public transport surface explicit.
var _ http.RoundTripper = (*Transport)(nil)
