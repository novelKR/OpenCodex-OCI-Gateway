package responses

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type terminalResponse struct {
	raw          []byte
	status       string
	output       []json.RawMessage
	createdFrame []byte
}

// SynthesizeTerminalSSE validates one bounded terminal Responses JSON object
// before writing any client bytes, then emits the minimal data-only event
// sequence consumed by Native Codex. It deliberately emits no synthetic
// deltas, IDs, retries, or transport policy.
func SynthesizeTerminalSSE(
	ctx context.Context,
	source io.Reader,
	destination io.Writer,
	limits Limits,
) (TerminalResult, error) {
	captured, err := CaptureResponseJSON(ctx, source, limits)
	if err != nil {
		return TerminalResult{}, err
	}
	defer captured.Close()
	return captured.SynthesizeTerminalSSE(ctx, destination, limits)
}

// CaptureResponseJSON stores one bounded upstream body without interpreting
// it. Callers can release their upstream execution permit as soon as this
// function succeeds, then validate and synthesize the body under a separate
// transform permit.
func CaptureResponseJSON(ctx context.Context, source io.Reader, limits Limits) (*CapturedResponse, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	stored, err := readStorage(ctx, source, limits.MaxResponseBytes, limits.MemoryThreshold, ErrResponseBodyTooLarge)
	if err != nil {
		return nil, err
	}
	return &CapturedResponse{stored: stored}, nil
}

// SynthesizeTerminalSSE validates and emits a previously captured Responses
// JSON body. It does not retain or close destination.
func (c *CapturedResponse) SynthesizeTerminalSSE(
	ctx context.Context,
	destination io.Writer,
	limits Limits,
) (TerminalResult, error) {
	if c == nil || c.stored == nil {
		return TerminalResult{}, ErrInvalidResponse
	}
	if err := limits.validate(); err != nil {
		return TerminalResult{}, err
	}
	reader, err := c.stored.reader()
	if err != nil {
		return TerminalResult{}, err
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return TerminalResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return TerminalResult{}, err
	}
	parsed, err := validateTerminalResponse(raw, limits.MaxOutputItems)
	if err != nil {
		return TerminalResult{}, err
	}

	if err := writeSSEData(ctx, destination, parsed.createdFrame); err != nil {
		return TerminalResult{}, err
	}
	for index, item := range parsed.output {
		prefix := []byte(`{"type":"response.output_item.done","output_index":` + strconv.Itoa(index) + `,"item":`)
		frame := make([]byte, 0, len(prefix)+len(item)+1)
		frame = append(frame, prefix...)
		frame = append(frame, item...)
		frame = append(frame, '}')
		if err := writeSSEData(ctx, destination, frame); err != nil {
			return TerminalResult{}, err
		}
	}
	terminalPrefix := []byte(`{"type":"response.` + parsed.status + `","response":`)
	terminal := make([]byte, 0, len(terminalPrefix)+len(parsed.raw)+1)
	terminal = append(terminal, terminalPrefix...)
	terminal = append(terminal, parsed.raw...)
	terminal = append(terminal, '}')
	if err := writeSSEData(ctx, destination, terminal); err != nil {
		return TerminalResult{}, err
	}
	if err := writeContext(ctx, destination, []byte("data: [DONE]\n\n")); err != nil {
		return TerminalResult{}, err
	}
	return TerminalResult{
		Status:      parsed.status,
		OutputItems: len(parsed.output),
		Bytes:       c.stored.size,
		Spilled:     c.stored.spilled,
	}, nil
}

func validateTerminalResponse(raw []byte, maxOutputItems int) (terminalResponse, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return terminalResponse{}, invalidResponse("empty body")
	}
	var compact bytes.Buffer
	compact.Grow(len(raw))
	if err := json.Compact(&compact, raw); err != nil {
		return terminalResponse{}, invalidResponse("decode JSON: %v", err)
	}
	raw = compact.Bytes()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return terminalResponse{}, invalidResponse("decode JSON: %v", err)
	}
	if fields == nil {
		return terminalResponse{}, invalidResponse("top-level value must be an object")
	}
	var id string
	if err := json.Unmarshal(fields["id"], &id); err != nil || strings.TrimSpace(id) == "" {
		return terminalResponse{}, invalidResponse("id must be a non-empty string")
	}
	if objectRaw, present := fields["object"]; present {
		var object string
		if err := json.Unmarshal(objectRaw, &object); err != nil || object != "response" {
			return terminalResponse{}, invalidResponse("object must be response when present")
		}
	}
	var status string
	if err := json.Unmarshal(fields["status"], &status); err != nil {
		return terminalResponse{}, invalidResponse("status must be a terminal string")
	}
	switch status {
	case "completed", "failed", "incomplete":
	default:
		return terminalResponse{}, invalidResponse("status %q is not terminal", status)
	}
	var output []json.RawMessage
	if err := json.Unmarshal(fields["output"], &output); err != nil || output == nil {
		return terminalResponse{}, invalidResponse("output must be an array")
	}
	if len(output) > maxOutputItems {
		return terminalResponse{}, invalidResponse("output exceeds %d items", maxOutputItems)
	}
	for index, item := range output {
		var itemFields map[string]json.RawMessage
		if err := json.Unmarshal(item, &itemFields); err != nil || itemFields == nil {
			return terminalResponse{}, invalidResponse("output item %d must be an object", index)
		}
		var itemType string
		if err := json.Unmarshal(itemFields["type"], &itemType); err != nil || strings.TrimSpace(itemType) == "" {
			return terminalResponse{}, invalidResponse("output item %d must have a non-empty type", index)
		}
		if itemType == "computer_call" {
			return terminalResponse{}, fmt.Errorf("%w: output item %d", ErrHostedComputerOutput, index)
		}
	}
	if usage, present := fields["usage"]; present && !bytes.Equal(bytes.TrimSpace(usage), []byte("null")) {
		if err := validateUsage(usage); err != nil {
			return terminalResponse{}, invalidResponse("usage: %v", err)
		}
	}

	createdFields := make(map[string]json.RawMessage, len(fields))
	for key, value := range fields {
		createdFields[key] = value
	}
	createdFields["status"] = json.RawMessage(`"in_progress"`)
	createdFields["output"] = json.RawMessage(`[]`)
	createdResponse, err := json.Marshal(createdFields)
	if err != nil {
		return terminalResponse{}, invalidResponse("encode created response: %v", err)
	}
	createdPrefix := []byte(`{"type":"response.created","response":`)
	createdFrame := make([]byte, 0, len(createdPrefix)+len(createdResponse)+1)
	createdFrame = append(createdFrame, createdPrefix...)
	createdFrame = append(createdFrame, createdResponse...)
	createdFrame = append(createdFrame, '}')
	return terminalResponse{
		raw:          raw,
		status:       status,
		output:       output,
		createdFrame: createdFrame,
	}, nil
}

func validateUsage(raw json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("must be an object")
	}
	return validateUsageObject(object)
}

func validateUsageObject(object map[string]any) error {
	for key, value := range object {
		switch typed := value.(type) {
		case nil:
			continue
		case json.Number:
			count, err := typed.Int64()
			if err != nil || count < 0 {
				return fmt.Errorf("%s must be a non-negative integer", key)
			}
		case map[string]any:
			if err := validateUsageObject(typed); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s has an invalid value", key)
		}
	}
	return nil
}

func invalidResponse(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidResponse, fmt.Sprintf(format, arguments...))
}

func writeSSEData(ctx context.Context, destination io.Writer, frame []byte) error {
	if err := writeContext(ctx, destination, []byte("data: ")); err != nil {
		return err
	}
	if err := writeContext(ctx, destination, frame); err != nil {
		return err
	}
	return writeContext(ctx, destination, []byte("\n\n"))
}

func writeContext(ctx context.Context, destination io.Writer, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for len(data) > 0 {
		count, err := destination.Write(data)
		if count > 0 {
			data = data[count:]
		}
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrNoProgress
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}
