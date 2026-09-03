package containerruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

const (
	runtimeLoopbackBaseURL       = "http://127.0.0.1:10210"
	maximumReadinessWait         = 30 * time.Second
	defaultReadinessRetryBackoff = 100 * time.Millisecond
	maximumReadinessRetryBackoff = time.Second
)

type RuntimeHTTPProber struct {
	client           *http.Client
	readinessTimeout time.Duration
	retryBackoff     time.Duration
}

func NewRuntimeHTTPProber() *RuntimeHTTPProber {
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: -1}
	transport := &http.Transport{
		Proxy: nil, DialContext: dialer.DialContext, DisableKeepAlives: true,
		MaxIdleConns: 0, IdleConnTimeout: time.Second,
	}
	return &RuntimeHTTPProber{client: &http.Client{
		Transport: transport, Timeout: 6 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") },
	}, readinessTimeout: maximumReadinessWait, retryBackoff: defaultReadinessRetryBackoff}
}

func newRuntimeHTTPProberWithClient(client *http.Client) *RuntimeHTTPProber {
	return &RuntimeHTTPProber{client: client, readinessTimeout: maximumReadinessWait, retryBackoff: defaultReadinessRetryBackoff}
}

func (p *RuntimeHTTPProber) Verify(ctx context.Context, apiToken, adminToken []byte) error {
	if p == nil || p.client == nil || !validSecret(apiToken) || !validSecret(adminToken) || bytes.Equal(apiToken, adminToken) {
		return ErrCredential
	}
	if err := p.waitUntilReady(ctx); err != nil {
		return ErrUnavailable
	}
	if body, status, err := p.get(ctx, "/v1/models", nil); err != nil || !isDenied(status) {
		zeroBytes(body)
		return ErrUnavailable
	} else {
		zeroBytes(body)
	}
	models, status, err := p.get(ctx, "/v1/models", apiToken)
	if err != nil || status < 200 || status >= 300 || !validModels(models) {
		zeroBytes(models)
		return ErrUnavailable
	}
	zeroBytes(models)
	if body, status, err := p.get(ctx, "/v1/models", adminToken); err != nil || !isDenied(status) {
		zeroBytes(body)
		return ErrUnavailable
	} else {
		zeroBytes(body)
	}
	if body, status, err := p.get(ctx, "/api/config", nil); err != nil || !isDenied(status) {
		zeroBytes(body)
		return ErrUnavailable
	} else {
		zeroBytes(body)
	}
	if body, status, err := p.get(ctx, "/api/config", apiToken); err != nil || !isDenied(status) {
		zeroBytes(body)
		return ErrUnavailable
	} else {
		zeroBytes(body)
	}
	configuration, status, err := p.get(ctx, "/api/config", adminToken)
	if err != nil || status < 200 || status >= 300 || !validJSONObject(configuration) {
		zeroBytes(configuration)
		return ErrUnavailable
	}
	zeroBytes(configuration)
	return nil
}

func (p *RuntimeHTTPProber) waitUntilReady(ctx context.Context) error {
	timeout := p.readinessTimeout
	if timeout <= 0 || timeout > maximumReadinessWait {
		timeout = maximumReadinessWait
	}
	backoff := p.retryBackoff
	if backoff <= 0 || backoff > maximumReadinessRetryBackoff {
		backoff = defaultReadinessRetryBackoff
	}
	readinessContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		health, status, err := p.get(readinessContext, "/healthz", nil)
		valid := err == nil && status == http.StatusOK && validHealthIdentity(health)
		zeroBytes(health)
		if valid {
			return nil
		}
		// A successful HTTP response with a non-retryable status, or a 200
		// response with the wrong identity, indicates a listener other than the
		// expected runtime. Network errors and transient server failures may be
		// startup races and are retried within the bounded readiness window.
		if err == nil && (status < http.StatusInternalServerError || status > 599) {
			return ErrUnavailable
		}
		timer := time.NewTimer(backoff)
		select {
		case <-readinessContext.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ErrUnavailable
		case <-timer.C:
		}
		if backoff < maximumReadinessRetryBackoff/2 {
			backoff *= 2
		} else {
			backoff = maximumReadinessRetryBackoff
		}
	}
}

func (p *RuntimeHTTPProber) get(ctx context.Context, path string, token []byte) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, runtimeLoopbackBaseURL+path, nil)
	if err != nil {
		return nil, 0, ErrUnavailable
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Connection", "close")
	if len(token) != 0 {
		request.Header.Set("X-OpenCodex-API-Key", string(token))
	}
	response, err := p.client.Do(request)
	if err != nil {
		return nil, 0, ErrUnavailable
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumCommandOutputBytes+1))
	if err != nil || len(body) > maximumCommandOutputBytes {
		zeroBytes(body)
		return nil, 0, ErrUnavailable
	}
	return body, response.StatusCode, nil
}

func validHealthIdentity(data []byte) bool {
	var value struct {
		Status  string `json:"status"`
		Service string `json:"service"`
		Port    int    `json:"port"`
	}
	if err := decodeBoundedJSON(data, &value); err != nil {
		return false
	}
	return value.Status == "ok" && value.Service == "opencodex" && value.Port == GuestServicePort
}

func validModels(data []byte) bool {
	var value struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	return decodeBoundedJSON(data, &value) == nil && len(value.Data) > 0
}

func validJSONObject(data []byte) bool {
	var value map[string]json.RawMessage
	return decodeBoundedJSON(data, &value) == nil && value != nil
}

func decodeBoundedJSON(data []byte, destination any) error {
	if len(data) == 0 || len(data) > maximumCommandOutputBytes || rejectDuplicateJSONKeys(data) != nil {
		return ErrUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return ErrUnavailable
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrUnavailable
	}
	return nil
}

func isDenied(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}
