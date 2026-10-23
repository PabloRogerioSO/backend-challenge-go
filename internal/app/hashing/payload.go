// Package hashing produces deterministic hashes of request payloads.
package hashing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// HashCanonical returns the hex SHA-256 of the canonical JSON form of payload:
// the payload is marshaled, decoded into generic values and encoded again, so
// object keys come out sorted at every depth and the result does not depend on
// struct field order or map iteration order. Numbers keep their literal form.
func HashCanonical(payload any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("hashing: marshal payload: %w", err)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return "", fmt.Errorf("hashing: decode payload: %w", err)
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(generic); err != nil {
		return "", fmt.Errorf("hashing: encode canonical payload: %w", err)
	}

	canonical := strings.TrimSuffix(buf.String(), "\n")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:]), nil
}
