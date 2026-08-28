package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

func TestParseGatewayCandidateAcceptsOnlyExactBoundedJSON(t *testing.T) {
	candidate, err := parseGatewayCandidate(strings.NewReader(`{"upstream_base_url":"https://gateway.example.test/v1"}`))
	if err != nil || candidate.UpstreamBaseURL != "https://gateway.example.test/v1" {
		t.Fatalf("candidate=%#v err=%v", candidate, err)
	}
	for _, payload := range []string{
		``,
		`{}`,
		`{"upstream_base_url":"https://gateway.example.test/v1","unknown":true}`,
		`{"upstream_base_url":"https://gateway.example.test/v1"}{}`,
		`{"upstream_base_url":"` + strings.Repeat("x", 4097) + `"}`,
		strings.Repeat(" ", maxGatewayCandidateBytes+1),
	} {
		if _, err := parseGatewayCandidate(strings.NewReader(payload)); !errors.Is(err, routing.ErrGatewayInvalidAddress) {
			t.Fatalf("payload length %d error = %v", len(payload), err)
		}
	}
}

func TestGatewayErrorsHaveBoundedJSONContracts(t *testing.T) {
	tests := []struct {
		err    error
		code   string
		action string
	}{
		{routing.ErrGatewayInvalidAddress, "invalid_address", "review_request"},
		{routing.ErrGatewayCredentialUnavailable, "credential_unavailable", "update_credentials"},
		{routing.ErrGatewayAuthenticationFailed, "authentication_failed", "update_credentials"},
		{routing.ErrGatewayUnreachable, "gateway_unreachable", "retry"},
		{routing.ErrGatewayCatalogInvalid, "catalog_invalid", "review_gateway"},
		{routing.ErrGatewayConfigChanged, "config_changed", "refresh_gateway"},
		{routing.ErrGatewayRoutingChanged, "routing_changed", "refresh_status"},
		{routing.ErrGatewayTransitionPending, "transition_pending", "refresh_status"},
		{routing.ErrGatewayRuntimeSwap, "runtime_swap_failed", "refresh_status"},
		{routing.ErrGatewayUnsupported, "gateway_unsupported", "manual_remediation"},
	}
	for _, test := range tests {
		envelope := safeOperationError(test.err)
		if envelope.Error.Code != test.code || envelope.Error.RecommendedAction != test.action {
			t.Fatalf("gateway error envelope=%#v want code=%s action=%s", envelope, test.code, test.action)
		}
		if strings.Contains(envelope.Error.MessageKey, "https://") {
			t.Fatalf("gateway error leaked address: %#v", envelope)
		}
	}
}
