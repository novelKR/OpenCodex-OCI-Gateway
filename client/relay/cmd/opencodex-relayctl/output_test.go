package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/handoff"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

func TestWriteOperationErrorEmitsBoundedSafeJSON(t *testing.T) {
	const secret = "/home/example/private/config.toml bearer-secret"
	var output bytes.Buffer
	if err := writeOperationError(&output, errors.New(secret)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), "config.toml") {
		t.Fatalf("structured error leaked diagnostics: %s", output.String())
	}
	if output.Len() > 1024 {
		t.Fatalf("structured error is unexpectedly large: %d", output.Len())
	}

	var envelope operationErrorEnvelope
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != 1 || envelope.OK || envelope.Error.Code != "operation_failed" || envelope.Error.MessageKey != envelope.Error.Code {
		t.Fatalf("operation error envelope = %#v", envelope)
	}
}

func TestSafeOperationErrorMapsRecoveryWithoutRawDetails(t *testing.T) {
	envelope := safeOperationError(routing.ErrRecoveryRequired)
	if envelope.Error.Code != "routing_recovery_required" || !envelope.Error.Retryable || envelope.Error.RecommendedAction != "open_recovery" {
		t.Fatalf("recovery envelope = %#v", envelope)
	}
	for _, test := range []struct {
		err       error
		code      string
		retryable bool
		action    string
	}{
		{routing.ErrNativeRepairUnavailable, "native_repair_unavailable", false, "manual_remediation"},
		{routing.ErrNativeRepairOwnerChanged, "native_repair_owner_changed", true, "refresh_status"},
		{routing.ErrNativeRepairArtifactsChanged, "native_repair_owner_changed", true, "refresh_status"},
		{routing.ErrNativeRepairConfigurationFailed, "native_owner_repair_failed", true, "refresh_status"},
		{routing.ErrNativeOwnerBusy, "native_owner_busy", true, "retry_owner_repair"},
		{routing.ErrNativeOwnerConfigurationInvalid, "native_owner_configuration_invalid", false, "manual_remediation"},
		{routing.ErrNativeOwnerRestoreFailed, "native_owner_restore_failed", true, "refresh_status"},
		{routing.ErrNativeOwnerResultInvalid, "native_owner_result_invalid", false, "manual_remediation"},
		{routing.ErrNativeRepairStateIncomplete, "native_state_repair_pending", true, "open_recovery"},
		{handoff.ErrUnsafeExecutable, "opencodex_candidate_changed", true, "rediscover_opencodex"},
		{routing.ErrNativeVerification, "native_routing_unverified", false, "manual_remediation"},
		{routing.ErrNativeRepairGenerationStale, "routing_generation_changed", true, "refresh_status"},
	} {
		result := safeOperationError(test.err)
		if result.Error.Code != test.code || result.Error.Retryable != test.retryable || result.Error.RecommendedAction != test.action {
			t.Fatalf("native repair envelope = %#v, want code=%s retryable=%t action=%s", result, test.code, test.retryable, test.action)
		}
	}
	desktop := safeOperationError(routing.ErrDesktopExitConfirmation)
	if desktop.Error.Code != "desktop_exit_confirmation_required" || desktop.Error.RecommendedAction != "retry_after_desktop_exit" {
		t.Fatalf("desktop envelope = %#v", desktop)
	}
}

func TestJSONOutputRequestedRecognizesOnlyExplicitJSONFlag(t *testing.T) {
	for _, args := range [][]string{{"mode", "status", "--json"}, {"mode", "status", "--json=true"}} {
		if !jsonOutputRequested(args) {
			t.Fatalf("JSON flag not recognized: %#v", args)
		}
	}
	for _, args := range [][]string{{"mode", "status"}, {"--config", "/tmp/--json"}, {"--json-output"}} {
		if jsonOutputRequested(args) {
			t.Fatalf("non-JSON invocation was recognized: %#v", args)
		}
	}
}
