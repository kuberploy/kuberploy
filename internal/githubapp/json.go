package githubapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maxJSONDepth = 64

// validateSingleJSON rejects trailing documents and duplicate object keys. Go's
// ordinary JSON unmarshal silently accepts duplicate keys, which is unsafe for
// signed or authorization-bearing provider input.
func validateSingleJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := validateJSONValue(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return errors.New("invalid trailing JSON data")
	}
	return nil
}

func validateJSONValue(dec *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return errors.New("JSON nesting is too deep")
	}
	token, err := dec.Token()
	if err != nil {
		return errors.New("invalid JSON value")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyToken, keyErr := dec.Token()
			if keyErr != nil {
				return errors.New("invalid JSON object key")
			}
			key, keyOK := keyToken.(string)
			if !keyOK {
				return errors.New("invalid JSON object key")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			seen[key] = struct{}{}
			if err = validateJSONValue(dec, depth+1); err != nil {
				return err
			}
		}
		end, endErr := dec.Token()
		if endErr != nil || end != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for dec.More() {
			if err = validateJSONValue(dec, depth+1); err != nil {
				return err
			}
		}
		end, endErr := dec.Token()
		if endErr != nil || end != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func decodeSingleJSON(data []byte, dst any) error {
	if err := validateSingleJSON(data); err != nil {
		return err
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return errors.New("JSON value does not match the expected shape")
	}
	return nil
}
