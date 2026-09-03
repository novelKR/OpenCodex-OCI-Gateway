package containerruntime

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRuntimeHTTPProberPollsReadinessBeforeAuthentication(t *testing.T) {
	secrets := testSecrets()
	healthCalls := 0
	paths := []string{}
	tokens := []string{}
	client := newProberTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		token := request.Header.Get("X-OpenCodex-API-Key")
		tokens = append(tokens, token)
		if request.URL.Path == "/healthz" {
			healthCalls++
			if healthCalls < 3 {
				writeProberResponse(response, http.StatusServiceUnavailable, `{}`)
				return
			}
			writeProberResponse(response, http.StatusOK, `{"status":"ok","service":"opencodex","port":10100}`)
			return
		}
		switch {
		case request.URL.Path == "/v1/models" && token == "":
			writeProberResponse(response, http.StatusUnauthorized, `{}`)
		case request.URL.Path == "/v1/models" && token == string(secrets.APIToken):
			writeProberResponse(response, http.StatusOK, `{"object":"list","data":[{}]}`)
		case request.URL.Path == "/v1/models" && token == string(secrets.AdminToken):
			writeProberResponse(response, http.StatusForbidden, `{}`)
		case request.URL.Path == "/api/config" && token == "":
			writeProberResponse(response, http.StatusUnauthorized, `{}`)
		case request.URL.Path == "/api/config" && token == string(secrets.APIToken):
			writeProberResponse(response, http.StatusForbidden, `{}`)
		case request.URL.Path == "/api/config" && token == string(secrets.AdminToken):
			writeProberResponse(response, http.StatusOK, `{}`)
		default:
			writeProberResponse(response, http.StatusInternalServerError, `{}`)
		}
	}))
	prober := newRuntimeHTTPProberWithClient(client)
	prober.readinessTimeout = 100 * time.Millisecond
	prober.retryBackoff = time.Millisecond
	guardCalls := 0
	if err := prober.Verify(context.Background(), secrets.APIToken, secrets.AdminToken, func(context.Context) error {
		guardCalls++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if healthCalls != 3 || len(paths) != 9 || guardCalls != 4 {
		t.Fatalf("health=%d paths=%#v guards=%d", healthCalls, paths, guardCalls)
	}
	for index := 0; index < healthCalls; index++ {
		if paths[index] != "/healthz" || tokens[index] != "" {
			t.Fatalf("authenticated request before readiness at %d: path=%q token=%q", index, paths[index], tokens[index])
		}
	}
}

func TestRuntimeHTTPProberReadinessIsContextBounded(t *testing.T) {
	requests := 0
	client := newProberTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/healthz" || request.Header.Get("X-OpenCodex-API-Key") != "" {
			t.Fatalf("request before readiness: %s", request.URL.Path)
		}
		writeProberResponse(response, http.StatusServiceUnavailable, `{}`)
	}))
	prober := newRuntimeHTTPProberWithClient(client)
	prober.readinessTimeout = 15 * time.Millisecond
	prober.retryBackoff = time.Millisecond
	started := time.Now()
	secrets := testSecrets()
	if err := prober.Verify(context.Background(), secrets.APIToken, secrets.AdminToken, func(context.Context) error { return nil }); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond || requests < 2 {
		t.Fatalf("readiness bound elapsed=%s requests=%d", elapsed, requests)
	}
}

func TestRuntimeHTTPProberDoesNotExposeCredentialAndRejectsFailedPostDialGuard(t *testing.T) {
	secrets := testSecrets()
	authenticatedRequests := 0
	client := newProberTestClient(t, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if token := request.Header.Get("X-OpenCodex-API-Key"); token != "" {
			authenticatedRequests++
		}
		switch {
		case request.URL.Path == "/healthz":
			writeProberResponse(response, http.StatusOK, `{"status":"ok","service":"opencodex","port":10100}`)
		case request.URL.Path == "/v1/models" && request.Header.Get("X-OpenCodex-API-Key") == "":
			writeProberResponse(response, http.StatusUnauthorized, `{}`)
		default:
			writeProberResponse(response, http.StatusInternalServerError, string(secrets.APIToken))
		}
	}))
	prober := newRuntimeHTTPProberWithClient(client)
	err := prober.Verify(context.Background(), secrets.APIToken, secrets.AdminToken, func(context.Context) error {
		return errors.New("owned runtime stopped")
	})
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), string(secrets.APIToken)) || strings.Contains(err.Error(), string(secrets.AdminToken)) {
		t.Fatalf("credential-bearing error=%q", err)
	}
	if authenticatedRequests != 0 {
		t.Fatalf("failed post-dial guard sent %d authenticated requests", authenticatedRequests)
	}
}

func newProberTestClient(t *testing.T, handler http.Handler) *http.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	transport := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp4", server.Listener.Addr().String())
		},
	}
	return &http.Client{Transport: transport, Timeout: time.Second}
}

func writeProberResponse(response http.ResponseWriter, status int, body string) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_, _ = io.WriteString(response, body)
}
