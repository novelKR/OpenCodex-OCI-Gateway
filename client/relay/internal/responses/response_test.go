package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSynthesizeTerminalSSEPreservesItemsAndTerminal(t *testing.T) {
	raw := []byte(`{
  "id":"resp_1",
  "object":"response",
  "status":"completed",
  "output":[
    {"id":"fc_1","type":"function_call","call_id":"call_1","name":"computer__get_app_state","arguments":"{\"screenshot\":\"data:image/png;base64,AA==\"}","extension":{"kept":true}},
    {"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}
  ],
  "usage":{"input_tokens":9,"output_tokens":15,"total_tokens":24,"input_tokens_details":{"cached_tokens":0}},
  "extension":"preserved"
}`)
	var output bytes.Buffer
	result, err := SynthesizeTerminalSSE(context.Background(), bytes.NewReader(raw), &output, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.OutputItems != 2 || result.Bytes != int64(len(raw)) {
		t.Fatalf("result = %+v", result)
	}
	frames := dataFrames(t, output.String())
	if len(frames) != 4 {
		t.Fatalf("frames = %d: %q", len(frames), output.String())
	}
	var created map[string]any
	if err := json.Unmarshal(frames[0], &created); err != nil {
		t.Fatal(err)
	}
	createdResponse := created["response"].(map[string]any)
	if created["type"] != "response.created" || createdResponse["status"] != "in_progress" || len(createdResponse["output"].([]any)) != 0 || createdResponse["extension"] != "preserved" {
		t.Fatalf("created frame = %#v", created)
	}
	var done map[string]any
	if err := json.Unmarshal(frames[1], &done); err != nil {
		t.Fatal(err)
	}
	item := done["item"].(map[string]any)
	if done["type"] != "response.output_item.done" || done["output_index"] != float64(0) || item["call_id"] != "call_1" || item["arguments"] != `{"screenshot":"data:image/png;base64,AA=="}` {
		t.Fatalf("done frame = %#v", done)
	}
	var terminal map[string]any
	if err := json.Unmarshal(frames[3], &terminal); err != nil {
		t.Fatal(err)
	}
	if terminal["type"] != "response.completed" || terminal["response"].(map[string]any)["extension"] != "preserved" {
		t.Fatalf("terminal frame = %#v", terminal)
	}
	if strings.Count(output.String(), "data: [DONE]\n\n") != 1 || !strings.HasSuffix(output.String(), "data: [DONE]\n\n") {
		t.Fatalf("DONE trailer = %q", output.String())
	}
}

func TestCapturedResponseCanBeSynthesizedAfterUpstreamCapture(t *testing.T) {
	raw := `{"id":"resp_capture","object":"response","status":"completed","output":[{"id":"msg_capture","type":"message","status":"completed","role":"assistant","content":[]}]}`
	captured, err := CaptureResponseJSON(context.Background(), strings.NewReader(raw), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer captured.Close()
	if captured.Bytes() != int64(len(raw)) || captured.Spilled() {
		t.Fatalf("capture bytes=%d spilled=%t", captured.Bytes(), captured.Spilled())
	}
	var output bytes.Buffer
	result, err := captured.SynthesizeTerminalSSE(context.Background(), &output, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.OutputItems != 1 {
		t.Fatalf("result = %#v", result)
	}
	if got := strings.Count(output.String(), "data: [DONE]\n\n"); got != 1 {
		t.Fatalf("DONE count = %d", got)
	}
}

func TestSynthesizeTerminalSSEPreservesFailedAndIncomplete(t *testing.T) {
	for _, status := range []string{"failed", "incomplete"} {
		t.Run(status, func(t *testing.T) {
			raw := `{"id":"resp","object":"response","status":"` + status + `","output":[]}`
			var output bytes.Buffer
			result, err := SynthesizeTerminalSSE(context.Background(), strings.NewReader(raw), &output, testLimits())
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != status || !strings.Contains(output.String(), `"type":"response.`+status+`"`) {
				t.Fatalf("status result = %+v, body = %s", result, output.String())
			}
		})
	}
}

func TestSynthesizeTerminalSSERejectsInvalidSnapshotsBeforeWriting(t *testing.T) {
	validPrefix := `{"id":"r","object":"response",`
	tests := []struct {
		name string
		body string
		err  error
	}{
		{name: "malformed", body: `{`, err: ErrInvalidResponse},
		{name: "empty id", body: validPrefix + `"status":"completed","output":[],"id":""}`, err: ErrInvalidResponse},
		{name: "running", body: validPrefix + `"status":"running","output":[]}`, err: ErrInvalidResponse},
		{name: "null output", body: validPrefix + `"status":"completed","output":null}`, err: ErrInvalidResponse},
		{name: "item type missing", body: validPrefix + `"status":"completed","output":[{"id":"x"}]}`, err: ErrInvalidResponse},
		{name: "negative usage", body: validPrefix + `"status":"completed","output":[],"usage":{"input_tokens":-1}}`, err: ErrInvalidResponse},
		{name: "fractional usage", body: validPrefix + `"status":"completed","output":[],"usage":{"input_tokens":1.5}}`, err: ErrInvalidResponse},
		{name: "hosted computer output", body: validPrefix + `"status":"completed","output":[{"type":"computer_call","id":"cc"}]}`, err: ErrHostedComputerOutput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			_, err := SynthesizeTerminalSSE(context.Background(), strings.NewReader(test.body), &output, testLimits())
			if !errors.Is(err, test.err) {
				t.Fatalf("error = %v, want %v", err, test.err)
			}
			if output.Len() != 0 {
				t.Fatalf("invalid snapshot wrote %q", output.String())
			}
		})
	}
}

func TestSynthesizeTerminalSSEEnforcesBodyAndItemLimits(t *testing.T) {
	raw := `{"id":"r","status":"completed","output":[]}`
	limits := testLimits()
	limits.MaxResponseBytes = int64(len(raw) - 1)
	if _, err := SynthesizeTerminalSSE(context.Background(), strings.NewReader(raw), &bytes.Buffer{}, limits); !errors.Is(err, ErrResponseBodyTooLarge) {
		t.Fatalf("body limit error = %v", err)
	}
	limits = testLimits()
	limits.MaxOutputItems = 1
	raw = `{"id":"r","status":"completed","output":[{"type":"message"},{"type":"message"}]}`
	if _, err := SynthesizeTerminalSSE(context.Background(), strings.NewReader(raw), &bytes.Buffer{}, limits); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("item limit error = %v", err)
	}
}

func TestSynthesizeTerminalSSECancellationAndSpill(t *testing.T) {
	raw := `{"id":"r","status":"completed","output":[{"type":"message","content":"large"}]}`
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	if _, err := SynthesizeTerminalSSE(cancelled, strings.NewReader(raw), &output, testLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatal("cancelled synthesis wrote output")
	}

	limits := testLimits()
	limits.MemoryThreshold = 8
	result, err := SynthesizeTerminalSSE(context.Background(), strings.NewReader(raw), &output, limits)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Spilled {
		t.Fatal("response did not use bounded spool")
	}
}

func dataFrames(t *testing.T, body string) [][]byte {
	t.Helper()
	parts := strings.Split(body, "\n\n")
	frames := make([][]byte, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "data: [DONE]" {
			continue
		}
		lines := strings.Split(part, "\n")
		if len(lines) != 1 || !strings.HasPrefix(lines[0], "data: ") {
			t.Fatalf("invalid physical SSE record %q", part)
		}
		frames = append(frames, []byte(strings.TrimPrefix(lines[0], "data: ")))
	}
	return frames
}
