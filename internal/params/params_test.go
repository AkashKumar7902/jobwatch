package params

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func decode(t *testing.T, src string) (Map, error) {
	t.Helper()
	var m Map
	err := yaml.Unmarshal([]byte(src), &m)
	return m, err
}

func TestScalarsOfAnyTypeBecomeStrings(t *testing.T) {
	m, err := decode(t, "a: 1\nb: true\nc: 1.5\nd: text")
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"a": "1", "b": "true", "c": "1.5", "d": "text"} {
		if got := m.Get(key); got != want {
			t.Errorf("Get(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestNullValuesAreAbsent(t *testing.T) {
	m, err := decode(t, "a:\nb: x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Require("a"); err == nil {
		t.Error("null value should fail Require")
	}
	if got := m.GetDefault("a", "fallback"); got != "fallback" {
		t.Errorf("null value should use default, got %q", got)
	}
}

func TestNonScalarValuesAreRejected(t *testing.T) {
	if _, err := decode(t, "a: [1, 2]"); err == nil {
		t.Error("list value should be a config error")
	}
	if _, err := decode(t, "a:\n  nested: true"); err == nil {
		t.Error("nested mapping should be a config error")
	}
}

func TestEnvExpansion(t *testing.T) {
	t.Setenv("JOBWATCH_TEST_VALUE", "hello")
	m, err := decode(t, "set: ${JOBWATCH_TEST_VALUE}\nunset: ${JOBWATCH_TEST_MISSING}\nmixed: pre-${JOBWATCH_TEST_VALUE}-post")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Get("set"); got != "hello" {
		t.Errorf("set ref = %q, want hello", got)
	}
	if got := m.Get("unset"); got != "${JOBWATCH_TEST_MISSING}" {
		t.Errorf("unset ref should stay literal, got %q", got)
	}
	if got := m.Get("mixed"); got != "pre-hello-post" {
		t.Errorf("mixed = %q", got)
	}
}
