package compat

import (
	"net/http"
	"testing"
)

func TestClassifyRejectsCatchAllAndVoiceByDefault(t *testing.T) {
	if _, err := Classify(http.MethodGet, "/v1/unknown", false, false); err == nil {
		t.Fatal("unknown endpoint was accepted")
	}
	if _, err := Classify(http.MethodPost, "/v1/live", false, false); err == nil {
		t.Fatal("voice endpoint was accepted while disabled")
	}
}

func TestClassifyPermitsExactWebsocketRoutes(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/live/call-1", "/v1/realtime", "/v1/realtime/calls/call-1"} {
		if _, err := Classify(http.MethodGet, path, true, true); err != nil {
			t.Fatalf("%s was rejected: %v", path, err)
		}
	}
	if _, err := Classify(http.MethodGet, "/v1/live/a/b", true, true); err == nil {
		t.Fatal("nested live call path was accepted")
	}
}
