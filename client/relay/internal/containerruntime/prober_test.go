package containerruntime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRuntimeHTTPProberPollsReadinessBeforeAuthentication(t *testing.T) {
	secrets := testSecrets()
	healthCalls := 0
	closedBodies := 0
	paths := []string{}
	tokens := []string{}
	client := &http.Client{Transport: proberRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		paths = append(paths, request.URL.Path)
		token := request.Header.Get("X-OpenCodex-API-Key")
		tokens = append(tokens, token)
		if request.URL.Path == "/healthz" {
			healthCalls++
			if healthCalls < 3 {
				return nil, errors.New("connection refused")
			}
			return proberResponse(http.StatusOK, `{"status":"ok","service":"opencodex","port":10100}`, &closedBodies), nil
		}
		switch {
		case request.URL.Path == "/v1/models" && token == "":
			return proberResponse(http.StatusUnauthorized, `{}`, &closedBodies), nil
		case request.URL.Path == "/v1/models" && token == string(secrets.APIToken):
			return proberResponse(http.StatusOK, `{"object":"list","data":[{}]}`, &closedBodies), nil
		case request.URL.Path == "/v1/models" && token == string(secrets.AdminToken):
			return proberResponse(http.StatusForbidden, `{}`, &closedBodies), nil
		case request.URL.Path == "/api/config" && token == "":
			return proberResponse(http.StatusUnauthorized, `{}`, &closedBodies), nil
		case request.URL.Path == "/api/config" && token == string(secrets.APIToken):
			return proberResponse(http.StatusForbidden, `{}`, &closedBodies), nil
		case request.URL.Path == "/api/config" && token == string(secrets.AdminToken):
			return proberResponse(http.StatusOK, `{}`, &closedBodies), nil
		default:
			return proberResponse(http.StatusInternalServerError, `{}`, &closedBodies), nil
		}
	})}
	prober := newRuntimeHTTPProberWithClient(client)
	prober.readinessTimeout = 100 * time.Millisecond
	prober.retryBackoff = time.Millisecond
	if err := prober.Verify(context.Background(), secrets.APIToken, secrets.AdminToken); err != nil {
		t.Fatal(err)
	}
	if healthCalls != 3 || len(paths) != 9 || closedBodies != 7 {
		t.Fatalf("health=%d paths=%#v closed=%d", healthCalls, paths, closedBodies)
	}
	for index := 0; index < healthCalls; index++ {
		if paths[index] != "/healthz" || tokens[index] != "" {
			t.Fatalf("authenticated request before readiness at %d: path=%q token=%q", index, paths[index], tokens[index])
		}
	}
}

func TestRuntimeHTTPProberReadinessIsContextBounded(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: proberRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.Path != "/healthz" || request.Header.Get("X-OpenCodex-API-Key") != "" {
			t.Fatalf("request before readiness: %s", request.URL.Path)
		}
		return nil, errors.New("connection refused")
	})}
	prober := newRuntimeHTTPProberWithClient(client)
	prober.readinessTimeout = 15 * time.Millisecond
	prober.retryBackoff = time.Millisecond
	started := time.Now()
	secrets := testSecrets()
	if err := prober.Verify(context.Background(), secrets.APIToken, secrets.AdminToken); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond || requests < 2 {
		t.Fatalf("readiness bound elapsed=%s requests=%d", elapsed, requests)
	}
}

func TestRuntimeHTTPProberDoesNotExposeCredentialInErrors(t *testing.T) {
	secrets := testSecrets()
	client := &http.Client{Transport: proberRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch {
		case request.URL.Path == "/healthz":
			return proberResponse(http.StatusOK, `{"status":"ok","service":"opencodex","port":10100}`, nil), nil
		case request.URL.Path == "/v1/models" && request.Header.Get("X-OpenCodex-API-Key") == "":
			return proberResponse(http.StatusUnauthorized, `{}`, nil), nil
		default:
			return nil, errors.New("transport diagnostic " + request.Header.Get("X-OpenCodex-API-Key"))
		}
	})}
	prober := newRuntimeHTTPProberWithClient(client)
	err := prober.Verify(context.Background(), secrets.APIToken, secrets.AdminToken)
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), string(secrets.APIToken)) || strings.Contains(err.Error(), string(secrets.AdminToken)) {
		t.Fatalf("credential-bearing error=%q", err)
	}
}

type proberRoundTripFunc func(*http.Request) (*http.Response, error)

func (f proberRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type proberCloseBody struct {
	io.Reader
	closed *int
}

func (b *proberCloseBody) Close() error {
	if b.closed != nil {
		*b.closed++
	}
	return nil
}

func proberResponse(status int, body string, closed *int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       &proberCloseBody{Reader: strings.NewReader(body), closed: closed},
	}
}
