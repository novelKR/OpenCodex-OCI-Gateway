package responses

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestPolicyUsesCaseInsensitiveExactModelIDs(t *testing.T) {
	policy, err := NewPolicy(map[string]string{"OpenCode-Go-Responses/GPT-5.6-Luna": "bounded_json"})
	if err != nil {
		t.Fatal(err)
	}
	if policy.Empty() {
		t.Fatal("non-empty policy reported empty")
	}
	if mode, ok := policy.ModeForModel("opencode-go-responses/gpt-5.6-luna"); !ok || mode != ModeBoundedJSON {
		t.Fatalf("exact folded lookup = %q, %v", mode, ok)
	}
	if _, ok := policy.ModeForModel("opencode-go-responses/gpt-5.6-luna:fast"); ok {
		t.Fatal("colon family inherited an exact model policy")
	}
}

func TestDefaultLimitsMatchRelaySafetyEnvelope(t *testing.T) {
	limits := DefaultLimits()
	if limits.MaxEncodedBytes != 32*MiB || limits.MaxDecodedBytes != 256*MiB ||
		limits.MemoryThreshold != MiB || limits.ZstdMaxWindowBytes != 8*uint64(MiB) ||
		limits.MaxResponseBytes != 32*MiB || limits.MaxOutputItems != 10_000 {
		t.Fatalf("default limits = %+v", limits)
	}
}

func TestPolicyRejectsWhitespaceDuplicatesAndUnknownModes(t *testing.T) {
	for _, modes := range []map[string]string{
		{" luna ": "bounded_json"},
		{"LUNA": "bounded_json", "luna": "bounded_json"},
		{"luna": "streaming"},
	} {
		if _, err := NewPolicy(modes); !errors.Is(err, ErrInvalidPolicy) {
			t.Fatalf("NewPolicy(%v) error = %v", modes, err)
		}
	}
}

func TestPrepareRequestMutatesOnlyTopLevelStreamToken(t *testing.T) {
	policy := mustPolicy(t)
	original := "{\n  \"model\" : \"OPENCODE-GO-RESPONSES/GPT-5.6-LUNA\",\n  \"stream\" : true,\n  \"input\" : [{\"role\":\"user\",\"content\":\"literal \\\"stream\\\":true remains\"}],\n  \"store\": false\n}"
	prepared, err := PrepareRequest(context.Background(), io.NopCloser(strings.NewReader(original)), "", policy, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if prepared.Action != ActionNormalize || !prepared.ModelMatched || !prepared.ClientRequestedStream || !prepared.Normalized {
		t.Fatalf("decision = %+v", prepared)
	}
	got := readPrepared(t, prepared)
	want := strings.Replace(original, "\"stream\" : true", "\"stream\" : false", 1)
	if got != want {
		t.Fatalf("rewritten request:\n%s\nwant:\n%s", got, want)
	}
}

func TestPrepareRequestPassthroughIsByteExact(t *testing.T) {
	policy := mustPolicy(t)
	original := []byte(`{"model":"gpt-5.6-luna","stream":true,"input":[]}`)
	prepared, err := PrepareRequest(context.Background(), io.NopCloser(bytes.NewReader(original)), "identity", policy, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if prepared.Action != ActionPassthrough || prepared.ModelMatched {
		t.Fatalf("decision = %+v", prepared)
	}
	got, err := io.ReadAll(prepared.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("identity passthrough changed bytes")
	}
}

func TestPrepareRequestZstdPassthroughAndNormalization(t *testing.T) {
	policy := mustPolicy(t)
	for _, test := range []struct {
		name       string
		model      string
		action     Action
		byteExact  bool
		wantStream string
	}{
		{name: "bare luna byte exact", model: "gpt-5.6-luna", action: ActionPassthrough, byteExact: true, wantStream: "true"},
		{name: "target recompressed", model: targetModel, action: ActionNormalize, wantStream: "false"},
	} {
		t.Run(test.name, func(t *testing.T) {
			plain := []byte(`{"model":"` + test.model + `","stream":true,"input":[{"role":"user","content":"hello"}]}`)
			encoded := encodeZstd(t, plain)
			prepared, err := PrepareRequest(context.Background(), io.NopCloser(bytes.NewReader(encoded)), "ZSTD", policy, testLimits())
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			if prepared.Action != test.action || prepared.ContentEncoding != "zstd" && test.action == ActionNormalize {
				t.Fatalf("decision = %+v", prepared)
			}
			gotEncoded, err := io.ReadAll(prepared.Body)
			if err != nil {
				t.Fatal(err)
			}
			if test.byteExact && !bytes.Equal(gotEncoded, encoded) {
				t.Fatal("zstd passthrough changed bytes")
			}
			got := decodeZstd(t, gotEncoded)
			if !bytes.Contains(got, []byte(`"stream":`+test.wantStream)) {
				t.Fatalf("decoded body = %s", got)
			}
		})
	}
}

func TestPrepareRequestHostedImagePassthroughAndComputerReject(t *testing.T) {
	policy := mustPolicy(t)
	tests := []struct {
		name   string
		body   string
		action Action
	}{
		{
			name:   "hosted image generation",
			body:   requestWith(`"tools":[{"type":"image_generation"}]`),
			action: ActionPassthrough,
		},
		{
			name:   "hosted image gen",
			body:   requestWith(`"tools":[{"type":"image_gen"}]`),
			action: ActionPassthrough,
		},
		{
			name:   "hosted image tool choice",
			body:   requestWith(`"tools":[{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}`),
			action: ActionPassthrough,
		},
		{
			name:   "hosted image additional tools",
			body:   requestWith(`"input":[{"type":"additional_tools","tools":[{"type":"image_generation"}]}]`),
			action: ActionPassthrough,
		},
		{
			name:   "hosted computer tool",
			body:   requestWith(`"tools":[{"type":"computer_use_preview"}]`),
			action: ActionRejectHostedComputer,
		},
		{
			name:   "hosted computer tool choice",
			body:   requestWith(`"tool_choice":{"type":"computer"}`),
			action: ActionRejectHostedComputer,
		},
		{
			name:   "hosted computer output",
			body:   requestWith(`"input":[{"type":"computer_call_output","call_id":"call_1","output":{"type":"computer_screenshot","image_url":"data:image/png;base64,AA=="}}]`),
			action: ActionRejectHostedComputer,
		},
		{
			name:   "ordinary computer function",
			body:   requestWith(`"tools":[{"type":"function","name":"computer","parameters":{"type":"object"}}]`),
			action: ActionNormalize,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepared, err := PrepareRequest(context.Background(), io.NopCloser(strings.NewReader(test.body)), "", policy, testLimits())
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			if prepared.Action != test.action {
				t.Fatalf("action = %v, want %v", prepared.Action, test.action)
			}
			if test.action != ActionNormalize && readPrepared(t, prepared) != test.body {
				t.Fatal("tool passthrough changed original bytes")
			}
		})
	}
}

func TestPrepareRequestAcceptsJSONEndingAtClosingBrace(t *testing.T) {
	prepared, err := PrepareRequest(
		context.Background(),
		io.NopCloser(strings.NewReader(`{"model":"other","stream":true}`)),
		"",
		mustPolicy(t),
		testLimits(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if prepared.Action != ActionPassthrough {
		t.Fatalf("action = %v", prepared.Action)
	}
}

func TestPrepareRequestEnforcesBoundsAndCancellation(t *testing.T) {
	policy := mustPolicy(t)
	body := requestWith(`"input":[]`)
	limits := testLimits()
	limits.MaxEncodedBytes = int64(len(body) - 1)
	if _, err := PrepareRequest(context.Background(), io.NopCloser(strings.NewReader(body)), "", policy, limits); !errors.Is(err, ErrEncodedBodyTooLarge) {
		t.Fatalf("encoded limit error = %v", err)
	}

	limits = testLimits()
	limits.MaxDecodedBytes = int64(len(body) - 1)
	encoded := encodeZstd(t, []byte(body))
	if _, err := PrepareRequest(context.Background(), io.NopCloser(bytes.NewReader(encoded)), "zstd", policy, limits); !errors.Is(err, ErrDecodedBodyTooLarge) {
		t.Fatalf("decoded limit error = %v", err)
	}

	if _, err := PrepareRequest(context.Background(), io.NopCloser(strings.NewReader(body)), "gzip", policy, testLimits()); !errors.Is(err, ErrUnsupportedContentEncoding) {
		t.Fatalf("encoding error = %v", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareRequest(cancelled, io.NopCloser(strings.NewReader(body)), "", policy, testLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestPrepareRequestRejectsBrokenAndConcatenatedZstd(t *testing.T) {
	plain := []byte(requestWith(`"input":[]`))
	encoded := encodeZstd(t, plain)
	for _, body := range [][]byte{
		encoded[:len(encoded)-1],
		append(bytes.Clone(encoded), encoded...),
	} {
		if _, err := PrepareRequest(context.Background(), io.NopCloser(bytes.NewReader(body)), "zstd", mustPolicy(t), testLimits()); !errors.Is(err, ErrMalformedRequest) {
			t.Fatalf("broken zstd error = %v", err)
		}
	}
}

func TestPrepareRequestRejectsZstdWindowAboveLimit(t *testing.T) {
	padding := strings.Repeat("x", 2*1024*1024)
	plain := []byte(requestWith(`"input":[{"role":"user","content":"` + padding + `"}]`))
	var encoded bytes.Buffer
	encoder, err := zstd.NewWriter(&encoded, zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(2*1024*1024))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	limits := testLimits()
	limits.ZstdMaxWindowBytes = 1024 * 1024
	if _, err := PrepareRequest(context.Background(), io.NopCloser(bytes.NewReader(encoded.Bytes())), "zstd", mustPolicy(t), limits); !errors.Is(err, ErrMalformedRequest) {
		t.Fatalf("oversized zstd window error = %v", err)
	}
}

func TestPrepareRequestRejectsCorruptTruncatedAndOversizedWindowZstd(t *testing.T) {
	policy := mustPolicy(t)
	body := []byte(requestWith(`"input":[{"role":"user","content":"zstd integrity"}]`))
	encoded := encodeZstd(t, body)
	corrupt := bytes.Clone(encoded)
	corrupt[len(corrupt)-1] ^= 0xff
	for name, payload := range map[string][]byte{
		"checksum":  corrupt,
		"truncated": encoded[:len(encoded)-3],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PrepareRequest(context.Background(), io.NopCloser(bytes.NewReader(payload)), "zstd", policy, testLimits()); !errors.Is(err, ErrMalformedRequest) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	var largeWindow bytes.Buffer
	encoder, err := zstd.NewWriter(&largeWindow, zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(1<<20))
	if err != nil {
		t.Fatal(err)
	}
	largeBody := []byte(requestWith(`"input":[{"role":"user","content":"` + strings.Repeat("x", 2<<20) + `"}]`))
	if _, err := encoder.Write(largeBody); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	limits := testLimits()
	limits.ZstdMaxWindowBytes = 1024
	if _, err := PrepareRequest(context.Background(), io.NopCloser(bytes.NewReader(largeWindow.Bytes())), "zstd", policy, limits); !errors.Is(err, ErrMalformedRequest) {
		t.Fatalf("window limit error = %v", err)
	}
}

func TestPrepareRequestRejectsMalformedRelevantShape(t *testing.T) {
	policy := mustPolicy(t)
	for _, body := range []string{
		`{"model":"x","stream":true`,
		`{"model":"` + targetModel + `","stream":"true"}`,
		`{"model":"` + targetModel + `","stream":true,"stream":false}`,
		`{"model":17,"stream":true}`,
	} {
		if _, err := PrepareRequest(context.Background(), io.NopCloser(strings.NewReader(body)), "", policy, testLimits()); !errors.Is(err, ErrMalformedRequest) {
			t.Fatalf("body %q error = %v", body, err)
		}
	}
}

func TestPrepareRequestUsesAnonymousProtectedSpool(t *testing.T) {
	policy := mustPolicy(t)
	limits := testLimits()
	limits.MemoryThreshold = 8
	prepared, err := PrepareRequest(context.Background(), io.NopCloser(strings.NewReader(requestWith(`"input":[{"role":"user","content":"large enough to spill"}]`))), "", policy, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if !prepared.Spilled {
		t.Fatal("large request did not report a spill")
	}
	file, ok := prepared.Body.(*os.File)
	if !ok {
		t.Fatalf("prepared body = %T, want anonymous file", prepared.Body)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("spool mode = %o", info.Mode().Perm())
	}
	if _, err := os.Stat(file.Name()); !os.IsNotExist(err) {
		t.Fatalf("spool still has a directory entry: %v", err)
	}
}

const targetModel = "opencode-go-responses/gpt-5.6-luna"

func mustPolicy(t *testing.T) Policy {
	t.Helper()
	policy, err := NewPolicy(map[string]string{targetModel: "bounded_json"})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func testLimits() Limits {
	limits := DefaultLimits()
	limits.MaxEncodedBytes = 4 * MiB
	limits.MaxDecodedBytes = 4 * MiB
	limits.MaxResponseBytes = 4 * MiB
	limits.MemoryThreshold = 64 * 1024
	return limits
}

func requestWith(extra string) string {
	return `{"model":"` + targetModel + `","stream":true,` + extra + `}`
}

func readPrepared(t *testing.T, prepared *PreparedRequest) string {
	t.Helper()
	data, err := io.ReadAll(prepared.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func encodeZstd(t *testing.T, plain []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	encoder, err := zstd.NewWriter(&encoded, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func decodeZstd(t *testing.T, encoded []byte) []byte {
	t.Helper()
	decoder, err := zstd.NewReader(bytes.NewReader(encoded), zstd.WithDecoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	plain, err := io.ReadAll(decoder)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}
