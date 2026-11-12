package convert

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func checkStrictObject(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("parse opts: %w", err)
	}
	if tok != json.Delim('{') {
		return fmt.Errorf("parse opts: top-level value must be a JSON object")
	}
	seen := make(map[string]struct{})
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("parse opts: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("parse opts: object key is not a string")
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("parse opts: duplicate key %q", key)
		}
		seen[key] = struct{}{}
		var discard json.RawMessage
		if err := dec.Decode(&discard); err != nil {
			return fmt.Errorf("parse opts: %w", err)
		}
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("parse opts: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("parse opts: trailing data after JSON object")
	}
	return nil
}

func pdfHexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
