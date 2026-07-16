// Package params provides the loosely-typed key/value settings blocks that
// sources, matchers and notifiers receive from the config file. Keeping one
// shared type means every pluggable component is configured the same way:
//
//	- name: greenhouse
//	  params:
//	    board_token: gitlab
package params

import (
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Map is a bag of string settings. YAML scalars of any type (ints, bools,
// floats) are accepted and stored as their string form, so users can write
// `max_years: 1` without quoting.
type Map map[string]string

// UnmarshalYAML accepts scalar values of any YAML type and stores their
// literal text. Non-scalar values (lists, nested maps) are config mistakes
// and rejected at load time; null values ("key:" left empty) are treated as
// absent.
func (m *Map) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("params: expected a mapping, got a %s", kindName(node.Kind))
	}
	*m = Map{}
	// A yaml mapping node stores [key1, val1, key2, val2, ...].
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		if v.Kind != yaml.ScalarNode {
			return fmt.Errorf("param %q: expected a scalar value, got a %s", k.Value, kindName(v.Kind))
		}
		if v.Tag == "!!null" {
			continue
		}
		(*m)[k.Value] = v.Value
	}
	return nil
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.SequenceNode:
		return "list"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	default:
		return "document"
	}
}

// Get returns the value for key, or "" when absent.
func (m Map) Get(key string) string { return m[key] }

// GetDefault returns the value for key, or def when absent/empty.
func (m Map) GetDefault(key, def string) string {
	if v, ok := m[key]; ok && v != "" {
		return v
	}
	return def
}

// Require returns the value for key or an error naming the missing key, so
// config mistakes surface with a clear message instead of a silent default.
func (m Map) Require(key string) (string, error) {
	v, ok := m[key]
	if !ok || v == "" {
		return "", fmt.Errorf("missing required param %q", key)
	}
	return v, nil
}

// Int parses key as an integer, returning def when the key is absent.
func (m Map) Int(key string, def int) (int, error) {
	v, ok := m[key]
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("param %q: expected integer, got %q", key, v)
	}
	return n, nil
}

// Float parses key as a float, returning def when the key is absent.
func (m Map) Float(key string, def float64) (float64, error) {
	v, ok := m[key]
	if !ok || v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("param %q: expected number, got %q", key, v)
	}
	return f, nil
}

// Bool parses key as a boolean, returning def when the key is absent.
func (m Map) Bool(key string, def bool) (bool, error) {
	v, ok := m[key]
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("param %q: expected true/false, got %q", key, v)
	}
	return b, nil
}
