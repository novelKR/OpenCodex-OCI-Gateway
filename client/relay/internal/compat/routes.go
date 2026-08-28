// Package compat defines the deliberately small public compatibility surface.
package compat

import (
	"errors"
	"net/http"
	"strings"
)

type RouteKind string

const (
	RouteModels    RouteKind = "models"
	RouteResponses RouteKind = "responses"
	RouteCompact   RouteKind = "compact"
	RouteImages    RouteKind = "images"
	RouteArtifacts RouteKind = "artifacts"
	RouteSearch    RouteKind = "search"
	RouteVoice     RouteKind = "voice"
)

// Classify accepts only OpenCodex compatibility endpoints. It intentionally
// does not provide a catch-all /v1 proxy.
func Classify(method, path string, websocket, voiceEnabled bool) (RouteKind, error) {
	if path == "/v1/models" && (method == http.MethodGet || method == http.MethodOptions) {
		return RouteModels, nil
	}
	if path == "/v1/responses" {
		if method == http.MethodPost || method == http.MethodOptions || (websocket && method == http.MethodGet) {
			return RouteResponses, nil
		}
	}
	if path == "/v1/responses/compact" && (method == http.MethodPost || method == http.MethodOptions) {
		return RouteCompact, nil
	}
	if (path == "/v1/images/generations" || path == "/v1/images/edits") && (method == http.MethodPost || method == http.MethodOptions) {
		return RouteImages, nil
	}
	if strings.HasPrefix(path, "/v1/opencodex/artifacts/") && method == http.MethodGet {
		id := strings.TrimPrefix(path, "/v1/opencodex/artifacts/")
		if validOpaquePath(id) {
			return RouteArtifacts, nil
		}
	}
	if path == "/v1/alpha/search" && (method == http.MethodPost || method == http.MethodOptions) {
		return RouteSearch, nil
	}
	if voiceEnabled && isVoiceRoute(method, path, websocket) {
		return RouteVoice, nil
	}
	return "", errors.New("endpoint is not enabled by the compatibility contract")
}

func isVoiceRoute(method, path string, websocket bool) bool {
	if (path == "/v1/live" || path == "/v1/realtime/calls") && (method == http.MethodPost || method == http.MethodOptions) {
		return true
	}
	if !websocket || method != http.MethodGet {
		return false
	}
	if strings.HasPrefix(path, "/v1/live/") {
		return validOpaquePath(strings.TrimPrefix(path, "/v1/live/"))
	}
	if path == "/v1/realtime" {
		return true // The relay preserves the call_id query parameter for OpenCodex to validate.
	}
	if strings.HasPrefix(path, "/v1/realtime/calls/") {
		return validOpaquePath(strings.TrimPrefix(path, "/v1/realtime/calls/"))
	}
	return false
}

func validOpaquePath(value string) bool {
	return value != "" && !strings.Contains(value, "/") && value != "." && value != ".."
}
