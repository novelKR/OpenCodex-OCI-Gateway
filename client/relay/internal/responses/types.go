// Package responses implements the bounded request and terminal-response
// transformations used by the relay's opt-in Responses compatibility path.
// It deliberately owns no HTTP transport or retry policy.
package responses

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

const (
	MiB = int64(1024 * 1024)

	ModeBoundedJSON Mode = "bounded_json"
)

var (
	ErrInvalidPolicy              = errors.New("invalid responses model policy")
	ErrInvalidLimits              = errors.New("invalid responses limits")
	ErrUnsupportedContentEncoding = errors.New("unsupported content encoding")
	ErrEncodedBodyTooLarge        = errors.New("encoded request body exceeds limit")
	ErrDecodedBodyTooLarge        = errors.New("decoded request body exceeds limit")
	ErrMalformedRequest           = errors.New("malformed Responses request")
	ErrResponseBodyTooLarge       = errors.New("Responses JSON body exceeds limit")
	ErrInvalidResponse            = errors.New("invalid terminal Responses JSON")
	ErrHostedComputerOutput       = errors.New("hosted computer output is unsupported")
)

type Mode string

type Policy struct {
	models map[string]Mode
}

// NewPolicy validates and canonicalizes an exact-model policy. Model IDs may
// be matched case-insensitively, but configured keys must themselves be
// canonical: surrounding whitespace and case-folded duplicates are rejected.
func NewPolicy(modelModes map[string]string) (Policy, error) {
	policy := Policy{models: make(map[string]Mode, len(modelModes))}
	for rawModel, rawMode := range modelModes {
		if rawModel == "" || strings.TrimSpace(rawModel) != rawModel {
			return Policy{}, fmt.Errorf("%w: model IDs must be non-empty and must not contain surrounding whitespace", ErrInvalidPolicy)
		}
		model := strings.ToLower(rawModel)
		if _, exists := policy.models[model]; exists {
			return Policy{}, fmt.Errorf("%w: model IDs must be unique case-insensitively", ErrInvalidPolicy)
		}
		mode := Mode(rawMode)
		if mode != ModeBoundedJSON {
			return Policy{}, fmt.Errorf("%w: unsupported mode %q for model %q", ErrInvalidPolicy, rawMode, rawModel)
		}
		policy.models[model] = mode
	}
	return policy, nil
}

func (p Policy) Empty() bool { return len(p.models) == 0 }

func (p Policy) ModeForModel(model string) (Mode, bool) {
	mode, ok := p.models[strings.ToLower(model)]
	return mode, ok
}

type Action uint8

const (
	ActionPassthrough Action = iota
	ActionNormalize
	ActionRejectHostedComputer
)

// Limits are injectable so the local and external relay profiles can enforce
// different encoded-body envelopes without changing the transformation code.
type Limits struct {
	MaxEncodedBytes    int64
	MaxDecodedBytes    int64
	MemoryThreshold    int64
	ZstdMaxWindowBytes uint64
	MaxResponseBytes   int64
	MaxOutputItems     int
}

func DefaultLimits() Limits {
	return Limits{
		MaxEncodedBytes:    32 * MiB,
		MaxDecodedBytes:    256 * MiB,
		MemoryThreshold:    MiB,
		ZstdMaxWindowBytes: 8 * uint64(MiB),
		MaxResponseBytes:   32 * MiB,
		MaxOutputItems:     10_000,
	}
}

func (l Limits) validate() error {
	if l.MaxEncodedBytes <= 0 || l.MaxDecodedBytes <= 0 || l.MemoryThreshold <= 0 ||
		l.ZstdMaxWindowBytes == 0 || l.MaxResponseBytes <= 0 || l.MaxOutputItems <= 0 {
		return ErrInvalidLimits
	}
	return nil
}

// PreparedRequest owns Body and any anonymous temporary spool backing it.
// Call Close after the upstream request completes, including on reject paths.
type PreparedRequest struct {
	Action                Action
	Body                  io.ReadCloser
	ContentEncoding       string
	ContentLength         int64
	EncodedBytes          int64
	DecodedBytes          int64
	ClientRequestedStream bool
	ModelMatched          bool
	Normalized            bool
	Spilled               bool

	closeOnce sync.Once
	closeErr  error
}

func (p *PreparedRequest) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		if p.Body != nil {
			p.closeErr = p.Body.Close()
		}
	})
	return p.closeErr
}

type TerminalResult struct {
	Status      string
	OutputItems int
	Bytes       int64
	Spilled     bool
}

// CapturedResponse owns one bounded, anonymous copy of an upstream Responses
// JSON body. Capture is deliberately separate from validation and SSE
// synthesis so the transport permit can be released before the memory-heavy
// transform stage begins.
type CapturedResponse struct {
	stored    *storage
	closeOnce sync.Once
	closeErr  error
}

func (c *CapturedResponse) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.closeErr = c.stored.close()
	})
	return c.closeErr
}

func (c *CapturedResponse) Bytes() int64 {
	if c == nil || c.stored == nil {
		return 0
	}
	return c.stored.size
}

func (c *CapturedResponse) Spilled() bool {
	return c != nil && c.stored != nil && c.stored.spilled
}
