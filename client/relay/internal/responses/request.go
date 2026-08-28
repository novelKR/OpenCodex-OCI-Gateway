package responses

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"
)

type requestScan struct {
	model                string
	modelSeen            bool
	streamSeen           bool
	stream               bool
	streamStart          int64
	streamEnd            int64
	hostedImage          bool
	hostedComputer       bool
	hostedComputerOutput bool
}

// PrepareRequest reads a Responses body once, classifies it structurally, and
// returns either the exact encoded bytes or an opt-in stream:false rewrite.
// It never performs an upstream request and never creates a replayable GetBody.
func PrepareRequest(
	ctx context.Context,
	body io.ReadCloser,
	contentEncoding string,
	policy Policy,
	limits Limits,
) (*PreparedRequest, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	if body == nil {
		return nil, fmt.Errorf("%w: request body is required", ErrMalformedRequest)
	}
	defer body.Close()

	encoding, err := classifyContentEncoding(contentEncoding)
	if err != nil {
		return nil, err
	}
	original, err := readStorage(ctx, body, limits.MaxEncodedBytes, limits.MemoryThreshold, ErrEncodedBodyTooLarge)
	if err != nil {
		return nil, err
	}
	originalOwned := true
	defer func() {
		if originalOwned {
			_ = original.close()
		}
	}()

	decoded := original
	decodedOwned := false
	spilled := original.spilled
	if encoding == "zstd" {
		reader, err := original.reader()
		if err != nil {
			return nil, err
		}
		decoder, err := zstd.NewReader(
			reader,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxWindow(limits.ZstdMaxWindowBytes),
			zstd.WithDecoderMaxMemory(uint64(limits.MaxDecodedBytes)+limits.ZstdMaxWindowBytes),
		)
		if err != nil {
			return nil, fmt.Errorf("%w: initialize zstd decoder: %v", ErrMalformedRequest, err)
		}
		decoded, err = readStorage(ctx, decoder, limits.MaxDecodedBytes, limits.MemoryThreshold, ErrDecodedBodyTooLarge)
		decoder.Close()
		if err != nil {
			if errorsIsLimit(err) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: decode zstd body: %v", ErrMalformedRequest, err)
		}
		decodedOwned = true
		spilled = spilled || decoded.spilled
		defer func() {
			if decodedOwned {
				_ = decoded.close()
			}
		}()
	}

	decodedReader, err := decoded.reader()
	if err != nil {
		return nil, err
	}
	scan, err := scanResponsesRequest(decodedReader)
	if err != nil {
		return nil, err
	}
	_, modelMatched := policy.ModeForModel(scan.model)
	action := ActionPassthrough
	if scan.hostedComputer || scan.hostedComputerOutput {
		action = ActionRejectHostedComputer
	} else if modelMatched && scan.stream && !scan.hostedImage {
		action = ActionNormalize
	}

	base := PreparedRequest{
		Action:                action,
		EncodedBytes:          original.size,
		DecodedBytes:          decoded.size,
		ClientRequestedStream: scan.stream,
		ModelMatched:          modelMatched,
		Normalized:            action == ActionNormalize,
		Spilled:               spilled,
	}
	if action != ActionNormalize {
		reader, err := original.takeReader()
		if err != nil {
			return nil, err
		}
		originalOwned = false
		base.Body = reader
		base.ContentEncoding = contentEncoding
		base.ContentLength = original.size
		return &base, nil
	}

	rewrittenWriter := &storageWriter{
		limit:      limits.MaxDecodedBytes,
		threshold:  limits.MemoryThreshold,
		limitError: ErrDecodedBodyTooLarge,
	}
	if err := copyStorageRange(ctx, rewrittenWriter, decoded, 0, scan.streamStart); err != nil {
		rewrittenWriter.close()
		return nil, err
	}
	if _, err := rewrittenWriter.Write([]byte("false")); err != nil {
		rewrittenWriter.close()
		return nil, err
	}
	if err := copyStorageRange(ctx, rewrittenWriter, decoded, scan.streamEnd, decoded.size-scan.streamEnd); err != nil {
		rewrittenWriter.close()
		return nil, err
	}
	rewritten := rewrittenWriter.finish()
	rewrittenOwned := true
	spilled = spilled || rewritten.spilled
	defer func() {
		if rewrittenOwned {
			_ = rewritten.close()
		}
	}()

	upstreamBody := rewritten
	upstreamOwned := false
	outputEncoding := ""
	if encoding == "zstd" {
		encodedWriter := &storageWriter{
			limit:      limits.MaxEncodedBytes,
			threshold:  limits.MemoryThreshold,
			limitError: ErrEncodedBodyTooLarge,
		}
		encoder, err := zstd.NewWriter(
			encodedWriter,
			zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(int(limits.ZstdMaxWindowBytes)),
		)
		if err != nil {
			encodedWriter.close()
			return nil, fmt.Errorf("initialize zstd encoder: %w", err)
		}
		rewrittenReader, err := rewritten.reader()
		if err != nil {
			encoder.Close()
			encodedWriter.close()
			return nil, err
		}
		if err := copyAllContext(ctx, encoder, rewrittenReader); err != nil {
			encoder.Close()
			encodedWriter.close()
			return nil, err
		}
		if err := encoder.Close(); err != nil {
			encodedWriter.close()
			return nil, err
		}
		upstreamBody = encodedWriter.finish()
		upstreamOwned = true
		outputEncoding = "zstd"
		spilled = spilled || upstreamBody.spilled
		defer func() {
			if upstreamOwned {
				_ = upstreamBody.close()
			}
		}()
	}

	reader, err := upstreamBody.takeReader()
	if err != nil {
		return nil, err
	}
	if upstreamBody == rewritten {
		rewrittenOwned = false
	} else {
		upstreamOwned = false
	}
	base.Body = reader
	base.ContentEncoding = outputEncoding
	base.ContentLength = upstreamBody.size
	base.Spilled = spilled
	return &base, nil
}

func classifyContentEncoding(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || strings.EqualFold(trimmed, "identity") {
		return "identity", nil
	}
	if strings.EqualFold(trimmed, "zstd") {
		return "zstd", nil
	}
	return "", fmt.Errorf("%w: %q", ErrUnsupportedContentEncoding, value)
}

func errorsIsLimit(err error) bool {
	return errors.Is(err, ErrDecodedBodyTooLarge) || errors.Is(err, ErrEncodedBodyTooLarge)
}

func copyAllContext(ctx context.Context, destination io.Writer, source io.Reader) error {
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, readErr := source.Read(buffer)
		if count > 0 {
			if _, err := destination.Write(buffer[:count]); err != nil {
				return err
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
		if count == 0 {
			return io.ErrNoProgress
		}
	}
}

type valueKind uint8

const (
	valueOther valueKind = iota
	valueString
	valueBool
)

type valueInfo struct {
	kind  valueKind
	text  string
	bool  bool
	start int64
	end   int64
}

type valueContext struct {
	inInput   bool
	toolArray bool
	toolEntry bool
}

type requestJSONScanner struct {
	reader *bufio.Reader
	offset int64
	result requestScan
	depth  int
}

func scanResponsesRequest(reader io.Reader) (requestScan, error) {
	scanner := requestJSONScanner{reader: bufio.NewReaderSize(reader, 32*1024)}
	if err := scanner.skipWhitespace(); err != nil {
		return requestScan{}, scanner.malformed(err)
	}
	first, err := scanner.peekByte()
	if err != nil || first != '{' {
		return requestScan{}, scanner.malformed(fmt.Errorf("top-level value must be an object"))
	}
	if _, err := scanner.parseValue(valueContext{}, true); err != nil {
		return requestScan{}, scanner.malformed(err)
	}
	if err := scanner.skipWhitespace(); err != nil {
		return requestScan{}, scanner.malformed(err)
	}
	if _, err := scanner.peekByte(); err != io.EOF {
		if err == nil {
			return requestScan{}, scanner.malformed(fmt.Errorf("trailing data"))
		}
		return requestScan{}, scanner.malformed(err)
	}
	return scanner.result, nil
}

func (s *requestJSONScanner) malformed(err error) error {
	return fmt.Errorf("%w at byte %d: %v", ErrMalformedRequest, s.offset, err)
}

func (s *requestJSONScanner) parseValue(ctx valueContext, root bool) (valueInfo, error) {
	if err := s.skipWhitespace(); err != nil {
		return valueInfo{}, err
	}
	start := s.offset
	byteValue, err := s.peekByte()
	if err != nil {
		return valueInfo{}, err
	}
	switch byteValue {
	case '{':
		if err := s.parseObject(ctx, root); err != nil {
			return valueInfo{}, err
		}
		return valueInfo{kind: valueOther, start: start, end: s.offset}, nil
	case '[':
		if err := s.parseArray(ctx); err != nil {
			return valueInfo{}, err
		}
		return valueInfo{kind: valueOther, start: start, end: s.offset}, nil
	case '"':
		text, overflow, err := s.parseString(4096)
		if err != nil {
			return valueInfo{}, err
		}
		if overflow {
			return valueInfo{kind: valueString, start: start, end: s.offset}, nil
		}
		return valueInfo{kind: valueString, text: text, start: start, end: s.offset}, nil
	case 't':
		if err := s.consumeLiteral("true"); err != nil {
			return valueInfo{}, err
		}
		return valueInfo{kind: valueBool, bool: true, start: start, end: s.offset}, nil
	case 'f':
		if err := s.consumeLiteral("false"); err != nil {
			return valueInfo{}, err
		}
		return valueInfo{kind: valueBool, bool: false, start: start, end: s.offset}, nil
	case 'n':
		if err := s.consumeLiteral("null"); err != nil {
			return valueInfo{}, err
		}
		return valueInfo{kind: valueOther, start: start, end: s.offset}, nil
	default:
		if byteValue == '-' || (byteValue >= '0' && byteValue <= '9') {
			if err := s.parseNumber(); err != nil {
				return valueInfo{}, err
			}
			return valueInfo{kind: valueOther, start: start, end: s.offset}, nil
		}
		return valueInfo{}, fmt.Errorf("unexpected byte %q", byteValue)
	}
}

func (s *requestJSONScanner) parseObject(ctx valueContext, root bool) error {
	if err := s.enter('{'); err != nil {
		return err
	}
	defer s.leave()
	if err := s.skipWhitespace(); err != nil {
		return err
	}
	if next, err := s.peekByte(); err == nil && next == '}' {
		_, _ = s.readByte()
		return nil
	}
	for {
		key, overflow, err := s.parseString(256)
		if err != nil {
			return err
		}
		if overflow {
			key = ""
		}
		if err := s.skipWhitespace(); err != nil {
			return err
		}
		if err := s.expect(':'); err != nil {
			return err
		}
		child := valueContext{inInput: ctx.inInput}
		if root && key == "input" {
			child.inInput = true
		}
		if key == "tools" && (root || ctx.inInput) {
			child.toolArray = true
		}
		if root && key == "tool_choice" {
			child.toolEntry = true
		}
		value, err := s.parseValue(child, false)
		if err != nil {
			return err
		}

		if root {
			switch key {
			case "model":
				if s.result.modelSeen {
					return fmt.Errorf("duplicate top-level model")
				}
				if value.kind != valueString || value.text == "" {
					return fmt.Errorf("top-level model must be a non-empty string")
				}
				s.result.modelSeen = true
				s.result.model = value.text
			case "stream":
				if s.result.streamSeen {
					return fmt.Errorf("duplicate top-level stream")
				}
				if value.kind != valueBool {
					return fmt.Errorf("top-level stream must be a boolean")
				}
				s.result.streamSeen = true
				s.result.stream = value.bool
				s.result.streamStart = value.start
				s.result.streamEnd = value.end
			}
		}
		if ctx.toolEntry && key == "type" && value.kind == valueString {
			switch value.text {
			case "image_generation", "image_gen":
				s.result.hostedImage = true
			case "computer", "computer_use_preview":
				s.result.hostedComputer = true
			}
		}
		if ctx.inInput && key == "type" && value.kind == valueString && value.text == "computer_call_output" {
			s.result.hostedComputerOutput = true
		}

		if err := s.skipWhitespace(); err != nil {
			return err
		}
		separator, err := s.readByte()
		if err != nil {
			return err
		}
		switch separator {
		case '}':
			return nil
		case ',':
			if err := s.skipWhitespace(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("expected object separator")
		}
	}
}

func (s *requestJSONScanner) parseArray(ctx valueContext) error {
	if err := s.enter('['); err != nil {
		return err
	}
	defer s.leave()
	if err := s.skipWhitespace(); err != nil {
		return err
	}
	if next, err := s.peekByte(); err == nil && next == ']' {
		_, _ = s.readByte()
		return nil
	}
	for {
		element := valueContext{inInput: ctx.inInput, toolEntry: ctx.toolArray}
		if _, err := s.parseValue(element, false); err != nil {
			return err
		}
		if err := s.skipWhitespace(); err != nil {
			return err
		}
		separator, err := s.readByte()
		if err != nil {
			return err
		}
		switch separator {
		case ']':
			return nil
		case ',':
			if err := s.skipWhitespace(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("expected array separator")
		}
	}
}

func (s *requestJSONScanner) enter(expected byte) error {
	if s.depth >= 512 {
		return fmt.Errorf("JSON nesting exceeds 512 levels")
	}
	if err := s.expect(expected); err != nil {
		return err
	}
	s.depth++
	return nil
}

func (s *requestJSONScanner) leave() { s.depth-- }

func (s *requestJSONScanner) parseString(captureLimit int) (string, bool, error) {
	if err := s.expect('"'); err != nil {
		return "", false, err
	}
	raw := make([]byte, 0, min(captureLimit, 64)+2)
	raw = append(raw, '"')
	overflow := false
	for {
		value, err := s.readByte()
		if err != nil {
			return "", false, err
		}
		if !overflow {
			if len(raw) >= captureLimit+1 {
				overflow = true
			} else {
				raw = append(raw, value)
			}
		}
		if value == '"' {
			break
		}
		if value < 0x20 {
			return "", false, fmt.Errorf("control byte in string")
		}
		if value != '\\' {
			continue
		}
		escape, err := s.readByte()
		if err != nil {
			return "", false, err
		}
		if !overflow {
			if len(raw) >= captureLimit+1 {
				overflow = true
			} else {
				raw = append(raw, escape)
			}
		}
		switch escape {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		case 'u':
			for range 4 {
				hex, err := s.readByte()
				if err != nil {
					return "", false, err
				}
				if !isHex(hex) {
					return "", false, fmt.Errorf("invalid unicode escape")
				}
				if !overflow {
					if len(raw) >= captureLimit+1 {
						overflow = true
					} else {
						raw = append(raw, hex)
					}
				}
			}
		default:
			return "", false, fmt.Errorf("invalid string escape")
		}
	}
	if overflow {
		return "", true, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", false, err
	}
	return text, false, nil
}

func (s *requestJSONScanner) parseNumber() error {
	if next, _ := s.peekByte(); next == '-' {
		_, _ = s.readByte()
	}
	next, err := s.peekByte()
	if err != nil {
		return err
	}
	if next == '0' {
		_, _ = s.readByte()
		if digit, err := s.peekByte(); err == nil && digit >= '0' && digit <= '9' {
			return fmt.Errorf("leading zero in number")
		}
	} else if next >= '1' && next <= '9' {
		for {
			digit, err := s.peekByte()
			if err != nil || digit < '0' || digit > '9' {
				break
			}
			_, _ = s.readByte()
		}
	} else {
		return fmt.Errorf("invalid number")
	}
	if next, err := s.peekByte(); err == nil && next == '.' {
		_, _ = s.readByte()
		if err := s.consumeDigits(); err != nil {
			return err
		}
	}
	if next, err := s.peekByte(); err == nil && (next == 'e' || next == 'E') {
		_, _ = s.readByte()
		if sign, err := s.peekByte(); err == nil && (sign == '+' || sign == '-') {
			_, _ = s.readByte()
		}
		if err := s.consumeDigits(); err != nil {
			return err
		}
	}
	return nil
}

func (s *requestJSONScanner) consumeDigits() error {
	count := 0
	for {
		digit, err := s.peekByte()
		if err != nil || digit < '0' || digit > '9' {
			break
		}
		_, _ = s.readByte()
		count++
	}
	if count == 0 {
		return fmt.Errorf("number requires digits")
	}
	return nil
}

func (s *requestJSONScanner) consumeLiteral(literal string) error {
	for index := range len(literal) {
		value, err := s.readByte()
		if err != nil || value != literal[index] {
			return fmt.Errorf("invalid literal")
		}
	}
	return nil
}

func (s *requestJSONScanner) skipWhitespace() error {
	for {
		value, err := s.peekByte()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if value != ' ' && value != '\t' && value != '\r' && value != '\n' {
			return nil
		}
		_, _ = s.readByte()
	}
}

func (s *requestJSONScanner) expect(expected byte) error {
	value, err := s.readByte()
	if err != nil {
		return err
	}
	if value != expected {
		return fmt.Errorf("expected %q", expected)
	}
	return nil
}

func (s *requestJSONScanner) peekByte() (byte, error) {
	value, err := s.reader.Peek(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}

func (s *requestJSONScanner) readByte() (byte, error) {
	value, err := s.reader.ReadByte()
	if err == nil {
		s.offset++
	}
	return value, err
}

func isHex(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'f') || (value >= 'A' && value <= 'F')
}
