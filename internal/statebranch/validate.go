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

	// statePrefix and previousStatePrefix are lifted out of the marker object
	// on source-marker records. They are the only nested values the publish
	// gate reads, because they are what authorizes a removal: see
	// removalIsExplained in state.go.
	statePrefix         string
	previousStatePrefix string
}

// Board-health bookkeeping lives on reserved record keys, never on postings.
// The validator re-states that contract instead of trusting the writer: a
// health object attached to a posting would multiply the per-record cost
// across every job we have ever seen, and a stray key under the source-marker
// namespace would look like "this board is already baselined" and suppress a
// legitimate seeding pass.
const (
	healthKeyPrefix = "__jobwatch_health__/"
	runKeyID        = "__jobwatch_health__/@run"

	// sourceMarkerKeyPrefix is the namespace of "we have adopted this board"
	// records. Its own contract is the mirror image of health's: the marker
	// object may appear ONLY here, because a marker on a posting would be
	// meaningless, and a marker record anywhere else would be read as an
	// adopted board and suppress a legitimate seeding pass.
	sourceMarkerKeyPrefix = "__jobwatch_source__/"
)

const (
	// Window caps mirror internal/health. They are duplicated rather than
	// imported on purpose: the point of validation is that a producer bug
	// cannot certify itself, which it would if both sides read one constant.
	maxRecentCounts  = 48
	maxNonzeroCounts = 16

	// Fetch errors are remote-controlled text. The producer clips them to a
	// couple of hundred bytes; this is a loose sanity ceiling that keeps a
	// hostile or malfunctioning endpoint from growing the state file, not a
	// mirror of the producer's exact clip.
	maxLastErrBytes = 1024

	// Counters are folded forward one run at a time and can only ever be
	// small. Bounding them keeps a corrupt file from decoding into values
	// that make later arithmetic meaningless.
	maxHealthCount = 1 << 31

	// State prefixes are built from config params and end in "/". The longest
	// one in the live catalog is under 100 bytes; this ceiling exists only so
	// a corrupt file cannot hand the publish gate a megabyte-long prefix to
	// scan every key against.
	maxPrefixBytes = 512
)

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
	seen := make(map[string]bool, 6)
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

		// Nested objects have to be dispatched before the value token is
		// read, because for them that token is only the opening brace.
		switch field {
		case "health":
			if !strings.HasPrefix(id, healthKeyPrefix) {
				return stateRecord{}, fmt.Errorf("job %q: health may only appear on %s records", id, healthKeyPrefix)
			}
			if err := decodeHealth(dec, id); err != nil {
				return stateRecord{}, err
			}
			continue
		case "run":
			if id != runKeyID {
				return stateRecord{}, fmt.Errorf("job %q: run may only appear on the %s record", id, runKeyID)
			}
			if err := decodeRun(dec, id); err != nil {
				return stateRecord{}, err
			}
			continue
		case "marker":
			if !strings.HasPrefix(id, sourceMarkerKeyPrefix) {
				return stateRecord{}, fmt.Errorf("job %q: marker may only appear on %s records", id, sourceMarkerKeyPrefix)
			}
			if err := decodeMarker(dec, id, &record); err != nil {
				return stateRecord{}, err
			}
			continue
		}

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

// decodeHealth validates one board-health object. Health rides on an ordinary
// record, so the record's own invariants (non-zero first_seen, non-empty
// title, notified implies matched) still apply unchanged to health keys — the
// writer gives them a real title and timestamp rather than an exemption.
//
// Every field is optional: a decoder that omits one gets Go's zero value, and
// zero is a meaningful state here (a board that has never returned a posting
// genuinely has no last_non_empty). Unknown fields are still rejected, so
// adding a field to store.Health without extending this function stops the
// state branch — which is the intended failure, loud and immediate.
func decodeHealth(dec *json.Decoder, id string) error {
	if err := openNested(dec, id, "health"); err != nil {
		return err
	}
	seen := make(map[string]bool, 20)
	for dec.More() {
		field, err := nestedField(dec, id, "health", seen)
		if err != nil {
			return err
		}

		// Arrays, like nested objects, must be dispatched before the value
		// token is read.
		switch field {
		case "recent":
			if err := decodeCounts(dec, id, "health.recent", maxRecentCounts); err != nil {
				return err
			}
			continue
		case "nonzero":
			if err := decodeCounts(dec, id, "health.nonzero", maxNonzeroCounts); err != nil {
				return err
			}
			continue
		}

		value, err := dec.Token()
		if err != nil {
			return fmt.Errorf("job %q field %q: decode value: %w", id, "health."+field, err)
		}
		path := "health." + field
		switch field {
		case "company", "src_type":
			if _, err := decodeString(id, path, value); err != nil {
				return err
			}
		case "last_err":
			text, err := decodeString(id, path, value)
			if err != nil {
				return err
			}
			if len(text) > maxLastErrBytes {
				return fmt.Errorf("job %q field %q must be at most %d bytes", id, path, maxLastErrBytes)
			}
		case "kind":
			text, err := decodeString(id, path, value)
			if err != nil {
				return err
			}
			// A closed vocabulary: an unrecognized kind would either be
			// silently dropped by a reader or mailed out as a condition
			// nobody can act on.
			switch text {
			case "", "dead", "erroring", "stillborn":
			default:
				return fmt.Errorf("job %q field %q must be \"\", \"dead\", \"erroring\" or \"stillborn\", got %q", id, path, text)
			}
		case "baselined":
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("job %q field %q must be a boolean", id, path)
			}
		case "first_fetch", "last_ok", "last_non_empty", "zero_since", "err_since",
			"raised_at", "sent_at", "eligible_at":
			if err := decodeOptionalTimestamp(id, path, value); err != nil {
				return err
			}
		case "last_non_empty_n", "non_empty_days", "fetches", "typical", "zero_runs", "err_runs":
			if _, err := decodeCount(id, path, value); err != nil {
				return err
			}
		default:
			return fmt.Errorf("job %q: unknown health field %q", id, field)
		}
	}
	return closeNested(dec, id, "health")
}

// decodeMarker validates one adopted-board object and lifts the two prefixes
// the publish gate needs out of it.
//
// Unlike health, this object is not merely descriptive: state_prefix decides
// whether an orphaned board is swept silently or reported as a migration, and
// previous_state_prefix is what AUTHORIZES a set of removals from the state
// file. So the fields are bounded — they come from the user's config, which is
// trusted, but a corrupt or hand-edited file must not be able to make the gate
// authorize a removal it cannot even print.
func decodeMarker(dec *json.Decoder, id string, record *stateRecord) error {
	if err := openNested(dec, id, "marker"); err != nil {
		return err
	}
	seen := make(map[string]bool, 5)
	for dec.More() {
		field, err := nestedField(dec, id, "marker", seen)
		if err != nil {
			return err
		}
		value, err := dec.Token()
		if err != nil {
			return fmt.Errorf("job %q field %q: decode value: %w", id, "marker."+field, err)
		}
		path := "marker." + field
		switch field {
		case "state_prefix", "previous_state_prefix", "moved_to":
			text, err := decodeString(id, path, value)
			if err != nil {
				return err
			}
			if len(text) > maxPrefixBytes {
				return fmt.Errorf("job %q field %q must be at most %d bytes", id, path, maxPrefixBytes)
			}
			switch field {
			case "state_prefix":
				record.statePrefix = text
			case "previous_state_prefix":
				record.previousStatePrefix = text
			}
		case "announced_at", "orphaned_at":
			if err := decodeOptionalTimestamp(id, path, value); err != nil {
				return err
			}
		default:
			return fmt.Errorf("job %q: unknown marker field %q", id, field)
		}
	}
	if record.previousStatePrefix != "" && record.statePrefix == "" {
		// A previous prefix with nowhere to point is exactly the shape a
		// removal-authorizing rule must not accept: it would name a set of
		// keys to drop and no keys to find them under.
		return fmt.Errorf("job %q: marker.previous_state_prefix requires a non-empty marker.state_prefix", id)
	}
	if record.previousStatePrefix != "" && record.previousStatePrefix == record.statePrefix {
		return fmt.Errorf("job %q: marker.previous_state_prefix must differ from marker.state_prefix", id)
	}
	return closeNested(dec, id, "marker")
}

// decodeRun validates the fleet-wide singleton object.
func decodeRun(dec *json.Decoder, id string) error {
	if err := openNested(dec, id, "run"); err != nil {
		return err
	}
	seen := make(map[string]bool, 3)
	for dec.More() {
		field, err := nestedField(dec, id, "run", seen)
		if err != nil {
			return err
		}
		value, err := dec.Token()
		if err != nil {
			return fmt.Errorf("job %q field %q: decode value: %w", id, "run."+field, err)
		}
		path := "run." + field
		switch field {
		case "last_run_at", "digest_sent_at", "all_fail_sent_at":
			if err := decodeOptionalTimestamp(id, path, value); err != nil {
				return err
			}
		case "all_fail_runs", "evaluated_since_digest", "matched_since_digest", "deferred_since_digest":
			if _, err := decodeCount(id, path, value); err != nil {
				return err
			}
		default:
			return fmt.Errorf("job %q: unknown run field %q", id, field)
		}
	}
	return closeNested(dec, id, "run")
}

func openNested(dec *json.Decoder, id, name string) error {
	opening, err := dec.Token()
	if err != nil {
		return fmt.Errorf("job %q field %q: decode value: %w", id, name, err)
	}
	if delim, ok := opening.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("job %q field %q must be an object", id, name)
	}
	return nil
}

func closeNested(dec *json.Decoder, id, name string) error {
	closing, err := dec.Token()
	if err != nil {
		return fmt.Errorf("job %q field %q: close object: %w", id, name, err)
	}
	if delim, ok := closing.(json.Delim); !ok || delim != '}' {
		return fmt.Errorf("job %q field %q is not properly closed", id, name)
	}
	return nil
}

// nestedField reads one field name inside a nested object, rejecting the same
// duplicate-key ambiguity the top level rejects.
func nestedField(dec *json.Decoder, id, name string, seen map[string]bool) (string, error) {
	token, err := dec.Token()
	if err != nil {
		return "", fmt.Errorf("job %q field %q: decode field: %w", id, name, err)
	}
	field, ok := token.(string)
	if !ok {
		return "", fmt.Errorf("job %q field %q contains a non-string field", id, name)
	}
	if seen[field] {
		return "", fmt.Errorf("job %q: duplicate field %q", id, name+"."+field)
	}
	seen[field] = true
	return field, nil
}

func decodeString(id, path string, value json.Token) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("job %q field %q must be a string", id, path)
	}
	return text, nil
}

// decodeOptionalTimestamp accepts the zero time, unlike first_seen. Health
// timestamps record events that may not have happened yet — a board we have
// never seen answer has no last_ok — and forbidding that would force the
// writer to invent one.
func decodeOptionalTimestamp(id, path string, value json.Token) error {
	text, err := decodeString(id, path, value)
	if err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339Nano, text); err != nil {
		return fmt.Errorf("job %q field %q must be an RFC3339 timestamp", id, path)
	}
	return nil
}

func decodeCount(id, path string, value json.Token) (int, error) {
	// encoding/json decodes every JSON number as float64, so "integral" and
	// "in range" have to be asserted rather than assumed.
	number, ok := value.(float64)
	if !ok {
		return 0, fmt.Errorf("job %q field %q must be a number", id, path)
	}
	if number != float64(int64(number)) || number < 0 || number > maxHealthCount {
		return 0, fmt.Errorf("job %q field %q must be a whole count between 0 and %d", id, path, maxHealthCount)
	}
	return int(number), nil
}

// decodeCounts validates a bounded array of posting counts. A JSON null is
// accepted because that is how Go marshals a nil slice, which is what a board
// observed only through failures has.
func decodeCounts(dec *json.Decoder, id, path string, limit int) error {
	opening, err := dec.Token()
	if err != nil {
		return fmt.Errorf("job %q field %q: decode value: %w", id, path, err)
	}
	if opening == nil {
		return nil
	}
	if delim, ok := opening.(json.Delim); !ok || delim != '[' {
		return fmt.Errorf("job %q field %q must be an array of counts", id, path)
	}
	n := 0
	for dec.More() {
		value, err := dec.Token()
		if err != nil {
			return fmt.Errorf("job %q field %q: decode element: %w", id, path, err)
		}
		if _, err := decodeCount(id, path, value); err != nil {
			return err
		}
		n++
		if n > limit {
			return fmt.Errorf("job %q field %q holds more than %d counts", id, path, limit)
		}
	}
	closing, err := dec.Token()
	if err != nil {
		return fmt.Errorf("job %q field %q: close array: %w", id, path, err)
	}
	if delim, ok := closing.(json.Delim); !ok || delim != ']' {
		return fmt.Errorf("job %q field %q is not properly closed", id, path)
	}
	return nil
}
