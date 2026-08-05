package statebranch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

type stateRecord struct {
	firstSeen time.Time
	title     string
	matched   bool
	notified  bool
}

// validateState validates the persisted wire format without first decoding it
// into a map. Keeping the token stream visible lets us reject duplicate job IDs
// and duplicate record fields, both of which encoding/json would otherwise
// silently overwrite.
func validateState(data []byte) (map[string]stateRecord, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("state must be valid UTF-8")
	}
	if err := validateUTF16Escapes(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))

	opening, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	if delim, ok := opening.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("state must be a JSON object keyed by job ID")
	}

	records := make(map[string]stateRecord)
	for dec.More() {
		idToken, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("decode job ID: %w", err)
		}
		id, ok := idToken.(string)
		if !ok {
			return nil, fmt.Errorf("state contains a non-string job ID")
		}
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("state contains an empty job ID")
		}
		if _, exists := records[id]; exists {
			return nil, fmt.Errorf("state contains duplicate job ID %q", id)
		}

		record, err := decodeRecord(dec, id)
		if err != nil {
			return nil, err
		}
		records[id] = record
	}

	closing, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("close state object: %w", err)
	}
	if delim, ok := closing.(json.Delim); !ok || delim != '}' {
		return nil, fmt.Errorf("state object is not properly closed")
	}
	if token, err := dec.Token(); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("decode trailing state data: %w", err)
		}
		return nil, fmt.Errorf("state contains trailing JSON value %v", token)
	}

	return records, nil
}

// encoding/json deliberately replaces unpaired UTF-16 surrogate escapes with
// U+FFFD. State validation must reject that lossy normalization so two distinct
// raw IDs cannot silently collapse to the same decoded key.
func validateUTF16Escapes(data []byte) error {
	inString := false
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || i+1 >= len(data) {
				continue
			}
			if data[i+1] != 'u' {
				i++
				continue
			}
			value, ok := decodeHex4(data, i+2)
			if !ok {
				continue // encoding/json reports malformed escape syntax later.
			}
			switch {
			case value >= 0xd800 && value <= 0xdbff:
				if i+12 > len(data) || data[i+6] != '\\' || data[i+7] != 'u' {
					return fmt.Errorf("state contains an unpaired UTF-16 surrogate escape")
				}
				low, ok := decodeHex4(data, i+8)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return fmt.Errorf("state contains an unpaired UTF-16 surrogate escape")
				}
				i += 11
			case value >= 0xdc00 && value <= 0xdfff:
				return fmt.Errorf("state contains an unpaired UTF-16 surrogate escape")
			default:
				i += 5
			}
		}
	}
	return nil
}

func decodeHex4(data []byte, start int) (uint16, bool) {
	if start+4 > len(data) {
		return 0, false
	}
	var value uint16
	for _, char := range data[start : start+4] {
		value <<= 4
		switch {
		case char >= '0' && char <= '9':
			value |= uint16(char - '0')
		case char >= 'a' && char <= 'f':
			value |= uint16(char-'a') + 10
		case char >= 'A' && char <= 'F':
			value |= uint16(char-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func decodeRecord(dec *json.Decoder, id string) (stateRecord, error) {
	opening, err := dec.Token()
	if err != nil {
		return stateRecord{}, fmt.Errorf("job %q: decode record: %w", id, err)
	}
	if delim, ok := opening.(json.Delim); !ok || delim != '{' {
		return stateRecord{}, fmt.Errorf("job %q: record must be an object", id)
	}

	var record stateRecord
	seen := make(map[string]bool, 4)
	for dec.More() {
		fieldToken, err := dec.Token()
		if err != nil {
			return stateRecord{}, fmt.Errorf("job %q: decode field: %w", id, err)
		}
		field, ok := fieldToken.(string)
		if !ok {
			return stateRecord{}, fmt.Errorf("job %q: record contains a non-string field", id)
		}
		if seen[field] {
			return stateRecord{}, fmt.Errorf("job %q: duplicate field %q", id, field)
		}
		seen[field] = true

		value, err := dec.Token()
		if err != nil {
			return stateRecord{}, fmt.Errorf("job %q field %q: decode value: %w", id, field, err)
		}
		switch field {
		case "first_seen":
			text, ok := value.(string)
			if !ok {
				return stateRecord{}, fmt.Errorf("job %q field %q must be a string", id, field)
			}
			record.firstSeen, err = time.Parse(time.RFC3339Nano, text)
			if err != nil || record.firstSeen.IsZero() {
				return stateRecord{}, fmt.Errorf("job %q field %q must be a non-zero RFC3339 timestamp", id, field)
			}
		case "title":
			record.title, ok = value.(string)
			if !ok {
				return stateRecord{}, fmt.Errorf("job %q field %q must be a string", id, field)
			}
		case "matched":
			record.matched, ok = value.(bool)
			if !ok {
				return stateRecord{}, fmt.Errorf("job %q field %q must be a boolean", id, field)
			}
		case "notified":
			record.notified, ok = value.(bool)
			if !ok {
				return stateRecord{}, fmt.Errorf("job %q field %q must be a boolean", id, field)
			}
		default:
			return stateRecord{}, fmt.Errorf("job %q: unknown field %q", id, field)
		}
	}

	closing, err := dec.Token()
	if err != nil {
		return stateRecord{}, fmt.Errorf("job %q: close record: %w", id, err)
	}
	if delim, ok := closing.(json.Delim); !ok || delim != '}' {
		return stateRecord{}, fmt.Errorf("job %q: record is not properly closed", id)
	}

	for _, field := range []string{"first_seen", "title", "matched", "notified"} {
		if !seen[field] {
			return stateRecord{}, fmt.Errorf("job %q: missing field %q", id, field)
		}
	}
	if strings.TrimSpace(record.title) == "" {
		return stateRecord{}, fmt.Errorf("job %q: title must not be empty", id)
	}
	if record.notified && !record.matched {
		return stateRecord{}, fmt.Errorf("job %q: notified record must also be matched", id)
	}
	return record, nil
}
