package loopbackauth

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransportDialsBeforeAuthorizationAndInjectsOnlyOnThatConnection(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	received := make(chan string, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		close(accepted)
		defer connection.Close()
		request, readErr := http.ReadRequest(bufio.NewReader(connection))
		if readErr != nil {
			return
		}
		received <- request.Header.Get(CredentialHeader)
		_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	}()
	template := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", listener.Addr().String())
	}}
	var releases atomic.Int64
	leaseHeld := atomic.Bool{}
	transport, err := NewTransport(template, func(context.Context) (func() error, error) {
		leaseHeld.Store(true)
		return func() error {
			leaseHeld.Store(false)
			releases.Add(1)
			return nil
		}, nil
	}, func(context.Context) (Authorization, error) {
		select {
		case <-accepted:
		case <-time.After(time.Second):
			t.Fatal("authorization ran before the loopback connection was accepted")
		}
		if !leaseHeld.Load() {
			t.Fatal("authorization ran without the pre-dial lifecycle lease")
		}
		return Authorization{Token: []byte("synthetic-api-token")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, "http://"+Authority+"/v1/models", nil)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	if releases.Load() != 1 {
		t.Fatal("binding lease was not released after the authenticated header write")
	}
	response.Body.Close()
	if token := <-received; token != "synthetic-api-token" {
		t.Fatalf("received credential = %q", token)
	}
	if request.Header.Get(CredentialHeader) != "" || releases.Load() != 1 {
		t.Fatalf("request credential=%q releases=%d", request.Header.Get(CredentialHeader), releases.Load())
	}
}

func TestTransportAuthorizationFailureWritesNoRequestBytes(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan []byte, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_ = connection.SetReadDeadline(time.Now().Add(time.Second))
		data, _ := io.ReadAll(connection)
		received <- data
	}()
	template := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", listener.Addr().String())
	}}
	transport, _ := NewTransport(template, noOpLease, func(context.Context) (Authorization, error) {
		return Authorization{}, errors.New("owned container is stopped")
	})
	request, _ := http.NewRequest(http.MethodGet, "http://"+Authority+"/v1/models", nil)
	if _, err := transport.RoundTrip(request); !errors.Is(err, ErrBinding) {
		t.Fatalf("RoundTrip error = %v", err)
	}
	if data := <-received; len(data) != 0 {
		t.Fatalf("failed authorization wrote %q", data)
	}
}

func TestTransportCancellationReleasesPredialLeaseAfterBlockedAuthorization(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan []byte, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_ = connection.SetReadDeadline(time.Now().Add(time.Second))
		data, _ := io.ReadAll(connection)
		received <- data
	}()
	template := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", listener.Addr().String())
	}}
	var releases atomic.Int64
	authorizationStarted := make(chan struct{})
	transport, err := NewTransport(template, func(context.Context) (func() error, error) {
		return func() error {
			releases.Add(1)
			return nil
		}, nil
	}, func(ctx context.Context) (Authorization, error) {
		close(authorizationStarted)
		<-ctx.Done()
		return Authorization{}, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+Authority+"/v1/models", nil)
	result := make(chan error, 1)
	go func() {
		_, roundTripErr := transport.RoundTrip(request)
		result <- roundTripErr
	}()
	select {
	case <-authorizationStarted:
	case <-time.After(time.Second):
		t.Fatal("authorization did not begin")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, ErrBinding) {
			t.Fatalf("cancelled RoundTrip error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled authorization did not unwind")
	}
	if releases.Load() != 1 {
		t.Fatalf("cancelled authorization released lease %d times", releases.Load())
	}
	select {
	case data := <-received:
		if len(data) != 0 {
			t.Fatalf("cancelled authorization wrote %q", data)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled connection was not closed")
	}
}

func TestCredentialConnBuffersSplitHeaderAndCoalescedBody(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	connection := newCredentialConn(client, Authorization{Token: []byte("bound-token")}, nil)
	received := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(io.LimitReader(server, 256))
		received <- string(data)
	}()
	parts := []string{
		"POST /v1/responses HTTP/1.1\r\nHost: 127.0.0.1:10210\r\nX-Test: a\r",
		"\nContent-Length: 4\r\n\r\nbody",
	}
	for _, part := range parts {
		if count, err := connection.Write([]byte(part)); err != nil || count != len(part) {
			t.Fatalf("Write = %d, %v", count, err)
		}
	}
	_ = connection.Close()
	value := <-received
	if strings.Count(value, CredentialHeader+": bound-token") != 1 || !strings.HasSuffix(value, "\r\n\r\nbody") {
		t.Fatalf("injected request = %q", value)
	}
}

func TestCredentialConnBoundsBlockedAuthenticatedHeaderWriteAndReleasesLease(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	var releases atomic.Int64
	connection := newCredentialConnWithTimeout(
		client,
		Authorization{Token: []byte("bound-token")},
		func() error {
			releases.Add(1)
			return nil
		},
		20*time.Millisecond,
	)
	result := make(chan error, 1)
	go func() {
		_, err := connection.Write([]byte("GET /v1/models HTTP/1.1\r\nHost: 127.0.0.1:10210\r\n\r\n"))
		result <- err
	}()

	select {
	case err := <-result:
		if !errors.Is(err, ErrBinding) {
			t.Fatalf("blocked authenticated header write error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked authenticated header write exceeded its deadline")
	}
	if releases.Load() != 1 {
		t.Fatalf("blocked authenticated header write released lease %d times", releases.Load())
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if releases.Load() != 1 {
		t.Fatalf("closed failed connection released lease %d times", releases.Load())
	}
	received, err := io.ReadAll(server)
	if err != nil {
		t.Fatal(err)
	}
	if len(received) != 0 {
		t.Fatalf("blocked authenticated header write exposed %q", received)
	}
}

func TestCredentialConnRejectsImpossibleBufferedHeaderWithoutWriting(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	var releases atomic.Int64
	connection := newCredentialConn(
		client,
		Authorization{Token: []byte("bound-token")},
		func() error {
			releases.Add(1)
			return nil
		},
	)
	connection.header = make([]byte, maximumHeaderBytes+1)
	if count, err := connection.Write([]byte("x")); count != 0 || !errors.Is(err, ErrBinding) {
		t.Fatalf("Write = %d, %v", count, err)
	}
	if releases.Load() != 1 {
		t.Fatalf("impossible header released lease %d times", releases.Load())
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTransportRejectsCallerCredentialAndUnsafeToken(t *testing.T) {
	template := &http.Transport{}
	transport, _ := NewTransport(template, noOpLease, func(context.Context) (Authorization, error) {
		return Authorization{Token: []byte("unsafe\r\ntoken")}, nil
	})
	request, _ := http.NewRequest(http.MethodGet, "http://"+Authority+"/v1/models", nil)
	request.Header.Set(CredentialHeader, "caller")
	if _, err := transport.RoundTrip(request); !errors.Is(err, ErrBinding) {
		t.Fatalf("caller credential error = %v", err)
	}
}

func TestTransportPreservesUpgradeWriteAndTCPHalfClose(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan string, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		reader := bufio.NewReader(connection)
		request, readErr := http.ReadRequest(reader)
		if readErr != nil || request.Header.Get(CredentialHeader) != "upgrade-token" {
			return
		}
		_, _ = io.WriteString(connection, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: opencodex-test\r\n\r\n")
		payload, _ := io.ReadAll(reader)
		received <- string(payload)
	}()
	template := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", listener.Addr().String())
	}}
	transport, err := NewTransport(template, noOpLease, func(context.Context) (Authorization, error) {
		return Authorization{Token: []byte("upgrade-token")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, "http://"+Authority+"/v1/responses", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "opencodex-test")
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d", response.StatusCode)
	}
	upgraded, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatalf("upgrade body type = %T", response.Body)
	}
	halfCloser, ok := response.Body.(interface{ CloseWrite() error })
	if !ok {
		t.Fatalf("upgrade body lost CloseWrite: %T", response.Body)
	}
	if _, err := upgraded.Write([]byte("client-frame")); err != nil {
		t.Fatal(err)
	}
	if err := halfCloser.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-received:
		if payload != "client-frame" {
			t.Fatalf("upgrade payload = %q", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("backend did not observe the TCP half-close")
	}
	_ = response.Body.Close()
}

func TestTransportRejectsCredentialTrailersBeforeLeaseOrDial(t *testing.T) {
	var leases, authorizations atomic.Int64
	transport, err := NewTransport(&http.Transport{}, func(context.Context) (func() error, error) {
		leases.Add(1)
		return func() error { return nil }, nil
	}, func(context.Context) (Authorization, error) {
		authorizations.Add(1)
		return Authorization{Token: []byte("bound-token")}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []*http.Request{
		func() *http.Request {
			value, _ := http.NewRequest(http.MethodPost, "http://"+Authority+"/v1/responses", strings.NewReader("body"))
			value.Trailer = http.Header{CredentialHeader: []string{"caller-token"}}
			return value
		}(),
		func() *http.Request {
			value, _ := http.NewRequest(http.MethodPost, "http://"+Authority+"/v1/responses", strings.NewReader("body"))
			value.Header.Set("Trailer", "X-Test, "+CredentialHeader)
			return value
		}(),
	} {
		if _, err := transport.RoundTrip(request); !errors.Is(err, ErrBinding) {
			t.Fatalf("credential trailer error = %v", err)
		}
	}
	if leases.Load() != 0 || authorizations.Load() != 0 {
		t.Fatalf("credential trailer crossed binding boundary: leases=%d authorizations=%d", leases.Load(), authorizations.Load())
	}
}

func noOpLease(context.Context) (func() error, error) {
	return func() error { return nil }, nil
}
