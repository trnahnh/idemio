package canonical

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/gowebpki/jcs"
)

const (
	Prefix = "sha256-jcs-v1:"

	maxSafeInteger = 1<<53 - 1
)

type Request struct {
	AgentID      string
	Operation    string
	Payload      json.RawMessage
	ResourceID   string
	ResourceType string
}

func Canonicalize(document []byte) ([]byte, error) {
	canonical, err := jcs.Transform(document)
	if err != nil {
		return nil, fmt.Errorf("canonicalize: %w", err)
	}
	return canonical, nil
}

func Hash(r Request) (string, error) {
	payload := r.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("null")
	}
	if err := validateNumbers(payload); err != nil {
		return "", err
	}

	document, err := json.Marshal(map[string]any{
		"agent_id":      r.AgentID,
		"operation":     r.Operation,
		"payload":       payload,
		"resource_id":   r.ResourceID,
		"resource_type": r.ResourceType,
	})
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}

	canonical, err := Canonicalize(document)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(canonical)
	return Prefix + hex.EncodeToString(sum[:]), nil
}

func validateNumbers(payload json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("payload is not valid JSON: %w", err)
	}
	return walk(value)
}

func walk(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for _, nested := range typed {
			if err := walk(nested); err != nil {
				return err
			}
		}
	case []any:
		for _, nested := range typed {
			if err := walk(nested); err != nil {
				return err
			}
		}
	case json.Number:
		return checkNumber(typed)
	}
	return nil
}

// JCS serializes numbers as IEEE-754 doubles, so a value the double cannot hold exactly
// would hash inconsistently with what the client sent: ADR-0003.
func checkNumber(number json.Number) error {
	asFloat, err := number.Float64()
	if err != nil {
		return fmt.Errorf("number %s is not representable as a double: %w", number, err)
	}
	if math.IsInf(asFloat, 0) || math.IsNaN(asFloat) {
		return fmt.Errorf("number %s is not finite", number)
	}
	if isIntegerLiteral(number.String()) && math.Abs(asFloat) > maxSafeInteger {
		return fmt.Errorf("integer %s is outside the exactly representable range", number)
	}
	return nil
}

func isIntegerLiteral(text string) bool {
	return !strings.ContainsAny(text, ".eE")
}
